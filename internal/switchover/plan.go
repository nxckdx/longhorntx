package switchover

import (
	"context"
	"fmt"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/nxckdx/longhorntx/internal/longhorn"
)

var (
	gvrPod = schema.GroupVersionResource{Version: "v1", Resource: "pods"}
	gvrPVC = schema.GroupVersionResource{Version: "v1", Resource: "persistentvolumeclaims"}
	gvrPV  = schema.GroupVersionResource{Version: "v1", Resource: "persistentvolumes"}
)

const longhornDriver = "driver.longhorn.io"

// Plan is everything the switch does, decided up front and shown before
// anything changes. It is also the journal written while it runs.
type Plan struct {
	Namespace string `json:"namespace"`
	PVC       string `json:"pvc"`

	OldVolume  string `json:"oldVolume"`
	OldPV      string `json:"oldPV"`
	OldReclaim string `json:"oldReclaimPolicy"`
	NewVolume  string `json:"newVolume"`
	NewSize    int64  `json:"newSize"`
	// ReusePV: a Released PV of the target volume (switching back) is
	// rebound instead of creating a new one.
	ReusePV string `json:"reusePV,omitempty"`

	StorageClass string            `json:"storageClass"`
	AccessModes  []string          `json:"accessModes"`
	VolumeMode   string            `json:"volumeMode"`
	Request      int64             `json:"request"` // storage request of the new PVC
	Labels       map[string]string `json:"labels,omitempty"`
	Annotations  map[string]string `json:"annotations,omitempty"`
	FSType       string            `json:"fsType"`
	PVAttributes map[string]string `json:"pvAttributes,omitempty"`

	// Freeze: the pods that mount the PVC and their owners; they are
	// stopped by freezing the PVC (no scaling, no GitOps changes)
	Freeze *FreezePlan `json:"freeze"`

	Blockers []string `json:"blockers,omitempty"` // reasons not to run
	Warnings []string `json:"warnings,omitempty"`
}

func (p *Plan) block(format string, a ...any) {
	p.Blockers = append(p.Blockers, fmt.Sprintf(format, a...))
}
func (p *Plan) warn(format string, a ...any) {
	p.Warnings = append(p.Warnings, fmt.Sprintf(format, a...))
}

// PVCRef is "namespace/name".
func (p *Plan) PVCRef() string { return p.Namespace + "/" + p.PVC }

// Build inspects the PVC, its current volume, the target volume, the pods
// that mount the PVC and their owners. Nothing is changed. newVolume must
// exist (restore it first); restoring is allowed – Run waits for it.
func Build(ctx context.Context, c *longhorn.Client, namespace, pvc, newVolume string) (*Plan, error) {
	return build(ctx, c, namespace, pvc, newVolume, 0)
}

// BuildPending plans a switch to a volume that does not exist yet (it is
// restored right before the switch); size is its future size. This finds
// blockers before anything is restored.
func BuildPending(ctx context.Context, c *longhorn.Client, namespace, pvc, newVolume string, size int64) (*Plan, error) {
	return build(ctx, c, namespace, pvc, newVolume, size)
}

func build(ctx context.Context, c *longhorn.Client, namespace, pvc, newVolume string, pendingSize int64) (*Plan, error) {
	dyn := c.Dynamic()
	p := &Plan{Namespace: namespace, PVC: pvc, NewVolume: newVolume}

	claim, err := dyn.Resource(gvrPVC).Namespace(namespace).Get(ctx, pvc, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("PVC %s/%s: %w", namespace, pvc, err)
	}
	if phase, _, _ := unstructured.NestedString(claim.Object, "status", "phase"); phase != "Bound" {
		p.block("PVC is %s, not Bound", phase)
	}
	p.OldPV, _, _ = unstructured.NestedString(claim.Object, "spec", "volumeName")
	p.StorageClass, _, _ = unstructured.NestedString(claim.Object, "spec", "storageClassName")
	p.AccessModes, _, _ = unstructured.NestedStringSlice(claim.Object, "spec", "accessModes")
	p.VolumeMode, _, _ = unstructured.NestedString(claim.Object, "spec", "volumeMode")
	if req, _, _ := unstructured.NestedString(claim.Object, "spec", "resources", "requests", "storage"); req != "" {
		if q, err := resource.ParseQuantity(req); err == nil {
			p.Request = q.Value()
		}
	}
	p.Labels = claim.GetLabels()
	p.Annotations = keepAnnotations(claim.GetAnnotations())

	// the PV and the Longhorn volume behind it
	if p.OldPV != "" {
		pv, err := dyn.Resource(gvrPV).Get(ctx, p.OldPV, metav1.GetOptions{})
		if err != nil {
			return nil, fmt.Errorf("PV %s: %w", p.OldPV, err)
		}
		drv, _, _ := unstructured.NestedString(pv.Object, "spec", "csi", "driver")
		if drv != longhornDriver {
			p.block("PV %s is not a Longhorn volume (driver %q)", p.OldPV, drv)
		}
		p.OldVolume, _, _ = unstructured.NestedString(pv.Object, "spec", "csi", "volumeHandle")
		p.OldReclaim, _, _ = unstructured.NestedString(pv.Object, "spec", "persistentVolumeReclaimPolicy")
		p.FSType, _, _ = unstructured.NestedString(pv.Object, "spec", "csi", "fsType")
		attrs, _, _ := unstructured.NestedStringMap(pv.Object, "spec", "csi", "volumeAttributes")
		p.PVAttributes = keepPVAttributes(attrs)
	}
	if p.FSType == "" {
		p.FSType = "ext4"
	}
	if p.OldVolume == newVolume {
		p.block("the PVC already uses %s", newVolume)
	}

	// the target volume
	nv := &longhorn.Volume{}
	if pendingSize > 0 {
		p.NewSize = pendingSize
		nv.Spec.Size = longhorn.Quantity(pendingSize)
		nv.Status.State = "detached"
	} else {
		v, err := c.Volume(ctx, newVolume)
		if err != nil {
			return nil, fmt.Errorf("target volume %s: %w (restore or clone it first)", newVolume, err)
		}
		nv = v
		p.NewSize = int64(nv.Spec.Size)
	}
	if nv.Status.State == "attached" && !nv.Status.RestoreRequired {
		p.block("target volume %s is attached (in use?) – it must be detached", newVolume)
	}
	if nv.Spec.DataEngine == "v2" {
		p.block("v2 data engine volumes are not supported by lhtx")
	}
	if pvName := nv.Status.KubernetesStatus.PVName; pvName != "" {
		pv, err := dyn.Resource(gvrPV).Get(ctx, pvName, metav1.GetOptions{})
		switch {
		case apierrors.IsNotFound(err):
		case err != nil:
			return nil, err
		default:
			phase, _, _ := unstructured.NestedString(pv.Object, "status", "phase")
			crNS, _, _ := unstructured.NestedString(pv.Object, "spec", "claimRef", "namespace")
			crName, _, _ := unstructured.NestedString(pv.Object, "spec", "claimRef", "name")
			switch {
			case phase == "Released" && crNS == namespace && crName == pvc:
				p.ReusePV = pvName // switching back to the old volume
			case phase == "Available" && crName == "":
				p.ReusePV = pvName
			default:
				p.block("target volume %s already has PV %s (%s, claim %s/%s)", newVolume, pvName, phase, crNS, crName)
			}
		}
	}
	if p.NewSize < p.Request {
		p.warn("the target volume (%s) is smaller than the PVC request (%s) – the backup is older than an expansion; the new PVC requests %s, expand it again afterwards (lhtx expand)",
			human(p.NewSize), human(p.Request), human(p.NewSize))
		p.Request = p.NewSize
	}

	// who mounts the PVC: they are stopped by freezing it
	fp, err := PlanFreeze(ctx, c, []string{p.PVCRef()}, "switch "+p.PVCRef()+" to "+newVolume)
	if err != nil {
		return nil, err
	}
	p.Freeze = fp
	p.Warnings = append(p.Warnings, fp.Warnings...)
	for _, pod := range fp.Pods {
		ns, n, _ := strings.Cut(pod, "/")
		if po, err := dyn.Resource(gvrPod).Namespace(ns).Get(ctx, n, metav1.GetOptions{}); err == nil {
			if k, cl := owner(po); k == "Cluster" {
				p.warn("this is an instance of the CloudNativePG cluster %s: restoring one instance's PVC does not restore the database "+
					"consistently (the other instances keep their data, the operator may re-sync or fail over) – prefer CNPG's own recovery", cl)
			}
		}
	}
	if p.OldReclaim == "Delete" {
		p.warn("the old PV has reclaim policy Delete: it is set to Retain first, so deleting the old PVC keeps the old volume for a rollback")
	}
	return p, nil
}

func mountsClaim(pod *unstructured.Unstructured, claim string) bool {
	vols, _, _ := unstructured.NestedSlice(pod.Object, "spec", "volumes")
	for _, v := range vols {
		if m, ok := v.(map[string]any); ok {
			if c, _, _ := unstructured.NestedString(m, "persistentVolumeClaim", "claimName"); c == claim {
				return true
			}
		}
	}
	return false
}

// owner returns the controller owner reference (kind, name).
func owner(o *unstructured.Unstructured) (string, string) {
	for _, r := range o.GetOwnerReferences() {
		if r.Controller != nil && *r.Controller {
			return r.Kind, r.Name
		}
	}
	if refs := o.GetOwnerReferences(); len(refs) > 0 {
		return refs[0].Kind, refs[0].Name
	}
	return "", ""
}

// keepAnnotations drops what the PV controller set for the old binding;
// Helm's ownership annotations and everything else stay.
func keepAnnotations(a map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range a {
		if strings.HasPrefix(k, "pv.kubernetes.io/") || strings.HasSuffix(k, "storage-provisioner") ||
			k == "kubectl.kubernetes.io/last-applied-configuration" {
			continue
		}
		out[k] = v
	}
	return out
}

// keepPVAttributes copies the CSI attributes of the old PV except those
// tied to the old volume.
func keepPVAttributes(a map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range a {
		if k == "fromBackup" || k == "dataSource" || strings.HasPrefix(k, "storage.kubernetes.io/") {
			continue
		}
		out[k] = v
	}
	return out
}

func human(n int64) string {
	return resource.NewQuantity(n, resource.BinarySI).String()
}

// Steps describes what Run will do, in order, for the preview.
func (p *Plan) Steps() []string {
	var out []string
	add := func(f string, a ...any) { out = append(out, fmt.Sprintf(f, a...)) }
	add("wait until volume %s is restored/ready and detached", p.NewVolume)
	pods := "no pod mounts it right now"
	if p.Freeze != nil && len(p.Freeze.Pods) > 0 {
		pods = fmt.Sprintf("delete its %d pod(s): %s", len(p.Freeze.Pods), strings.Join(firstN(p.Freeze.Pods, 3), ", "))
	}
	if p.Freeze != nil {
		for _, g := range p.Freeze.Groups {
			if g.Pre != nil {
				add("pre-freeze hook of %s: %s", g.name(), g.Pre)
			}
		}
	}
	add("freeze %s (admission policy: no pod may mount it, only lhtx may delete/create it) and %s", p.PVCRef(), pods)
	if p.Freeze != nil {
		for _, g := range p.Freeze.Groups {
			if g.Hibernate {
				add("hibernate the CloudNativePG cluster %s (the operator stops it cleanly)", g.Owner.Name)
			}
		}
	}
	add("wait until volume %s is detached", p.OldVolume)
	if p.OldReclaim != "Retain" {
		add("PV %s: reclaim policy %s → Retain (keeps volume %s)", p.OldPV, p.OldReclaim, p.OldVolume)
	}
	add("delete PVC %s  ← from here no automatic rollback", p.PVCRef())
	if p.ReusePV != "" {
		add("rebind PV %s to %s", p.ReusePV, p.PVCRef())
	} else {
		add("create PV %s (volume %s, %s, class %s)", p.NewVolume, p.NewVolume, human(p.NewSize), p.StorageClass)
	}
	add("create PVC %s (same labels/annotations, %s) bound to it, wait until Bound", p.PVCRef(), human(p.Request))
	owners := "their controllers"
	if p.Freeze != nil && len(p.Freeze.Owners) > 0 {
		var o []string
		for _, x := range p.Freeze.Owners {
			o = append(o, x.Kind+"/"+x.Name)
		}
		owners = strings.Join(firstN(o, 3), ", ")
	}
	add("unfreeze; %s start the pods again (nudged), wait until the volume is attached", owners)
	if p.Freeze != nil {
		for _, g := range p.Freeze.Groups {
			if g.Post != nil {
				add("post-unfreeze hook of %s: %s", g.name(), g.Post)
			}
		}
	}
	add("keep volume %s for a rollback (lhtx switch %s --to-volume %s)", p.OldVolume, p.PVCRef(), p.OldVolume)
	return out
}
