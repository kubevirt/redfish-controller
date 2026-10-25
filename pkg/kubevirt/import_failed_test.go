package kubevirt

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes/fake"
	kubevirtv1 "kubevirt.io/api/core/v1"
)

const (
	failedTestPod    = "copy-iso-test-vm-bootiso-1"
	failedTestLabel  = ImportingLabelPrefix + "cdrom0"
	failedTestMarker = ImportFailedAnnotationPrefix + "cdrom0"
	failedTestURLAn  = VirtualMediaURLAnnotationPrefix + "cdrom0"
)

// patchCounter counts the patches that reach the dynamic client, so a test
// can show that a state change is one write and not a sequence.
type patchCounter struct {
	dynamic.Interface
	patches int32
}

func (p *patchCounter) Resource(gvr schema.GroupVersionResource) dynamic.NamespaceableResourceInterface {
	return &countingNamespaceable{p.Interface.Resource(gvr), p}
}

type countingNamespaceable struct {
	dynamic.NamespaceableResourceInterface
	p *patchCounter
}

func (c *countingNamespaceable) Namespace(ns string) dynamic.ResourceInterface {
	return &countingResource{c.NamespaceableResourceInterface.Namespace(ns), c.p}
}

type countingResource struct {
	dynamic.ResourceInterface
	p *patchCounter
}

func (c *countingResource) Patch(ctx context.Context, name string, pt types.PatchType, data []byte, opts metav1.PatchOptions, subresources ...string) (*unstructured.Unstructured, error) {
	atomic.AddInt32(&c.p.patches, 1)
	return c.ResourceInterface.Patch(ctx, name, pt, data, opts, subresources...)
}

func failedImportPod(phase corev1.PodPhase) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      failedTestPod,
			Namespace: "test-ns",
			Labels: map[string]string{
				ImportPodVMLabel:     "test-vm",
				ImportPodVolumeLabel: "cdrom0",
			},
		},
		Status: corev1.PodStatus{Phase: phase},
	}
}

func withCDROMVolume(vm *kubevirtv1.VirtualMachine, claim string) *kubevirtv1.VirtualMachine {
	vm.Spec.Template.Spec.Domain.Devices.Disks = append(vm.Spec.Template.Spec.Domain.Devices.Disks, kubevirtv1.Disk{
		Name:       "cdrom0",
		DiskDevice: kubevirtv1.DiskDevice{CDRom: &kubevirtv1.CDRomTarget{Bus: kubevirtv1.DiskBusSATA}},
	})
	vm.Spec.Template.Spec.Volumes = append(vm.Spec.Template.Spec.Volumes, kubevirtv1.Volume{
		Name: "cdrom0",
		VolumeSource: kubevirtv1.VolumeSource{
			PersistentVolumeClaim: &kubevirtv1.PersistentVolumeClaimVolumeSource{
				PersistentVolumeClaimVolumeSource: corev1.PersistentVolumeClaimVolumeSource{ClaimName: claim},
			},
		},
	})
	return vm
}

func boundPVC(name string) *corev1.PersistentVolumeClaim {
	return &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "test-ns"},
		Status: corev1.PersistentVolumeClaimStatus{
			Phase:    corev1.ClaimBound,
			Capacity: corev1.ResourceList{"storage": resource.MustParse("1Gi")},
		},
	}
}

// A failed import must not let the deferred power-on start the VM on an
// empty CD-ROM, and the change must be a single write because the VM watcher
// acts on the first event that shows the importing label gone.
func TestHandleImportPodCompleted_FailedImportDropsDeferredPowerOn(t *testing.T) {
	mockDynamic := NewMockDynamicClient()
	halted := kubevirtv1.RunStrategyHalted
	vm := newTestVM("test-ns", "test-vm", map[string]string{
		failedTestLabel:       failedTestPod,
		PowerAfterImportLabel: "On",
	}, map[string]string{failedTestURLAn: "http://example.com/boot.iso"})
	vm.Spec.RunStrategy = &halted
	mockDynamic.AddVM(vm)
	counter := &patchCounter{Interface: mockDynamic}
	fakeK8s := fake.NewSimpleClientset(failedImportPod(corev1.PodFailed))
	client := NewClientWithClients(fakeK8s, counter, 30*time.Second, nil)

	if err := client.reconcileExistingImportPods("test-ns"); err != nil {
		t.Fatalf("reconcileExistingImportPods failed: %v", err)
	}

	updated, _ := mockDynamic.GetVM("test-ns", "test-vm")
	labels, annotations := updated.GetLabels(), updated.GetAnnotations()
	if got, found := labels[failedTestLabel]; found {
		t.Errorf("importing label still set: %q", got)
	}
	if got, found := labels[PowerAfterImportLabel]; found {
		t.Errorf("deferred power command still set: %q", got)
	}
	if got := annotations[failedTestMarker]; got != failedTestPod {
		t.Errorf("annotation %s = %q, want %q", failedTestMarker, got, failedTestPod)
	}
	if got := annotations[failedTestURLAn]; got != "http://example.com/boot.iso" {
		t.Errorf("URL annotation was changed: %q", got)
	}
	if got := updated.Spec.RunStrategy; got == nil || *got != kubevirtv1.RunStrategyHalted {
		t.Errorf("runStrategy = %v, want Halted", got)
	}
	if n := atomic.LoadInt32(&counter.patches); n != 1 {
		t.Errorf("VM was patched %d times, want exactly 1", n)
	}
	if _, err := fakeK8s.CoreV1().Pods("test-ns").Get(context.Background(), failedTestPod, metav1.GetOptions{}); err == nil {
		t.Error("failed import pod should have been deleted")
	}
}

func TestHandleImportPodCompleted_SucceededImportKeepsDeferredPowerOn(t *testing.T) {
	mockDynamic := NewMockDynamicClient()
	mockDynamic.AddVM(newTestVM("test-ns", "test-vm", map[string]string{
		failedTestLabel:       failedTestPod,
		PowerAfterImportLabel: "On",
	}, nil))
	fakeK8s := fake.NewSimpleClientset(failedImportPod(corev1.PodSucceeded))
	client := NewClientWithClients(fakeK8s, mockDynamic, 30*time.Second, nil)

	if err := client.reconcileExistingImportPods("test-ns"); err != nil {
		t.Fatalf("reconcileExistingImportPods failed: %v", err)
	}

	updated, _ := mockDynamic.GetVM("test-ns", "test-vm")
	if _, found := updated.GetLabels()[failedTestLabel]; found {
		t.Error("importing label should have been removed")
	}
	if got := updated.GetLabels()[PowerAfterImportLabel]; got != "On" {
		t.Errorf("deferred power command = %q, want On (the watcher runs it)", got)
	}
	if got, found := updated.GetAnnotations()[failedTestMarker]; found {
		t.Errorf("a successful import must not set %s (got %q)", failedTestMarker, got)
	}
}

func TestHandleImportPodCompleted_StaleFailedPodIsIgnored(t *testing.T) {
	mockDynamic := NewMockDynamicClient()
	mockDynamic.AddVM(newTestVM("test-ns", "test-vm", map[string]string{
		failedTestLabel:       "copy-iso-test-vm-bootiso-2",
		PowerAfterImportLabel: "On",
	}, nil))
	fakeK8s := fake.NewSimpleClientset(failedImportPod(corev1.PodFailed))
	client := NewClientWithClients(fakeK8s, mockDynamic, 30*time.Second, nil)

	if err := client.reconcileExistingImportPods("test-ns"); err != nil {
		t.Fatalf("reconcileExistingImportPods failed: %v", err)
	}

	updated, _ := mockDynamic.GetVM("test-ns", "test-vm")
	if got := updated.GetLabels()[failedTestLabel]; got != "copy-iso-test-vm-bootiso-2" {
		t.Errorf("importing label of the newer import was changed: %q", got)
	}
	if got := updated.GetLabels()[PowerAfterImportLabel]; got != "On" {
		t.Errorf("deferred power command was changed: %q", got)
	}
	if _, found := updated.GetAnnotations()[failedTestMarker]; found {
		t.Error("a stale pod must not mark the device as failed")
	}
}

// Inserted must mean "there is an image to boot": not while the import is
// still writing it, and not after the import failed.
func TestIsVirtualMediaInserted_ImportingOrFailedIsNotInserted(t *testing.T) {
	tests := []struct {
		name        string
		labels      map[string]string
		annotations map[string]string
		want        bool
	}{
		{"import done", nil, nil, true},
		{"import running", map[string]string{failedTestLabel: failedTestPod}, nil, false},
		{"import failed", nil, map[string]string{failedTestMarker: failedTestPod}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mockDynamic := NewMockDynamicClient()
			mockDynamic.AddVM(withCDROMVolume(newTestVM("test-ns", "test-vm", tt.labels, tt.annotations), "test-vm-bootiso"))
			fakeK8s := fake.NewSimpleClientset(boundPVC("test-vm-bootiso"))
			client := NewClientWithClients(fakeK8s, mockDynamic, 30*time.Second, nil)

			inserted, err := client.IsVirtualMediaInserted("test-ns", "test-vm", "cdrom0")
			if err != nil {
				t.Fatalf("IsVirtualMediaInserted failed: %v", err)
			}
			if inserted != tt.want {
				t.Errorf("Inserted = %v, want %v", inserted, tt.want)
			}
		})
	}
}

// After a failure the next InsertMedia, same image or not, must import again.
func TestCheckInsertedMedia_FailedImportIsNotInserted(t *testing.T) {
	for _, url := range []string{"http://example.com/boot.iso", "http://example.com/other.iso"} {
		t.Run(url, func(t *testing.T) {
			mockDynamic := NewMockDynamicClient()
			mockDynamic.AddVM(withCDROMVolume(newTestVM("test-ns", "test-vm", nil, map[string]string{
				failedTestURLAn:  "http://example.com/boot.iso",
				failedTestMarker: failedTestPod,
			}), "test-vm-bootiso"))
			client := NewClientWithClients(fake.NewSimpleClientset(), mockDynamic, 30*time.Second, nil)

			state, err := client.CheckInsertedMedia("test-ns", "test-vm", "cdrom0", url)
			if err != nil {
				t.Fatalf("CheckInsertedMedia failed: %v", err)
			}
			if state != MediaStateNone {
				t.Errorf("state = %d, want MediaStateNone", state)
			}
		})
	}
}

func TestInsertVirtualMedia_ClearsFailedImportMarker(t *testing.T) {
	client, mockDynamic, _ := setupInsertVirtualMediaTest(t, false)
	vm, _ := mockDynamic.GetVM("test-ns", "test-vm")
	annotations := vm.GetAnnotations()
	if annotations == nil {
		annotations = map[string]string{}
	}
	annotations[failedTestMarker] = failedTestPod
	vm.SetAnnotations(annotations)
	if err := mockDynamic.AddVM(vm); err != nil {
		t.Fatalf("AddVM failed: %v", err)
	}

	if err := client.insertVirtualMediaAsync("test-ns", "test-vm", "cdrom0", "http://example.com/image.iso"); err != nil {
		t.Fatalf("insertVirtualMediaAsync failed: %v", err)
	}

	updated, _ := mockDynamic.GetVM("test-ns", "test-vm")
	if got, found := updated.GetAnnotations()[failedTestMarker]; found {
		t.Errorf("annotation %s = %q survived the new insert", failedTestMarker, got)
	}
}

func TestEjectVirtualMedia_ClearsFailedImportMarker(t *testing.T) {
	mockDynamic := NewMockDynamicClient()
	mockDynamic.AddVM(withCDROMVolume(newTestVM("test-ns", "test-vm", nil, map[string]string{
		failedTestURLAn:  "http://example.com/boot.iso",
		failedTestMarker: failedTestPod,
	}), "test-vm-bootiso-123"))
	client := NewClientWithClients(fake.NewSimpleClientset(), mockDynamic, 30*time.Second, nil)

	if err := client.EjectVirtualMedia("test-ns", "test-vm", "cdrom0"); err != nil {
		t.Fatalf("EjectVirtualMedia failed: %v", err)
	}

	updated, _ := mockDynamic.GetVM("test-ns", "test-vm")
	if got, found := updated.GetAnnotations()[failedTestMarker]; found {
		t.Errorf("annotation %s = %q survived the eject", failedTestMarker, got)
	}
}
