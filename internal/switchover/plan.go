package switchover

import (
	"context"
	"fmt"
	"sort"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"

	"github.com/nxckdx/longhorntx/internal/longhorn"
)

var (
	gvrPod         = schema.GroupVersionResource{Version: "v1", Resource: "pods"}
	gvrPVC         = schema.GroupVersionResource{Version: "v1", Resource: "persistentvolumeclaims"}
	gvrPV          = schema.GroupVersionResource{Version: "v1", Resource: "persistentvolumes"}
	gvrReplicaSet  = schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "replicasets"}
	gvrDeployment  = schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}
	gvrStatefulSet = schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "statefulsets"}
	gvrHPA         = schema.GroupVersionResource{Group: "autoscaling", Version: "v2", Resource: "horizontalpodautoscalers"}
)

const longhornDriver = "driver.longhorn.io"

// Workload is a Deployment or StatefulSet that mounts the PVC.
type Workload struct {
	Kind     string `json:"kind"` // Deployment | StatefulSet
	Name     string `json:"name"`
	Replicas int64  `json:"replicas"` // before the switch
}

func (w Workload) String() string { return w.Kind + "/" + w.Name }

func (w Workload) gvr() schema.GroupVersionResource {
	if w.Kind == "StatefulSet" {
		return gvrStatefulSet
	}
	return gvrDeployment
}

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

	Workloads []Workload `json:"workloads"`
	Flux      []FluxRef  `json:"flux"`

	Blockers []string `json:"blockers,omitempty"` // reasons not to run
	Warnings []string `json:"warnings,omitempty"`

	pods int // pods that mount the PVC
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
	flux := map[string]FluxRef{}
	addFlux(flux, fluxOwners(claim.GetLabels())...)
	if u := unsupportedOwner(claim.GetLabels(), claim.GetAnnotations()); u != "" {
		p.block("the PVC is managed by %s", u)
	}

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

	// who mounts the PVC
	if err := p.findWorkloads(ctx, dyn, flux); err != nil {
		return nil, err
	}
	p.Flux = sortedFlux(flux)
	for i := range p.Flux {
		s, err := readSuspended(ctx, dyn, p.Flux[i])
		if err != nil {
			if apierrors.IsNotFound(err) || strings.Contains(err.Error(), "could not find the requested resource") {
				p.block("%s is named in the labels but not found", p.Flux[i])
				continue
			}
			return nil, err
		}
		p.Flux[i].WasSuspended = s
	}
	p.addWarnings()
	return p, nil
}

func (p *Plan) findWorkloads(ctx context.Context, dyn dynamic.Interface, flux map[string]FluxRef) error {
	pods, err := dyn.Resource(gvrPod).Namespace(p.Namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("pods in %s: %w", p.Namespace, err)
	}
	seen := map[string]bool{}
	for _, pod := range pods.Items {
		if !mountsClaim(&pod, p.PVC) {
			continue
		}
		p.pods++
		kind, name := owner(&pod)
		switch kind {
		case "ReplicaSet":
			rs, err := dyn.Resource(gvrReplicaSet).Namespace(p.Namespace).Get(ctx, name, metav1.GetOptions{})
			if err != nil {
				return fmt.Errorf("ReplicaSet %s: %w", name, err)
			}
			k, n := owner(rs)
			if k != "Deployment" {
				p.block("pod %s belongs to ReplicaSet %s without a Deployment", pod.GetName(), name)
				continue
			}
			kind, name = k, n
		case "StatefulSet":
		case "Cluster":
			p.block("pod %s belongs to a CloudNativePG Cluster (%s): its PVCs are managed by the operator – use CNPG's own recovery, or restore into a new volume and bootstrap a new cluster from it", pod.GetName(), name)
			continue
		case "":
			p.block("pod %s has no owner – delete it yourself first", pod.GetName())
			continue
		default:
			p.block("pod %s belongs to a %s (%s) – lhtx can only scale Deployments and StatefulSets", pod.GetName(), kind, name)
			continue
		}
		key := kind + "/" + name
		if seen[key] {
			continue
		}
		seen[key] = true
		w := Workload{Kind: kind, Name: name}
		obj, err := dyn.Resource(w.gvr()).Namespace(p.Namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("%s: %w", w, err)
		}
		if k, n := owner(obj); k != "" {
			p.block("%s is managed by %s %s (an operator) – it would scale it back up; stop it through the operator", w, k, n)
			continue
		}
		w.Replicas, _, _ = unstructured.NestedInt64(obj.Object, "spec", "replicas")
		if _, found, _ := unstructured.NestedInt64(obj.Object, "spec", "replicas"); !found {
			w.Replicas = 1
		}
		addFlux(flux, fluxOwners(obj.GetLabels())...)
		if u := unsupportedOwner(obj.GetLabels(), obj.GetAnnotations()); u != "" {
			p.block("%s is managed by %s", w, u)
		}
		p.Workloads = append(p.Workloads, w)
	}
	sort.Slice(p.Workloads, func(i, j int) bool { return p.Workloads[i].String() < p.Workloads[j].String() })

	// an autoscaler would undo scaling to 0
	hpas, err := dyn.Resource(gvrHPA).Namespace(p.Namespace).List(ctx, metav1.ListOptions{})
	if err == nil {
		for _, h := range hpas.Items {
			k, _, _ := unstructured.NestedString(h.Object, "spec", "scaleTargetRef", "kind")
			n, _, _ := unstructured.NestedString(h.Object, "spec", "scaleTargetRef", "name")
			for _, w := range p.Workloads {
				if w.Kind == k && w.Name == n {
					p.block("HorizontalPodAutoscaler %s scales %s – it would scale it back up", h.GetName(), w)
				}
			}
		}
	}
	return nil
}

func (p *Plan) addWarnings() {
	if p.pods == 0 {
		p.warn("no running pod uses the PVC: nothing is scaled (anything started meanwhile would block the switch)")
	}
	for _, w := range p.Workloads {
		if w.Kind == "StatefulSet" && w.Replicas > 1 {
			p.warn("%s has %d replicas: all of them stop during the switch, not only the one using this PVC", w, w.Replicas)
		}
	}
	for _, f := range p.Flux {
		switch {
		case f.WasSuspended:
			p.warn("%s is already suspended and stays suspended", f)
		case f.Kind == "Kustomization":
			p.warn("while %s is suspended Flux applies none of its objects (only for the few minutes of the switch)", f)
		}
	}
	if p.OldReclaim == "Delete" {
		p.warn("the old PV has reclaim policy Delete: it is set to Retain first, so deleting the old PVC keeps the old volume for a rollback")
	}
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
	for _, f := range p.Flux {
		if !f.WasSuspended {
			add("suspend %s (spec.suspend: true)", f)
		}
	}
	for _, w := range p.Workloads {
		add("scale %s from %d to 0", w, w.Replicas)
	}
	add("wait until no pod uses %s and volume %s is detached", p.PVCRef(), p.OldVolume)
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
	for _, w := range p.Workloads {
		add("scale %s back to %d", w, w.Replicas)
	}
	for _, f := range p.Flux {
		if !f.WasSuspended {
			add("resume %s and request a reconcile", f)
		}
	}
	add("keep volume %s for a rollback (lhtx switch %s --to-volume %s)", p.OldVolume, p.PVCRef(), p.OldVolume)
	return out
}
