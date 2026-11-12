package kubevirt

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/kubevirt/redfish-controller/pkg/logger"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
)

// InsertMedia is answered with 202 before its job runs, and the job labels the
// VM with the import pod only after creating it. Ironic sends Power On right
// after InsertMedia, and power-on is deferred only while the importing label
// is on the VM. Without a marker written before the 202, Power On lands in
// that gap, the VM starts with an empty CD-ROM and takes the RWO ISO volume
// away from the import pod. The handler therefore writes a placeholder value
// into the importing label up front; the job later replaces it with the pod
// name, so the pod watcher's ownership check is unchanged.
//
// The placeholder carries its creation time. The job queue is in memory, so a
// restart loses queued jobs, and a job can also fail before it ever creates a
// pod: a placeholder nobody replaces would defer power actions forever. The
// job's terminal failure clears it, and the periodic reconcile sweeps
// placeholders older than stalePendingImportAge.
const ImportPendingPrefix = "pending-"

const stalePendingImportAge = 5 * time.Minute

func importPendingValue(now time.Time) string {
	return ImportPendingPrefix + strconv.FormatInt(now.Unix(), 10)
}

func isPendingImport(value string) bool {
	return strings.HasPrefix(value, ImportPendingPrefix)
}

// pendingImportIsStale reports whether a placeholder is old enough that its job
// cannot still be queued. A placeholder that cannot be parsed counts as stale.
func pendingImportIsStale(value string, now time.Time) bool {
	if !isPendingImport(value) {
		return false
	}
	created, err := strconv.ParseInt(strings.TrimPrefix(value, ImportPendingPrefix), 10, 64)
	if err != nil {
		return true
	}
	return now.Sub(time.Unix(created, 0)) > stalePendingImportAge
}

// MarkImportPending records on the VM that an import of imageURL into mediaID
// has been accepted but not started. It returns false, without touching the
// VM, when an import is already recorded for the device.
func (c *Client) MarkImportPending(namespace, name, mediaID, imageURL string) (bool, error) {
	vm, err := c.GetVM(namespace, name)
	if err != nil {
		return false, fmt.Errorf("failed to get VM %s/%s: %w", namespace, name, err)
	}
	labelKey := ImportingLabelPrefix + mediaID
	if vm.GetLabels()[labelKey] != "" {
		return false, nil
	}

	patchJSON := fmt.Sprintf(`{"metadata":{"labels":{%q:%q},"annotations":{%q:%q}}}`,
		labelKey, importPendingValue(time.Now()), VirtualMediaURLAnnotationPrefix+mediaID, imageURL)
	if err := c.patchVMMetadata(namespace, name, patchJSON); err != nil {
		return false, fmt.Errorf("failed to mark import pending on VM %s/%s: %w", namespace, name, err)
	}
	logger.Info("Marked import of %s as pending on VM %s/%s", mediaID, namespace, name)
	return true, nil
}

// ClearPendingImport removes the placeholder for mediaID. A label that already
// names an import pod or a CDI import is owned by the watchers and is left alone.
func (c *Client) ClearPendingImport(namespace, name, mediaID string) error {
	vm, err := c.GetVM(namespace, name)
	if err != nil {
		return fmt.Errorf("failed to get VM %s/%s: %w", namespace, name, err)
	}
	labelKey := ImportingLabelPrefix + mediaID
	if !isPendingImport(vm.GetLabels()[labelKey]) {
		return nil
	}
	if err := c.removeImportingLabel(namespace, name, mediaID); err != nil {
		return err
	}
	logger.Info("Cleared pending import of %s from VM %s/%s", mediaID, namespace, name)
	return nil
}

// reconcileStalePendingImports removes placeholders whose job can no longer be
// waiting in the queue (see ImportPendingPrefix).
func (c *Client) reconcileStalePendingImports(namespace string, now time.Time) error {
	ctx, cancel := context.WithTimeout(context.Background(), c.timeout)
	defer cancel()

	vmList, err := c.dynamicClient.Resource(virtualMachinesGVR).Namespace(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("failed to list VMs in namespace %s: %w", namespace, err)
	}

	for _, item := range vmList.Items {
		for key, value := range item.GetLabels() {
			if !strings.HasPrefix(key, ImportingLabelPrefix) || !pendingImportIsStale(value, now) {
				continue
			}
			mediaID := strings.TrimPrefix(key, ImportingLabelPrefix)
			logger.Warning("Import of %s on VM %s/%s was accepted but never started (label %s=%s), clearing it", mediaID, namespace, item.GetName(), key, value)
			if err := c.removeImportingLabel(namespace, item.GetName(), mediaID); err != nil {
				logger.Error("Failed to clear stale pending import on VM %s/%s: %v", namespace, item.GetName(), err)
			}
		}
	}
	return nil
}

var virtualMachinesGVR = schema.GroupVersionResource{Group: "kubevirt.io", Version: "v1", Resource: "virtualmachines"}

// patchVMMetadata applies a merge patch that only touches labels or
// annotations. Such a patch is atomic on the API server and cannot clobber a
// concurrent spec write, so it needs no resourceVersion.
func (c *Client) patchVMMetadata(namespace, name, patchJSON string) error {
	ctx, cancel := context.WithTimeout(context.Background(), c.timeout)
	defer cancel()
	_, err := c.dynamicClient.Resource(virtualMachinesGVR).Namespace(namespace).Patch(ctx, name, types.MergePatchType, []byte(patchJSON), metav1.PatchOptions{})
	return err
}
