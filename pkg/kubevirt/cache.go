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

// A read-only cache of VirtualMachine and VirtualMachineInstance objects.
//
// Every Redfish request fans out to several Kubernetes API calls, and nearly
// all of them are reads of the same VM/VMI objects (a GET System is five GETs,
// a boot-device PATCH is eleven). With many BareMetalHosts polling at once,
// those reads are what saturate the client-side rate limiter. The informers
// here keep one watch per namespace per resource open instead, so the
// read-only Redfish paths are served from memory.
//
// Only read-only inventory lookups use the cache (see getVMForRead and its
// callers). Every read-modify-write path still reads from the API server,
// because a patch computed from a stale object would clobber fields written
// moments earlier. Two further rules keep cached reads correct:
//
//   - A cache that has not finished its initial sync is not consulted at all,
//     and once it has synced, its answer (including "not found") is
//     authoritative. Falling back to a GET on every miss would cost a GET on
//     every power poll of a stopped VM, which has no VMI, and that is the
//     dominant state of a BMH during inspection and provisioning.
//   - After this process writes a VM or VMI, reads of that object bypass the
//     cache until the informer has received the resourceVersion the write
//     returned (or, for a delete, until the cached copy is gone or marked
//     for deletion). A read that follows our own write therefore never sees
//     the pre-write state, however far the watch lags. writeBypassMax bounds
//     this, so a watch that broke and re-listed past our version cannot force
//     direct reads forever.
//
// Writes made by anyone else (oc edit, KubeVirt's own controllers) reach the
// cache when their watch event arrives, usually milliseconds later. Until then
// a cached read returns the previous version. That is the price of the cache;
// it is acceptable because no write path reads from it.

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/informers"
	toolscache "k8s.io/client-go/tools/cache"
	kubevirtv1 "kubevirt.io/api/core/v1"

	"github.com/kubevirt/redfish-controller/pkg/logger"
)

var (
	vmGVR  = schema.GroupVersionResource{Group: "kubevirt.io", Version: "v1", Resource: "virtualmachines"}
	vmiGVR = schema.GroupVersionResource{Group: "kubevirt.io", Version: "v1", Resource: "virtualmachineinstances"}
)

// writeBypassMax caps how long a write the cache has not yet reflected keeps
// reads of that object direct. On a healthy API server the informer sees our
// write within milliseconds and the bypass ends then; this only matters when
// the watch broke and the re-list skipped past our version.
const writeBypassMax = 30 * time.Second

// seenVersionsMax caps how many recently stored versions the write tracker
// remembers per object.
const seenVersionsMax = 32

// pendingWrite is a write by this process that the cache may not reflect yet.
type pendingWrite struct {
	resourceVersion string // returned by the API server; empty for a delete
	deleted         bool
	at              time.Time
}

type seenVersion struct {
	resourceVersion string
	at              time.Time
}

// writeTracker remembers the VM and VMI writes by this process that the cache
// has not reflected yet.
//
// It also remembers the versions the informers stored recently. On a real API
// server the watch event for our own write often reaches the informer before
// the write call returns, and KubeVirt's controllers write a newer status
// within milliseconds. Without that history the write would be recorded after
// the cache had already seen it, and the cache would hold only the newer
// version, so reads would bypass it until writeBypassMax.
type writeTracker struct {
	mu        sync.Mutex
	pending   map[string]pendingWrite
	seen      map[string][]seenVersion
	lastSweep time.Time
	now       func() time.Time
}

func newWriteTracker() *writeTracker {
	return &writeTracker{
		pending: make(map[string]pendingWrite),
		seen:    make(map[string][]seenVersion),
		now:     time.Now,
	}
}

func writeKey(gvr schema.GroupVersionResource, namespace, name string) string {
	return gvr.Resource + "/" + namespace + "/" + name
}

// sweepLocked drops pending writes and seen versions older than
// writeBypassMax. It scans everything, so it runs at most once per
// writeBypassMax.
func (t *writeTracker) sweepLocked(now time.Time) {
	if now.Sub(t.lastSweep) <= writeBypassMax {
		return
	}
	t.lastSweep = now
	for key, p := range t.pending {
		if now.Sub(p.at) > writeBypassMax {
			delete(t.pending, key)
		}
	}
	for key, versions := range t.seen {
		if now.Sub(versions[len(versions)-1].at) > writeBypassMax {
			delete(t.seen, key)
		}
	}
}

func (t *writeTracker) seenLocked(key, resourceVersion string) bool {
	for _, v := range t.seen[key] {
		if v.resourceVersion == resourceVersion {
			return true
		}
	}
	return false
}

func (t *writeTracker) record(gvr schema.GroupVersionResource, namespace, name string, w pendingWrite) {
	if t == nil || (gvr != vmGVR && gvr != vmiGVR) {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	w.at = t.now()
	t.sweepLocked(w.at)
	key := writeKey(gvr, namespace, name)
	if !w.deleted && w.resourceVersion != "" && t.seenLocked(key, w.resourceVersion) {
		// The cache already stored this write; an older pending write of
		// ours is superseded by it.
		delete(t.pending, key)
		return
	}
	t.pending[key] = w
}

// note records a successful create, update or patch and the resourceVersion
// the API server returned for it. Resources other than VM and VMI are ignored.
func (t *writeTracker) note(gvr schema.GroupVersionResource, namespace, name, resourceVersion string) {
	t.record(gvr, namespace, name, pendingWrite{resourceVersion: resourceVersion})
}

func (t *writeTracker) noteDelete(gvr schema.GroupVersionResource, namespace, name string) {
	t.record(gvr, namespace, name, pendingWrite{deleted: true})
}

// observed is called by the informers for every object version they store.
// Seeing the version our write produced means the cache has reflected it,
// even if a newer version has replaced it by the time anyone reads.
func (t *writeTracker) observed(gvr schema.GroupVersionResource, namespace, name, resourceVersion string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	t.sweepLocked(now)
	key := writeKey(gvr, namespace, name)
	if p, ok := t.pending[key]; ok && !p.deleted && p.resourceVersion != "" && p.resourceVersion == resourceVersion {
		delete(t.pending, key)
	}

	versions := t.seen[key]
	for len(versions) > 0 && (len(versions) >= seenVersionsMax || now.Sub(versions[0].at) > writeBypassMax) {
		versions = versions[1:]
	}
	t.seen[key] = append(versions, seenVersion{resourceVersion: resourceVersion, at: now})
}

// forget drops the versions remembered for an object the informer deleted,
// so the history does not grow with every VMI ever created.
func (t *writeTracker) forget(gvr schema.GroupVersionResource, namespace, name string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.seen, writeKey(gvr, namespace, name))
}

// cacheReflects reports whether the cached copy (nil when the cache has none)
// may be served, given this process's writes to the object.
func (t *writeTracker) cacheReflects(gvr schema.GroupVersionResource, namespace, name string, cached *unstructured.Unstructured) bool {
	if t == nil {
		return true
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	key := writeKey(gvr, namespace, name)
	p, ok := t.pending[key]
	if !ok {
		return true
	}

	var caughtUp bool
	if p.deleted {
		caughtUp = cached == nil || cached.GetDeletionTimestamp() != nil
	} else {
		// Equal versions also cover a no-op write, which the API server
		// acknowledges with the current version and no watch event.
		caughtUp = cached != nil && p.resourceVersion != "" && cached.GetResourceVersion() == p.resourceVersion
	}
	if caughtUp {
		delete(t.pending, key)
		return true
	}
	if t.now().Sub(p.at) > writeBypassMax {
		delete(t.pending, key)
		logger.Warning("VM cache has not reflected this process's write to %s within %s; serving it from the cache again", key, writeBypassMax)
		return true
	}
	return false
}

// trackingDynamicClient wraps a dynamic client so that every successful write
// to a VM or VMI is recorded in the writeTracker. Reads pass straight through.
type trackingDynamicClient struct {
	dynamic.Interface
	tracker *writeTracker
}

func newTrackingDynamicClient(inner dynamic.Interface, tracker *writeTracker) dynamic.Interface {
	if inner == nil {
		return nil
	}
	return &trackingDynamicClient{Interface: inner, tracker: tracker}
}

func (d *trackingDynamicClient) Resource(gvr schema.GroupVersionResource) dynamic.NamespaceableResourceInterface {
	return &trackingNamespaceableResource{
		NamespaceableResourceInterface: d.Interface.Resource(gvr),
		gvr:                            gvr,
		tracker:                        d.tracker,
	}
}

type trackingNamespaceableResource struct {
	dynamic.NamespaceableResourceInterface
	gvr     schema.GroupVersionResource
	tracker *writeTracker
}

func (r *trackingNamespaceableResource) Namespace(namespace string) dynamic.ResourceInterface {
	return &trackingResource{
		ResourceInterface: r.NamespaceableResourceInterface.Namespace(namespace),
		gvr:               r.gvr,
		namespace:         namespace,
		tracker:           r.tracker,
	}
}

type trackingResource struct {
	dynamic.ResourceInterface
	gvr       schema.GroupVersionResource
	namespace string
	tracker   *writeTracker
}

func (r *trackingResource) Create(ctx context.Context, obj *unstructured.Unstructured, options metav1.CreateOptions, subresources ...string) (*unstructured.Unstructured, error) {
	out, err := r.ResourceInterface.Create(ctx, obj, options, subresources...)
	if err == nil {
		r.tracker.note(r.gvr, r.namespace, obj.GetName(), out.GetResourceVersion())
	}
	return out, err
}

func (r *trackingResource) Update(ctx context.Context, obj *unstructured.Unstructured, options metav1.UpdateOptions, subresources ...string) (*unstructured.Unstructured, error) {
	out, err := r.ResourceInterface.Update(ctx, obj, options, subresources...)
	if err == nil {
		r.tracker.note(r.gvr, r.namespace, obj.GetName(), out.GetResourceVersion())
	}
	return out, err
}

func (r *trackingResource) UpdateStatus(ctx context.Context, obj *unstructured.Unstructured, options metav1.UpdateOptions) (*unstructured.Unstructured, error) {
	out, err := r.ResourceInterface.UpdateStatus(ctx, obj, options)
	if err == nil {
		r.tracker.note(r.gvr, r.namespace, obj.GetName(), out.GetResourceVersion())
	}
	return out, err
}

func (r *trackingResource) Patch(ctx context.Context, name string, pt types.PatchType, data []byte, options metav1.PatchOptions, subresources ...string) (*unstructured.Unstructured, error) {
	out, err := r.ResourceInterface.Patch(ctx, name, pt, data, options, subresources...)
	if err == nil {
		r.tracker.note(r.gvr, r.namespace, name, out.GetResourceVersion())
	}
	return out, err
}

func (r *trackingResource) Delete(ctx context.Context, name string, options metav1.DeleteOptions, subresources ...string) error {
	err := r.ResourceInterface.Delete(ctx, name, options, subresources...)
	if err == nil {
		r.tracker.noteDelete(r.gvr, r.namespace, name)
	}
	return err
}

// vmCache holds one VM informer and one VMI informer per configured namespace.
type vmCache struct {
	stopCh    chan struct{}
	factories []dynamicinformer.DynamicSharedInformerFactory
	vms       map[string]informers.GenericInformer
	vmis      map[string]informers.GenericInformer
}

// newVMCache starts informers for every distinct, non-empty namespace. It does
// not wait for them to sync; readers check HasSynced per call. Every version
// the informers store is reported to writes.
func newVMCache(client dynamic.Interface, namespaces []string, writes *writeTracker) *vmCache {
	vc := &vmCache{
		stopCh: make(chan struct{}),
		vms:    make(map[string]informers.GenericInformer),
		vmis:   make(map[string]informers.GenericInformer),
	}
	seen := make(map[string]bool)
	for _, namespace := range namespaces {
		if namespace == "" || seen[namespace] {
			continue
		}
		seen[namespace] = true

		factory := dynamicinformer.NewFilteredDynamicSharedInformerFactory(client, 0, namespace, nil)
		vmInformer := factory.ForResource(vmGVR)
		vmiInformer := factory.ForResource(vmiGVR)
		// Transforms and handlers must be set before the factory starts.
		for gvr, inf := range map[schema.GroupVersionResource]informers.GenericInformer{vmGVR: vmInformer, vmiGVR: vmiInformer} {
			if err := inf.Informer().SetTransform(stripManagedFields); err != nil {
				logger.Error("Failed to set informer transform for namespace %s: %v", namespace, err)
			}
			if _, err := inf.Informer().AddEventHandler(observeVersions(gvr, writes)); err != nil {
				logger.Error("Failed to add informer handler for namespace %s: %v", namespace, err)
			}
		}
		vc.vms[namespace] = vmInformer
		vc.vmis[namespace] = vmiInformer
		vc.factories = append(vc.factories, factory)
		factory.Start(vc.stopCh)
	}
	return vc
}

// stop closes the stop channel first so the informer goroutines exit, then
// waits for them. The other order deadlocks.
func (vc *vmCache) stop() {
	if vc == nil {
		return
	}
	close(vc.stopCh)
	for _, factory := range vc.factories {
		factory.Shutdown()
	}
}

// informer returns the synced informer for gvr in namespace, or false when
// there is none or it has not finished its initial list.
func (vc *vmCache) informer(gvr schema.GroupVersionResource, namespace string) (informers.GenericInformer, bool) {
	if vc == nil {
		return nil, false
	}
	m := vc.vms
	if gvr == vmiGVR {
		m = vc.vmis
	}
	inf, ok := m[namespace]
	if !ok || !inf.Informer().HasSynced() {
		return nil, false
	}
	return inf, true
}

// observeVersions reports every version an informer stores to the write
// tracker, and every object it deletes so the tracker can forget it. The
// shared informer updates its store before it notifies handlers, so once a
// version is observed here, the store holds it or a newer one.
func observeVersions(gvr schema.GroupVersionResource, writes *writeTracker) toolscache.ResourceEventHandler {
	report := func(obj interface{}) {
		if m, err := meta.Accessor(obj); err == nil {
			writes.observed(gvr, m.GetNamespace(), m.GetName(), m.GetResourceVersion())
		}
	}
	return toolscache.ResourceEventHandlerFuncs{
		AddFunc:    report,
		UpdateFunc: func(_, obj interface{}) { report(obj) },
		DeleteFunc: func(obj interface{}) {
			if tombstone, ok := obj.(toolscache.DeletedFinalStateUnknown); ok {
				obj = tombstone.Obj
			}
			if m, err := meta.Accessor(obj); err == nil {
				writes.forget(gvr, m.GetNamespace(), m.GetName())
			}
		},
	}
}

// stripManagedFields drops the managedFields block before an object enters
// the store; it is the largest part of a VM object and nothing here reads it.
func stripManagedFields(obj interface{}) (interface{}, error) {
	if u, ok := obj.(*unstructured.Unstructured); ok {
		u.SetManagedFields(nil)
	}
	return obj, nil
}

// vmCacheEnabled reads kubevirt.cache_enabled from the app config; the cache
// is on unless the config says otherwise.
func (c *Client) vmCacheEnabled() bool {
	if c.appConfig == nil {
		return true
	}
	if cfg, ok := c.appConfig.(interface{ GetVMCacheEnabled() bool }); ok {
		return cfg.GetVMCacheEnabled()
	}
	return true
}

// restartVMCache replaces the running cache with one covering namespaces.
// Called with the watchers on startup and on config reload.
func (c *Client) restartVMCache(namespaces []string) {
	c.cacheMu.Lock()
	defer c.cacheMu.Unlock()

	if c.vmCache != nil {
		c.vmCache.stop()
		c.vmCache = nil
	}
	if !c.vmCacheEnabled() {
		logger.Info("VM cache disabled by configuration; VM/VMI reads go to the API server")
		return
	}
	if c.dynamicClient == nil {
		return
	}
	c.vmCache = newVMCache(c.dynamicClient, namespaces, c.writes)
	logger.Info("VM cache started for namespaces %v", namespaces)
}

func (c *Client) stopVMCache() {
	c.cacheMu.Lock()
	defer c.cacheMu.Unlock()
	if c.vmCache != nil {
		c.vmCache.stop()
		c.vmCache = nil
	}
}

func (c *Client) currentVMCache() *vmCache {
	c.cacheMu.RLock()
	defer c.cacheMu.RUnlock()
	return c.vmCache
}

// GetVMForRead returns a VM for a read-only purpose. It is served from the
// informer cache when the cache is synced for the namespace and reflects every
// write this process made to the VM; otherwise it is a GetVM. Callers that go
// on to patch the VM must use GetVM.
func (c *Client) GetVMForRead(namespace, name string) (*kubevirtv1.VirtualMachine, error) {
	return c.getVMForRead(namespace, name)
}

func (c *Client) getVMForRead(namespace, name string) (*kubevirtv1.VirtualMachine, error) {
	start := time.Now()
	if u, served, err := c.cachedObject(vmGVR, namespace, name); served {
		c.trackOperation("GetVM.cache", time.Since(start))
		if err != nil {
			return nil, fmt.Errorf("failed to get VM: %w", err)
		}
		return unstructuredToVM(u)
	}
	c.trackOperation("GetVM.direct", time.Since(start))
	return c.GetVM(namespace, name)
}

// getVMIForRead is GetVMI served from the cache under the same rules as
// getVMForRead. A synced cache's "not found" means the VM has no running
// instance, which is what callers use to report power state Off.
func (c *Client) getVMIForRead(namespace, name string) (*kubevirtv1.VirtualMachineInstance, error) {
	start := time.Now()
	if u, served, err := c.cachedObject(vmiGVR, namespace, name); served {
		c.trackOperation("GetVMI.cache", time.Since(start))
		if err != nil {
			return nil, fmt.Errorf("failed to get VMI %s: %w", name, err)
		}
		return unstructuredToVMI(u)
	}
	c.trackOperation("GetVMI.direct", time.Since(start))
	return c.GetVMI(namespace, name)
}

// cachedObject looks the object up in the cache. served is false when the
// caller must ask the API server instead: the cache is not synced for the
// namespace, the lister failed, or the cache does not yet reflect a write by
// this process. When served, err is a NotFound error if the object is absent,
// and the returned object is a copy the caller may modify.
func (c *Client) cachedObject(gvr schema.GroupVersionResource, namespace, name string) (u *unstructured.Unstructured, served bool, err error) {
	inf, ok := c.currentVMCache().informer(gvr, namespace)
	if !ok {
		return nil, false, nil
	}
	obj, err := inf.Lister().ByNamespace(namespace).Get(name)
	switch {
	case err == nil:
		if u, ok = obj.(*unstructured.Unstructured); !ok {
			return nil, false, nil
		}
	case !apierrors.IsNotFound(err):
		return nil, false, nil
	}
	if !c.writes.cacheReflects(gvr, namespace, name, u) {
		return nil, false, nil
	}
	if u == nil {
		return nil, true, err
	}
	return u.DeepCopy(), true, nil
}

// listVMNamesFromCache returns the names of the VMs in namespace matching
// selector, or false when the cache is not synced for that namespace.
func (c *Client) listVMNamesFromCache(namespace string, selector labels.Selector) ([]string, bool) {
	inf, ok := c.currentVMCache().informer(vmGVR, namespace)
	if !ok {
		return nil, false
	}
	objs, err := inf.Lister().ByNamespace(namespace).List(selector)
	if err != nil {
		return nil, false
	}
	names := make([]string, 0, len(objs))
	for _, obj := range objs {
		if u, ok := obj.(*unstructured.Unstructured); ok {
			names = append(names, u.GetName())
		}
	}
	c.trackOperation("ListVMs.cache", 0)
	return names, true
}

// filterVMNames applies the selector's explicit name list, if any, and sorts
// the result so collections render in a stable order regardless of source.
func filterVMNames(names []string, vmSelector *VMSelectorConfig) []string {
	var out []string
	if vmSelector != nil && len(vmSelector.Names) > 0 {
		allowed := make(map[string]bool, len(vmSelector.Names))
		for _, n := range vmSelector.Names {
			allowed[n] = true
		}
		for _, n := range names {
			if allowed[n] {
				out = append(out, n)
			}
		}
	} else {
		out = append(out, names...)
	}
	sort.Strings(out)
	return out
}
