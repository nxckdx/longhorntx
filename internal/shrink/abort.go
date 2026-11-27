package shrink

import (
	"context"
	"fmt"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"

	"github.com/nxckdx/longhorntx/internal/longhorn"
	"github.com/nxckdx/longhorntx/internal/switchover"
)

// AnnShrinkOf: on the new volume, names the volume it replaces.
const AnnShrinkOf = "lhtx.io/shrink-of"

// Leftovers is what an interrupted shrink of one PVC left behind (lhtx was
// killed during the copy, the laptop closed): its freeze, the copy/probe
// pods, the temporary PVC and PV, the new volume.
type Leftovers struct {
	Namespace, PVC string
	OldVolume      string // the volume the PVC uses now
	Swapped        bool   // the PVC already uses the new volume: only the freeze is left
	Freezes        []switchover.Freeze
	Pods           []string // namespace/name
	TempPVC        string
	TempPVs        []string
	NewVolumes     []*longhorn.Volume
	Problem        string // why Abort cannot clean up
}

// Empty: nothing left over.
func (l *Leftovers) Empty() bool {
	return len(l.Freezes) == 0 && len(l.Pods) == 0 && l.TempPVC == "" && len(l.TempPVs) == 0 && len(l.NewVolumes) == 0
}

// FindLeftovers looks for what a shrink of namespace/pvc left behind.
func FindLeftovers(ctx context.Context, c *longhorn.Client, namespace, pvc string) (*Leftovers, error) {
	dyn := c.Dynamic()
	l := &Leftovers{Namespace: namespace, PVC: pvc}
	claim, err := dyn.Resource(gvrPVC).Namespace(namespace).Get(ctx, pvc, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		l.Problem = fmt.Sprintf("PVC %s/%s does not exist: the shrink stopped after the old PVC was deleted – finish it with lhtx switch --status (journal) and lhtx --rw switch %s/%s --to-volume NEW, or go back with --to-volume OLD",
			namespace, pvc, namespace, pvc)
	case err != nil:
		return nil, err
	default:
		pvName, _, _ := unstructured.NestedString(claim.Object, "spec", "volumeName")
		if pv, err := dyn.Resource(gvrPV).Get(ctx, pvName, metav1.GetOptions{}); err == nil {
			l.OldVolume, _, _ = unstructured.NestedString(pv.Object, "spec", "csi", "volumeHandle")
		}
	}
	if fs, err := switchover.ActiveFreezes(ctx, dyn); err == nil {
		for _, f := range fs {
			if strings.HasPrefix(f.Reason, "shrink "+namespace+"/"+pvc+" ") {
				l.Freezes = append(l.Freezes, f)
			}
		}
	}
	if pl, err := dyn.Resource(gvrPod).Namespace(namespace).List(ctx, metav1.ListOptions{LabelSelector: LabelCopy}); err == nil {
		for _, it := range pl.Items {
			l.Pods = append(l.Pods, namespace+"/"+it.GetName())
		}
	}
	tmp := tempPVCName(pvc)
	if _, err := dyn.Resource(gvrPVC).Namespace(namespace).Get(ctx, tmp, metav1.GetOptions{}); err == nil {
		l.TempPVC = tmp
	}
	if pvl, err := dyn.Resource(gvrPV).List(ctx, metav1.ListOptions{LabelSelector: LabelCopy + "=target"}); err == nil {
		for _, it := range pvl.Items {
			ns, _, _ := unstructured.NestedString(it.Object, "spec", "claimRef", "namespace")
			n, _, _ := unstructured.NestedString(it.Object, "spec", "claimRef", "name")
			if ns == namespace && n == tmp {
				l.TempPVs = append(l.TempPVs, it.GetName())
			}
		}
	}
	vols, err := c.Volumes(ctx)
	if err != nil {
		return nil, err
	}
	var current *longhorn.Volume
	for _, v := range vols {
		if v.Name == l.OldVolume {
			current = v
		}
	}
	if current != nil && current.Annotations[AnnShrinkOf] != "" {
		l.Swapped = true // the PVC runs on a shrunk volume: the swap happened
	}
	for _, v := range vols {
		if of := v.Annotations[AnnShrinkOf]; of != "" && of == l.OldVolume && v.Name != l.OldVolume {
			l.NewVolumes = append(l.NewVolumes, v)
		}
	}
	return l, nil
}

func tempPVCName(pvc string) string {
	n := "lhtx-shrink-" + pvc
	if len(n) > 63 {
		n = strings.TrimRight(n[:63], "-")
	}
	return n
}

// Lines describe what Abort does.
func (l *Leftovers) Lines() []string {
	var out []string
	for _, p := range l.Pods {
		out = append(out, "delete pod "+p)
	}
	if l.TempPVC != "" {
		out = append(out, "delete PVC "+l.Namespace+"/"+l.TempPVC)
	}
	for _, pv := range l.TempPVs {
		out = append(out, "delete PV "+pv+" (reclaim Retain: the volume is not touched by this)")
	}
	for _, v := range l.NewVolumes {
		out = append(out, fmt.Sprintf("delete volume %s (the unfinished copy; %s stays as it is)", v.Name, l.OldVolume))
	}
	for _, f := range l.Freezes {
		out = append(out, fmt.Sprintf("unfreeze %s (%s): the app starts again on %s", f.Name, f.Reason, l.OldVolume))
	}
	return out
}

// Abort removes the leftovers and ends the freeze: the app runs on the old
// volume again. Refused when the PVC is gone (past the point of no return).
func Abort(ctx context.Context, c *longhorn.Client, l *Leftovers, log func(string, ...any)) error {
	if l.Problem != "" {
		return fmt.Errorf("%s", l.Problem)
	}
	dyn := c.Dynamic()
	for _, p := range l.Pods {
		ns, n, _ := strings.Cut(p, "/")
		if err := dyn.Resource(gvrPod).Namespace(ns).Delete(ctx, n, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("delete pod %s: %w", p, err)
		}
		for i := 0; i < 100; i++ {
			if _, err := dyn.Resource(gvrPod).Namespace(ns).Get(ctx, n, metav1.GetOptions{}); apierrors.IsNotFound(err) {
				break
			}
			time.Sleep(3 * time.Second)
		}
		log("✔ pod %s deleted", p)
	}
	if l.TempPVC != "" {
		if err := dyn.Resource(gvrPVC).Namespace(l.Namespace).Delete(ctx, l.TempPVC, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("delete PVC %s: %w", l.TempPVC, err)
		}
		log("✔ PVC %s/%s deleted", l.Namespace, l.TempPVC)
	}
	for _, pv := range l.TempPVs {
		if err := dyn.Resource(gvrPV).Delete(ctx, pv, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("delete PV %s: %w", pv, err)
		}
		log("✔ PV %s deleted", pv)
	}
	for _, v := range l.NewVolumes {
		u := types.UID(v.UID)
		err := dyn.Resource(longhorn.GVRVolume).Namespace(c.Namespace).Delete(ctx, v.Name, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &u}})
		if err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("delete volume %s: %w", v.Name, err)
		}
		log("✔ volume %s deleted", v.Name)
	}
	for _, f := range l.Freezes {
		if err := switchover.Unfreeze(ctx, c, f, switchover.FreezeOptions{Log: log}); err != nil {
			return fmt.Errorf("unfreeze %s: %w", f.Name, err)
		}
	}
	return nil
}

// LeftoverChecks (lhtx doctor): volumes made by a shrink that no PVC uses
// – an interrupted shrink left them. vols: the Longhorn volumes.
func LeftoverChecks(vols []*longhorn.Volume) []longhorn.Finding {
	var out []longhorn.Finding
	byName := map[string]*longhorn.Volume{}
	for _, v := range vols {
		byName[v.Name] = v
	}
	for _, v := range vols {
		of := v.Annotations[AnnShrinkOf]
		pvc := v.BoundPVC()
		_, claim, _ := strings.Cut(pvc, "/")
		if of == "" || (pvc != "" && !strings.HasPrefix(claim, "lhtx-shrink-")) {
			continue // not made by a shrink, or in use by its PVC: done
		}
		hint := "lhtx --rw shrink --abort NAMESPACE/PVC (or delete the volume)"
		if old := byName[of]; old != nil && old.BoundPVC() != "" {
			hint = "lhtx --rw shrink --abort " + old.BoundPVC()
		}
		out = append(out, longhorn.Finding{Severity: longhorn.SevWarning, Check: "shrink", Object: "volume/" + v.Name,
			Message: fmt.Sprintf("made by a shrink of %s and not in use by an app – a shrink is running or was interrupted", of), Hint: hint})
	}
	return out
}
