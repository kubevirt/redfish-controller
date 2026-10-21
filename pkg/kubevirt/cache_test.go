/*
 * This file is part of the KubeVirt Redfish project
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 *
 * Copyright 2025 KubeVirt Redfish project and its authors.
 *
 */

package kubevirt

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8stesting "k8s.io/client-go/testing"
)

func testVM(namespace, name string, lbls map[string]string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetAPIVersion("kubevirt.io/v1")
	u.SetKind("VirtualMachine")
	u.SetNamespace(namespace)
	u.SetName(name)
	u.SetLabels(lbls)
	return u
}

// newCacheTestClient builds a Client over a fake dynamic client, wrapped by
// the write tracker exactly as the constructors do.
func newCacheTestClient(t *testing.T, appConfig interface{}, objs ...runtime.Object) (*Client, *dynamicfake.FakeDynamicClient) {
	t.Helper()
	fake := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
		vmGVR:  "VirtualMachineList",
		vmiGVR: "VirtualMachineInstanceList",
	}, objs...)
	writes := newWriteTracker()
	c := &Client{
		dynamicClient: newTrackingDynamicClient(fake, writes),
		writes:        writes,
		timeout:       5 * time.Second,
		appConfig:     appConfig,
	}
	c.metrics.operationCounts = make(map[string]int64)
	c.metrics.operationTimes = make(map[string]time.Duration)
	return c, fake
}

func waitSynced(t *testing.T, c *Client, namespace string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		_, vmOK := c.currentVMCache().informer(vmGVR, namespace)
		_, vmiOK := c.currentVMCache().informer(vmiGVR, namespace)
		if vmOK && vmiOK {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("cache for namespace %s did not sync", namespace)
}

func failGets(fake *dynamicfake.FakeDynamicClient, msg string) {
	fake.PrependReactor("get", "virtualmachines", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New(msg)
	})
	fake.PrependReactor("get", "virtualmachineinstances", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New(msg)
	})
}

func TestVMCacheServesReads(t *testing.T) {
	c, fake := newCacheTestClient(t, nil,
		testVM("ns", "vm-a", map[string]string{"redfish-enabled": "true"}),
		testVM("ns", "vm-b", nil),
	)
	// Duplicate and empty namespaces must be tolerated.
	c.restartVMCache([]string{"ns", "ns", ""})
	defer c.stopVMCache()
	waitSynced(t, c, "ns")

	// From here on any GET that reaches the API server fails, so a
	// successful read proves the cache is on the path.
	failGets(fake, "unexpected API server GET")

	vm, err := c.getVMForRead("ns", "vm-a")
	if err != nil {
		t.Fatalf("cached read failed: %v", err)
	}
	if vm.Name != "vm-a" || vm.Namespace != "ns" {
		t.Fatalf("unexpected VM from cache: %s/%s", vm.Namespace, vm.Name)
	}

	// A synced cache's not-found is authoritative: no fallback GET.
	if _, err := c.getVMForRead("ns", "missing"); !apierrors.IsNotFound(err) {
		t.Fatalf("expected NotFound from cache, got %v", err)
	}

	// No VMI exists for vm-a, which callers read as power state Off.
	if _, err := c.getVMIForRead("ns", "vm-a"); !apierrors.IsNotFound(err) {
		t.Fatalf("expected NotFound VMI from cache, got %v", err)
	}

	// The direct path still goes to the API server.
	if _, err := c.GetVM("ns", "vm-a"); err == nil {
		t.Fatal("expected direct GetVM to reach the API server and fail")
	}

	// A namespace the cache does not cover is not served.
	if _, err := c.getVMForRead("other", "vm-a"); err == nil || !strings.Contains(err.Error(), "unexpected API server GET") {
		t.Fatalf("expected fallback to API server for uncovered namespace, got %v", err)
	}

	// List honours the label selector and reports coverage.
	names, ok := c.listVMNamesFromCache("ns", labels.SelectorFromSet(labels.Set{"redfish-enabled": "true"}))
	if !ok || !reflect.DeepEqual(names, []string{"vm-a"}) {
		t.Fatalf("selector list: ok=%v names=%v", ok, names)
	}
	names, ok = c.listVMNamesFromCache("ns", labels.Everything())
	if !ok || len(names) != 2 {
		t.Fatalf("everything list: ok=%v names=%v", ok, names)
	}
	if _, ok := c.listVMNamesFromCache("other", labels.Everything()); ok {
		t.Fatal("uncovered namespace must not be served from cache")
	}
}

// fakeClock is the write tracker's clock in tests.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock() *fakeClock { return &fakeClock{now: time.Unix(1_700_000_000, 0)} }

func (f *fakeClock) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

func (f *fakeClock) Advance(d time.Duration) {
	f.mu.Lock()
	f.now = f.now.Add(d)
	f.mu.Unlock()
}

func withRV(u *unstructured.Unstructured, rv string) *unstructured.Unstructured {
	u.SetResourceVersion(rv)
	return u
}

// newWriteTestClient returns a synced client over VMs vm-a and vm-b at
// resourceVersion 1, with a fake clock and every API server GET failing, so
// readsFromCache can tell a cached read from a direct one.
func newWriteTestClient(t *testing.T) (*Client, *dynamicfake.FakeDynamicClient, *fakeClock) {
	t.Helper()
	c, fake := newCacheTestClient(t, nil,
		withRV(testVM("ns", "vm-a", nil), "1"),
		withRV(testVM("ns", "vm-b", nil), "1"),
	)
	clock := newFakeClock()
	c.writes.now = clock.Now
	c.restartVMCache([]string{"ns"})
	t.Cleanup(c.stopVMCache)
	waitSynced(t, c, "ns")
	failGets(fake, "api server GET")
	return c, fake, clock
}

func readsFromCache(c *Client, name string) bool {
	_, err := c.getVMForRead("ns", name)
	return err == nil || !strings.Contains(err.Error(), "api server GET")
}

// eventually polls cond, because informer event delivery is asynchronous
// even over the fake client.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting until %s", what)
}

// acceptUnseen makes the API server acknowledge a verb on VM name, answering
// with resourceVersion rv, without storing anything. The informer never hears
// of it: the state of a cache whose watch lags behind our write.
func acceptUnseen(fake *dynamicfake.FakeDynamicClient, verb, name, rv string) {
	fake.PrependReactor(verb, "virtualmachines", func(a k8stesting.Action) (bool, runtime.Object, error) {
		var actionName string
		switch a := a.(type) {
		case k8stesting.PatchAction:
			actionName = a.GetName()
		case k8stesting.DeleteAction:
			actionName = a.GetName()
		}
		if actionName != name {
			return false, nil, nil
		}
		if verb == "delete" {
			return true, nil, nil
		}
		return true, withRV(testVM("ns", name, nil), rv), nil
	})
}

func patchVM(t *testing.T, c *Client, name string) {
	t.Helper()
	if _, err := c.dynamicClient.Resource(vmGVR).Namespace("ns").Patch(context.Background(), name,
		types.MergePatchType, []byte(`{"metadata":{"labels":{"x":"y"}}}`), metav1.PatchOptions{}); err != nil {
		t.Fatalf("patch %s: %v", name, err)
	}
}

func TestVMCacheBypassUntilOwnWriteIsObserved(t *testing.T) {
	c, fake, clock := newWriteTestClient(t)
	acceptUnseen(fake, "patch", "vm-a", "2")
	patchVM(t, c, "vm-a")

	if readsFromCache(c, "vm-a") {
		t.Fatal("a read after our own write must bypass a cache that has not seen it")
	}
	if !readsFromCache(c, "vm-b") {
		t.Fatal("reads of other VMs must still come from the cache")
	}

	// No fixed delay ends the bypass: while the cache lags, reads stay direct.
	clock.Advance(writeBypassMax - time.Second)
	if readsFromCache(c, "vm-a") {
		t.Fatal("bypass ended before the cache saw our write")
	}

	if err := fake.Tracker().Update(vmGVR, withRV(testVM("ns", "vm-a", nil), "2"), "ns"); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the cache serves vm-a once it holds our version", func() bool { return readsFromCache(c, "vm-a") })
	vm, err := c.getVMForRead("ns", "vm-a")
	if err != nil || vm.ResourceVersion != "2" {
		t.Fatalf("cached read: rv=%v err=%v", vm, err)
	}
}

func TestVMCacheBypassEndsWhenOwnWriteIsSuperseded(t *testing.T) {
	c, fake, _ := newWriteTestClient(t)
	acceptUnseen(fake, "patch", "vm-a", "2")
	patchVM(t, c, "vm-a")

	// The informer sees our version and then a newer one written by someone
	// else. The cache never holds version 2 when we read, but it has seen it.
	for _, rv := range []string{"2", "3"} {
		if err := fake.Tracker().Update(vmGVR, withRV(testVM("ns", "vm-a", nil), rv), "ns"); err != nil {
			t.Fatal(err)
		}
	}
	eventually(t, "the cache serves vm-a after seeing our version", func() bool { return readsFromCache(c, "vm-a") })
	vm, err := c.getVMForRead("ns", "vm-a")
	if err != nil || vm.ResourceVersion != "3" {
		t.Fatalf("cached read: vm=%v err=%v", vm, err)
	}
}

func TestVMCacheNoBypassWhenOwnWriteWasObservedBeforePatchReturned(t *testing.T) {
	// On a real API server the watch event for our write can reach the
	// informer before Patch returns, and virt-controller writes a newer status
	// right after. The cache has seen our version before we record the write.
	c, fake, _ := newWriteTestClient(t)
	for _, rv := range []string{"2", "3"} {
		if err := fake.Tracker().Update(vmGVR, withRV(testVM("ns", "vm-a", nil), rv), "ns"); err != nil {
			t.Fatal(err)
		}
	}
	// Wait on the handler, not the store: the informer updates the store
	// before it notifies handlers.
	eventually(t, "the informer reported versions 2 and 3", func() bool {
		c.writes.mu.Lock()
		defer c.writes.mu.Unlock()
		key := writeKey(vmGVR, "ns", "vm-a")
		return c.writes.seenLocked(key, "2") && c.writes.seenLocked(key, "3")
	})

	acceptUnseen(fake, "patch", "vm-a", "2")
	patchVM(t, c, "vm-a")
	if !readsFromCache(c, "vm-a") {
		t.Fatal("a write the cache saw before Patch returned must not bypass it")
	}
}

func TestWriteTrackerForgetsDeletedObjects(t *testing.T) {
	w := newWriteTracker()
	w.observed(vmGVR, "ns", "vm-a", "2")
	w.observed(vmiGVR, "ns", "vm-a", "5")
	w.forget(vmiGVR, "ns", "vm-a")

	w.mu.Lock()
	defer w.mu.Unlock()
	if _, ok := w.seen[writeKey(vmiGVR, "ns", "vm-a")]; ok {
		t.Fatal("versions of a deleted object must be dropped")
	}
	if _, ok := w.seen[writeKey(vmGVR, "ns", "vm-a")]; !ok {
		t.Fatal("versions of other objects must be kept")
	}
}

func TestWriteTrackerSeenHistoryIsBounded(t *testing.T) {
	w := newWriteTracker()
	clock := newFakeClock()
	w.now = clock.Now
	for i := 0; i < 3*seenVersionsMax; i++ {
		w.observed(vmGVR, "ns", "vm-a", fmt.Sprint(i))
	}
	w.mu.Lock()
	n := len(w.seen[writeKey(vmGVR, "ns", "vm-a")])
	w.mu.Unlock()
	if n > seenVersionsMax {
		t.Fatalf("kept %d versions, want at most %d", n, seenVersionsMax)
	}

	clock.Advance(writeBypassMax + time.Second)
	w.observed(vmGVR, "ns", "vm-b", "1")
	w.mu.Lock()
	_, stale := w.seen[writeKey(vmGVR, "ns", "vm-a")]
	w.mu.Unlock()
	if stale {
		t.Fatal("versions older than writeBypassMax must be pruned")
	}
}

func TestVMCacheNoOpWriteDoesNotBypass(t *testing.T) {
	// The API server does not bump resourceVersion or emit a watch event for
	// a write that changes nothing; the cache already holds that version.
	c, fake, _ := newWriteTestClient(t)
	acceptUnseen(fake, "patch", "vm-a", "1")
	patchVM(t, c, "vm-a")
	if !readsFromCache(c, "vm-a") {
		t.Fatal("a no-op write must not bypass a cache that already holds its version")
	}
}

func TestVMCacheBypassIsBounded(t *testing.T) {
	c, fake, clock := newWriteTestClient(t)
	acceptUnseen(fake, "patch", "vm-a", "2")
	patchVM(t, c, "vm-a")
	clock.Advance(writeBypassMax + time.Second)
	if !readsFromCache(c, "vm-a") {
		t.Fatal("a write the cache never sees must stop forcing direct reads after writeBypassMax")
	}
	c.writes.mu.Lock()
	n := len(c.writes.pending)
	c.writes.mu.Unlock()
	if n != 0 {
		t.Fatalf("expired write still tracked: %d entries", n)
	}
}

func TestVMCacheBypassAfterOwnDelete(t *testing.T) {
	c, fake, _ := newWriteTestClient(t)
	acceptUnseen(fake, "delete", "vm-a", "")
	acceptUnseen(fake, "delete", "vm-b", "")
	for _, name := range []string{"vm-a", "vm-b"} {
		if err := c.dynamicClient.Resource(vmGVR).Namespace("ns").Delete(context.Background(), name, metav1.DeleteOptions{}); err != nil {
			t.Fatal(err)
		}
		if readsFromCache(c, name) {
			t.Fatalf("a read of %s after our delete must bypass a cache that still holds it", name)
		}
	}

	// vm-a is removed outright; vm-b is held by a finalizer and only gains a
	// deletionTimestamp. Either one shows the cache has seen the delete.
	if err := fake.Tracker().Delete(vmGVR, "ns", "vm-a"); err != nil {
		t.Fatal(err)
	}
	terminating := withRV(testVM("ns", "vm-b", nil), "2")
	now := metav1.Now()
	terminating.SetDeletionTimestamp(&now)
	if err := fake.Tracker().Update(vmGVR, terminating, "ns"); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the cache answers NotFound for vm-a", func() bool {
		_, err := c.getVMForRead("ns", "vm-a")
		return apierrors.IsNotFound(err)
	})
	eventually(t, "the cache serves the terminating vm-b", func() bool { return readsFromCache(c, "vm-b") })
}

func TestVMCacheFailedWriteDoesNotBypass(t *testing.T) {
	c, fake, _ := newWriteTestClient(t)
	fake.PrependReactor("patch", "virtualmachines", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("patch rejected")
	})
	if _, err := c.dynamicClient.Resource(vmGVR).Namespace("ns").Patch(context.Background(), "vm-a",
		types.MergePatchType, []byte(`{}`), metav1.PatchOptions{}); err == nil {
		t.Fatal("expected the patch to fail")
	}
	if !readsFromCache(c, "vm-a") {
		t.Fatal("a failed write must not bypass the cache")
	}
}

type cacheOff struct{}

func (cacheOff) GetVMCacheEnabled() bool { return false }

func TestVMCacheDisabledByConfig(t *testing.T) {
	c, _ := newCacheTestClient(t, cacheOff{}, testVM("ns", "vm-a", nil))
	c.restartVMCache([]string{"ns"})
	defer c.stopVMCache()
	if c.currentVMCache() != nil {
		t.Fatal("cache must not start when disabled by config")
	}
	vm, err := c.getVMForRead("ns", "vm-a")
	if err != nil || vm.Name != "vm-a" {
		t.Fatalf("direct read with cache disabled: vm=%v err=%v", vm, err)
	}
}

func TestVMCacheRestartAndStop(t *testing.T) {
	c, _ := newCacheTestClient(t, nil, testVM("ns1", "a", nil), testVM("ns2", "b", nil))
	c.restartVMCache([]string{"ns1"})
	waitSynced(t, c, "ns1")
	if _, ok := c.currentVMCache().informer(vmGVR, "ns2"); ok {
		t.Fatal("ns2 should not be covered yet")
	}
	// Config reload: a new namespace set replaces the old cache without deadlocking.
	c.restartVMCache([]string{"ns2"})
	waitSynced(t, c, "ns2")
	if _, ok := c.currentVMCache().informer(vmGVR, "ns1"); ok {
		t.Fatal("ns1 should no longer be covered")
	}
	c.stopVMCache()
	if c.currentVMCache() != nil {
		t.Fatal("cache should be nil after stop")
	}
	// Reads after stop fall back to the API server.
	if _, err := c.getVMForRead("ns2", "b"); err != nil {
		t.Fatalf("direct read after stop: %v", err)
	}
}

func TestFilterVMNames(t *testing.T) {
	in := []string{"c", "a", "b"}
	if got := filterVMNames(in, nil); !reflect.DeepEqual(got, []string{"a", "b", "c"}) {
		t.Fatalf("nil selector: %v", got)
	}
	if got := filterVMNames(in, &VMSelectorConfig{Names: []string{"b", "zzz"}}); !reflect.DeepEqual(got, []string{"b"}) {
		t.Fatalf("names filter: %v", got)
	}
	if got := filterVMNames(nil, &VMSelectorConfig{Names: []string{"b"}}); got != nil {
		t.Fatalf("empty input should stay nil, got %v", got)
	}
}
