package kubevirt

import (
	"strings"
	"testing"
	"time"

	"k8s.io/client-go/kubernetes/fake"
)

const (
	pendingTestURL   = "http://example.com/boot.iso"
	pendingTestLabel = ImportingLabelPrefix + "cdrom0"
	pendingTestURLAn = VirtualMediaURLAnnotationPrefix + "cdrom0"
)

func TestMarkImportPending_SetsPlaceholderLabelAndURL(t *testing.T) {
	mockDynamic := NewMockDynamicClient()
	mockDynamic.AddVM(newTestVM("test-ns", "test-vm", nil, nil))
	client := NewClientWithClients(fake.NewSimpleClientset(), mockDynamic, 30*time.Second, nil)

	marked, err := client.MarkImportPending("test-ns", "test-vm", "cdrom0", pendingTestURL)
	if err != nil {
		t.Fatalf("MarkImportPending failed: %v", err)
	}
	if !marked {
		t.Fatal("expected the VM to be marked")
	}

	vm, _ := mockDynamic.GetVM("test-ns", "test-vm")
	if got := vm.GetLabels()[pendingTestLabel]; !isPendingImport(got) {
		t.Errorf("label %s = %q, want a %q placeholder", pendingTestLabel, got, ImportPendingPrefix)
	}
	if got := vm.GetAnnotations()[pendingTestURLAn]; got != pendingTestURL {
		t.Errorf("annotation %s = %q, want %q", pendingTestURLAn, got, pendingTestURL)
	}
	if vm.GetLabels()["existing-label"] != "original-value" || vm.GetAnnotations()["existing-annotation"] != "original-value" {
		t.Errorf("unrelated metadata was modified: labels=%v annotations=%v", vm.GetLabels(), vm.GetAnnotations())
	}
}

func TestMarkImportPending_DoesNotOverwriteRunningImport(t *testing.T) {
	for _, existing := range []string{"copy-iso-test-vm-bootiso-1", importPendingValue(time.Now())} {
		t.Run(existing, func(t *testing.T) {
			mockDynamic := NewMockDynamicClient()
			mockDynamic.AddVM(newTestVM("test-ns", "test-vm",
				map[string]string{pendingTestLabel: existing},
				map[string]string{pendingTestURLAn: pendingTestURL}))
			client := NewClientWithClients(fake.NewSimpleClientset(), mockDynamic, 30*time.Second, nil)

			marked, err := client.MarkImportPending("test-ns", "test-vm", "cdrom0", "http://example.com/other.iso")
			if err != nil {
				t.Fatalf("MarkImportPending failed: %v", err)
			}
			if marked {
				t.Error("expected the existing import to be left alone")
			}
			vm, _ := mockDynamic.GetVM("test-ns", "test-vm")
			if got := vm.GetLabels()[pendingTestLabel]; got != existing {
				t.Errorf("label changed from %q to %q", existing, got)
			}
			if got := vm.GetAnnotations()[pendingTestURLAn]; got != pendingTestURL {
				t.Errorf("annotation changed to %q", got)
			}
		})
	}
}

func TestMarkImportPending_VMNotFound(t *testing.T) {
	client := NewClientWithClients(fake.NewSimpleClientset(), NewMockDynamicClient(), 30*time.Second, nil)
	if _, err := client.MarkImportPending("test-ns", "missing", "cdrom0", pendingTestURL); err == nil {
		t.Fatal("expected an error for a missing VM")
	}
}

func TestClearPendingImport_RemovesOnlyThePlaceholder(t *testing.T) {
	tests := []struct {
		name      string
		value     string
		wantLabel string
	}{
		{"placeholder is removed", importPendingValue(time.Now()), ""},
		{"helper pod name is kept", "copy-iso-test-vm-bootiso-1", "copy-iso-test-vm-bootiso-1"},
		{"CDI import is kept", CDIImportPrefix + "test-vm-bootiso-1", CDIImportPrefix + "test-vm-bootiso-1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mockDynamic := NewMockDynamicClient()
			mockDynamic.AddVM(newTestVM("test-ns", "test-vm", map[string]string{pendingTestLabel: tt.value}, nil))
			before, _ := mockDynamic.GetVM("test-ns", "test-vm")
			client := NewClientWithClients(fake.NewSimpleClientset(), mockDynamic, 30*time.Second, nil)

			if err := client.ClearPendingImport("test-ns", "test-vm", "cdrom0"); err != nil {
				t.Fatalf("ClearPendingImport failed: %v", err)
			}

			after, _ := mockDynamic.GetVM("test-ns", "test-vm")
			if got := after.GetLabels()[pendingTestLabel]; got != tt.wantLabel {
				t.Errorf("label = %q, want %q", got, tt.wantLabel)
			}
			if after.GetLabels()["existing-label"] != "original-value" {
				t.Errorf("unrelated label was modified: %v", after.GetLabels())
			}
			assertVMUnchangedExceptLabels(t, before, after)
		})
	}
}

func TestClearPendingImport_NoLabelIsNoop(t *testing.T) {
	mockDynamic := NewMockDynamicClient()
	mockDynamic.AddVM(newTestVM("test-ns", "test-vm", nil, nil))
	client := NewClientWithClients(fake.NewSimpleClientset(), mockDynamic, 30*time.Second, nil)
	if err := client.ClearPendingImport("test-ns", "test-vm", "cdrom0"); err != nil {
		t.Fatalf("ClearPendingImport failed: %v", err)
	}
}

func TestPendingImportValue(t *testing.T) {
	now := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	value := importPendingValue(now)
	if !strings.HasPrefix(value, ImportPendingPrefix) {
		t.Fatalf("value %q lacks the %q prefix", value, ImportPendingPrefix)
	}
	if !isPendingImport(value) || isPendingImport("copy-iso-vm-1") || isPendingImport("") {
		t.Error("isPendingImport misclassifies values")
	}

	if pendingImportIsStale(value, now.Add(stalePendingImportAge-time.Second)) {
		t.Error("a placeholder younger than the limit must not be stale")
	}
	if !pendingImportIsStale(value, now.Add(stalePendingImportAge+time.Second)) {
		t.Error("a placeholder older than the limit must be stale")
	}
	if !pendingImportIsStale(ImportPendingPrefix+"garbage", now) {
		t.Error("an unparsable placeholder must be treated as stale")
	}
	if pendingImportIsStale("copy-iso-vm-1", now.Add(24*time.Hour)) {
		t.Error("a helper pod name is never a stale placeholder")
	}
}

// A placeholder whose job was lost (the queue lives in memory, so a restart
// drops it) would defer power actions for that VM forever. The periodic sweep
// removes placeholders older than stalePendingImportAge and nothing else.
func TestReconcileStalePendingImports(t *testing.T) {
	now := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	mockDynamic := NewMockDynamicClient()
	fresh := importPendingValue(now.Add(-time.Minute))
	stale := importPendingValue(now.Add(-stalePendingImportAge - time.Minute))
	mockDynamic.AddVM(newTestVM("test-ns", "fresh", map[string]string{pendingTestLabel: fresh}, nil))
	mockDynamic.AddVM(newTestVM("test-ns", "stale", map[string]string{pendingTestLabel: stale}, nil))
	mockDynamic.AddVM(newTestVM("test-ns", "importing", map[string]string{pendingTestLabel: "copy-iso-importing-1"}, nil))
	mockDynamic.AddVM(newTestVM("test-ns", "two-devices", map[string]string{
		pendingTestLabel:                stale,
		ImportingLabelPrefix + "cdrom1": "copy-iso-two-devices-1",
	}, nil))
	mockDynamic.AddVM(newTestVM("other-ns", "stale", map[string]string{pendingTestLabel: stale}, nil))
	client := NewClientWithClients(fake.NewSimpleClientset(), mockDynamic, 30*time.Second, nil)

	if err := client.reconcileStalePendingImports("test-ns", now); err != nil {
		t.Fatalf("reconcileStalePendingImports failed: %v", err)
	}

	want := map[string]map[string]string{
		"test-ns/fresh":       {pendingTestLabel: fresh},
		"test-ns/stale":       {pendingTestLabel: ""},
		"test-ns/importing":   {pendingTestLabel: "copy-iso-importing-1"},
		"test-ns/two-devices": {pendingTestLabel: "", ImportingLabelPrefix + "cdrom1": "copy-iso-two-devices-1"},
		"other-ns/stale":      {pendingTestLabel: stale},
	}
	for key, labels := range want {
		ns, name, _ := strings.Cut(key, "/")
		vm, err := mockDynamic.GetVM(ns, name)
		if err != nil {
			t.Fatalf("VM %s: %v", key, err)
		}
		for k, v := range labels {
			if got := vm.GetLabels()[k]; got != v {
				t.Errorf("VM %s: label %s = %q, want %q", key, k, got, v)
			}
		}
	}
}

// Ironic retries InsertMedia if the first answer is lost. While the placeholder
// is on the VM there is no PVC yet, and the retry must not queue a second job.
func TestCheckInsertedMedia_PendingWithoutPVC(t *testing.T) {
	tests := []struct {
		name string
		url  string
		want MediaState
	}{
		{"same URL is importing", pendingTestURL, MediaStateImporting},
		{"different URL conflicts", "http://example.com/other.iso", MediaStateConflict},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mockDynamic := NewMockDynamicClient()
			mockDynamic.AddVM(newTestVM("test-ns", "test-vm",
				map[string]string{pendingTestLabel: importPendingValue(time.Now())},
				map[string]string{pendingTestURLAn: pendingTestURL}))
			client := NewClientWithClients(fake.NewSimpleClientset(), mockDynamic, 30*time.Second, nil)

			state, err := client.CheckInsertedMedia("test-ns", "test-vm", "cdrom0", tt.url)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if state != tt.want {
				t.Errorf("state = %d, want %d", state, tt.want)
			}
		})
	}
}

// A cleared placeholder leaves the URL annotation behind; with no PVC and no
// label that must read as "nothing inserted" so a new insert is accepted.
func TestCheckInsertedMedia_StaleURLAnnotationAlone(t *testing.T) {
	mockDynamic := NewMockDynamicClient()
	mockDynamic.AddVM(newTestVM("test-ns", "test-vm", nil, map[string]string{pendingTestURLAn: pendingTestURL}))
	client := NewClientWithClients(fake.NewSimpleClientset(), mockDynamic, 30*time.Second, nil)

	state, err := client.CheckInsertedMedia("test-ns", "test-vm", "cdrom0", "http://example.com/other.iso")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if state != MediaStateNone {
		t.Errorf("state = %d, want MediaStateNone", state)
	}
}

// The insert job replaces the placeholder with the helper pod name, so the
// pod watcher's "is this pod still the tracked import" check keeps working.
func TestSetImportingLabel_ReplacesPlaceholder(t *testing.T) {
	mockDynamic := NewMockDynamicClient()
	mockDynamic.AddVM(newTestVM("test-ns", "test-vm", map[string]string{pendingTestLabel: importPendingValue(time.Now())}, nil))
	client := NewClientWithClients(fake.NewSimpleClientset(), mockDynamic, 30*time.Second, nil)

	if err := client.setImportingLabel("test-ns", "test-vm", "cdrom0", "copy-iso-test-vm-bootiso-1"); err != nil {
		t.Fatalf("setImportingLabel failed: %v", err)
	}
	vm, _ := mockDynamic.GetVM("test-ns", "test-vm")
	if got := vm.GetLabels()[pendingTestLabel]; got != "copy-iso-test-vm-bootiso-1" {
		t.Errorf("label = %q, want the pod name", got)
	}
}
