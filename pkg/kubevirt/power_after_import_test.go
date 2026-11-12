package kubevirt

import (
	"testing"
	"time"

	"k8s.io/client-go/kubernetes/fake"
)

func TestClearPowerAfterImportLabel(t *testing.T) {
	mockDynamic := NewMockDynamicClient()
	mockDynamic.AddVM(newTestVM("test-ns", "test-vm", map[string]string{
		PowerAfterImportLabel:           "On",
		ImportingLabelPrefix + "cdrom0": "copy-iso-test-vm-1",
	}, nil))
	before, _ := mockDynamic.GetVM("test-ns", "test-vm")
	client := NewClientWithClients(fake.NewSimpleClientset(), mockDynamic, 30*time.Second, nil)

	if err := client.ClearPowerAfterImportLabel("test-ns", "test-vm"); err != nil {
		t.Fatalf("ClearPowerAfterImportLabel failed: %v", err)
	}

	after, _ := mockDynamic.GetVM("test-ns", "test-vm")
	if got, found := after.GetLabels()[PowerAfterImportLabel]; found {
		t.Errorf("label %s = %q, want it removed", PowerAfterImportLabel, got)
	}
	if got := after.GetLabels()[ImportingLabelPrefix+"cdrom0"]; got != "copy-iso-test-vm-1" {
		t.Errorf("importing label was modified: %q", got)
	}
	if after.GetLabels()["existing-label"] != "original-value" {
		t.Errorf("unrelated label was modified: %v", after.GetLabels())
	}
	assertVMUnchangedExceptLabels(t, before, after)
}

func TestClearPowerAfterImportLabel_NoLabelIsNoop(t *testing.T) {
	mockDynamic := NewMockDynamicClient()
	mockDynamic.AddVM(newTestVM("test-ns", "test-vm", nil, nil))
	client := NewClientWithClients(fake.NewSimpleClientset(), mockDynamic, 30*time.Second, nil)
	if err := client.ClearPowerAfterImportLabel("test-ns", "test-vm"); err != nil {
		t.Fatalf("ClearPowerAfterImportLabel failed: %v", err)
	}
}

func TestClearPowerAfterImportLabel_VMNotFound(t *testing.T) {
	client := NewClientWithClients(fake.NewSimpleClientset(), NewMockDynamicClient(), 30*time.Second, nil)
	if err := client.ClearPowerAfterImportLabel("test-ns", "missing"); err == nil {
		t.Fatal("expected an error for a missing VM")
	}
}
