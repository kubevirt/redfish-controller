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

package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/kubevirt/redfish-controller/pkg/auth"
	"github.com/kubevirt/redfish-controller/pkg/config"
	"github.com/kubevirt/redfish-controller/pkg/kubevirt"
	"github.com/kubevirt/redfish-controller/pkg/logger"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	kubevirtv1 "kubevirt.io/api/core/v1"
)

func newDevicesTestServer(t *testing.T, arch string) *Server {
	t.Helper()
	return newDevicesTestServerWith(t, arch, nil, nil)
}

// newDevicesTestServerWith adds an optional running VMI and a custom fake Kubernetes client.
func newDevicesTestServerWith(t *testing.T, arch string, vmi *kubevirtv1.VirtualMachineInstance, k8s *fake.Clientset) *Server {
	t.Helper()
	mock := kubevirt.NewMockDynamicClient()
	vm := &kubevirtv1.VirtualMachine{
		ObjectMeta: metav1.ObjectMeta{Name: "vm-1", Namespace: "ns"},
		Spec: kubevirtv1.VirtualMachineSpec{Template: &kubevirtv1.VirtualMachineInstanceTemplateSpec{
			Spec: kubevirtv1.VirtualMachineInstanceSpec{
				Architecture: arch,
				Domain: kubevirtv1.DomainSpec{
					CPU: &kubevirtv1.CPU{Cores: 2, Sockets: 2, Threads: 2, Model: "Skylake-Client"},
					Resources: kubevirtv1.ResourceRequirements{
						Requests: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("512Mi")},
					},
					Devices: kubevirtv1.Devices{
						Disks: []kubevirtv1.Disk{
							{Name: "root", DiskDevice: kubevirtv1.DiskDevice{Disk: &kubevirtv1.DiskTarget{Bus: "virtio"}}},
							{Name: "data", DiskDevice: kubevirtv1.DiskDevice{Disk: &kubevirtv1.DiskTarget{Bus: "sata"}}},
						},
						Interfaces: []kubevirtv1.Interface{
							{Name: "default", MacAddress: "02:00:00:00:00:01", InterfaceBindingMethod: kubevirtv1.InterfaceBindingMethod{Masquerade: &kubevirtv1.InterfaceMasquerade{}}},
						},
					},
				},
				Volumes: []kubevirtv1.Volume{
					{Name: "root", VolumeSource: kubevirtv1.VolumeSource{ContainerDisk: &kubevirtv1.ContainerDiskSource{Image: "img"}}},
					{Name: "data", VolumeSource: kubevirtv1.VolumeSource{EmptyDisk: &kubevirtv1.EmptyDiskSource{Capacity: resource.MustParse("10Gi")}}},
				},
			},
		}},
	}
	require.NoError(t, mock.AddVM(vm))
	if vmi != nil {
		require.NoError(t, mock.AddVMI(vmi))
	}
	if k8s == nil {
		k8s = fake.NewSimpleClientset()
	}

	cfg := &config.Config{
		Server: config.ServerConfig{Host: "localhost", Port: 8080, TestMode: true},
		Auth: config.AuthConfig{Users: []config.UserConfig{
			{Username: "u", Password: config.PasswordConfig{Plain: "p"}, Chassis: []string{"c"}},
			{Username: "outsider", Password: config.PasswordConfig{Plain: "p"}, Chassis: []string{"other"}},
		}},
		Chassis: []config.ChassisConfig{{Name: "c", Namespace: "ns"}, {Name: "other", Namespace: "other-ns"}},
	}
	client := kubevirt.NewClientWithClients(k8s, mock, 30*time.Second, nil)
	return NewServer(cfg, client)
}

func getSystemPath(srv *Server, path string) *httptest.ResponseRecorder {
	return getSystemPathAs(srv, path, &auth.User{Username: "u", Chassis: []string{"c"}})
}

// getSystemPathAs sends a GET as the given user; a nil user sends no authentication context.
func getSystemPathAs(srv *Server, path string, user *auth.User) *httptest.ResponseRecorder {
	req := httptest.NewRequest("GET", path, nil)
	if user != nil {
		req = req.WithContext(logger.WithAuth(req.Context(), &auth.AuthContext{User: user}))
	}
	w := httptest.NewRecorder()
	srv.handleSystem(w, req)
	return w
}

func TestSystemDevices(t *testing.T) {
	srv := newDevicesTestServer(t, "")

	get := func(t *testing.T, path string) map[string]interface{} {
		t.Helper()
		w := getSystemPath(srv, path)
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		var body map[string]interface{}
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
		return body
	}

	t.Run("system links the sub-resources and summarises hardware", func(t *testing.T) {
		b := get(t, "/redfish/v1/Systems/vm-1")
		assert.Equal(t, "/redfish/v1/Systems/vm-1/Memory", b["Memory"].(map[string]interface{})["@odata.id"])
		assert.Equal(t, "/redfish/v1/Systems/vm-1/Processors", b["Processors"].(map[string]interface{})["@odata.id"])
		assert.Equal(t, 0.5, b["MemorySummary"].(map[string]interface{})["TotalSystemMemoryGiB"])
		ps := b["ProcessorSummary"].(map[string]interface{})
		assert.EqualValues(t, 2, ps["Count"], "Count is the socket count")
		assert.EqualValues(t, 4, ps["CoreCount"])
		assert.EqualValues(t, 8, ps["LogicalProcessorCount"])
		assert.Equal(t, true, ps["ThreadingEnabled"])
		assert.Equal(t, "Skylake-Client", ps["Model"])
		assert.NotContains(t, ps, "Oem", "ProcessorSummary has no Oem property in any schema version")
		topo := b["Oem"].(map[string]interface{})["KubeVirt"].(map[string]interface{})["Processors"].(map[string]interface{})
		assert.EqualValues(t, 2, topo["Sockets"])
		assert.EqualValues(t, 2, topo["CoresPerSocket"])
		assert.EqualValues(t, 2, topo["ThreadsPerCore"])
	})

	t.Run("resources declare a schema version that defines what they emit", func(t *testing.T) {
		for path, want := range map[string]string{
			"/redfish/v1/Systems/vm-1":                            "#ComputerSystem.v1_15_0.ComputerSystem",
			"/redfish/v1/Systems/vm-1/Memory/1":                   "#Memory.v1_1_0.Memory",
			"/redfish/v1/Systems/vm-1/Processors/CPU0":            "#Processor.v1_0_0.Processor",
			"/redfish/v1/Systems/vm-1/Storage/1":                  "#Storage.v1_0_0.Storage",
			"/redfish/v1/Systems/vm-1/Storage/1/Drives/root":      "#Drive.v1_0_0.Drive",
			"/redfish/v1/Systems/vm-1/EthernetInterfaces/default": "#EthernetInterface.v1_1_0.EthernetInterface",
		} {
			assert.Equal(t, want, get(t, path)["@odata.type"], path)
		}
	})

	t.Run("memory", func(t *testing.T) {
		c := get(t, "/redfish/v1/Systems/vm-1/Memory")
		assert.EqualValues(t, 1, c["Members@odata.count"])
		m := get(t, "/redfish/v1/Systems/vm-1/Memory/1")
		assert.EqualValues(t, 512, m["CapacityMiB"])
		assert.EqualValues(t, 0.5, m["Oem"].(map[string]interface{})["KubeVirt"].(map[string]interface{})["CapacityGiB"])
	})

	t.Run("processors report cores and threads per socket", func(t *testing.T) {
		c := get(t, "/redfish/v1/Systems/vm-1/Processors")
		assert.EqualValues(t, 2, c["Members@odata.count"])
		p := get(t, "/redfish/v1/Systems/vm-1/Processors/CPU1")
		assert.Equal(t, "1", p["Socket"])
		assert.Equal(t, "CPU", p["ProcessorType"])
		assert.EqualValues(t, 2, p["TotalCores"])
		assert.EqualValues(t, 4, p["TotalThreads"])
		assert.Equal(t, "Skylake-Client", p["Model"])
		assert.NotContains(t, p, "ProcessorArchitecture", "VM declares no architecture, so none is guessed")
		assert.NotContains(t, p, "InstructionSet")
	})

	t.Run("storage lists every disk with its capacity", func(t *testing.T) {
		assert.EqualValues(t, 1, get(t, "/redfish/v1/Systems/vm-1/Storage/")["Members@odata.count"], "trailing slash lists the one controller")
		s := get(t, "/redfish/v1/Systems/vm-1/Storage/1")
		assert.EqualValues(t, 2, s["Drives@odata.count"])
		root := get(t, "/redfish/v1/Systems/vm-1/Storage/1/Drives/root")
		assert.NotContains(t, root, "CapacityBytes", "containerDisk has no declared size")
		data := get(t, "/redfish/v1/Systems/vm-1/Storage/1/Drives/data")
		assert.EqualValues(t, 10<<30, data["CapacityBytes"])
		assert.EqualValues(t, 10, data["Oem"].(map[string]interface{})["KubeVirt"].(map[string]interface{})["CapacityGiB"])
		assert.NotContains(t, root, "Oem")
		assert.Equal(t, "SATA", data["Protocol"])
	})

	t.Run("ethernet interfaces expose the MAC", func(t *testing.T) {
		c := get(t, "/redfish/v1/Systems/vm-1/EthernetInterfaces")
		assert.EqualValues(t, 1, c["Members@odata.count"])
		e := get(t, "/redfish/v1/Systems/vm-1/EthernetInterfaces/default")
		assert.Equal(t, "02:00:00:00:00:01", e["MACAddress"])
		assert.Equal(t, "NoLink", e["LinkStatus"], "VM is not running")
		assert.NotContains(t, e, "SpeedMbps")
	})

	t.Run("unknown members are 404, not the system body", func(t *testing.T) {
		for _, p := range []string{
			"/redfish/v1/Systems/vm-1/Memory/2",
			"/redfish/v1/Systems/vm-1/Processors/CPU2",
			"/redfish/v1/Systems/vm-1/Processors/CPUx",
			"/redfish/v1/Systems/vm-1/Processors/CPU01",
			"/redfish/v1/Systems/vm-1/Storage/2",
			"/redfish/v1/Systems/vm-1/Storage/1/Drives/nope",
			"/redfish/v1/Systems/vm-1/EthernetInterfaces/nope",
			"/redfish/v1/Systems/vm-1/EthernetInterfaces/default/extra",
		} {
			assert.Equal(t, http.StatusNotFound, getSystemPath(srv, p).Code, p)
		}
	})

	t.Run("non-GET is rejected", func(t *testing.T) {
		req := httptest.NewRequest("POST", "/redfish/v1/Systems/vm-1/Storage", nil)
		req = req.WithContext(logger.WithAuth(req.Context(), &auth.AuthContext{User: &auth.User{Username: "u", Chassis: []string{"c"}}}))
		w := httptest.NewRecorder()
		srv.handleSystem(w, req)
		assert.Equal(t, http.StatusMethodNotAllowed, w.Code)
	})
}

func TestProcessorArchitecture(t *testing.T) {
	for _, tc := range []struct{ arch, wantArch, wantISA string }{
		{"amd64", "x86", "x86-64"},
		{"arm64", "ARM", "ARM-A64"},
		{"s390x", "", ""},
		{"ppc64le", "", ""},
	} {
		t.Run(tc.arch, func(t *testing.T) {
			srv := newDevicesTestServer(t, tc.arch)
			w := getSystemPath(srv, "/redfish/v1/Systems/vm-1/Processors/CPU0")
			require.Equal(t, http.StatusOK, w.Code, w.Body.String())
			var p map[string]interface{}
			require.NoError(t, json.Unmarshal(w.Body.Bytes(), &p))
			if tc.wantArch == "" {
				assert.NotContains(t, p, "ProcessorArchitecture")
				assert.NotContains(t, p, "InstructionSet")
				return
			}
			assert.Equal(t, tc.wantArch, p["ProcessorArchitecture"])
			assert.Equal(t, tc.wantISA, p["InstructionSet"])
		})
	}
}

// A VM that declares nothing (no disks, NICs, CPU or memory) must still give
// well-formed resources: empty collections as [] (not null), no invented
// values, and 404 for members that do not exist.
func TestSystemDevices_EmptyVM(t *testing.T) {
	for name, spec := range map[string]*kubevirtv1.VirtualMachineInstanceTemplateSpec{
		"empty spec":  {},
		"no template": nil,
	} {
		t.Run(name, func(t *testing.T) {
			mock := kubevirt.NewMockDynamicClient()
			require.NoError(t, mock.AddVM(&kubevirtv1.VirtualMachine{
				ObjectMeta: metav1.ObjectMeta{Name: "vm-empty", Namespace: "ns"},
				Spec:       kubevirtv1.VirtualMachineSpec{Template: spec},
			}))
			cfg := &config.Config{
				Server: config.ServerConfig{Host: "localhost", Port: 8080, TestMode: true},
				Auth: config.AuthConfig{Users: []config.UserConfig{
					{Username: "u", Password: config.PasswordConfig{Plain: "p"}, Chassis: []string{"c"}},
				}},
				Chassis: []config.ChassisConfig{{Name: "c", Namespace: "ns"}},
			}
			srv := NewServer(cfg, kubevirt.NewClientWithClients(fake.NewSimpleClientset(), mock, 30*time.Second, nil))

			raw := func(path string) (int, string) {
				w := getSystemPath(srv, path)
				return w.Code, w.Body.String()
			}
			get := func(path string) map[string]interface{} {
				code, body := raw(path)
				require.Equal(t, http.StatusOK, code, "%s: %s", path, body)
				var m map[string]interface{}
				require.NoError(t, json.Unmarshal([]byte(body), &m))
				return m
			}
			const base = "/redfish/v1/Systems/vm-empty"

			sys := get(base)
			assert.Empty(t, sys["MemorySummary"], "no memory declared, so no total is invented")
			assert.EqualValues(t, 1, sys["ProcessorSummary"].(map[string]interface{})["Count"])

			storage := get(base + "/Storage/1")
			assert.EqualValues(t, 0, storage["Drives@odata.count"])
			assert.Equal(t, []interface{}{}, storage["Drives"], "Drives must be [] and not null")
			assert.Equal(t, http.StatusNotFound, getSystemPath(srv, base+"/Storage/1/Drives/root").Code)

			nics := get(base + "/EthernetInterfaces")
			assert.EqualValues(t, 0, nics["Members@odata.count"])
			assert.Equal(t, []interface{}{}, nics["Members"], "Members must be [] and not null")
			assert.Equal(t, http.StatusNotFound, getSystemPath(srv, base+"/EthernetInterfaces/default").Code)

			mem := get(base + "/Memory/1")
			assert.NotContains(t, mem, "CapacityMiB")
			assert.NotContains(t, mem, "Oem")

			assert.EqualValues(t, 1, get(base + "/Processors")["Members@odata.count"])
			cpu := get(base + "/Processors/CPU0")
			assert.EqualValues(t, 1, cpu["TotalCores"])
			assert.EqualValues(t, 1, cpu["TotalThreads"])
			assert.NotContains(t, cpu, "Model")
			assert.NotContains(t, cpu, "InstructionSet")
		})
	}
}

// Users without access to the VM's chassis, or without any authentication
// context, must get nothing from any of the new endpoints.
func TestSystemDevices_Unauthorized(t *testing.T) {
	srv := newDevicesTestServer(t, "amd64")
	leaks := []string{"CapacityMiB", "CapacityBytes", "MACAddress", "TotalCores", "KubeVirt", "LinkStatus"}

	for _, path := range []string{
		"/redfish/v1/Systems/vm-1/Memory",
		"/redfish/v1/Systems/vm-1/Memory/1",
		"/redfish/v1/Systems/vm-1/Processors",
		"/redfish/v1/Systems/vm-1/Processors/CPU0",
		"/redfish/v1/Systems/vm-1/Storage",
		"/redfish/v1/Systems/vm-1/Storage/1",
		"/redfish/v1/Systems/vm-1/Storage/1/Drives/root",
		"/redfish/v1/Systems/vm-1/EthernetInterfaces",
		"/redfish/v1/Systems/vm-1/EthernetInterfaces/default",
		"/redfish/v1/Systems/ns.vm-1/Storage/1/Drives/data",
	} {
		t.Run(path, func(t *testing.T) {
			outsider := getSystemPathAs(srv, path, &auth.User{Username: "outsider", Chassis: []string{"other"}})
			assert.Equal(t, http.StatusNotFound, outsider.Code, "a user of another chassis")
			anonymous := getSystemPathAs(srv, path, nil)
			assert.Equal(t, http.StatusForbidden, anonymous.Code, "no authentication context")
			for _, body := range []string{outsider.Body.String(), anonymous.Body.String()} {
				for _, leak := range leaks {
					assert.NotContains(t, body, leak)
				}
			}
		})
	}
}

// A running VM reports LinkUp, the MAC KubeVirt assigned and its addresses, split
// into IPv4 and IPv6. Anything that is not an IP address is dropped.
func TestSystemDevices_RunningNIC(t *testing.T) {
	vmi := &kubevirtv1.VirtualMachineInstance{
		ObjectMeta: metav1.ObjectMeta{Name: "vm-1", Namespace: "ns"},
		Status: kubevirtv1.VirtualMachineInstanceStatus{Interfaces: []kubevirtv1.VirtualMachineInstanceNetworkInterface{
			{Name: "default", MAC: "52:54:00:aa:bb:cc", IPs: []string{"10.0.0.5", "fd00::5", "not-an-ip", "10.0.0.6"}},
		}},
	}
	srv := newDevicesTestServerWith(t, "", vmi, nil)
	w := getSystemPath(srv, "/redfish/v1/Systems/vm-1/EthernetInterfaces/default")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var nic map[string]interface{}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &nic))

	assert.Equal(t, "LinkUp", nic["LinkStatus"])
	assert.Equal(t, "52:54:00:aa:bb:cc", nic["MACAddress"], "the running MAC wins over the spec MAC")
	assert.Equal(t, []interface{}{
		map[string]interface{}{"Address": "10.0.0.5"},
		map[string]interface{}{"Address": "10.0.0.6"},
	}, nic["IPv4Addresses"])
	assert.Equal(t, []interface{}{map[string]interface{}{"Address": "fd00::5"}}, nic["IPv6Addresses"])
}

// When the cluster cannot be read for a reason other than "not found", the
// endpoints answer 500 instead of a plausible but wrong 200.
func TestSystemDevices_ClusterReadErrors(t *testing.T) {
	t.Run("PVC lookup forbidden gives 500 for the drive, names still list", func(t *testing.T) {
		mock := kubevirt.NewMockDynamicClient()
		require.NoError(t, mock.AddVM(&kubevirtv1.VirtualMachine{
			ObjectMeta: metav1.ObjectMeta{Name: "vm-pvc", Namespace: "ns"},
			Spec: kubevirtv1.VirtualMachineSpec{Template: &kubevirtv1.VirtualMachineInstanceTemplateSpec{
				Spec: kubevirtv1.VirtualMachineInstanceSpec{
					Domain: kubevirtv1.DomainSpec{Devices: kubevirtv1.Devices{Disks: []kubevirtv1.Disk{
						{Name: "data", DiskDevice: kubevirtv1.DiskDevice{Disk: &kubevirtv1.DiskTarget{Bus: "virtio"}}},
					}}},
					Volumes: []kubevirtv1.Volume{{Name: "data", VolumeSource: kubevirtv1.VolumeSource{
						PersistentVolumeClaim: &kubevirtv1.PersistentVolumeClaimVolumeSource{
							PersistentVolumeClaimVolumeSource: corev1.PersistentVolumeClaimVolumeSource{ClaimName: "claim"}}}}},
				},
			}},
		}))
		k8s := fake.NewSimpleClientset()
		k8s.PrependReactor("get", "persistentvolumeclaims", func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "persistentvolumeclaims"}, "claim", errors.New("no access"))
		})
		cfg := &config.Config{
			Server: config.ServerConfig{Host: "localhost", Port: 8080, TestMode: true},
			Auth: config.AuthConfig{Users: []config.UserConfig{
				{Username: "u", Password: config.PasswordConfig{Plain: "p"}, Chassis: []string{"c"}},
			}},
			Chassis: []config.ChassisConfig{{Name: "c", Namespace: "ns"}},
		}
		srv := NewServer(cfg, kubevirt.NewClientWithClients(k8s, mock, 30*time.Second, nil))

		w := getSystemPath(srv, "/redfish/v1/Systems/vm-pvc/Storage/1/Drives/data")
		assert.Equal(t, http.StatusInternalServerError, w.Code, "a size that cannot be read is an error, not a guess")
		assert.NotContains(t, w.Body.String(), "CapacityBytes")
		assert.Equal(t, http.StatusOK, getSystemPath(srv, "/redfish/v1/Systems/vm-pvc/Storage/1").Code,
			"the controller listing needs no PVC lookup")
	})

	t.Run("CPU topology unreadable leaves the CPU fields out", func(t *testing.T) {
		srv := newDevicesTestServer(t, "")
		summary, oem := srv.processorFields("ns", "does-not-exist")
		assert.Nil(t, summary, "no guessed ProcessorSummary")
		assert.Nil(t, oem, "no guessed Oem topology")
		summary, oem = srv.processorFields("ns", "vm-1")
		require.NotNil(t, summary)
		assert.Equal(t, 2, summary.Count)
		assert.NotNil(t, oem)
	})
}
