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
	"fmt"
	"math"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kubevirtv1 "kubevirt.io/api/core/v1"
)

// VMDisk describes one disk attached to a VM.
type VMDisk struct {
	Name string
	Bus  string
	// CapacityBytes is nil when the spec does not declare a size
	// (containerDisk, cloudInit, ...) and always nil from GetVMDisks.
	CapacityBytes *int64
}

// VMNIC describes one network interface of a VM.
type VMNIC struct {
	Name string
	// MAC is empty until KubeVirt reports it (VM never started and no macAddress in the spec).
	MAC string
	IPs []string
	// Running is true when the VMI exists and reports this interface.
	Running bool
}

// VMMemoryMiB returns the VM memory in MiB (see GetVMMemory for the source order).
func (c *Client) VMMemoryMiB(namespace, name string) (int64, error) {
	gib, err := c.GetVMMemory(namespace, name)
	if err != nil {
		return 0, err
	}
	return int64(math.Round(gib * 1024)), nil
}

// vmDisks lists the disks of a VM in spec order together with their backing
// volume (nil when the spec has none). CD-ROM devices are excluded because
// they are exposed as VirtualMedia. It makes no API calls.
func vmDisks(vm *kubevirtv1.VirtualMachine) (disks []VMDisk, volumes []*kubevirtv1.Volume) {
	if vm.Spec.Template == nil {
		return nil, nil
	}
	byName := map[string]*kubevirtv1.Volume{}
	for i := range vm.Spec.Template.Spec.Volumes {
		v := &vm.Spec.Template.Spec.Volumes[i]
		byName[v.Name] = v
	}
	for _, d := range vm.Spec.Template.Spec.Domain.Devices.Disks {
		if d.CDRom != nil {
			continue
		}
		disk := VMDisk{Name: d.Name}
		switch {
		case d.Disk != nil:
			disk.Bus = string(d.Disk.Bus)
		case d.LUN != nil:
			disk.Bus = string(d.LUN.Bus)
		}
		disks = append(disks, disk)
		volumes = append(volumes, byName[d.Name])
	}
	return disks, volumes
}

// GetVMDisks returns the disks of a VM in spec order with name and bus only.
// CapacityBytes is left unset on purpose: resolving a size can cost an API
// call per disk, so use GetVMDisk when the size of one disk is needed.
func (c *Client) GetVMDisks(namespace, name string) ([]VMDisk, error) {
	vm, err := c.GetVM(namespace, name)
	if err != nil {
		return nil, err
	}
	disks, _ := vmDisks(vm)
	return disks, nil
}

// GetVMDisk returns one disk of a VM including its capacity, or nil if the VM
// has no such disk. Only that disk's size is looked up.
func (c *Client) GetVMDisk(namespace, name, disk string) (*VMDisk, error) {
	vm, err := c.GetVM(namespace, name)
	if err != nil {
		return nil, err
	}
	disks, volumes := vmDisks(vm)
	for i := range disks {
		if disks[i].Name != disk {
			continue
		}
		if volumes[i] != nil {
			capacity, err := c.volumeCapacityBytes(namespace, vm, *volumes[i])
			if err != nil {
				return nil, err
			}
			disks[i].CapacityBytes = capacity
		}
		return &disks[i], nil
	}
	return nil, nil
}

// volumeCapacityBytes resolves the declared size of a volume, or nil if it has
// none. A PVC that does not exist yet falls back to the DataVolume template;
// any other failure to read the PVC (forbidden, timeout, ...) is returned so
// the caller does not report a size that may be wrong.
func (c *Client) volumeCapacityBytes(namespace string, vm *kubevirtv1.VirtualMachine, vol kubevirtv1.Volume) (*int64, error) {
	var claim string
	switch {
	case vol.PersistentVolumeClaim != nil:
		claim = vol.PersistentVolumeClaim.ClaimName
	case vol.DataVolume != nil:
		claim = vol.DataVolume.Name
	case vol.Ephemeral != nil && vol.Ephemeral.PersistentVolumeClaim != nil:
		claim = vol.Ephemeral.PersistentVolumeClaim.ClaimName
	case vol.EmptyDisk != nil:
		if vol.EmptyDisk.Capacity.IsZero() {
			return nil, nil
		}
		b := vol.EmptyDisk.Capacity.Value()
		return &b, nil
	case vol.HostDisk != nil:
		if vol.HostDisk.Capacity.IsZero() {
			return nil, nil
		}
		b := vol.HostDisk.Capacity.Value()
		return &b, nil
	default:
		return nil, nil
	}

	// Prefer the real PVC (actual provisioned size), fall back to the DataVolume template.
	if c.kubernetesClient != nil {
		ctx, cancel := context.WithTimeout(context.Background(), c.timeout)
		defer cancel()
		pvc, err := c.kubernetesClient.CoreV1().PersistentVolumeClaims(namespace).Get(ctx, claim, metav1.GetOptions{})
		switch {
		case err == nil:
			if q, ok := pvc.Status.Capacity[corev1.ResourceStorage]; ok {
				b := q.Value()
				return &b, nil
			}
			if q, ok := pvc.Spec.Resources.Requests[corev1.ResourceStorage]; ok {
				b := q.Value()
				return &b, nil
			}
		case !apierrors.IsNotFound(err):
			return nil, fmt.Errorf("failed to get PVC %s/%s: %w", namespace, claim, err)
		}
	}
	for _, dv := range vm.Spec.DataVolumeTemplates {
		if dv.Name != claim {
			continue
		}
		if dv.Spec.Storage != nil {
			if q, ok := dv.Spec.Storage.Resources.Requests[corev1.ResourceStorage]; ok {
				b := q.Value()
				return &b, nil
			}
		}
		if dv.Spec.PVC != nil {
			if q, ok := dv.Spec.PVC.Resources.Requests[corev1.ResourceStorage]; ok {
				b := q.Value()
				return &b, nil
			}
		}
	}
	return nil, nil
}

// GetVMNICs returns every network interface of a VM. The MAC and IPs come
// from the running VMI when available, otherwise from the VM spec.
func (c *Client) GetVMNICs(namespace, name string) ([]VMNIC, error) {
	vm, err := c.GetVM(namespace, name)
	if err != nil {
		return nil, err
	}
	if vm.Spec.Template == nil {
		return nil, nil
	}

	// No VMI means the VM is not running. Any other failure is returned: reporting
	// NoLink for a running VM whose VMI could not be read would be wrong.
	status := map[string]kubevirtv1.VirtualMachineInstanceNetworkInterface{}
	vmi, err := c.GetVMI(namespace, name)
	switch {
	case err == nil:
		for _, s := range vmi.Status.Interfaces {
			status[s.Name] = s
		}
	case !apierrors.IsNotFound(err):
		return nil, err
	}

	var nics []VMNIC
	for _, i := range vm.Spec.Template.Spec.Domain.Devices.Interfaces {
		nic := VMNIC{Name: i.Name, MAC: i.MacAddress}
		if s, ok := status[i.Name]; ok {
			nic.Running = true
			if s.MAC != "" {
				nic.MAC = s.MAC
			}
			nic.IPs = s.IPs
		}
		nics = append(nics, nic)
	}
	return nics, nil
}
