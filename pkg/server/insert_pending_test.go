package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kubevirt/redfish-controller/pkg/auth"
	"github.com/kubevirt/redfish-controller/pkg/config"
	"github.com/kubevirt/redfish-controller/pkg/kubevirt"
	"github.com/kubevirt/redfish-controller/pkg/logger"
	"github.com/kubevirt/redfish-controller/pkg/redfish"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	kubevirtv1 "kubevirt.io/api/core/v1"
)

const (
	pendingTestNS    = "ns-a"
	pendingTestVM    = "vm-a"
	pendingTestURL   = "http://example.com/boot.iso"
	pendingTestLabel = kubevirt.ImportingLabelPrefix + "cdrom0"
)

// pendingTestServer builds a server whose task manager has no workers: queued
// jobs never run, which is the state Ironic's Power On meets in production
// when it arrives right after InsertMedia.
func pendingTestServer(t *testing.T) (*Server, *kubevirt.MockDynamicClient) {
	t.Helper()
	mockDynamic := kubevirt.NewMockDynamicClient()
	halted := kubevirtv1.RunStrategyHalted
	if err := mockDynamic.AddVM(&kubevirtv1.VirtualMachine{
		ObjectMeta: metav1.ObjectMeta{
			Name:      pendingTestVM,
			Namespace: pendingTestNS,
			Labels:    map[string]string{"redfish-enabled": "true"},
		},
		Spec: kubevirtv1.VirtualMachineSpec{
			RunStrategy: &halted,
			Template: &kubevirtv1.VirtualMachineInstanceTemplateSpec{
				Spec: kubevirtv1.VirtualMachineInstanceSpec{
					Domain: kubevirtv1.DomainSpec{
						Devices: kubevirtv1.Devices{
							Disks: []kubevirtv1.Disk{{Name: "rootdisk"}},
						},
					},
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
				Namespace:  pendingTestNS,
				VMSelector: &kubevirt.VMSelectorConfig{Labels: map[string]string{"redfish-enabled": "true"}},
			},
		},
	}
	server := NewServer(cfg, client)
	server.taskManager = NewTaskManager(0, client)
	t.Cleanup(server.taskManager.Stop)
	return server, mockDynamic
}

func pendingTestRequest(method, path, body string) *http.Request {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	authCtx := &auth.AuthContext{User: &auth.User{Username: "user", Chassis: []string{"chassis-a"}}}
	return req.WithContext(logger.WithAuth(req.Context(), authCtx))
}

func insertMedia(server *Server, url string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	req := pendingTestRequest("POST", "/redfish/v1/Systems/"+pendingTestVM+"/VirtualMedia/Cd/Actions/VirtualMedia.InsertMedia",
		`{"Image":"`+url+`","Inserted":true,"WriteProtected":true}`)
	server.handleInsertVirtualMedia(w, req, pendingTestVM, "Cd")
	return w
}

func powerAction(server *Server, resetType string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	req := pendingTestRequest("POST", "/redfish/v1/Systems/"+pendingTestVM+"/Actions/ComputerSystem.Reset",
		`{"ResetType":"`+resetType+`"}`)
	server.handlePowerAction(w, req, pendingTestVM)
	return w
}

func taskCount(server *Server) int {
	server.taskManager.taskMutex.RLock()
	defer server.taskManager.taskMutex.RUnlock()
	return len(server.taskManager.tasks)
}

// The insert job is what creates the import pod and labels the VM, and it runs
// some time after the 202 is sent. Power On arriving in between must already
// find the VM marked, otherwise it starts the VM on an empty CD-ROM.
func TestHandleInsertVirtualMedia_MarksVMBeforeAnswering(t *testing.T) {
	server, mockDynamic := pendingTestServer(t)

	w := insertMedia(server, pendingTestURL)
	if w.Code != http.StatusAccepted {
		t.Fatalf("InsertMedia status = %d, body %s", w.Code, w.Body.String())
	}
	if n := taskCount(server); n != 1 {
		t.Fatalf("tasks = %d, want 1", n)
	}

	vm, _ := mockDynamic.GetVM(pendingTestNS, pendingTestVM)
	if got := vm.GetLabels()[pendingTestLabel]; !strings.HasPrefix(got, kubevirt.ImportPendingPrefix) {
		t.Fatalf("label %s = %q after InsertMedia, want a %q placeholder", pendingTestLabel, got, kubevirt.ImportPendingPrefix)
	}
	if got := vm.GetAnnotations()[kubevirt.VirtualMediaURLAnnotationPrefix+"cdrom0"]; got != pendingTestURL {
		t.Errorf("URL annotation = %q, want %q", got, pendingTestURL)
	}

	w = powerAction(server, "On")
	if w.Code != http.StatusAccepted {
		t.Fatalf("Power On status = %d, want 202 (deferred); body %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "deferred") {
		t.Errorf("Power On body does not say deferred: %s", w.Body.String())
	}
	vm, _ = mockDynamic.GetVM(pendingTestNS, pendingTestVM)
	if got := vm.GetLabels()[kubevirt.PowerAfterImportLabel]; got != "On" {
		t.Errorf("label %s = %q, want On", kubevirt.PowerAfterImportLabel, got)
	}
	if got := vm.Spec.RunStrategy; got == nil || *got != kubevirtv1.RunStrategyHalted {
		t.Errorf("VM was started while its import was pending: runStrategy = %v", got)
	}
}

// Ironic retries a lost InsertMedia. While the placeholder is on the VM the
// retry is answered as "in progress" and no second job is queued; a different
// image is a conflict.
func TestHandleInsertVirtualMedia_RepeatWhilePending(t *testing.T) {
	server, _ := pendingTestServer(t)
	if w := insertMedia(server, pendingTestURL); w.Code != http.StatusAccepted {
		t.Fatalf("first InsertMedia status = %d", w.Code)
	}

	w := insertMedia(server, pendingTestURL)
	if w.Code != http.StatusAccepted {
		t.Errorf("repeated InsertMedia status = %d, want 202", w.Code)
	}
	var body map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || !strings.Contains(body["Message"], "in progress") {
		t.Errorf("repeated InsertMedia body = %s, want an in-progress message", w.Body.String())
	}
	if n := taskCount(server); n != 1 {
		t.Errorf("tasks = %d after the repeat, want 1", n)
	}

	w = insertMedia(server, "http://example.com/other.iso")
	if w.Code != http.StatusConflict {
		t.Errorf("InsertMedia with another image status = %d, want 409", w.Code)
	}
	if n := taskCount(server); n != 1 {
		t.Errorf("tasks = %d after the conflict, want 1", n)
	}
}

// When the job gives up, the placeholder must go, or every later power action
// on that VM would be deferred forever.
func TestWorker_FailedInsertJobClearsPlaceholder(t *testing.T) {
	server, mockDynamic := pendingTestServer(t)
	tm := server.taskManager
	if _, err := server.kubevirtClient.MarkImportPending(pendingTestNS, pendingTestVM, "cdrom0", "://not-a-url"); err != nil {
		t.Fatalf("MarkImportPending failed: %v", err)
	}

	// The task is created the way the handler does it; its job is then run
	// directly, with no retries, on a worker that was never started. The URL
	// cannot be parsed, so the job fails before it changes the VM or creates
	// a pod.
	taskID := tm.CreateTask("insert", pendingTestNS, pendingTestVM, "cdrom0", "://not-a-url")
	job := &Job{
		ID:     "job-1",
		TaskID: taskID,
		Type:   TaskTypeVirtualMediaInsert,
		Payload: map[string]string{
			"namespace": pendingTestNS,
			"vmName":    pendingTestVM,
			"mediaID":   "cdrom0",
			"imageURL":  "://not-a-url",
		},
	}
	NewWorker(1, tm).processJob(job)

	task, ok := tm.GetTask(taskID)
	if !ok || task.TaskState != redfish.TaskStateException {
		t.Fatalf("task state = %v, want Exception", task)
	}
	vm, _ := mockDynamic.GetVM(pendingTestNS, pendingTestVM)
	if got, ok := vm.GetLabels()[pendingTestLabel]; ok {
		t.Errorf("label %s = %q is still on the VM after the job failed", pendingTestLabel, got)
	}
}
