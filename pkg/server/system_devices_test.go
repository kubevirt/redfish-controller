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
 * Copyright 2026 KubeVirt Redfish project and its authors.
 *
 */

package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
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
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	kubevirtv1 "kubevirt.io/api/core/v1"
)

func newDevicesTestServer(t *testing.T, arch string) *Server {
	t.Helper()
	return newDevicesTestServerWith(t, arch, nil, nil)
}

// newDevicesTestServerWith builds the fixed VM "vm-1" (2x2x2 CPU, 512Mi, one NIC
// with MAC 02:00:00:00:00:01, two disks) and optionally a running VMI and a custom
// fake Kubernetes client. The VMI is NOT derived from the VM: a test that passes
// one decides which values it shares with the VM and which deliberately differ.
// Name and namespace must match ("vm-1"/"ns"), which is how the pair is tied together.
func newDevicesTestServerWith(t *testing.T, arch string, vmi *kubevirtv1.VirtualMachineInstance, k8s *fake.Clientset, wrap ...func(dynamic.Interface) dynamic.Interface) *Server {
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
		require.Equal(t, vm.Name, vmi.Name, "the VMI must belong to the VM")
		require.Equal(t, vm.Namespace, vmi.Namespace, "the VMI must belong to the VM")
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
	var dyn dynamic.Interface = mock
	for _, w := range wrap {
		dyn = w(dyn)
	}
	client := kubevirt.NewClientWithClients(k8s, dyn, 30*time.Second, nil)
	return NewServer(cfg, client)
}

// failVMIReads makes every VMI Get fail with err; everything else passes through.
func failVMIReads(err error) func(dynamic.Interface) dynamic.Interface {
	return func(d dynamic.Interface) dynamic.Interface { return failingVMIs{Interface: d, err: err} }
}

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

func (g failingVMIGet) Get(context.Context, string, metav1.GetOptions, ...string) (*unstructured.Unstructured, error) {
	return nil, g.err
}

// vmiOf builds a VMI that belongs to the fixed VM "vm-1".
func vmiOf(spec kubevirtv1.VirtualMachineInstanceSpec, status kubevirtv1.VirtualMachineInstanceStatus) *kubevirtv1.VirtualMachineInstance {
	return &kubevirtv1.VirtualMachineInstance{ObjectMeta: metav1.ObjectMeta{Name: "vm-1", Namespace: "ns"}, Spec: spec, Status: status}
}

func getJSON(t *testing.T, srv *Server, path string) map[string]interface{} {
	t.Helper()
	w := getSystemPath(srv, path)
	require.Equal(t, http.StatusOK, w.Code, path+": "+w.Body.String())
	var m map[string]interface{}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &m))
	return m
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

	// A PATCH under /Systems/{id}/ is a boot update on the system itself. On these
	// read-only resources it must be answered with 405, not treated as a boot update.
	t.Run("non-GET is rejected", func(t *testing.T) {
		bootBody := `{"Boot": {"BootSourceOverrideTarget": "Cd", "BootSourceOverrideEnabled": "Once"}}`
		for _, method := range []string{"POST", "PATCH", "PUT", "DELETE"} {
			for _, p := range []string{
				"/redfish/v1/Systems/vm-1/Memory/1",
				"/redfish/v1/Systems/vm-1/Processors",
				"/redfish/v1/Systems/vm-1/Storage",
				"/redfish/v1/Systems/vm-1/Storage/1/Drives/root",
				"/redfish/v1/Systems/vm-1/EthernetInterfaces/default",
			} {
				req := httptest.NewRequest(method, p, strings.NewReader(bootBody))
				req = req.WithContext(logger.WithAuth(req.Context(), &auth.AuthContext{User: &auth.User{Username: "u", Chassis: []string{"c"}}}))
				w := httptest.NewRecorder()
				srv.handleSystem(w, req)
				assert.Equal(t, http.StatusMethodNotAllowed, w.Code, method+" "+p)
				assert.Equal(t, "GET", w.Header().Get("Allow"), method+" "+p)
			}
		}
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
	vmi := vmiOf(
		kubevirtv1.VirtualMachineInstanceSpec{Domain: kubevirtv1.DomainSpec{Devices: kubevirtv1.Devices{
			Interfaces: []kubevirtv1.Interface{{Name: "default"}}}}},
		kubevirtv1.VirtualMachineInstanceStatus{Interfaces: []kubevirtv1.VirtualMachineInstanceNetworkInterface{
			{Name: "default", MAC: "52:54:00:aa:bb:cc", IPs: []string{"10.0.0.5", "fd00::5", "not-an-ip", "10.0.0.6"}},
		}})
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

// The running VMI is what the VM is doing now; the VM spec is only what is staged
// for the next start. Every value the VMI sets must win over the VM's.
func TestSystemDevices_RunningVMIWinsOverVMSpec(t *testing.T) {
	running := resource.MustParse("1Gi")
	vmi := &kubevirtv1.VirtualMachineInstance{
		ObjectMeta: metav1.ObjectMeta{Name: "vm-1", Namespace: "ns"},
		Spec: kubevirtv1.VirtualMachineInstanceSpec{
			Architecture: "arm64",
			Domain: kubevirtv1.DomainSpec{
				CPU:    &kubevirtv1.CPU{Sockets: 1, Cores: 3, Threads: 1, Model: "running-model"},
				Memory: &kubevirtv1.Memory{Guest: &running},
			},
		},
	}
	// The VM spec says amd64, 2 sockets x 2 cores x 2 threads, Skylake-Client, 512Mi.
	srv := newDevicesTestServerWith(t, "amd64", vmi, nil)
	get := func(path string) map[string]interface{} {
		w := getSystemPath(srv, path)
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		var m map[string]interface{}
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &m))
		return m
	}

	cpu := get("/redfish/v1/Systems/vm-1/Processors/CPU0")
	assert.Equal(t, "running-model", cpu["Model"])
	assert.EqualValues(t, 3, cpu["TotalCores"])
	assert.Equal(t, "ARM", cpu["ProcessorArchitecture"])
	assert.Equal(t, http.StatusNotFound, getSystemPath(srv, "/redfish/v1/Systems/vm-1/Processors/CPU1").Code,
		"the VMI has 1 socket, the staged VM spec has 2")

	assert.EqualValues(t, 1024, get("/redfish/v1/Systems/vm-1/Memory/1")["CapacityMiB"])

	summary := get("/redfish/v1/Systems/vm-1")["ProcessorSummary"].(map[string]interface{})
	assert.EqualValues(t, 3, summary["LogicalProcessorCount"])
}

// When the cluster cannot be read for a reason other than "not found", the
// endpoints answer 500 instead of a plausible but wrong 200. The 500 (not 403)
// is deliberate: the caller already passed the access check, and it is the
// controller's own service account that lacks the permission. That is a server
// fault the client cannot fix, so it must not look like the caller was denied.
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

// A POST to the Reset action of the system itself reaches the power handler. The
// same action path below a read-only resource must not: it answers 405 instead
// of power-cycling the VM from /Memory/1.
func TestSystemDevices_ResetActionOnlyOnTheSystem(t *testing.T) {
	srv := newDevicesTestServer(t, "")
	post := func(path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", path, strings.NewReader(body))
		req = req.WithContext(logger.WithAuth(req.Context(), &auth.AuthContext{User: &auth.User{Username: "u", Chassis: []string{"c"}}}))
		w := httptest.NewRecorder()
		srv.handleSystem(w, req)
		return w
	}

	for _, p := range []string{
		"/redfish/v1/Systems/vm-1/Memory/1/Actions/ComputerSystem.Reset",
		"/redfish/v1/Systems/vm-1/Processors/Actions/ComputerSystem.Reset",
		"/redfish/v1/Systems/vm-1/Storage/1/Actions/ComputerSystem.Reset",
		"/redfish/v1/Systems/vm-1/EthernetInterfaces/default/Actions/ComputerSystem.Reset",
	} {
		w := post(p, `{"ResetType":"ForceOff"}`)
		assert.Equal(t, http.StatusMethodNotAllowed, w.Code, p)
		assert.Equal(t, "GET", w.Header().Get("Allow"), p)
	}

	// The real action still goes to handlePowerAction, which rejects a bad body
	// with 400 (a 405 or a 200 would mean it was routed somewhere else).
	for _, p := range []string{
		"/redfish/v1/Systems/vm-1/Actions/ComputerSystem.Reset",
		"/redfish/v1/Systems/vm-1/Actions/ComputerSystem.Reset/",
	} {
		assert.Equal(t, http.StatusBadRequest, post(p, "not json").Code, p)
	}
}

func TestIsResetActionPath(t *testing.T) {
	for path, want := range map[string]bool{
		"/redfish/v1/Systems/vm-1/Actions/ComputerSystem.Reset":                 true,
		"/redfish/v1/Systems/vm-1/Actions/ComputerSystem.Reset/":                true,
		"/redfish/v1/Systems/vm-1/Memory/1/Actions/ComputerSystem.Reset":        false,
		"/redfish/v1/Systems/vm-1/Actions/ComputerSystem.Reset/extra":           false,
		"/redfish/v1/Systems/vm-1/Actions":                                      false,
		"/redfish/v1/Systems/vm-1":                                              false,
		"/redfish/v1/Systems/vm-1/Actions/ComputerSystem.Reset/Actions/x/y/z/w": false,
	} {
		assert.Equal(t, want, isResetActionPath(strings.Split(path, "/")), path)
	}
}

// Whatever the endpoint, a VMI that cannot be read (not "not found") is a 500
// with no data, never the staged VM spec or an empty list.
func TestSystemDevices_VMIReadErrorsAre500(t *testing.T) {
	forbidden := apierrors.NewForbidden(schema.GroupResource{Resource: "virtualmachineinstances"}, "vm-1", errors.New("no access"))
	srv := newDevicesTestServerWith(t, "", nil, nil, failVMIReads(forbidden))
	for _, p := range []string{
		"/redfish/v1/Systems/vm-1/Memory/1",
		"/redfish/v1/Systems/vm-1/Processors",
		"/redfish/v1/Systems/vm-1/Processors/CPU0",
		"/redfish/v1/Systems/vm-1/Storage/1",
		"/redfish/v1/Systems/vm-1/Storage/1/Drives/data",
		"/redfish/v1/Systems/vm-1/EthernetInterfaces",
		"/redfish/v1/Systems/vm-1/EthernetInterfaces/default",
	} {
		w := getSystemPath(srv, p)
		assert.Equal(t, http.StatusInternalServerError, w.Code, p)
		for _, leak := range []string{"CapacityMiB", "TotalCores", "MACAddress", "CapacityBytes", "Drives\":[{", "Members\":[{"} {
			assert.NotContains(t, w.Body.String(), leak, p)
		}
	}
}

// 3 sockets x 2 cores x 1 thread: no threading, three processors, and every
// number comes from the right field (the sockets, cores and threads all differ).
func TestSystemDevices_AsymmetricTopology(t *testing.T) {
	for _, tc := range []struct {
		name                           string
		sockets, cores, threads        uint32
		wantThreading                  bool
		wantTotalThreads, wantLogicals int
	}{
		{"no threading", 3, 2, 1, false, 2, 6},
		{"threading", 1, 3, 4, true, 12, 12},
	} {
		t.Run(tc.name, func(t *testing.T) {
			vmi := vmiOf(kubevirtv1.VirtualMachineInstanceSpec{Domain: kubevirtv1.DomainSpec{
				CPU: &kubevirtv1.CPU{Sockets: tc.sockets, Cores: tc.cores, Threads: tc.threads}}}, kubevirtv1.VirtualMachineInstanceStatus{})
			srv := newDevicesTestServerWith(t, "", vmi, nil)

			coll := getJSON(t, srv, "/redfish/v1/Systems/vm-1/Processors")
			assert.EqualValues(t, tc.sockets, coll["Members@odata.count"])
			last := int(tc.sockets) - 1
			cpu := getJSON(t, srv, "/redfish/v1/Systems/vm-1/Processors/CPU"+strconv.Itoa(last))
			assert.Equal(t, "vCPU Socket "+strconv.Itoa(last), cpu["Name"])
			assert.Equal(t, "CPU"+strconv.Itoa(last), cpu["Id"])
			assert.EqualValues(t, tc.cores, cpu["TotalCores"])
			assert.EqualValues(t, tc.wantTotalThreads, cpu["TotalThreads"])
			assert.Equal(t, http.StatusNotFound, getSystemPath(srv, "/redfish/v1/Systems/vm-1/Processors/CPU"+strconv.Itoa(int(tc.sockets))).Code)

			sys := getJSON(t, srv, "/redfish/v1/Systems/vm-1")
			sum := sys["ProcessorSummary"].(map[string]interface{})
			assert.EqualValues(t, tc.sockets, sum["Count"])
			assert.EqualValues(t, tc.sockets*tc.cores, sum["CoreCount"])
			assert.EqualValues(t, tc.wantLogicals, sum["LogicalProcessorCount"])
			assert.Equal(t, tc.wantThreading, sum["ThreadingEnabled"])
			oem := sys["Oem"].(map[string]interface{})["KubeVirt"].(map[string]interface{})["Processors"].(map[string]interface{})
			assert.EqualValues(t, tc.sockets, oem["Sockets"])
			assert.EqualValues(t, tc.cores, oem["CoresPerSocket"])
			assert.EqualValues(t, tc.threads, oem["ThreadsPerCore"])
		})
	}
}

// During a hotplug the status says what the guest has; it wins over the VMI spec
// for cores and threads as well as sockets, and for memory.
func TestSystemDevices_HotplugStatusWins(t *testing.T) {
	spec := kubevirtv1.VirtualMachineInstanceSpec{Domain: kubevirtv1.DomainSpec{CPU: &kubevirtv1.CPU{Sockets: 1, Cores: 1, Threads: 1}}}
	guest := resource.MustParse("2Gi")
	vmi := vmiOf(spec, kubevirtv1.VirtualMachineInstanceStatus{
		CurrentCPUTopology: &kubevirtv1.CPUTopology{Sockets: 2, Cores: 3, Threads: 2},
		Memory:             &kubevirtv1.MemoryStatus{GuestCurrent: &guest},
	})
	srv := newDevicesTestServerWith(t, "", vmi, nil)

	cpu := getJSON(t, srv, "/redfish/v1/Systems/vm-1/Processors/CPU1")
	assert.EqualValues(t, 3, cpu["TotalCores"])
	assert.EqualValues(t, 6, cpu["TotalThreads"])
	assert.EqualValues(t, 2048, getJSON(t, srv, "/redfish/v1/Systems/vm-1/Memory/1")["CapacityMiB"])
	sys := getJSON(t, srv, "/redfish/v1/Systems/vm-1")
	assert.EqualValues(t, 12, sys["ProcessorSummary"].(map[string]interface{})["LogicalProcessorCount"])
	assert.EqualValues(t, 2, sys["MemorySummary"].(map[string]interface{})["TotalSystemMemoryGiB"])
}

// Odd member paths are 404 and a trailing slash on a member is tolerated.
func TestSystemDevices_OddPaths(t *testing.T) {
	srv := newDevicesTestServer(t, "")
	for _, p := range []string{
		"/redfish/v1/Systems/vm-1/Processors/CPU-1",
		"/redfish/v1/Systems/vm-1/Processors/CPU0/extra",
		"/redfish/v1/Systems/vm-1/Storage/1/foo",
		"/redfish/v1/Systems/vm-1/Storage/1/Drives",
		"/redfish/v1/Systems/vm-1/Storage/1/Drives/root/x",
		"/redfish/v1/Systems/vm-1/Memory/1/x",
		"/redfish/v1/Systems/vm-1/EthernetInterfaces/default/x",
	} {
		assert.Equal(t, http.StatusNotFound, getSystemPath(srv, p).Code, p)
	}
	for _, p := range []string{
		"/redfish/v1/Systems/vm-1/Memory/1/",
		"/redfish/v1/Systems/vm-1/Processors/CPU0/",
		"/redfish/v1/Systems/vm-1/EthernetInterfaces/",
		"/redfish/v1/Systems/vm-1/Storage/1/",
	} {
		assert.Equal(t, http.StatusOK, getSystemPath(srv, p).Code, p)
	}
}

// A VMI that is Succeeded or Failed is a stopped VM: everything is read from the
// VM spec and the NIC has no link.
func TestSystemDevices_FinishedVMIReadsTheVM(t *testing.T) {
	for _, phase := range []kubevirtv1.VirtualMachineInstancePhase{kubevirtv1.Succeeded, kubevirtv1.Failed} {
		t.Run(string(phase), func(t *testing.T) {
			guest := resource.MustParse("8Gi")
			vmi := vmiOf(kubevirtv1.VirtualMachineInstanceSpec{Domain: kubevirtv1.DomainSpec{
				CPU:     &kubevirtv1.CPU{Sockets: 1, Cores: 1, Threads: 1, Model: "dead-vmi"},
				Memory:  &kubevirtv1.Memory{Guest: &guest},
				Devices: kubevirtv1.Devices{Interfaces: []kubevirtv1.Interface{{Name: "default"}}},
			}}, kubevirtv1.VirtualMachineInstanceStatus{
				Phase: phase,
				Interfaces: []kubevirtv1.VirtualMachineInstanceNetworkInterface{
					{Name: "default", MAC: "52:54:00:de:ad:00", IPs: []string{"10.9.9.9"}}},
			})
			srv := newDevicesTestServerWith(t, "", vmi, nil)

			// The VM spec has 2x2x2, Skylake-Client, 512Mi and MAC 02:00:00:00:00:01.
			assert.Equal(t, "Skylake-Client", getJSON(t, srv, "/redfish/v1/Systems/vm-1/Processors/CPU1")["Model"])
			assert.EqualValues(t, 512, getJSON(t, srv, "/redfish/v1/Systems/vm-1/Memory/1")["CapacityMiB"])
			nic := getJSON(t, srv, "/redfish/v1/Systems/vm-1/EthernetInterfaces/default")
			assert.Equal(t, "NoLink", nic["LinkStatus"])
			assert.Equal(t, "02:00:00:00:00:01", nic["MACAddress"])
			assert.Nil(t, nic["IPv4Addresses"])
		})
	}
}

// A virtual NIC the VMI reports as down is NoLink, an up or unreported state on a
// running NIC is LinkUp.
func TestSystemDevices_NICLinkState(t *testing.T) {
	spec := kubevirtv1.VirtualMachineInstanceSpec{Domain: kubevirtv1.DomainSpec{Devices: kubevirtv1.Devices{
		Interfaces: []kubevirtv1.Interface{{Name: "up"}, {Name: "down"}, {Name: "unreported"}, {Name: "booting"}}}}}
	vmi := vmiOf(spec, kubevirtv1.VirtualMachineInstanceStatus{Interfaces: []kubevirtv1.VirtualMachineInstanceNetworkInterface{
		{Name: "up", LinkState: "up"}, {Name: "down", LinkState: "down"}, {Name: "unreported"},
	}})
	srv := newDevicesTestServerWith(t, "", vmi, nil)
	for name, want := range map[string]string{"up": "LinkUp", "down": "NoLink", "unreported": "LinkUp", "booting": "NoLink"} {
		assert.Equal(t, want, getJSON(t, srv, "/redfish/v1/Systems/vm-1/EthernetInterfaces/"+name)["LinkStatus"], name)
	}
}

// Disks and NICs of a running VM are the VMI's: a volume added to the running VM
// only is listed, one staged in the VM spec is not.
func TestSystemDevices_StorageAndNICsFollowTheVMI(t *testing.T) {
	vmi := vmiOf(kubevirtv1.VirtualMachineInstanceSpec{Domain: kubevirtv1.DomainSpec{Devices: kubevirtv1.Devices{
		Disks: []kubevirtv1.Disk{
			{Name: "root", DiskDevice: kubevirtv1.DiskDevice{Disk: &kubevirtv1.DiskTarget{Bus: "virtio"}}},
			{Name: "hotplugged", DiskDevice: kubevirtv1.DiskDevice{Disk: &kubevirtv1.DiskTarget{Bus: "scsi"}}},
		},
		Interfaces: []kubevirtv1.Interface{{Name: "default"}, {Name: "hot-nic"}},
	}}}, kubevirtv1.VirtualMachineInstanceStatus{})
	srv := newDevicesTestServerWith(t, "", vmi, nil)

	// The VM spec has the disks root and data and the NIC default.
	st := getJSON(t, srv, "/redfish/v1/Systems/vm-1/Storage/1")
	assert.Equal(t, []interface{}{
		map[string]interface{}{"@odata.id": "/redfish/v1/Systems/vm-1/Storage/1/Drives/root"},
		map[string]interface{}{"@odata.id": "/redfish/v1/Systems/vm-1/Storage/1/Drives/hotplugged"},
	}, st["Drives"])
	assert.Equal(t, http.StatusNotFound, getSystemPath(srv, "/redfish/v1/Systems/vm-1/Storage/1/Drives/data").Code, "staged in the VM only")
	assert.Equal(t, http.StatusOK, getSystemPath(srv, "/redfish/v1/Systems/vm-1/Storage/1/Drives/hotplugged").Code)
	assert.EqualValues(t, 2, getJSON(t, srv, "/redfish/v1/Systems/vm-1/EthernetInterfaces")["Members@odata.count"])
}
