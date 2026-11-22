package longhorn

import (
	"context"
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

var (
	GVRPVC          = schema.GroupVersionResource{Version: "v1", Resource: "persistentvolumeclaims"}
	GVRStorageClass = schema.GroupVersionResource{Group: "storage.k8s.io", Version: "v1", Resource: "storageclasses"}
)

// PVCInfo is what an expansion needs to know about the volume's PVC.
type PVCInfo struct {
	Namespace, Name string
	ResourceVersion string
	Requested       int64 // spec.resources.requests.storage
	StorageClass    string
	AllowExpansion  bool // StorageClass allowVolumeExpansion
	ClassMissing    bool
	Labels          map[string]string
}

// PVCInfo reads the volume's PVC and its StorageClass (two small GETs).
// Returns nil for volumes without a PVC.
func (c *Client) PVCInfo(ctx context.Context, v *Volume) (*PVCInfo, error) {
	ks := v.Status.KubernetesStatus
	if ks.PVCName == "" || ks.Namespace == "" {
		return nil, nil
	}
	u, err := c.dyn.Resource(GVRPVC).Namespace(ks.Namespace).Get(ctx, ks.PVCName, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("PVC %s/%s: %w", ks.Namespace, ks.PVCName, err)
	}
	var pvc struct {
		Spec struct {
			StorageClassName string `json:"storageClassName"`
			Resources        struct {
				Requests map[string]string `json:"requests"`
			} `json:"resources"`
		} `json:"spec"`
	}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, &pvc); err != nil {
		return nil, err
	}
	info := &PVCInfo{Namespace: ks.Namespace, Name: ks.PVCName, ResourceVersion: u.GetResourceVersion(),
		StorageClass: pvc.Spec.StorageClassName, Labels: u.GetLabels()}
	if q, err := resource.ParseQuantity(pvc.Spec.Resources.Requests["storage"]); err == nil {
		info.Requested = q.Value()
	}
	if info.StorageClass != "" {
		sc, err := c.dyn.Resource(GVRStorageClass).Get(ctx, info.StorageClass, metav1.GetOptions{})
		if err != nil {
			info.ClassMissing = true
		} else {
			info.AllowExpansion, _, _ = unstructured.NestedBool(sc.Object, "allowVolumeExpansion")
		}
	}
	return info, nil
}

// ParseSize parses "20Gi", "+5Gi" (relative to current) or plain bytes.
func ParseSize(s string, current int64) (int64, error) {
	s = strings.TrimSpace(s)
	rel := strings.HasPrefix(s, "+")
	q, err := resource.ParseQuantity(strings.TrimPrefix(s, "+"))
	if err != nil {
		return 0, fmt.Errorf("invalid size %q (examples: 20Gi, +5Gi)", s)
	}
	n := q.Value()
	if rel {
		n += current
	}
	return n, nil
}

// ExpandVolume grows a volume. A volume bound to a PVC is expanded through
// the PVC (the CSI resizer then grows the Longhorn volume and the
// filesystem) – this is what longhorn-manager does too, and it keeps the PVC
// size in sync. Volumes without a PVC get spec.size patched directly.
func ExpandVolume(v *Volume, pvc *PVCInfo, size int64) (*Action, error) {
	if err := checkVolumeWritable(v); err != nil {
		return nil, err
	}
	cur := int64(v.Spec.Size)
	if size <= cur {
		return nil, fmt.Errorf("new size %s must be larger than the current %s (volumes cannot shrink)", humanBytes(size), humanBytes(cur))
	}
	// Longhorn sizes are multiples of 2 MiB; round up like longhorn-manager.
	const align = 2 << 20
	if size%align != 0 {
		size += align - size%align
	}
	if v.Spec.DataEngine == "v2" {
		return nil, fmt.Errorf("expansion of v2 data engine volumes is not supported by lhtx")
	}
	sizeQ := resource.NewQuantity(size, resource.BinarySI).String()
	a := &Action{
		Title: fmt.Sprintf("Expand %s from %s to %s", volLabel(v), humanBytes(cur), humanBytes(size)),
		Verb:  VerbPatch,
	}
	if pvc != nil {
		if pvc.ClassMissing {
			return nil, fmt.Errorf("StorageClass %q of PVC %s/%s not found: expansion through the PVC is not possible", pvc.StorageClass, pvc.Namespace, pvc.Name)
		}
		if pvc.StorageClass != "" && !pvc.AllowExpansion {
			return nil, fmt.Errorf("StorageClass %q does not set allowVolumeExpansion: true", pvc.StorageClass)
		}
		a.GVR, a.Namespace, a.Name = GVRPVC, pvc.Namespace, pvc.Name
		a.Patch = patchWithRV(pvc.ResourceVersion, map[string]any{
			"resources": map[string]any{"requests": map[string]any{"storage": sizeQ}},
		})
		a.Changes = []Change{{"spec.resources.requests.storage",
			resource.NewQuantity(pvc.Requested, resource.BinarySI).String(), sizeQ}}
		a.Warnings = append(a.Warnings, "expanded through the PVC: the CSI resizer grows the Longhorn volume, the filesystem grows while the volume is attached")
	} else {
		a.GVR, a.Name = GVRVolume, v.Name
		a.Patch = patchWithRV(v.ResourceVersion, map[string]any{"size": fmt.Sprint(size)})
		a.Changes = []Change{{"spec.size", fmt.Sprint(cur), fmt.Sprint(size)}}
		a.Warnings = append(a.Warnings, "volume has no PVC: only the block device grows, resize the filesystem yourself")
	}
	a.Warnings = append(a.Warnings, "expansion cannot be undone (Longhorn volumes cannot shrink)")
	if v.Status.Robustness != "healthy" && v.Status.State == "attached" {
		a.Warnings = append(a.Warnings, "volume is "+v.Status.Robustness+": Longhorn may refuse to expand until all replicas are healthy")
	}
	a.Warnings = append(a.Warnings, fmt.Sprintf("every replica disk needs %s more scheduled space", humanBytes(size-cur)))
	return a, nil
}
