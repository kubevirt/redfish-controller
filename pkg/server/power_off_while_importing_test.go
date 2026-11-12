package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kubevirt/redfish-controller/pkg/auth"
	"github.com/kubevirt/redfish-controller/pkg/config"
	"github.com/kubevirt/redfish-controller/pkg/kubevirt"
	"github.com/kubevirt/redfish-controller/pkg/logger"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	kubevirtv1 "kubevirt.io/api/core/v1"
)

const (
	poffTestNS = "ns-a"
	poffTestVM = "vm-a"
)

// poffTestServer builds a server over a halted VM whose ISO import is still
// in progress (importing label set), the state in which Ironic may decide to
// power the host off.
func poffTestServer(t *testing.T) (*Server, *kubevirt.MockDynamicClient) {
	t.Helper()
	mockDynamic := kubevirt.NewMockDynamicClient()
	halted := kubevirtv1.RunStrategyHalted
	if err := mockDynamic.AddVM(&kubevirtv1.VirtualMachine{
		ObjectMeta: metav1.ObjectMeta{
			Name:      poffTestVM,
			Namespace: poffTestNS,
			Labels: map[string]string{
				"redfish-enabled":                        "true",
				kubevirt.ImportingLabelPrefix + "cdrom0": "copy-iso-vm-a-bootiso-1",
			},
		},
		Spec: kubevirtv1.VirtualMachineSpec{
			RunStrategy: &halted,
			Template: &kubevirtv1.VirtualMachineInstanceTemplateSpec{
				Spec: kubevirtv1.VirtualMachineInstanceSpec{
					Domain:  kubevirtv1.DomainSpec{Devices: kubevirtv1.Devices{Disks: []kubevirtv1.Disk{{Name: "rootdisk"}}}},
					Volumes: []kubevirtv1.Volume{{Name: "rootdisk"}},
				},
			},
		},
	}); err != nil {
		t.Fatalf("AddVM failed: %v", err)
	}
	client := kubevirt.NewClientWithClients(fake.NewSimpleClientset(), mockDynamic, 30*time.Second, nil)

	cfg := &config.Config{
		Server: config.ServerConfig{Host: "localhost", Port: 8080, TestMode: true},
		Auth: config.AuthConfig{
			Users: []config.UserConfig{
				{Username: "user", Password: config.PasswordConfig{Plain: "pass"}, Chassis: []string{"chassis-a"}},
			},
		},
		Chassis: []config.ChassisConfig{
			{
				Name:       "chassis-a",
				Namespace:  poffTestNS,
				VMSelector: &kubevirt.VMSelectorConfig{Labels: map[string]string{"redfish-enabled": "true"}},
			},
		},
	}
	return NewServer(cfg, client), mockDynamic
}

func poffReset(server *Server, resetType string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", "/redfish/v1/Systems/"+poffTestVM+"/Actions/ComputerSystem.Reset",
		strings.NewReader(`{"ResetType":"`+resetType+`"}`))
	req.Header.Set("Content-Type", "application/json")
	authCtx := &auth.AuthContext{User: &auth.User{Username: "user", Chassis: []string{"chassis-a"}}}
	req = req.WithContext(logger.WithAuth(req.Context(), authCtx))
	w := httptest.NewRecorder()
	server.handlePowerAction(w, req, poffTestVM)
	return w
}

func poffVM(t *testing.T, mockDynamic *kubevirt.MockDynamicClient) *kubevirtv1.VirtualMachine {
	t.Helper()
	vm, err := mockDynamic.GetVM(poffTestNS, poffTestVM)
	if err != nil {
		t.Fatalf("GetVM failed: %v", err)
	}
	return vm
}

// Actions that would start the VM wait for the import; the rest run now.
func TestHandlePowerAction_OnlyStartingActionsAreDeferredWhileImporting(t *testing.T) {
	tests := []struct {
		resetType string
		deferred  bool
	}{
		{"On", true},
		{"ForceRestart", true},
		{"GracefulRestart", true},
		{"ForceOff", false},
		{"GracefulShutdown", false},
	}
	for _, tt := range tests {
		t.Run(tt.resetType, func(t *testing.T) {
			server, mockDynamic := poffTestServer(t)

			w := poffReset(server, tt.resetType)

			vm := poffVM(t, mockDynamic)
			deferredTo := vm.GetLabels()[kubevirt.PowerAfterImportLabel]
			if tt.deferred {
				if w.Code != http.StatusAccepted || !strings.Contains(w.Body.String(), "deferred") {
					t.Fatalf("%s: status %d, body %s; want 202 deferred", tt.resetType, w.Code, w.Body.String())
				}
				if deferredTo != tt.resetType {
					t.Errorf("%s: label %s = %q, want %q", tt.resetType, kubevirt.PowerAfterImportLabel, deferredTo, tt.resetType)
				}
				return
			}
			if w.Code != http.StatusOK {
				t.Fatalf("%s: status %d, body %s; want 200", tt.resetType, w.Code, w.Body.String())
			}
			if deferredTo != "" {
				t.Errorf("%s: label %s = %q, want none", tt.resetType, kubevirt.PowerAfterImportLabel, deferredTo)
			}
			if got := vm.Spec.RunStrategy; got == nil || *got != kubevirtv1.RunStrategyHalted {
				t.Errorf("%s: runStrategy = %v, want Halted", tt.resetType, got)
			}
		})
	}
}

// Ironic powers a host off when it gives up waiting for it. A power-off that
// arrives while a power-on is deferred must withdraw the power-on, otherwise
// the VM starts when the import finishes although it was told to stop.
func TestHandlePowerAction_PowerOffWithdrawsDeferredPowerOn(t *testing.T) {
	for _, off := range []string{"ForceOff", "GracefulShutdown"} {
		t.Run(off, func(t *testing.T) {
			server, mockDynamic := poffTestServer(t)

			if w := poffReset(server, "On"); w.Code != http.StatusAccepted {
				t.Fatalf("On: status %d, want 202", w.Code)
			}
			if got := poffVM(t, mockDynamic).GetLabels()[kubevirt.PowerAfterImportLabel]; got != "On" {
				t.Fatalf("label %s = %q after On, want On", kubevirt.PowerAfterImportLabel, got)
			}

			if w := poffReset(server, off); w.Code != http.StatusOK {
				t.Fatalf("%s: status %d, body %s; want 200", off, w.Code, w.Body.String())
			}

			vm := poffVM(t, mockDynamic)
			if got, found := vm.GetLabels()[kubevirt.PowerAfterImportLabel]; found {
				t.Errorf("label %s = %q is still set after %s", kubevirt.PowerAfterImportLabel, got, off)
			}
			if got := vm.GetLabels()[kubevirt.ImportingLabelPrefix+"cdrom0"]; got != "copy-iso-vm-a-bootiso-1" {
				t.Errorf("importing label was changed by the power-off: %q", got)
			}
			if got := vm.Spec.RunStrategy; got == nil || *got != kubevirtv1.RunStrategyHalted {
				t.Errorf("runStrategy = %v, want Halted", got)
			}
		})
	}
}
