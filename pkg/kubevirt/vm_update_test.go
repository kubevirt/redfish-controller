package kubevirt

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	kubevirtv1 "kubevirt.io/api/core/v1"
)

const (
	testISOURL   = "https://example.com/boot.iso"
	cdromDevice  = "cdrom0"
	rootDiskName = "rootdisk"
)

// newRootDiskVM mirrors the VMs Ironic drives: one virtio root disk that is
// the only boot device, and no CD-ROM yet.
func newRootDiskVM(namespace, name string) *kubevirtv1.VirtualMachine {
	rootBootOrder := uint(1)
	return &kubevirtv1.VirtualMachine{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   namespace,
			Labels:      map[string]string{},
			Annotations: map[string]string{},
		},
		Spec: kubevirtv1.VirtualMachineSpec{
			Template: &kubevirtv1.VirtualMachineInstanceTemplateSpec{
				Spec: kubevirtv1.VirtualMachineInstanceSpec{
					Domain: kubevirtv1.DomainSpec{
						Devices: kubevirtv1.Devices{
							Disks: []kubevirtv1.Disk{{
								Name:       rootDiskName,
								BootOrder:  &rootBootOrder,
								DiskDevice: kubevirtv1.DiskDevice{Disk: &kubevirtv1.DiskTarget{Bus: kubevirtv1.DiskBusVirtio}},
							}},
						},
					},
					Volumes: []kubevirtv1.Volume{{
						Name:         rootDiskName,
						VolumeSource: kubevirtv1.VolumeSource{DataVolume: &kubevirtv1.DataVolumeSource{Name: name + "-root"}},
					}},
				},
			},
		},
	}
}

// withRootBootOrder sets the root disk's boot order. A previous enroll cycle
// leaves it at 2 (the CD-ROM was 1) after the eject removed the CD-ROM, which
// is the state the corrupted production VMs were in: the next boot-once then
// changes the disks array (2 -> 1), so its patch carries the whole array.
func withRootBootOrder(vm *kubevirtv1.VirtualMachine, order uint) *kubevirtv1.VirtualMachine {
	diskByName(vm, rootDiskName).BootOrder = &order
	return vm
}

// withBootOnceToCD puts the VM in the state SetBootOnce leaves behind when
// it ran before any CD-ROM existed: boot-once label, saved original boot
// order with only the root disk, override annotations, root disk still first.
func withBootOnceToCD(vm *kubevirtv1.VirtualMachine) *kubevirtv1.VirtualMachine {
	vm.Labels[BootOnceLabel] = "enabled"
	vm.Annotations[BootOnceOriginalConfigAnnotation] = `[{"diskName":"rootdisk","bootOrder":1}]`
	vm.Annotations[BootOnceVMIUIDAnnotation] = ""
	vm.Annotations["redfish.boot.source.override.enabled"] = "Once"
	vm.Annotations["redfish.boot.source.override.target"] = "Cd"
	vm.Annotations["redfish.boot.source.override.mode"] = "UEFI"
	return vm
}

func withCDROM(vm *kubevirtv1.VirtualMachine, claimName string) *kubevirtv1.VirtualMachine {
	if vm.Annotations == nil {
		vm.Annotations = map[string]string{}
	}
	addCDROMDisk(vm, cdromDevice)
	vm.Spec.Template.Spec.Volumes = append(vm.Spec.Template.Spec.Volumes, kubevirtv1.Volume{
		Name: cdromDevice,
		VolumeSource: kubevirtv1.VolumeSource{PersistentVolumeClaim: &kubevirtv1.PersistentVolumeClaimVolumeSource{
			PersistentVolumeClaimVolumeSource: corev1.PersistentVolumeClaimVolumeSource{ClaimName: claimName},
		}},
	})
	vm.Annotations[VirtualMediaURLAnnotationPrefix+cdromDevice] = testISOURL
	return vm
}

func newVMWriterTestClient(t *testing.T, vms ...*kubevirtv1.VirtualMachine) (*Client, *MockDynamicClient, *fake.Clientset) {
	t.Helper()
	mock := NewMockDynamicClient()
	for _, vm := range vms {
		if err := mock.AddVM(vm); err != nil {
			t.Fatalf("failed to add VM %s: %v", vm.Name, err)
		}
	}
	cfg := &MockConfig{}
	cfg.dataVolumeConfig.storageSize = "3Gi"
	cfg.dataVolumeConfig.vmUpdateTimeout = "30s"
	cfg.dataVolumeConfig.isoDownloadTimeout = "30m"
	cfg.dataVolumeConfig.helperImage = "alpine:latest"

	k8s := fake.NewSimpleClientset()
	client := NewClientWithClients(k8s, mock, 30*time.Second, cfg)
	client.sleep = func(time.Duration) {}
	return client, mock, k8s
}

func diskByName(vm *kubevirtv1.VirtualMachine, name string) *kubevirtv1.Disk {
	for i := range vm.Spec.Template.Spec.Domain.Devices.Disks {
		if vm.Spec.Template.Spec.Domain.Devices.Disks[i].Name == name {
			return &vm.Spec.Template.Spec.Domain.Devices.Disks[i]
		}
	}
	return nil
}

func volumeByName(vm *kubevirtv1.VirtualMachine, name string) *kubevirtv1.Volume {
	for i := range vm.Spec.Template.Spec.Volumes {
		if vm.Spec.Template.Spec.Volumes[i].Name == name {
			return &vm.Spec.Template.Spec.Volumes[i]
		}
	}
	return nil
}

func bootOrderOf(disk *kubevirtv1.Disk) string {
	if disk == nil {
		return "<no disk>"
	}
	if disk.BootOrder == nil {
		return "none"
	}
	return fmt.Sprint(*disk.BootOrder)
}

func assertBootOrder(t *testing.T, vm *kubevirtv1.VirtualMachine, diskName, want string) {
	t.Helper()
	if got := bootOrderOf(diskByName(vm, diskName)); got != want {
		t.Errorf("disk %s: boot order %s, want %s", diskName, got, want)
	}
}

// assertVolumeDiskPairs checks the invariant the bug violated: an inserted
// virtual media device has both its volume and its disk entry, or neither.
func assertVolumeDiskPairs(t *testing.T, vm *kubevirtv1.VirtualMachine) {
	t.Helper()
	for _, device := range virtualMediaDevices(vm) {
		if hasVolume(vm, device) != hasDisk(vm, device) {
			t.Errorf("VM %s: device %s has volume=%v disk=%v, must have both or neither", vm.Name, device, hasVolume(vm, device), hasDisk(vm, device))
		}
	}
	for _, volume := range vm.Spec.Template.Spec.Volumes {
		if strings.HasPrefix(volume.Name, "cdrom") && !hasDisk(vm, volume.Name) {
			t.Errorf("VM %s: volume %s has no disk entry; KubeVirt would attach it as a writable hard disk", vm.Name, volume.Name)
		}
	}
}

// landInsertDirectly simulates the insert worker's patch reaching the API
// server while another writer sits between its read and its write.
func landInsertDirectly(t *testing.T, mock *MockDynamicClient, namespace, name string) {
	t.Helper()
	if err := mock.MutateVM(namespace, name, func(vm *kubevirtv1.VirtualMachine) { withCDROM(vm, name+"-bootiso-1") }); err != nil {
		t.Fatalf("failed to inject the insert: %v", err)
	}
}

func TestComputeVMPatch_CarriesResourceVersion(t *testing.T) {
	vm := newRootDiskVM("ns", "vm")
	vm.ResourceVersion = "42"
	modified := vm.DeepCopy()
	modified.Annotations["a"] = "b"

	patch, err := computeVMPatch(vm, modified)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]interface{}
	if err := json.Unmarshal(patch, &fields); err != nil {
		t.Fatal(err)
	}
	metadata, _ := fields["metadata"].(map[string]interface{})
	if metadata["resourceVersion"] != "42" {
		t.Errorf("patch %s does not carry resourceVersion 42", patch)
	}
	if annotations, _ := metadata["annotations"].(map[string]interface{}); annotations["a"] != "b" {
		t.Errorf("patch %s lost the annotation change", patch)
	}
}

func TestComputeVMPatch_NoChangeStaysEmpty(t *testing.T) {
	vm := newRootDiskVM("ns", "vm")
	vm.ResourceVersion = "42"

	patch, err := computeVMPatch(vm, vm.DeepCopy())
	if err != nil {
		t.Fatal(err)
	}
	if !isEmptyMergePatch(patch) {
		t.Errorf("expected an empty patch for an unchanged VM, got %s", patch)
	}
}

func TestMockPatch_StaleResourceVersionIsRejected(t *testing.T) {
	_, mock, _ := newVMWriterTestClient(t, newRootDiskVM("ns", "vm"))
	gvr := schema.GroupVersionResource{Group: "kubevirt.io", Version: "v1", Resource: "virtualmachines"}
	ctx := context.Background()

	stale := []byte(`{"metadata":{"resourceVersion":"999","annotations":{"x":"y"}}}`)
	if _, err := mock.Resource(gvr).Namespace("ns").Patch(ctx, "vm", types.MergePatchType, stale, metav1.PatchOptions{}); !apierrors.IsConflict(err) {
		t.Fatalf("expected a Conflict for a stale resourceVersion, got %v", err)
	}

	current, err := mock.GetVM("ns", "vm")
	if err != nil {
		t.Fatal(err)
	}
	fresh := []byte(fmt.Sprintf(`{"metadata":{"resourceVersion":%q,"annotations":{"x":"y"}}}`, current.ResourceVersion))
	if _, err := mock.Resource(gvr).Namespace("ns").Patch(ctx, "vm", types.MergePatchType, fresh, metav1.PatchOptions{}); err != nil {
		t.Fatalf("patch with the current resourceVersion failed: %v", err)
	}

	after, _ := mock.GetVM("ns", "vm")
	if after.ResourceVersion == current.ResourceVersion {
		t.Errorf("resourceVersion was not bumped by a successful write")
	}
	if after.Annotations["x"] != "y" {
		t.Errorf("patch was not applied")
	}

	// A patch without a resourceVersion is applied blindly, like the API server does.
	blind := []byte(`{"metadata":{"labels":{"l":"v"}}}`)
	if _, err := mock.Resource(gvr).Namespace("ns").Patch(ctx, "vm", types.MergePatchType, blind, metav1.PatchOptions{}); err != nil {
		t.Fatalf("patch without resourceVersion failed: %v", err)
	}
}

// TestSetBootOnce_StaleRead_WithoutResourceVersionDropsCDROMDisk reproduces
// the production corruption: SetBootOnce reads the VM, the insert lands, then
// the boot-once patch carrying the stale disks array lands on top. Without
// the resourceVersion precondition (the behaviour before this fix) the CD-ROM
// disk entry is deleted while its volume survives.
func TestSetBootOnce_StaleRead_WithoutResourceVersionDropsCDROMDisk(t *testing.T) {
	client, mock, _ := newVMWriterTestClient(t, withRootBootOrder(newRootDiskVM("ns", "vm"), 2))
	mock.IgnoreResourceVersion = true

	var injected bool
	mock.BeforePatch = func(_ schema.GroupVersionResource, namespace, name string, _ types.PatchType, _ []byte) {
		if !injected {
			injected = true
			landInsertDirectly(t, mock, namespace, name)
		}
	}

	if err := client.SetBootOnce("ns", "vm", "Cd"); err != nil {
		t.Fatalf("SetBootOnce failed: %v", err)
	}

	got, _ := mock.GetVM("ns", "vm")
	if diskByName(got, cdromDevice) != nil {
		t.Fatalf("expected the stale boot-once patch to delete the %s disk entry (the bug), but it is still there", cdromDevice)
	}
	if volumeByName(got, cdromDevice) == nil {
		t.Fatalf("expected the %s volume to survive the stale patch", cdromDevice)
	}
}

// TestSetBootOnce_StaleRead_RetriesOnFreshCopy is the same interleaving with
// the fix: the stale patch is rejected, SetBootOnce re-reads, sees the
// CD-ROM and orders it first.
func TestSetBootOnce_StaleRead_RetriesOnFreshCopy(t *testing.T) {
	client, mock, _ := newVMWriterTestClient(t, withRootBootOrder(newRootDiskVM("ns", "vm"), 2))

	var patches int
	mock.BeforePatch = func(_ schema.GroupVersionResource, namespace, name string, _ types.PatchType, _ []byte) {
		patches++
		if patches == 1 {
			landInsertDirectly(t, mock, namespace, name)
		}
	}

	if err := client.SetBootOnce("ns", "vm", "Cd"); err != nil {
		t.Fatalf("SetBootOnce failed: %v", err)
	}

	got, _ := mock.GetVM("ns", "vm")
	assertVolumeDiskPairs(t, got)
	assertBootOrder(t, got, cdromDevice, "1")
	assertBootOrder(t, got, rootDiskName, "2")
	if patches != 2 {
		t.Errorf("expected the rejected patch and one retry, got %d patch attempts", patches)
	}
	if cfg := got.Annotations[BootOnceOriginalConfigAnnotation]; !strings.Contains(cfg, `"diskName":"cdrom0"`) {
		t.Errorf("original boot order captured on the retry should include the CD-ROM, got %s", cfg)
	}
}

func TestUpdateVM_GivesUpAfterRepeatedConflicts(t *testing.T) {
	client, mock, _ := newVMWriterTestClient(t, newRootDiskVM("ns", "vm"))

	var patches int
	mock.BeforePatch = func(_ schema.GroupVersionResource, namespace, name string, _ types.PatchType, _ []byte) {
		patches++
		_ = mock.MutateVM(namespace, name, func(vm *kubevirtv1.VirtualMachine) { vm.Annotations["touched"] = fmt.Sprint(patches) })
	}

	_, err := client.updateVM(context.Background(), "ns", "vm", func(vm *kubevirtv1.VirtualMachine) error {
		vm.SetAnnotations(map[string]string{"mine": "x"})
		return nil
	})
	if err == nil || !apierrors.IsConflict(err) {
		t.Fatalf("expected a Conflict after exhausting retries, got %v", err)
	}
	if patches != vmUpdateMaxAttempts {
		t.Errorf("expected %d attempts, got %d", vmUpdateMaxAttempts, patches)
	}
}

func TestUpdateVM_NoWriteWhenNothingChanged(t *testing.T) {
	client, mock, _ := newVMWriterTestClient(t, newRootDiskVM("ns", "vm"))

	var patches int
	mock.BeforePatch = func(schema.GroupVersionResource, string, string, types.PatchType, []byte) { patches++ }

	if _, err := client.updateVM(context.Background(), "ns", "vm", func(*kubevirtv1.VirtualMachine) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if patches != 0 {
		t.Errorf("a mutation that changes nothing must not write, got %d patches", patches)
	}
}

func TestInsertVirtualMedia_ConflictRetryKeepsPVCNameAndOtherWriters(t *testing.T) {
	client, mock, k8s := newVMWriterTestClient(t, newRootDiskVM("ns", "vm"))

	var injected bool
	mock.BeforePatch = func(_ schema.GroupVersionResource, namespace, name string, pt types.PatchType, data []byte) {
		if !injected && strings.Contains(string(data), "cdrom0") {
			injected = true
			_ = mock.MutateVM(namespace, name, func(vm *kubevirtv1.VirtualMachine) { vm.Annotations["someone-else"] = "was-here" })
		}
	}

	if err := client.insertVirtualMediaAsync("ns", "vm", cdromDevice, testISOURL); err != nil {
		t.Fatalf("insert failed: %v", err)
	}

	got, _ := mock.GetVM("ns", "vm")
	assertVolumeDiskPairs(t, got)
	if got.Annotations["someone-else"] != "was-here" {
		t.Errorf("the concurrent writer's change was clobbered")
	}
	assertBootOrder(t, got, rootDiskName, "1")
	assertBootOrder(t, got, cdromDevice, "none")

	claim := volumeByName(got, cdromDevice).PersistentVolumeClaim.ClaimName
	pvcs, err := k8s.CoreV1().PersistentVolumeClaims("ns").List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(pvcs.Items) != 1 || pvcs.Items[0].Name != claim {
		var names []string
		for _, pvc := range pvcs.Items {
			names = append(names, pvc.Name)
		}
		t.Errorf("the retried insert must keep one PVC named like the volume (%s), got PVCs %v", claim, names)
	}
}

func TestInsertVirtualMedia_AfterBootOnceToCD_PutsCDFirst(t *testing.T) {
	client, mock, _ := newVMWriterTestClient(t, withBootOnceToCD(newRootDiskVM("ns", "vm")))

	if err := client.insertVirtualMediaAsync("ns", "vm", cdromDevice, testISOURL); err != nil {
		t.Fatalf("insert failed: %v", err)
	}

	got, _ := mock.GetVM("ns", "vm")
	assertVolumeDiskPairs(t, got)
	assertBootOrder(t, got, cdromDevice, "1")
	assertBootOrder(t, got, rootDiskName, "2")

	// After the one-time boot the watcher restores the saved order; the CD-ROM
	// must then end up with no boot order, not a duplicate of the root disk's.
	restored := got.DeepCopy()
	if err := client.restoreBootOrder(restored, got.Annotations[BootOnceOriginalConfigAnnotation]); err != nil {
		t.Fatal(err)
	}
	assertBootOrder(t, restored, rootDiskName, "1")
	assertBootOrder(t, restored, cdromDevice, "none")
}

func TestInsertVirtualMedia_WithoutBootOverride_LeavesBootOrderAlone(t *testing.T) {
	client, mock, _ := newVMWriterTestClient(t, newRootDiskVM("ns", "vm"))

	if err := client.insertVirtualMediaAsync("ns", "vm", cdromDevice, testISOURL); err != nil {
		t.Fatalf("insert failed: %v", err)
	}

	got, _ := mock.GetVM("ns", "vm")
	assertVolumeDiskPairs(t, got)
	assertBootOrder(t, got, rootDiskName, "1")
	assertBootOrder(t, got, cdromDevice, "none")
}

// The boot PATCH handler records the override annotations first and the
// boot-once state a moment later. An insert that lands in between must not
// reorder, or SetBootOnce would save the reordered disks as the original.
func TestInsertVirtualMedia_BetweenAnnotationsAndBootOnce_DoesNotReorder(t *testing.T) {
	vm := newRootDiskVM("ns", "vm")
	vm.Annotations["redfish.boot.source.override.enabled"] = "Once"
	vm.Annotations["redfish.boot.source.override.target"] = "Cd"
	client, mock, _ := newVMWriterTestClient(t, vm)

	if err := client.insertVirtualMediaAsync("ns", "vm", cdromDevice, testISOURL); err != nil {
		t.Fatalf("insert failed: %v", err)
	}
	got, _ := mock.GetVM("ns", "vm")
	assertBootOrder(t, got, rootDiskName, "1")
	assertBootOrder(t, got, cdromDevice, "none")

	if err := client.SetBootOnce("ns", "vm", "Cd"); err != nil {
		t.Fatalf("SetBootOnce failed: %v", err)
	}
	got, _ = mock.GetVM("ns", "vm")
	assertBootOrder(t, got, cdromDevice, "1")
	assertBootOrder(t, got, rootDiskName, "2")
	if cfg := got.Annotations[BootOnceOriginalConfigAnnotation]; !strings.Contains(cfg, `{"diskName":"rootdisk","bootOrder":1}`) || !strings.Contains(cfg, `{"diskName":"cdrom0"}`) {
		t.Errorf("original boot order must record root first and the CD-ROM without an order, got %s", cfg)
	}
}

// TestConcurrentVMWriters_ConvergeRegardlessOfOrder runs Ironic's request
// mix (insert, boot override annotations, boot-once; then eject with another
// boot-once) concurrently on many VMs, through the real client code and the
// mock's API-server semantics. Whatever the interleaving, every VM must end
// with its CD-ROM volume and disk paired and the CD-ROM first in the boot
// order; different VMs must not block each other.
func TestConcurrentVMWriters_ConvergeRegardlessOfOrder(t *testing.T) {
	const vmCount = 24
	var vms []*kubevirtv1.VirtualMachine
	initialRootOrder := map[string]uint{}
	for i := 0; i < vmCount; i++ {
		vm := newRootDiskVM("ns", fmt.Sprintf("vm-%d", i))
		// Half the VMs look like a first enroll (root first), half like the
		// next cycle after an eject (root second), where a stale boot-once
		// patch rewrites the disks array.
		initialRootOrder[vm.Name] = uint(1 + i%2)
		vms = append(vms, withRootBootOrder(vm, initialRootOrder[vm.Name]))
	}
	client, mock, _ := newVMWriterTestClient(t, vms...)

	// Widen the window between every writer's read and its write so the
	// goroutines really interleave instead of running one after another.
	var patches int64
	mock.BeforePatch = func(schema.GroupVersionResource, string, string, types.PatchType, []byte) {
		if atomic.AddInt64(&patches, 1)%3 == 0 {
			time.Sleep(time.Millisecond)
		}
	}

	bootOptions := map[string]interface{}{"bootSourceOverrideEnabled": "Once", "bootSourceOverrideTarget": "Cd", "bootSourceOverrideMode": "UEFI"}
	var wg sync.WaitGroup
	fail := func(format string, args ...interface{}) { t.Errorf(format, args...) }

	for _, vm := range vms {
		name := vm.Name
		wg.Add(3)
		go func() {
			defer wg.Done()
			if err := client.insertVirtualMediaAsync("ns", name, cdromDevice, testISOURL); err != nil {
				fail("%s: insert failed: %v", name, err)
			}
		}()
		go func() {
			defer wg.Done()
			if err := client.RecordVMBootOptionsAsAnnotations("ns", name, bootOptions); err != nil {
				fail("%s: record boot options failed: %v", name, err)
			}
		}()
		go func() {
			defer wg.Done()
			if err := client.SetBootOnce("ns", name, "Cd"); err != nil {
				fail("%s: SetBootOnce failed: %v", name, err)
			}
		}()
	}
	wg.Wait()

	for _, vm := range vms {
		got, err := mock.GetVM("ns", vm.Name)
		if err != nil {
			t.Fatal(err)
		}
		assertVolumeDiskPairs(t, got)
		assertBootOrder(t, got, cdromDevice, "1")
		assertBootOrder(t, got, rootDiskName, "2")
		if got.Labels[BootOnceLabel] != "enabled" {
			t.Errorf("%s: boot-once label missing", vm.Name)
		}
		wantRoot := fmt.Sprintf(`{"diskName":"rootdisk","bootOrder":%d}`, initialRootOrder[vm.Name])
		if cfg := got.Annotations[BootOnceOriginalConfigAnnotation]; !strings.Contains(cfg, wantRoot) || !strings.Contains(cfg, `{"diskName":"cdrom0"}`) {
			t.Errorf("%s: original boot order must keep the root disk at %d and the CD-ROM unordered, got %s", vm.Name, initialRootOrder[vm.Name], cfg)
		}
	}

	// Ironic's next cycle: eject while a new boot-once arrives.
	for _, vm := range vms {
		name := vm.Name
		wg.Add(2)
		go func() {
			defer wg.Done()
			if err := client.EjectVirtualMedia("ns", name, cdromDevice); err != nil {
				fail("%s: eject failed: %v", name, err)
			}
		}()
		go func() {
			defer wg.Done()
			if err := client.SetBootOnce("ns", name, "Cd"); err != nil {
				fail("%s: second SetBootOnce failed: %v", name, err)
			}
		}()
	}
	wg.Wait()

	for _, vm := range vms {
		got, _ := mock.GetVM("ns", vm.Name)
		assertVolumeDiskPairs(t, got)
		if hasVolume(got, cdromDevice) || hasDisk(got, cdromDevice) {
			t.Errorf("%s: CD-ROM still present after eject", vm.Name)
		}
		if _, ok := got.Annotations[VirtualMediaURLAnnotationPrefix+cdromDevice]; ok {
			t.Errorf("%s: virtual media annotation still present after eject", vm.Name)
		}
	}

	if client.vmLocks.size() != 0 {
		t.Errorf("per-VM locks leaked: %d entries still alive", client.vmLocks.size())
	}
}

func TestRepairVirtualMediaDevices_RestoresMissingDiskAndBootOrder(t *testing.T) {
	// The exact state found on the corrupted production VMs: volume and
	// annotation present, disk entry gone, boot-once to CD pending.
	vm := withBootOnceToCD(withCDROM(newRootDiskVM("ns", "vm"), "vm-bootiso-1"))
	vm.Spec.Template.Spec.Domain.Devices.Disks = vm.Spec.Template.Spec.Domain.Devices.Disks[:1]
	client, mock, _ := newVMWriterTestClient(t, vm)

	client.repairVirtualMediaDevices(vm)

	got, _ := mock.GetVM("ns", "vm")
	assertVolumeDiskPairs(t, got)
	disk := diskByName(got, cdromDevice)
	if disk == nil || disk.CDRom == nil || disk.CDRom.Bus != kubevirtv1.DiskBusSATA {
		t.Fatalf("expected a SATA CD-ROM disk entry to be restored, got %+v", disk)
	}
	assertBootOrder(t, got, cdromDevice, "1")
	assertBootOrder(t, got, rootDiskName, "2")
	if cfg := got.Annotations[BootOnceOriginalConfigAnnotation]; !strings.Contains(cfg, `{"diskName":"cdrom0"}`) {
		t.Errorf("saved original boot order must now list the CD-ROM without an order, got %s", cfg)
	}
}

func TestRepairVirtualMediaDevices_NoWriteWhenHealthyOrUnmanaged(t *testing.T) {
	healthy := withCDROM(newRootDiskVM("ns", "healthy"), "claim")

	// A cdrom volume without the bridge's annotation is not ours to touch.
	unmanaged := newRootDiskVM("ns", "unmanaged")
	unmanaged.Spec.Template.Spec.Volumes = append(unmanaged.Spec.Template.Spec.Volumes, kubevirtv1.Volume{
		Name:         "cdrom1",
		VolumeSource: kubevirtv1.VolumeSource{ContainerDisk: &kubevirtv1.ContainerDiskSource{Image: "x"}},
	})

	client, mock, _ := newVMWriterTestClient(t, healthy, unmanaged)
	var patches int
	mock.BeforePatch = func(schema.GroupVersionResource, string, string, types.PatchType, []byte) { patches++ }

	client.repairVirtualMediaDevices(healthy)
	client.repairVirtualMediaDevices(unmanaged)

	if patches != 0 {
		t.Errorf("expected no writes, got %d", patches)
	}
}

// TestHandleVMUpdate_RepairsThenRestoresAfterReboot drives the watcher entry
// point on a corrupted VM whose one-time boot has completed (VMI UID changed):
// the disk entry comes back and the saved boot order is restored in the same pass.
func TestHandleVMUpdate_RepairsThenRestoresAfterReboot(t *testing.T) {
	vm := withBootOnceToCD(withCDROM(newRootDiskVM("ns", "vm"), "vm-bootiso-1"))
	vm.Spec.Template.Spec.Domain.Devices.Disks = vm.Spec.Template.Spec.Domain.Devices.Disks[:1]
	vm.Annotations[BootOnceVMIUIDAnnotation] = "old-vmi"
	client, mock, _ := newVMWriterTestClient(t, vm)
	if err := mock.AddVMI(&kubevirtv1.VirtualMachineInstance{ObjectMeta: metav1.ObjectMeta{Name: "vm", Namespace: "ns", UID: "new-vmi"}}); err != nil {
		t.Fatal(err)
	}

	client.handleVMUpdate(vm)

	got, _ := mock.GetVM("ns", "vm")
	assertVolumeDiskPairs(t, got)
	assertBootOrder(t, got, rootDiskName, "1")
	assertBootOrder(t, got, cdromDevice, "none")
	if got.Labels[BootOnceLabel] != "" {
		t.Errorf("boot-once label should be cleared after restore")
	}
	if _, ok := got.Annotations[BootOnceOriginalConfigAnnotation]; ok {
		t.Errorf("boot-once annotations should be cleared after restore")
	}
}
