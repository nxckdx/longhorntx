// Package shrink makes a Longhorn volume smaller – something neither
// Longhorn nor Kubernetes can do. It creates a new, smaller volume with the
// same settings (and the same key, if encrypted), freezes the PVC like a
// switch, copies the files with `lhtx copy` in a pod running lhtx's own
// image, verifies the copy, and swaps the PVC to the new volume. Until the
// old PVC is deleted every failure rolls back (the app starts again on the
// old volume); the old volume is kept either way as the way back.
package shrink

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"

	"github.com/nxckdx/longhorntx/internal/copytree"
	"github.com/nxckdx/longhorntx/internal/longhorn"
	"github.com/nxckdx/longhorntx/internal/switchover"
)

var (
	gvrPod = schema.GroupVersionResource{Version: "v1", Resource: "pods"}
	gvrPVC = schema.GroupVersionResource{Version: "v1", Resource: "persistentvolumeclaims"}
	gvrPV  = schema.GroupVersionResource{Version: "v1", Resource: "persistentvolumes"}
	gvrNS  = schema.GroupVersionResource{Version: "v1", Resource: "namespaces"}
)

// LabelCopy marks the copy pod (the freeze lets it mount the PVC).
const LabelCopy = "lhtx.io/shrink"

// Headroom: the new size must leave this much of it free after the copy.
const Headroom = 0.10

// Plan is everything shrink will do, decided before anything changes.
type Plan struct {
	Switch    *switchover.Plan `json:"switch"`
	OldSize   int64            `json:"oldSize"`
	NewSize   int64            `json:"newSize"`
	Used      int64            `json:"used"` // bytes used in the filesystem; -1 = unknown
	UsedFrom  string           `json:"usedFrom,omitempty"`
	TempPVC   string           `json:"tempPVC"`
	TempPV    string           `json:"tempPV"`
	CopyPod   string           `json:"copyPod"`
	Image     string           `json:"image"`
	Volume    map[string]any   `json:"volume"` // the new Longhorn volume (spec)
	VolLabels map[string]any   `json:"volumeLabels,omitempty"`
	Blockers  []string         `json:"blockers,omitempty"`
	Warnings  []string         `json:"warnings,omitempty"`
}

func (p *Plan) block(f string, a ...any) { p.Blockers = append(p.Blockers, fmt.Sprintf(f, a...)) }
func (p *Plan) warn(f string, a ...any)  { p.Warnings = append(p.Warnings, fmt.Sprintf(f, a...)) }

// shrinkName: "<pvc>-shrink-<MMDD>", shortened to Longhorn's limit.
func shrinkName(pvc string, now time.Time) string {
	suffix := "-shrink-" + now.Format("0102")
	base := pvc
	if max := longhorn.MaxVolumeName - len(suffix); len(base) > max {
		base = strings.TrimRight(base[:max], "-")
	}
	return base + suffix
}

// copied from the old volume's spec to the new one: everything that is a
// setting, nothing that is state
var keepSpec = []string{
	"numberOfReplicas", "dataLocality", "accessMode", "encrypted", "nodeSelector", "diskSelector",
	"replicaAutoBalance", "replicaSoftAntiAffinity", "replicaZoneSoftAntiAffinity", "replicaDiskSoftAntiAffinity",
	"revisionCounterDisabled", "unmapMarkSnapChainRemoved", "snapshotDataIntegrity", "backupCompressionMethod",
	"dataEngine", "frontend", "staleReplicaTimeout", "offlineRebuilding", "freezeFilesystemForSnapshot",
	"backupTargetName", "snapshotMaxCount", "snapshotMaxSize", "rebuildConcurrentSyncLimit",
}

// Build plans the shrink of namespace/pvc to size bytes. Nothing changes.
func Build(ctx context.Context, c *longhorn.Client, namespace, pvc string, size int64, image string, now time.Time) (*Plan, error) {
	name := shrinkName(pvc, now)
	sw, err := switchover.BuildPending(ctx, c, namespace, pvc, name, size, false)
	if err != nil {
		return nil, err
	}
	p := &Plan{Switch: sw, NewSize: size, Used: -1, Image: image,
		TempPVC: "lhtx-shrink-" + pvc, TempPV: "lhtx-shrink-" + name}
	if len(p.TempPVC) > 63 {
		p.TempPVC = strings.TrimRight(p.TempPVC[:63], "-")
	}
	h := sha256.Sum256([]byte(namespace + "/" + pvc + now.String()))
	p.CopyPod = "lhtx-shrink-copy-" + hex.EncodeToString(h[:])[:8]
	// BuildPending compared encryption with "false": that check is about
	// restores, not this – drop its blocker, the new volume follows the old
	sw.Blockers = without(sw.Blockers, "encrypted")
	sw.Request = size
	sw.Freeze.AllowLabel = LabelCopy
	sw.Freeze.Reason = fmt.Sprintf("shrink %s to %s", sw.PVCRef(), human(size))

	old, err := c.Dynamic().Resource(longhorn.GVRVolume).Namespace(c.Namespace).Get(ctx, sw.OldVolume, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("volume %s: %w", sw.OldVolume, err)
	}
	ov, err := longhorn.ConvertVolume(old)
	if err != nil {
		return nil, err
	}
	p.OldSize = int64(ov.Spec.Size)
	spec, _, _ := unstructured.NestedMap(old.Object, "spec")
	p.Volume = map[string]any{"size": strconv.FormatInt(size, 10)}
	for _, k := range keepSpec {
		if v, ok := spec[k]; ok {
			p.Volume[k] = v
		}
	}
	p.VolLabels = map[string]any{}
	for k, v := range old.GetLabels() { // recurring jobs and groups: backups go on
		if strings.HasPrefix(k, "recurring-job.longhorn.io/") || strings.HasPrefix(k, "recurring-job-group.longhorn.io/") {
			p.VolLabels[k] = v
		}
	}

	// checks
	switch {
	case sw.VolumeMode == "Block":
		p.block("the PVC is a raw block device – there is no filesystem to copy files from")
	case ov.Spec.AccessMode == "rwx":
		p.block("RWX volumes are not supported by lhtx shrink yet")
	case ov.Spec.DataEngine == "v2":
		p.block("v2 data engine volumes are not supported")
	case size >= p.OldSize:
		p.block("%s is not smaller than the volume (%s) – to grow it use lhtx expand", human(size), human(p.OldSize))
	case size < 64<<20:
		p.block("%s is too small", human(size))
	}
	if exists(ctx, c, longhorn.GVRVolume, c.Namespace, name) {
		p.block("volume %s exists already (a shrink today?) – delete it or wait a day", name)
	}
	if exists(ctx, c, gvrPVC, namespace, p.TempPVC) {
		p.block("PVC %s/%s exists already (left over from a shrink?) – delete it first", namespace, p.TempPVC)
	}
	if ns, err := c.Dynamic().Resource(gvrNS).Get(ctx, namespace, metav1.GetOptions{}); err == nil {
		if ns.GetLabels()["pod-security.kubernetes.io/enforce"] == "restricted" {
			p.block("namespace %s enforces the \"restricted\" Pod Security level: the copy pod must run as root to keep file owners", namespace)
		}
	}
	if sw.Freeze != nil && len(sw.Freeze.Pods) == 0 {
		p.warn("no pod mounts the PVC right now")
	}

	// how much is used: kubelet knows (needs nodes/proxy); else the copy
	// pod checks it before copying
	if node := ov.Status.CurrentNodeID; node != "" && ov.Status.State == "attached" {
		if u, err := c.VolumeUsage(ctx, node, namespace, pvc); err == nil {
			p.Used, p.UsedFrom = u, "kubelet on "+node
		}
	}
	if p.Used >= 0 {
		if need := int64(float64(p.Used) / (1 - Headroom)); size < need {
			p.block("%s used – the new size must be at least %s (%d %% kept free)", human(p.Used), human(need), int(Headroom*100))
		}
	} else {
		p.warn("how much of the volume is used is not known here (kubelet's stats need nodes/proxy or the volume is not mounted) – the copy pod checks it first and stops (rollback) if it does not fit")
	}
	p.Blockers = append(p.Blockers, sw.Blockers...)
	p.Warnings = append(p.Warnings, sw.Warnings...)
	if owners := sw.Freeze.Owners; len(owners) > 0 {
		for _, o := range owners {
			if o.Kind == "StatefulSet" {
				p.warn("StatefulSet %s: its volumeClaimTemplate still asks for the old size – only new replicas use it", o.Name)
			}
		}
	}
	if gitops := sw.Labels["kustomize.toolkit.fluxcd.io/name"] + sw.Labels["helm.toolkit.fluxcd.io/name"] + sw.Labels["app.kubernetes.io/managed-by"]; gitops != "" {
		p.warn("the PVC is managed (%s): change its size in Git/Helm to %s first – otherwise the next sync asks for the old size again and Longhorn expands the volume back", gitops, human(size))
	}
	return p, nil
}

func without(ss []string, sub string) []string {
	var out []string
	for _, s := range ss {
		if !strings.Contains(s, sub) {
			out = append(out, s)
		}
	}
	return out
}

func exists(ctx context.Context, c *longhorn.Client, g schema.GroupVersionResource, ns, name string) bool {
	_, err := c.Dynamic().Resource(g).Namespace(ns).Get(ctx, name, metav1.GetOptions{})
	return err == nil
}

func human(n int64) string { return resource.NewQuantity(n, resource.BinarySI).String() }

// Steps for the preview.
func (p *Plan) Steps() []string {
	sw := p.Switch
	enc := ""
	if p.Volume["encrypted"] == true {
		enc = ", encrypted with the same key"
	}
	out := []string{
		fmt.Sprintf("create volume %s (%s instead of %s, same settings%s) with PV %s and PVC %s/%s",
			sw.NewVolume, human(p.NewSize), human(p.OldSize), enc, p.TempPV, sw.Namespace, p.TempPVC),
	}
	for _, s := range sw.Steps() {
		if strings.HasPrefix(s, "wait until volume "+sw.NewVolume) {
			continue
		}
		out = append(out, s)
		if strings.HasPrefix(s, "wait until volume "+sw.OldVolume+" is detached") {
			out = append(out,
				fmt.Sprintf("copy pod %s (image %s, root): copy all files from %s to the new volume, then compare both trees", p.CopyPod, p.Image, sw.PVCRef()),
				fmt.Sprintf("delete the copy pod, PVC %s and PV %s (the new volume stays)", p.TempPVC, p.TempPV))
		}
	}
	return out
}

// Options: Log gets progress lines; Wait bounds waits for pods/PVCs.
type Options struct {
	Log     func(format string, a ...any)
	Journal string
	Wait    time.Duration
	Poll    time.Duration
}

// Run carries out the plan.
func Run(ctx context.Context, c *longhorn.Client, p *Plan, o Options) error {
	if len(p.Blockers) > 0 {
		return fmt.Errorf("shrink not possible: %s", strings.Join(p.Blockers, "; "))
	}
	if o.Wait <= 0 {
		o.Wait = 10 * time.Minute
	}
	if o.Poll <= 0 {
		o.Poll = 3 * time.Second
	}
	r := &runner{c: c, p: p, o: o, ctx: ctx}
	if err := r.createTarget(); err != nil {
		r.cleanup(true)
		return err
	}
	err := switchover.Run(ctx, c, p.Switch, switchover.Options{
		Log: o.Log, Journal: o.Journal, Wait: o.Wait, Poll: o.Poll,
		AfterFreeze: r.copy,
	})
	if err != nil && !r.swapped(ctx) {
		r.cleanup(true)
	}
	return err
}

type runner struct {
	c      *longhorn.Client
	p      *Plan
	o      Options
	ctx    context.Context
	volUID string
}

func (r *runner) createTarget() error {
	sw, p := r.p.Switch, r.p
	dyn := r.c.Dynamic()
	vol := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": longhorn.Group + "/" + longhorn.Version, "kind": "Volume",
		"metadata": map[string]any{"name": sw.NewVolume, "namespace": r.c.Namespace, "labels": p.VolLabels,
			"annotations": map[string]any{"lhtx.io/shrink-of": sw.OldVolume}},
		"spec": p.Volume,
	}}
	created, err := dyn.Resource(longhorn.GVRVolume).Namespace(r.c.Namespace).Create(r.ctx, vol, metav1.CreateOptions{FieldManager: "lhtx"})
	if err != nil {
		return fmt.Errorf("create volume %s: %w", sw.NewVolume, err)
	}
	r.volUID = string(created.GetUID())
	r.o.Log("volume %s created (%s)", sw.NewVolume, human(p.NewSize))

	attrs := map[string]any{}
	for k, v := range sw.PVAttributes {
		attrs[k] = v
	}
	csi := map[string]any{"driver": "driver.longhorn.io", "fsType": sw.FSType, "volumeHandle": sw.NewVolume, "volumeAttributes": attrs}
	for k, v := range sw.SecretRefs {
		csi[k] = v
	}
	pv := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": "PersistentVolume",
		"metadata": map[string]any{"name": p.TempPV, "labels": map[string]any{LabelCopy: "target"}},
		"spec": map[string]any{
			"capacity":                      map[string]any{"storage": human(p.NewSize)},
			"accessModes":                   []any{"ReadWriteOnce"},
			"volumeMode":                    "Filesystem",
			"persistentVolumeReclaimPolicy": "Retain",
			"storageClassName":              sw.StorageClass,
			"claimRef":                      map[string]any{"apiVersion": "v1", "kind": "PersistentVolumeClaim", "namespace": sw.Namespace, "name": p.TempPVC},
			"csi":                           csi,
		},
	}}
	if _, err := dyn.Resource(gvrPV).Create(r.ctx, pv, metav1.CreateOptions{FieldManager: "lhtx"}); err != nil {
		return fmt.Errorf("create PV %s: %w", p.TempPV, err)
	}
	pvc := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": "PersistentVolumeClaim",
		"metadata": map[string]any{"name": p.TempPVC, "namespace": sw.Namespace, "labels": map[string]any{LabelCopy: "target"}},
		"spec": map[string]any{
			"accessModes":      []any{"ReadWriteOnce"},
			"storageClassName": sw.StorageClass,
			"volumeMode":       "Filesystem",
			"volumeName":       p.TempPV,
			"resources":        map[string]any{"requests": map[string]any{"storage": human(p.NewSize)}},
		},
	}}
	if _, err := dyn.Resource(gvrPVC).Namespace(sw.Namespace).Create(r.ctx, pvc, metav1.CreateOptions{FieldManager: "lhtx"}); err != nil {
		return fmt.Errorf("create PVC %s: %w", p.TempPVC, err)
	}
	r.o.Log("PV %s and PVC %s/%s created", p.TempPV, sw.Namespace, p.TempPVC)
	return nil
}

// copy runs while the PVC is frozen and its volume detached.
func (r *runner) copy(ctx context.Context) error {
	sw, p := r.p.Switch, r.p
	dyn := r.c.Dynamic()
	root := int64(0)
	pod := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": "Pod",
		"metadata": map[string]any{"name": p.CopyPod, "namespace": sw.Namespace,
			"labels": map[string]any{LabelCopy: "copy", "app.kubernetes.io/managed-by": "lhtx"}},
		"spec": map[string]any{
			"restartPolicy":                 "Never",
			"terminationGracePeriodSeconds": int64(5),
			"securityContext":               map[string]any{"runAsUser": root, "runAsGroup": root},
			"containers": []any{map[string]any{
				"name": "copy", "image": p.Image, "imagePullPolicy": "IfNotPresent",
				"args": []any{"copy", "/src", "/dst"},
				"volumeMounts": []any{
					map[string]any{"name": "src", "mountPath": "/src", "readOnly": true},
					map[string]any{"name": "dst", "mountPath": "/dst"},
				},
				"resources": map[string]any{"requests": map[string]any{"cpu": "100m", "memory": "64Mi"}},
			}},
			"volumes": []any{
				map[string]any{"name": "src", "persistentVolumeClaim": map[string]any{"claimName": sw.PVC, "readOnly": true}},
				map[string]any{"name": "dst", "persistentVolumeClaim": map[string]any{"claimName": p.TempPVC}},
			},
		},
	}}
	if _, err := dyn.Resource(gvrPod).Namespace(sw.Namespace).Create(ctx, pod, metav1.CreateOptions{FieldManager: "lhtx"}); err != nil {
		return fmt.Errorf("start copy pod: %w", err)
	}
	r.o.Log("copy pod %s started", p.CopyPod)
	rep, err := r.followCopy(ctx)
	r.deletePod(context.WithoutCancel(ctx))
	if err != nil {
		return err
	}
	r.o.Log("copied and verified: %d files, %d directories, %d links, %s in %s",
		rep.Stats.Files, rep.Stats.Dirs, rep.Stats.Symlinks+rep.Stats.Hardlinks, human(rep.Stats.Bytes), (time.Duration(rep.Seconds) * time.Second).String())
	// the temporary PVC and PV go; the switch binds the PVC to the volume
	if err := r.deleteTemp(ctx); err != nil {
		return err
	}
	return r.waitDetached(ctx)
}

// copyReport mirrors cli.CopyReport (the copy pod's JSON lines).
type copyReport struct {
	Phase   string         `json:"phase"`
	Stats   copytree.Stats `json:"stats"`
	Used    int64          `json:"used"`
	Free    int64          `json:"free"`
	Diff    []string       `json:"diff"`
	Error   string         `json:"error"`
	Seconds float64        `json:"seconds"`
}

func (r *runner) followCopy(ctx context.Context) (*copyReport, error) {
	sw, p := r.p.Switch, r.p
	start := time.Now()
	lastLog := time.Time{}
	var last copyReport
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(r.o.Poll):
		}
		po, err := r.c.Dynamic().Resource(gvrPod).Namespace(sw.Namespace).Get(ctx, p.CopyPod, metav1.GetOptions{})
		if err != nil {
			return nil, fmt.Errorf("copy pod: %w", err)
		}
		phase, _, _ := unstructured.NestedString(po.Object, "status", "phase")
		if phase == "Running" || phase == "Succeeded" || phase == "Failed" {
			if line, err := r.c.PodLogTail(ctx, sw.Namespace, p.CopyPod, 2); err == nil {
				for _, l := range strings.Split(strings.TrimSpace(line), "\n") {
					var x copyReport
					if json.Unmarshal([]byte(l), &x) == nil && x.Phase != "" {
						last = x
					}
				}
			}
		}
		switch phase {
		case "Succeeded":
			if last.Phase != "done" {
				return nil, fmt.Errorf("the copy pod ended without reporting success")
			}
			return &last, nil
		case "Failed":
			msg := last.Error
			if msg == "" {
				msg = "the copy pod failed (kubectl -n " + sw.Namespace + " logs " + p.CopyPod + ")"
			}
			if len(last.Diff) > 0 {
				msg += ": " + strings.Join(last.Diff, "; ")
			}
			return nil, fmt.Errorf("copy: %s", msg)
		case "Pending":
			if time.Since(start) > r.o.Wait {
				return nil, fmt.Errorf("the copy pod did not start within %s (%s)", r.o.Wait, waitingReason(po))
			}
		}
		if time.Since(lastLog) > 15*time.Second {
			lastLog = time.Now()
			switch {
			case phase == "Pending":
				r.o.Log("… copy pod %s: starting (%s)", p.CopyPod, waitingReason(po))
			case last.Phase == "check" && last.Used > 0:
				r.o.Log("… %s used, %s free in the new volume – copying", human(last.Used), human(last.Free))
			case last.Phase == "copy":
				pct := ""
				if last.Used > 0 {
					pct = fmt.Sprintf(" (~%d %%)", min(99, last.Stats.Bytes*100/last.Used))
				}
				r.o.Log("… copying: %d files, %s%s", last.Stats.Files, human(last.Stats.Bytes), pct)
			case last.Phase == "verify":
				r.o.Log("… comparing both trees")
			}
		}
	}
}

func waitingReason(po *unstructured.Unstructured) string {
	cs, _, _ := unstructured.NestedSlice(po.Object, "status", "containerStatuses")
	for _, x := range cs {
		if m, ok := x.(map[string]any); ok {
			if w, _, _ := unstructured.NestedString(m, "state", "waiting", "reason"); w != "" {
				return w
			}
		}
	}
	return "scheduling / attaching volumes"
}

func (r *runner) deletePod(ctx context.Context) {
	sw, p := r.p.Switch, r.p
	res := r.c.Dynamic().Resource(gvrPod).Namespace(sw.Namespace)
	if err := res.Delete(ctx, p.CopyPod, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		r.o.Log("delete copy pod: %v", err)
		return
	}
	deadline := time.Now().Add(r.o.Wait)
	for time.Now().Before(deadline) {
		if _, err := res.Get(ctx, p.CopyPod, metav1.GetOptions{}); apierrors.IsNotFound(err) {
			return
		}
		time.Sleep(r.o.Poll)
	}
}

func (r *runner) deleteTemp(ctx context.Context) error {
	sw, p := r.p.Switch, r.p
	dyn := r.c.Dynamic()
	if err := dyn.Resource(gvrPVC).Namespace(sw.Namespace).Delete(ctx, p.TempPVC, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete PVC %s: %w", p.TempPVC, err)
	}
	deadline := time.Now().Add(r.o.Wait)
	for {
		_, err := dyn.Resource(gvrPVC).Namespace(sw.Namespace).Get(ctx, p.TempPVC, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			break
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("PVC %s is not gone after %s", p.TempPVC, r.o.Wait)
		}
		time.Sleep(r.o.Poll)
	}
	if err := dyn.Resource(gvrPV).Delete(ctx, p.TempPV, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete PV %s: %w", p.TempPV, err)
	}
	for {
		_, err := dyn.Resource(gvrPV).Get(ctx, p.TempPV, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			r.o.Log("PVC %s and PV %s deleted (volume %s stays)", p.TempPVC, p.TempPV, sw.NewVolume)
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("PV %s is not gone after %s", p.TempPV, r.o.Wait)
		}
		time.Sleep(r.o.Poll)
	}
}

// waitDetached: both volumes free again before the PVC is swapped.
func (r *runner) waitDetached(ctx context.Context) error {
	sw := r.p.Switch
	deadline := time.Now().Add(r.o.Wait)
	for {
		ok := true
		for _, name := range []string{sw.OldVolume, sw.NewVolume} {
			v, err := r.c.Volume(ctx, name)
			if err != nil || v.Status.State != "detached" {
				ok = false
			}
		}
		if ok {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("volumes %s / %s are not detached after %s", sw.OldVolume, sw.NewVolume, r.o.Wait)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(r.o.Poll):
		}
	}
}

// swapped: the PVC uses the new volume (the switch got past its point of
// no return) – then nothing of the target may be cleaned up.
func (r *runner) swapped(ctx context.Context) bool {
	sw := r.p.Switch
	pvc, err := r.c.Dynamic().Resource(gvrPVC).Namespace(sw.Namespace).Get(context.WithoutCancel(ctx), sw.PVC, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return true // deleted: past the point of no return
	}
	if err != nil {
		return true // unknown: do not touch anything
	}
	vn, _, _ := unstructured.NestedString(pvc.Object, "spec", "volumeName")
	return vn != sw.OldPV
}

// cleanup after a failure before the swap: the copy pod, the temporary
// PVC/PV and the new volume go (the old volume was never changed).
func (r *runner) cleanup(deleteVolume bool) {
	ctx := context.WithoutCancel(r.ctx)
	sw, p := r.p.Switch, r.p
	r.deletePod(ctx)
	_ = r.c.Dynamic().Resource(gvrPVC).Namespace(sw.Namespace).Delete(ctx, p.TempPVC, metav1.DeleteOptions{})
	_ = r.c.Dynamic().Resource(gvrPV).Delete(ctx, p.TempPV, metav1.DeleteOptions{})
	if deleteVolume && r.volUID != "" {
		u := types.UID(r.volUID)
		err := r.c.Dynamic().Resource(longhorn.GVRVolume).Namespace(r.c.Namespace).Delete(ctx, sw.NewVolume,
			metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &u}})
		if err == nil || apierrors.IsNotFound(err) {
			r.o.Log("cleaned up: copy pod, PVC %s, PV %s and volume %s removed; %s is unchanged", p.TempPVC, p.TempPV, sw.NewVolume, sw.OldVolume)
			return
		}
		r.o.Log("cleanup: delete volume %s: %v", sw.NewVolume, err)
	}
}
