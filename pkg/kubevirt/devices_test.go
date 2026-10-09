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
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
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

func TestGetVMCPU_Topology(t *testing.T) {
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
			got, err := c.GetVMCPU("ns", "vm")
			if err != nil || got != tc.want {
				t.Errorf("GetVMCPU = %d, %v; want %d", got, err, tc.want)
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
	if err != nil || got != (CPUTopology{Sockets: 2, Cores: 4, Threads: 2, Model: "host-model", Architecture: "amd64"}) || got.VCPUs() != 16 {
		t.Errorf("topology = %+v, %v", got, err)
	}
	if got.Architecture != "amd64" {
		t.Errorf("unset architecture = %q, want the amd64 default", got.Architecture)
	}
	if _, err := c.GetVMCPUTopology("ns", "nope"); err == nil {
		t.Error("expected an error for a missing VM")
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
	c := NewClientWithClients(k8s, mock, 30*time.Second, nil)

	disks, err := c.GetVMDisks("ns", "vm")
	if err != nil {
		t.Fatal(err)
	}
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
	if len(disks) != len(want) {
		t.Fatalf("got %d disks (%+v), want %d (cdrom must be excluded)", len(disks), disks, len(want))
	}
	for i, w := range want {
		d := disks[i]
		if d.Name != w.name || d.Bus != w.bus {
			t.Errorf("disk %d = %s/%s, want %s/%s", i, d.Name, d.Bus, w.name, w.bus)
		}
		switch {
		case w.size < 0 && d.CapacityBytes != nil:
			t.Errorf("disk %s: capacity %d, want unknown", d.Name, *d.CapacityBytes)
		case w.size >= 0 && (d.CapacityBytes == nil || *d.CapacityBytes != w.size):
			t.Errorf("disk %s: capacity %v, want %d", d.Name, d.CapacityBytes, w.size)
		}
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
