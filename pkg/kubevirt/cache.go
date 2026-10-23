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
// all of them are reads of the same VM/VMI objects (a GET System is five GETs).
// With many BareMetalHosts polling at once, those reads are what saturate the
// client-side rate limiter. For every configured namespace the client keeps
// one client-go informer for VMs and one for VMIs, so the read-only Redfish
// paths are served from memory.
//
// Two rules keep cached reads correct:
//
//   - Only read-only inventory lookups use the cache (getVMForRead and its
//     callers). Every path that reads an object in order to write it, and the
//     handler that reads an object back right after writing it, reads from
//     the API server (GetVM, GetVMBootOptionsLive). A write is therefore
//     never computed from, or reported from, a copy the watch has not caught
//     up with.
//   - A cache that has not finished its initial list is not consulted, and
//     once it has, its answer is final, including "not found". Falling back
//     to a GET on every miss would cost a GET on every power poll of a
//     stopped VM, which has no VMI, and that is the dominant state of a BMH
//     during inspection and provisioning.
//
// A write by anyone, this process included, reaches the cache with its watch
// event, usually within milliseconds. Until then a cached read returns the
// previous version. No write path reads from the cache, so a stale read can
// make a response out of date but can never be written back over a change.

import (
	"fmt"
	"sort"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/informers"
	kubevirtv1 "kubevirt.io/api/core/v1"

	"github.com/kubevirt/redfish-controller/pkg/logger"
)

var (
	vmGVR  = schema.GroupVersionResource{Group: "kubevirt.io", Version: "v1", Resource: "virtualmachines"}
	vmiGVR = schema.GroupVersionResource{Group: "kubevirt.io", Version: "v1", Resource: "virtualmachineinstances"}
)

// vmCache holds one VM informer and one VMI informer per configured namespace.
type vmCache struct {
	stopCh    chan struct{}
	factories []dynamicinformer.DynamicSharedInformerFactory
	vms       map[string]informers.GenericInformer
	vmis      map[string]informers.GenericInformer
}

// newVMCache starts informers for every distinct, non-empty namespace. It does
// not wait for them to sync; readers check HasSynced per call.
func newVMCache(client dynamic.Interface, namespaces []string) *vmCache {
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
		// Transforms must be set before the factory starts.
		for _, inf := range []informers.GenericInformer{vmInformer, vmiInformer} {
			if err := inf.Informer().SetTransform(stripManagedFields); err != nil {
				logger.Error("Failed to set informer transform for namespace %s: %v", namespace, err)
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

// stripManagedFields drops the managedFields block before an object enters
// the store; it is the largest part of a VM object and nothing here reads it.
func stripManagedFields(obj interface{}) (interface{}, error) {
	if u, ok := obj.(*unstructured.Unstructured); ok {
		u.SetManagedFields(nil)
	}
	return obj, nil
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
	if c.dynamicClient == nil {
		return
	}
	c.vmCache = newVMCache(c.dynamicClient, namespaces)
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
// informer cache when the cache is synced for the namespace; otherwise it is
// a GetVM. Callers that go on to patch the VM must use GetVM.
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
// namespace or the lister failed. When served, err is a NotFound error if the
// object is absent, and the returned object is a copy the caller may modify.
func (c *Client) cachedObject(gvr schema.GroupVersionResource, namespace, name string) (*unstructured.Unstructured, bool, error) {
	inf, ok := c.currentVMCache().informer(gvr, namespace)
	if !ok {
		return nil, false, nil
	}
	obj, err := inf.Lister().ByNamespace(namespace).Get(name)
	if apierrors.IsNotFound(err) {
		return nil, true, err
	}
	if err != nil {
		return nil, false, nil
	}
	u, ok := obj.(*unstructured.Unstructured)
	if !ok {
		return nil, false, nil
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
