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
	"math"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kubevirtv1 "kubevirt.io/api/core/v1"
)

// VMDisk describes one disk attached to a VM.
type VMDisk struct {
	Name string
	Bus  string
	// CapacityBytes is nil when the spec does not declare a size
	// (containerDisk, cloudInit, ...).
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

// GetVMDisks returns every disk of a VM in spec order. CD-ROM devices are
// excluded because they are exposed as VirtualMedia.
func (c *Client) GetVMDisks(namespace, name string) ([]VMDisk, error) {
	vm, err := c.GetVM(namespace, name)
	if err != nil {
		return nil, err
	}
	if vm.Spec.Template == nil {
		return nil, nil
	}

	volumes := map[string]kubevirtv1.Volume{}
	for _, v := range vm.Spec.Template.Spec.Volumes {
		volumes[v.Name] = v
	}

	var disks []VMDisk
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
		if vol, ok := volumes[d.Name]; ok {
			disk.CapacityBytes = c.volumeCapacityBytes(namespace, vm, vol)
		}
		disks = append(disks, disk)
	}
	return disks, nil
}

// volumeCapacityBytes resolves the declared size of a volume, or nil if unknown.
func (c *Client) volumeCapacityBytes(namespace string, vm *kubevirtv1.VirtualMachine, vol kubevirtv1.Volume) *int64 {
	var claim string
	switch {
	case vol.PersistentVolumeClaim != nil:
		claim = vol.PersistentVolumeClaim.ClaimName
	case vol.DataVolume != nil:
		claim = vol.DataVolume.Name
	case vol.Ephemeral != nil && vol.Ephemeral.PersistentVolumeClaim != nil:
		claim = vol.Ephemeral.PersistentVolumeClaim.ClaimName
	case vol.EmptyDisk != nil:
		b := vol.EmptyDisk.Capacity.Value()
		return &b
	case vol.HostDisk != nil:
		if vol.HostDisk.Capacity.IsZero() {
			return nil
		}
		b := vol.HostDisk.Capacity.Value()
		return &b
	default:
		return nil
	}

	// Prefer the real PVC (actual provisioned size), fall back to the DataVolume template.
	if c.kubernetesClient != nil {
		ctx, cancel := context.WithTimeout(context.Background(), c.timeout)
		defer cancel()
		pvc, err := c.kubernetesClient.CoreV1().PersistentVolumeClaims(namespace).Get(ctx, claim, metav1.GetOptions{})
		if err == nil {
			if q, ok := pvc.Status.Capacity[corev1.ResourceStorage]; ok {
				b := q.Value()
				return &b
			}
			if q, ok := pvc.Spec.Resources.Requests[corev1.ResourceStorage]; ok {
				b := q.Value()
				return &b
			}
		}
	}
	for _, dv := range vm.Spec.DataVolumeTemplates {
		if dv.Name != claim {
			continue
		}
		if dv.Spec.Storage != nil {
			if q, ok := dv.Spec.Storage.Resources.Requests[corev1.ResourceStorage]; ok {
				b := q.Value()
				return &b
			}
		}
		if dv.Spec.PVC != nil {
			if q, ok := dv.Spec.PVC.Resources.Requests[corev1.ResourceStorage]; ok {
				b := q.Value()
				return &b
			}
		}
	}
	return nil
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

	status := map[string]kubevirtv1.VirtualMachineInstanceNetworkInterface{}
	if vmi, err := c.GetVMI(namespace, name); err == nil {
		for _, s := range vmi.Status.Interfaces {
			status[s.Name] = s
		}
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
