package kubevirt

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	kubevirtv1 "kubevirt.io/api/core/v1"

	"github.com/kubevirt/redfish-controller/pkg/logger"
)

const (
	// vmUpdateMaxAttempts bounds how often updateVM re-reads the VM and
	// re-applies a mutation after the API server rejected the write as stale.
	vmUpdateMaxAttempts = 5
	vmUpdateBaseBackoff = 50 * time.Millisecond
	vmUpdateMaxBackoff  = 500 * time.Millisecond
)

// updateVM is the only path for changing a VirtualMachine by reading it,
// editing a copy and patching the difference back.
//
// Two things make such a read-modify-write safe against another writer:
//
//   - A per-VM lock serializes the writers inside this process. An HTTP
//     handler (boot override), a task worker (virtual media insert) and the
//     boot-once watcher can all touch the same VM within milliseconds of each
//     other. The lock covers only the read-modify-write; slow work such as
//     creating PVCs or downloading ISOs must stay outside it.
//   - The patch carries the resourceVersion of the object it was computed
//     from (see computeVMPatch), so the API server rejects it with a Conflict
//     when anyone changed the VM in between, instead of letting a stale copy
//     of the disks or volumes array silently replace a newer one. The
//     mutation is then re-run on a fresh read. This also covers writers
//     outside this process.
//
// Because mutate can run more than once it must not have side effects beyond
// the copy it is given, and it must take every decision from that copy.
// Nothing inside mutate may call updateVM again: the lock is not re-entrant.
// The read is always a live GET; an object served from a cache would both
// manufacture spurious conflicts and be re-read unchanged on retry.
func (c *Client) updateVM(ctx context.Context, namespace, name string, mutate func(vm *kubevirtv1.VirtualMachine) error) (*kubevirtv1.VirtualMachine, error) {
	unlock := c.lockVM(namespace, name)
	defer unlock()

	gvr := schema.GroupVersionResource{Group: "kubevirt.io", Version: "v1", Resource: "virtualmachines"}

	for attempt := 1; ; attempt++ {
		vm, err := c.GetVM(namespace, name)
		if err != nil {
			return nil, fmt.Errorf("failed to get VM %s: %w", name, err)
		}

		vmCopy := vm.DeepCopy()
		if err := mutate(vmCopy); err != nil {
			return nil, err
		}

		patch, err := computeVMPatch(vm, vmCopy)
		if err != nil {
			return nil, fmt.Errorf("failed to compute VM patch: %w", err)
		}
		if isEmptyMergePatch(patch) {
			return vm, nil
		}

		updated, err := c.dynamicClient.Resource(gvr).Namespace(namespace).Patch(ctx, name, types.MergePatchType, patch, metav1.PatchOptions{})
		if err == nil {
			result, convErr := unstructuredToVM(updated)
			if convErr != nil {
				return vmCopy, nil
			}
			return result, nil
		}
		if !apierrors.IsConflict(err) || attempt >= vmUpdateMaxAttempts {
			return nil, fmt.Errorf("failed to update VM %s/%s: %w", namespace, name, err)
		}

		logger.Info("VM %s/%s changed while an update was being prepared, retrying on a fresh copy (attempt %d/%d)", namespace, name, attempt, vmUpdateMaxAttempts)
		c.sleepFor(vmUpdateBackoff(attempt))
	}
}

func (c *Client) lockVM(namespace, name string) func() {
	if c.vmLocks == nil {
		return func() {}
	}
	return c.vmLocks.Lock(namespace + "/" + name)
}

func (c *Client) sleepFor(d time.Duration) {
	if c.sleep != nil {
		c.sleep(d)
		return
	}
	time.Sleep(d)
}

func vmUpdateBackoff(attempt int) time.Duration {
	d := vmUpdateBaseBackoff * time.Duration(attempt)
	if d > vmUpdateMaxBackoff {
		return vmUpdateMaxBackoff
	}
	return d
}

func isEmptyMergePatch(patch []byte) bool {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(patch, &fields); err != nil {
		return false
	}
	return len(fields) == 0
}

// withResourceVersion sets metadata.resourceVersion in a JSON merge patch.
func withResourceVersion(patch []byte, resourceVersion string) ([]byte, error) {
	var fields map[string]interface{}
	if err := json.Unmarshal(patch, &fields); err != nil {
		return nil, fmt.Errorf("failed to decode merge patch: %w", err)
	}
	metadata, _ := fields["metadata"].(map[string]interface{})
	if metadata == nil {
		metadata = map[string]interface{}{}
		fields["metadata"] = metadata
	}
	metadata["resourceVersion"] = resourceVersion
	return json.Marshal(fields)
}

// addCDROMDisk appends a SATA CD-ROM disk entry named deviceName unless one
// already exists. It returns whether the entry was added.
func addCDROMDisk(vm *kubevirtv1.VirtualMachine, deviceName string) bool {
	if vm.Spec.Template == nil {
		vm.Spec.Template = &kubevirtv1.VirtualMachineInstanceTemplateSpec{}
	}
	for _, disk := range vm.Spec.Template.Spec.Domain.Devices.Disks {
		if disk.Name == deviceName {
			return false
		}
	}
	vm.Spec.Template.Spec.Domain.Devices.Disks = append(vm.Spec.Template.Spec.Domain.Devices.Disks, kubevirtv1.Disk{
		Name: deviceName,
		DiskDevice: kubevirtv1.DiskDevice{
			CDRom: &kubevirtv1.CDRomTarget{Bus: kubevirtv1.DiskBusSATA},
		},
	})
	return true
}

// bootOverrideWantsCD reports whether a one-time boot override to CD was
// recorded on the VM before the CD-ROM device existed, so a device added now
// must be placed first in the boot order.
//
// It requires the boot-once state to be fully recorded (label and saved
// original boot order), not just the override annotations: handleBootUpdate
// writes the annotations first and the boot-once state a moment later, and
// reordering in between would make SetBootOnce capture the reordered disks
// as the "original" boot order. A permanent boot order change (SetBootOrder)
// clears the override annotations, so it never reaches here.
func bootOverrideWantsCD(vm *kubevirtv1.VirtualMachine) bool {
	annotations := vm.GetAnnotations()
	return annotations["redfish.boot.source.override.enabled"] == "Once" &&
		annotations["redfish.boot.source.override.target"] == "Cd" &&
		vm.GetLabels()[BootOnceLabel] == "enabled" &&
		annotations[BootOnceOriginalConfigAnnotation] != ""
}

// applyPendingCDBootOverride re-stamps the boot order so CD-ROM devices come
// first and records the new device in the saved original boot order with no
// boot order of its own. The restore after the one-time boot then clears the
// device's bootOrder instead of leaving two disks at the same position.
func (c *Client) applyPendingCDBootOverride(vm *kubevirtv1.VirtualMachine, deviceName string) error {
	if err := c.modifyVmBootOrder(vm, "Cd"); err != nil {
		return err
	}
	return addDiskToOriginalBootConfig(vm, deviceName)
}

func addDiskToOriginalBootConfig(vm *kubevirtv1.VirtualMachine, diskName string) error {
	annotations := vm.GetAnnotations()
	configJSON := annotations[BootOnceOriginalConfigAnnotation]
	if configJSON == "" {
		return nil
	}
	var bootOrders []BootOrderConfig
	if err := json.Unmarshal([]byte(configJSON), &bootOrders); err != nil {
		return fmt.Errorf("failed to unmarshal boot order config: %w", err)
	}
	for _, entry := range bootOrders {
		if entry.DiskName == diskName {
			return nil
		}
	}
	bootOrders = append(bootOrders, BootOrderConfig{DiskName: diskName})
	updated, err := json.Marshal(bootOrders)
	if err != nil {
		return fmt.Errorf("failed to marshal boot order config: %w", err)
	}
	annotations[BootOnceOriginalConfigAnnotation] = string(updated)
	vm.SetAnnotations(annotations)
	return nil
}

// virtualMediaDevices lists the devices the bridge has inserted media into,
// taken from the virtual-media.redfish/<device> annotations.
func virtualMediaDevices(vm *kubevirtv1.VirtualMachine) []string {
	var devices []string
	for key := range vm.GetAnnotations() {
		if strings.HasPrefix(key, VirtualMediaURLAnnotationPrefix) {
			devices = append(devices, strings.TrimPrefix(key, VirtualMediaURLAnnotationPrefix))
		}
	}
	return devices
}

func hasDisk(vm *kubevirtv1.VirtualMachine, name string) bool {
	if vm.Spec.Template == nil {
		return false
	}
	for _, disk := range vm.Spec.Template.Spec.Domain.Devices.Disks {
		if disk.Name == name {
			return true
		}
	}
	return false
}

func hasVolume(vm *kubevirtv1.VirtualMachine, name string) bool {
	if vm.Spec.Template == nil {
		return false
	}
	for _, volume := range vm.Spec.Template.Spec.Volumes {
		if volume.Name == name {
			return true
		}
	}
	return false
}

// virtualMediaDevicesMissingDisk returns the inserted devices whose volume is
// present but whose disk entry is not. A volume without a disk entry is the
// half-state a lost write leaves behind; KubeVirt attaches it as a writable
// hard disk, which fails on a read-only ISO and leaves the VM crash-looping.
func virtualMediaDevicesMissingDisk(vm *kubevirtv1.VirtualMachine) []string {
	var missing []string
	for _, device := range virtualMediaDevices(vm) {
		if hasVolume(vm, device) && !hasDisk(vm, device) {
			missing = append(missing, device)
		}
	}
	return missing
}

// repairVirtualMediaDevices restores the disk entry of every inserted device
// that lost it, and places it first in the boot order when a boot override to
// CD is pending. It is run from the VM watcher, so the repair happens on the
// next event or reconcile tick without any long-lived goroutine, and the
// decision is re-taken on a fresh copy inside the locked update.
func (c *Client) repairVirtualMediaDevices(vm *kubevirtv1.VirtualMachine) {
	namespace, name := vm.GetNamespace(), vm.GetName()

	// The opposite half-state is only logged: it is not the state a lost
	// write produces, and the next insert or eject rewrites both halves.
	// The watcher sees a VM only when it changes, so this does not repeat.
	for _, device := range virtualMediaDevices(vm) {
		if hasDisk(vm, device) && !hasVolume(vm, device) {
			logger.Warning("VM %s/%s has a disk entry for virtual media device %s but no volume", namespace, name, device)
		}
	}
	if len(virtualMediaDevicesMissingDisk(vm)) == 0 {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), c.timeout)
	defer cancel()

	var repaired []string
	_, err := c.updateVM(ctx, namespace, name, func(vmCopy *kubevirtv1.VirtualMachine) error {
		repaired = virtualMediaDevicesMissingDisk(vmCopy)
		for _, device := range repaired {
			addCDROMDisk(vmCopy, device)
			if bootOverrideWantsCD(vmCopy) {
				if err := c.applyPendingCDBootOverride(vmCopy, device); err != nil {
					return err
				}
			}
		}
		return nil
	})
	if err != nil {
		logger.Error("Failed to repair virtual media disk entries for VM %s/%s: %v", namespace, name, err)
		return
	}
	if len(repaired) > 0 {
		logger.Warning("Repaired VM %s/%s: re-added the missing CD-ROM disk entry for virtual media device(s) %v", namespace, name, repaired)
	}
}
