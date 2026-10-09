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
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	kubevirtv1 "kubevirt.io/api/core/v1"
	cdiv1beta1 "kubevirt.io/containerized-data-importer-api/pkg/apis/core/v1beta1"
)

func deviceTestVM(spec kubevirtv1.VirtualMachineInstanceSpec) *kubevirtv1.VirtualMachine {
	return &kubevirtv1.VirtualMachine{
		ObjectMeta: metav1.ObjectMeta{Name: "vm", Namespace: "ns"},
		Spec: kubevirtv1.VirtualMachineSpec{
			Template: &kubevirtv1.VirtualMachineInstanceTemplateSpec{Spec: spec},
		},
	}
}

func TestGetVMCPUTopology_VCPUs(t *testing.T) {
	cases := []struct {
		name string
		cpu  kubevirtv1.CPU
		want int
	}{
		{"cores only", kubevirtv1.CPU{Cores: 2}, 2},
		{"cores and sockets", kubevirtv1.CPU{Cores: 2, Sockets: 2}, 4},
		{"cores sockets threads", kubevirtv1.CPU{Cores: 2, Sockets: 2, Threads: 2}, 8},
		{"threads only", kubevirtv1.CPU{Threads: 2}, 2},
		{"empty cpu block", kubevirtv1.CPU{}, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mock := NewMockDynamicClient()
			cpu := tc.cpu
			if err := mock.AddVM(deviceTestVM(kubevirtv1.VirtualMachineInstanceSpec{Domain: kubevirtv1.DomainSpec{CPU: &cpu}})); err != nil {
				t.Fatal(err)
			}
			c := NewClientWithClients(fake.NewSimpleClientset(), mock, 30*time.Second, nil)
			topo, err := c.GetVMCPUTopology("ns", "vm")
			if got := topo.VCPUs(); err != nil || got != tc.want {
				t.Errorf("vCPUs = %d, %v; want %d", got, err, tc.want)
			}
		})
	}
}

func TestGetVMCPUTopology(t *testing.T) {
	mock := NewMockDynamicClient()
	cpu := kubevirtv1.CPU{Cores: 4, Sockets: 2, Threads: 2, Model: "host-model"}
	if err := mock.AddVM(deviceTestVM(kubevirtv1.VirtualMachineInstanceSpec{Domain: kubevirtv1.DomainSpec{CPU: &cpu}})); err != nil {
		t.Fatal(err)
	}
	c := NewClientWithClients(fake.NewSimpleClientset(), mock, 30*time.Second, nil)
	got, err := c.GetVMCPUTopology("ns", "vm")
	if err != nil || got != (CPUTopology{Sockets: 2, Cores: 4, Threads: 2, Model: "host-model"}) || got.VCPUs() != 16 {
		t.Errorf("topology = %+v, %v", got, err)
	}
	if got.Architecture != "" {
		t.Errorf("unset architecture = %q, want it left empty, not guessed", got.Architecture)
	}
	if zero, err := c.GetVMCPUTopology("ns", "nope"); err == nil || zero != (CPUTopology{}) {
		t.Errorf("missing VM: got %+v, %v; want the zero topology and an error, never a guess", zero, err)
	}
}

func TestGetVMMemory_Sources(t *testing.T) {
	guest := resource.MustParse("4Gi")
	cases := []struct {
		name   string
		domain kubevirtv1.DomainSpec
		want   float64
	}{
		{"guest memory wins", kubevirtv1.DomainSpec{
			Memory:    &kubevirtv1.Memory{Guest: &guest},
			Resources: kubevirtv1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("1Gi")}},
		}, 4},
		{"resources.requests", kubevirtv1.DomainSpec{
			Resources: kubevirtv1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("128Mi")}},
		}, 0.125},
		{"resources.limits", kubevirtv1.DomainSpec{
			Resources: kubevirtv1.ResourceRequirements{Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("2Gi")}},
		}, 2},
		{"nothing declared reports 0, not a guess", kubevirtv1.DomainSpec{}, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mock := NewMockDynamicClient()
			if err := mock.AddVM(deviceTestVM(kubevirtv1.VirtualMachineInstanceSpec{Domain: tc.domain})); err != nil {
				t.Fatal(err)
			}
			c := NewClientWithClients(fake.NewSimpleClientset(), mock, 30*time.Second, nil)
			got, err := c.GetVMMemory("ns", "vm")
			if err != nil || got != tc.want {
				t.Errorf("GetVMMemory = %v, %v; want %v", got, err, tc.want)
			}
		})
	}
}

func TestGetVMMemory_MissingVMIsAnError(t *testing.T) {
	c := NewClientWithClients(fake.NewSimpleClientset(), NewMockDynamicClient(), 30*time.Second, nil)
	if _, err := c.GetVMMemory("ns", "nope"); err == nil {
		t.Error("expected an error for a missing VM")
	}
}

func TestGetVMDisks(t *testing.T) {
	dvSize := resource.MustParse("20Gi")
	spec := kubevirtv1.VirtualMachineInstanceSpec{
		Domain: kubevirtv1.DomainSpec{Devices: kubevirtv1.Devices{Disks: []kubevirtv1.Disk{
			{Name: "root", DiskDevice: kubevirtv1.DiskDevice{Disk: &kubevirtv1.DiskTarget{Bus: "virtio"}}},
			{Name: "data", DiskDevice: kubevirtv1.DiskDevice{Disk: &kubevirtv1.DiskTarget{Bus: "sata"}}},
			{Name: "tpl", DiskDevice: kubevirtv1.DiskDevice{Disk: &kubevirtv1.DiskTarget{Bus: "virtio"}}},
			{Name: "scratch", DiskDevice: kubevirtv1.DiskDevice{Disk: &kubevirtv1.DiskTarget{Bus: "virtio"}}},
			{Name: "cloud", DiskDevice: kubevirtv1.DiskDevice{Disk: &kubevirtv1.DiskTarget{Bus: "virtio"}}},
			{Name: "cdrom0", DiskDevice: kubevirtv1.DiskDevice{CDRom: &kubevirtv1.CDRomTarget{}}},
		}}},
		Volumes: []kubevirtv1.Volume{
			{Name: "root", VolumeSource: kubevirtv1.VolumeSource{ContainerDisk: &kubevirtv1.ContainerDiskSource{Image: "img"}}},
			{Name: "data", VolumeSource: kubevirtv1.VolumeSource{PersistentVolumeClaim: &kubevirtv1.PersistentVolumeClaimVolumeSource{
				PersistentVolumeClaimVolumeSource: corev1.PersistentVolumeClaimVolumeSource{ClaimName: "data-pvc"}}}},
			{Name: "tpl", VolumeSource: kubevirtv1.VolumeSource{DataVolume: &kubevirtv1.DataVolumeSource{Name: "tpl-dv"}}},
			{Name: "scratch", VolumeSource: kubevirtv1.VolumeSource{EmptyDisk: &kubevirtv1.EmptyDiskSource{Capacity: resource.MustParse("1Gi")}}},
			{Name: "cloud", VolumeSource: kubevirtv1.VolumeSource{CloudInitNoCloud: &kubevirtv1.CloudInitNoCloudSource{UserData: "x"}}},
		},
	}
	vm := deviceTestVM(spec)
	vm.Spec.DataVolumeTemplates = []kubevirtv1.DataVolumeTemplateSpec{{
		ObjectMeta: metav1.ObjectMeta{Name: "tpl-dv"},
		Spec: cdiv1beta1.DataVolumeSpec{Storage: &cdiv1beta1.StorageSpec{
			Resources: corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: dvSize}},
		}},
	}}

	mock := NewMockDynamicClient()
	if err := mock.AddVM(vm); err != nil {
		t.Fatal(err)
	}
	k8s := fake.NewSimpleClientset(&corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "data-pvc", Namespace: "ns"},
		Status:     corev1.PersistentVolumeClaimStatus{Capacity: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("100Gi")}},
	})
	pvcGets := 0
	k8s.PrependReactor("get", "persistentvolumeclaims", func(k8stesting.Action) (bool, runtime.Object, error) {
		pvcGets++
		return false, nil, nil // count only, let the fake answer
	})
	c := NewClientWithClients(k8s, mock, 30*time.Second, nil)

	want := []struct {
		name string
		bus  string
		size int64 // -1 = unknown
	}{
		{"root", "virtio", -1},
		{"data", "sata", 100 << 30},
		{"tpl", "virtio", 20 << 30},
		{"scratch", "virtio", 1 << 30},
		{"cloud", "virtio", -1},
	}

	// The list carries names and bus only and must not touch any PVC.
	disks, err := c.GetVMDisks("ns", "vm")
	if err != nil {
		t.Fatal(err)
	}
	if len(disks) != len(want) {
		t.Fatalf("got %d disks (%+v), want %d (cdrom must be excluded)", len(disks), disks, len(want))
	}
	for i, w := range want {
		if disks[i].Name != w.name || disks[i].Bus != w.bus || disks[i].CapacityBytes != nil {
			t.Errorf("disk %d = %+v, want %s/%s and no capacity", i, disks[i], w.name, w.bus)
		}
	}
	if pvcGets != 0 {
		t.Errorf("GetVMDisks made %d PVC lookups, want 0", pvcGets)
	}

	// A single disk resolves its own size, with at most one PVC lookup.
	for _, w := range want {
		pvcGets = 0
		d, err := c.GetVMDisk("ns", "vm", w.name)
		if err != nil || d == nil {
			t.Fatalf("GetVMDisk(%s) = %v, %v", w.name, d, err)
		}
		if d.Bus != w.bus {
			t.Errorf("disk %s bus = %s, want %s", w.name, d.Bus, w.bus)
		}
		switch {
		case w.size < 0 && d.CapacityBytes != nil:
			t.Errorf("disk %s: capacity %d, want unknown", w.name, *d.CapacityBytes)
		case w.size >= 0 && (d.CapacityBytes == nil || *d.CapacityBytes != w.size):
			t.Errorf("disk %s: capacity %v, want %d", w.name, d.CapacityBytes, w.size)
		}
		if pvcGets > 1 {
			t.Errorf("GetVMDisk(%s) made %d PVC lookups, want at most 1", w.name, pvcGets)
		}
	}

	// Unknown disks and CD-ROMs (exposed as VirtualMedia) are not drives.
	for _, name := range []string{"nope", "cdrom0"} {
		if d, err := c.GetVMDisk("ns", "vm", name); err != nil || d != nil {
			t.Errorf("GetVMDisk(%s) = %v, %v; want nil, nil", name, d, err)
		}
	}
	if _, err := c.GetVMDisk("ns", "missing-vm", "root"); err == nil {
		t.Error("expected an error for a missing VM")
	}
}

// The cost of reading one drive must not depend on how many disks the VM has.
func TestGetVMDisk_LookupsDoNotGrowWithDiskCount(t *testing.T) {
	const n = 24
	spec := kubevirtv1.VirtualMachineInstanceSpec{}
	var pvcs []runtime.Object
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("d%d", i)
		claim := fmt.Sprintf("claim-%d", i)
		spec.Domain.Devices.Disks = append(spec.Domain.Devices.Disks,
			kubevirtv1.Disk{Name: name, DiskDevice: kubevirtv1.DiskDevice{Disk: &kubevirtv1.DiskTarget{Bus: "virtio"}}})
		spec.Volumes = append(spec.Volumes, kubevirtv1.Volume{Name: name, VolumeSource: kubevirtv1.VolumeSource{
			PersistentVolumeClaim: &kubevirtv1.PersistentVolumeClaimVolumeSource{
				PersistentVolumeClaimVolumeSource: corev1.PersistentVolumeClaimVolumeSource{ClaimName: claim}}}})
		pvcs = append(pvcs, &corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{Name: claim, Namespace: "ns"},
			Spec: corev1.PersistentVolumeClaimSpec{Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: *resource.NewQuantity(int64(i+1)<<30, resource.BinarySI)}}},
		})
	}
	mock := NewMockDynamicClient()
	if err := mock.AddVM(deviceTestVM(spec)); err != nil {
		t.Fatal(err)
	}
	k8s := fake.NewSimpleClientset(pvcs...)
	pvcGets := 0
	k8s.PrependReactor("get", "persistentvolumeclaims", func(k8stesting.Action) (bool, runtime.Object, error) {
		pvcGets++
		return false, nil, nil
	})
	c := NewClientWithClients(k8s, mock, 30*time.Second, nil)

	if disks, err := c.GetVMDisks("ns", "vm"); err != nil || len(disks) != n || pvcGets != 0 {
		t.Fatalf("GetVMDisks: %d disks, %d PVC lookups, err %v; want %d, 0", len(disks), pvcGets, err, n)
	}
	d, err := c.GetVMDisk("ns", "vm", "d23")
	if err != nil || d == nil || d.CapacityBytes == nil || *d.CapacityBytes != 24<<30 {
		t.Fatalf("GetVMDisk(d23) = %+v, %v", d, err)
	}
	if pvcGets != 1 {
		t.Errorf("reading one of %d drives made %d PVC lookups, want 1", n, pvcGets)
	}
}

func TestGetVMNICs(t *testing.T) {
	spec := kubevirtv1.VirtualMachineInstanceSpec{Domain: kubevirtv1.DomainSpec{Devices: kubevirtv1.Devices{Interfaces: []kubevirtv1.Interface{
		{Name: "default", InterfaceBindingMethod: kubevirtv1.InterfaceBindingMethod{Masquerade: &kubevirtv1.InterfaceMasquerade{}}},
		{Name: "fixed", MacAddress: "02:00:00:00:00:01"},
	}}}}

	t.Run("stopped VM uses the spec MAC and reports no link", func(t *testing.T) {
		mock := NewMockDynamicClient()
		if err := mock.AddVM(deviceTestVM(spec)); err != nil {
			t.Fatal(err)
		}
		c := NewClientWithClients(fake.NewSimpleClientset(), mock, 30*time.Second, nil)
		nics, err := c.GetVMNICs("ns", "vm")
		if err != nil || len(nics) != 2 {
			t.Fatalf("GetVMNICs = %+v, %v", nics, err)
		}
		if nics[0].MAC != "" || nics[0].Running {
			t.Errorf("default: %+v, want empty MAC and not running", nics[0])
		}
		if nics[1].MAC != "02:00:00:00:00:01" || nics[1].Running {
			t.Errorf("fixed: %+v", nics[1])
		}
	})

	t.Run("running VM reports the VMI MAC and IPs", func(t *testing.T) {
		mock := NewMockDynamicClient()
		if err := mock.AddVM(deviceTestVM(spec)); err != nil {
			t.Fatal(err)
		}
		vmi := &kubevirtv1.VirtualMachineInstance{
			ObjectMeta: metav1.ObjectMeta{Name: "vm", Namespace: "ns"},
			Status: kubevirtv1.VirtualMachineInstanceStatus{Interfaces: []kubevirtv1.VirtualMachineInstanceNetworkInterface{
				{Name: "default", MAC: "52:54:00:aa:bb:cc", IPs: []string{"10.0.0.5", "fd00::5"}},
			}},
		}
		if err := mock.AddVMI(vmi); err != nil {
			t.Fatal(err)
		}
		c := NewClientWithClients(fake.NewSimpleClientset(), mock, 30*time.Second, nil)
		nics, err := c.GetVMNICs("ns", "vm")
		if err != nil || len(nics) != 2 {
			t.Fatalf("GetVMNICs = %+v, %v", nics, err)
		}
		if nics[0].MAC != "52:54:00:aa:bb:cc" || !nics[0].Running || len(nics[0].IPs) != 2 {
			t.Errorf("default: %+v", nics[0])
		}
		if nics[1].Running {
			t.Errorf("fixed has no status entry, must not be running: %+v", nics[1])
		}
	})
}

func TestGetVMCPUTopology_Architecture(t *testing.T) {
	mock := NewMockDynamicClient()
	if err := mock.AddVM(deviceTestVM(kubevirtv1.VirtualMachineInstanceSpec{Architecture: "arm64"})); err != nil {
		t.Fatal(err)
	}
	c := NewClientWithClients(fake.NewSimpleClientset(), mock, 30*time.Second, nil)
	got, err := c.GetVMCPUTopology("ns", "vm")
	if err != nil || got.Architecture != "arm64" {
		t.Errorf("architecture = %q, %v; want arm64", got.Architecture, err)
	}
}

// failingVMIs makes every VMI Get fail with err; everything else passes through.
type failingVMIs struct {
	dynamic.Interface
	err error
}

func (f failingVMIs) Resource(gvr schema.GroupVersionResource) dynamic.NamespaceableResourceInterface {
	r := f.Interface.Resource(gvr)
	if gvr.Resource == "virtualmachineinstances" {
		return failingVMIResource{NamespaceableResourceInterface: r, err: f.err}
	}
	return r
}

type failingVMIResource struct {
	dynamic.NamespaceableResourceInterface
	err error
}

func (r failingVMIResource) Namespace(ns string) dynamic.ResourceInterface {
	return failingVMIGet{ResourceInterface: r.NamespaceableResourceInterface.Namespace(ns), err: r.err}
}

type failingVMIGet struct {
	dynamic.ResourceInterface
	err error
}

func (r failingVMIGet) Get(context.Context, string, metav1.GetOptions, ...string) (*unstructured.Unstructured, error) {
	return nil, r.err
}

// A PVC that cannot be read for any reason other than "not found" must surface
// as an error instead of a size taken from somewhere else.
func TestGetVMDisk_PVCReadErrorsAreNotHidden(t *testing.T) {
	templateSize := resource.MustParse("20Gi")
	vm := deviceTestVM(kubevirtv1.VirtualMachineInstanceSpec{
		Domain: kubevirtv1.DomainSpec{Devices: kubevirtv1.Devices{Disks: []kubevirtv1.Disk{
			{Name: "data", DiskDevice: kubevirtv1.DiskDevice{Disk: &kubevirtv1.DiskTarget{Bus: "virtio"}}},
		}}},
		Volumes: []kubevirtv1.Volume{{Name: "data", VolumeSource: kubevirtv1.VolumeSource{
			DataVolume: &kubevirtv1.DataVolumeSource{Name: "data-dv"}}}},
	})
	vm.Spec.DataVolumeTemplates = []kubevirtv1.DataVolumeTemplateSpec{{
		ObjectMeta: metav1.ObjectMeta{Name: "data-dv"},
		Spec: cdiv1beta1.DataVolumeSpec{Storage: &cdiv1beta1.StorageSpec{
			Resources: corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: templateSize}},
		}},
	}}
	gr := schema.GroupResource{Resource: "persistentvolumeclaims"}

	for name, tc := range map[string]struct {
		err      error
		wantErr  bool
		wantSize int64
	}{
		"not found falls back to the template": {apierrors.NewNotFound(gr, "data-dv"), false, 20 << 30},
		"forbidden is returned":                {apierrors.NewForbidden(gr, "data-dv", errors.New("no access")), true, 0},
		"timeout is returned":                  {apierrors.NewTimeoutError("slow", 1), true, 0},
		"server error is returned":             {apierrors.NewInternalError(errors.New("boom")), true, 0},
	} {
		t.Run(name, func(t *testing.T) {
			mock := NewMockDynamicClient()
			if err := mock.AddVM(vm.DeepCopy()); err != nil {
				t.Fatal(err)
			}
			k8s := fake.NewSimpleClientset()
			k8s.PrependReactor("get", "persistentvolumeclaims", func(k8stesting.Action) (bool, runtime.Object, error) {
				return true, nil, tc.err
			})
			c := NewClientWithClients(k8s, mock, 30*time.Second, nil)
			d, err := c.GetVMDisk("ns", "vm", "data")
			if tc.wantErr {
				if err == nil || d != nil {
					t.Fatalf("got %+v, %v; want an error and no disk", d, err)
				}
				return
			}
			if err != nil || d == nil || d.CapacityBytes == nil || *d.CapacityBytes != tc.wantSize {
				t.Fatalf("got %+v, %v; want %d bytes from the template", d, err, tc.wantSize)
			}
		})
	}
}

func TestGetVMDisk_ZeroSizedEmptyDiskHasNoCapacity(t *testing.T) {
	mock := NewMockDynamicClient()
	if err := mock.AddVM(deviceTestVM(kubevirtv1.VirtualMachineInstanceSpec{
		Domain: kubevirtv1.DomainSpec{Devices: kubevirtv1.Devices{Disks: []kubevirtv1.Disk{
			{Name: "scratch", DiskDevice: kubevirtv1.DiskDevice{Disk: &kubevirtv1.DiskTarget{Bus: "virtio"}}},
		}}},
		Volumes: []kubevirtv1.Volume{{Name: "scratch", VolumeSource: kubevirtv1.VolumeSource{EmptyDisk: &kubevirtv1.EmptyDiskSource{}}}},
	})); err != nil {
		t.Fatal(err)
	}
	c := NewClientWithClients(fake.NewSimpleClientset(), mock, 30*time.Second, nil)
	d, err := c.GetVMDisk("ns", "vm", "scratch")
	if err != nil || d == nil || d.CapacityBytes != nil {
		t.Errorf("got %+v, %v; want the drive without a capacity, not 0 bytes", d, err)
	}
}

// Only "no VMI" means the VM is stopped. Failing to read the VMI of a VM that may
// be running must be an error, not a NoLink answer.
func TestGetVMNICs_VMIReadErrorsAreNotHidden(t *testing.T) {
	spec := kubevirtv1.VirtualMachineInstanceSpec{Domain: kubevirtv1.DomainSpec{Devices: kubevirtv1.Devices{Interfaces: []kubevirtv1.Interface{
		{Name: "default", MacAddress: "02:00:00:00:00:01"},
	}}}}
	gr := schema.GroupResource{Group: "kubevirt.io", Resource: "virtualmachineinstances"}

	for name, tc := range map[string]struct {
		err     error
		wantErr bool
	}{
		"not found means stopped": {apierrors.NewNotFound(gr, "vm"), false},
		"forbidden is returned":   {apierrors.NewForbidden(gr, "vm", errors.New("no access")), true},
		"timeout is returned":     {apierrors.NewTimeoutError("slow", 1), true},
	} {
		t.Run(name, func(t *testing.T) {
			mock := NewMockDynamicClient()
			if err := mock.AddVM(deviceTestVM(spec)); err != nil {
				t.Fatal(err)
			}
			c := NewClientWithClients(fake.NewSimpleClientset(), failingVMIs{Interface: mock, err: tc.err}, 30*time.Second, nil)
			nics, err := c.GetVMNICs("ns", "vm")
			if tc.wantErr {
				if err == nil || nics != nil {
					t.Fatalf("got %+v, %v; want an error and no NICs", nics, err)
				}
				return
			}
			if err != nil || len(nics) != 1 || nics[0].Running || nics[0].MAC != "02:00:00:00:00:01" {
				t.Fatalf("got %+v, %v; want one stopped NIC with the spec MAC", nics, err)
			}
		})
	}
}
