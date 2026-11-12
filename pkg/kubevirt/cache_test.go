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
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
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

func withBootTarget(u *unstructured.Unstructured, target string) *unstructured.Unstructured {
	u.SetAnnotations(map[string]string{"redfish.boot.source.override.target": target})
	return u
}

func newCacheTestClient(t *testing.T, objs ...runtime.Object) (*Client, *dynamicfake.FakeDynamicClient) {
	t.Helper()
	fake := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
		vmGVR:  "VirtualMachineList",
		vmiGVR: "VirtualMachineInstanceList",
	}, objs...)
	c := &Client{
		dynamicClient: fake,
		timeout:       5 * time.Second,
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
	c, fake := newCacheTestClient(t,
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

// The boot-device PATCH handler writes the VM and then reads it back to build
// its response. That read-back must come from the API server, because the
// watch event for the write may not have reached the cache yet.
func TestGetVMBootOptionsLiveReadsAPIServer(t *testing.T) {
	c, fake := newCacheTestClient(t, withBootTarget(testVM("ns", "vm-a", nil), "Hdd"))
	c.restartVMCache([]string{"ns"})
	defer c.stopVMCache()
	waitSynced(t, c, "ns")

	// The API server holds a newer version the watch has not delivered.
	live := withBootTarget(testVM("ns", "vm-a", nil), "Cd")
	fake.PrependReactor("get", "virtualmachines", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, live, nil
	})

	cached, err := c.GetVMBootOptions("ns", "vm-a")
	if err != nil {
		t.Fatalf("cached boot options: %v", err)
	}
	if cached["bootSourceOverrideTarget"] != "Hdd" {
		t.Fatalf("cached read should still hold the old target, got %v", cached["bootSourceOverrideTarget"])
	}

	fresh, err := c.GetVMBootOptionsLive("ns", "vm-a")
	if err != nil {
		t.Fatalf("live boot options: %v", err)
	}
	if fresh["bootSourceOverrideTarget"] != "Cd" {
		t.Fatalf("live read must come from the API server, got %v", fresh["bootSourceOverrideTarget"])
	}
}

func TestVMCacheRestartAndStop(t *testing.T) {
	c, _ := newCacheTestClient(t, testVM("ns1", "a", nil), testVM("ns2", "b", nil))
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

// A client built without a dynamic client, as the server tests do, must not
// start a cache or panic.
func TestVMCacheWithoutDynamicClient(t *testing.T) {
	c := &Client{}
	c.restartVMCache([]string{"ns"})
	if c.currentVMCache() != nil {
		t.Fatal("cache must not start without a dynamic client")
	}
	c.stopVMCache()
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
