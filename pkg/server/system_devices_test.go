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
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	kubevirtv1 "kubevirt.io/api/core/v1"
)

func newDevicesTestServer(t *testing.T, arch string) *Server {
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

	cfg := &config.Config{
		Server: config.ServerConfig{Host: "localhost", Port: 8080, TestMode: true},
		Auth: config.AuthConfig{Users: []config.UserConfig{
			{Username: "u", Password: config.PasswordConfig{Plain: "p"}, Chassis: []string{"c"}},
		}},
		Chassis: []config.ChassisConfig{{Name: "c", Namespace: "ns"}},
	}
	client := kubevirt.NewClientWithClients(fake.NewSimpleClientset(), mock, 30*time.Second, nil)
	return NewServer(cfg, client)
}

func getSystemPath(srv *Server, path string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("GET", path, nil)
	authCtx := &auth.AuthContext{User: &auth.User{Username: "u", Chassis: []string{"c"}}}
	req = req.WithContext(logger.WithAuth(req.Context(), authCtx))
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
		topo := ps["Oem"].(map[string]interface{})["KubeVirt"].(map[string]interface{})
		assert.EqualValues(t, 2, topo["Sockets"])
		assert.EqualValues(t, 2, topo["CoresPerSocket"])
		assert.EqualValues(t, 2, topo["ThreadsPerCore"])
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
		get(t, "/redfish/v1/Systems/vm-1/Storage/")
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

func TestRedfishArchitecture(t *testing.T) {
	for in, want := range map[string][2]string{
		"amd64":   {"x86", "x86-64"},
		"arm64":   {"ARM", "ARM-A64"},
		"s390x":   {"", ""},
		"ppc64le": {"", ""},
	} {
		arch, isa := redfishArchitecture(in)
		assert.Equal(t, want, [2]string{arch, isa}, in)
	}
}

func TestProcessorArchitecture(t *testing.T) {
	for _, tc := range []struct{ arch, wantArch, wantISA string }{
		{"amd64", "x86", "x86-64"},
		{"arm64", "ARM", "ARM-A64"},
		{"s390x", "", ""},
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
