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
	"github.com/nxckdx/longhorntx/internal/format"
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

// AnnAccessMode: on an RWX volume a shrink set to rwo for its copy – the
// access mode to set back (lhtx shrink --abort does, doctor warns).
const AnnAccessMode = "lhtx.io/shrink-access-mode"

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
	// Node: where the probe and copy pods run (the old volume's node, the
	// node of its replica for strict-local; "" = the scheduler picks for
	// the probe and the copy follows it). NodeWhy says why.
	Node        string   `json:"node,omitempty"`
	NodeWhy     string   `json:"nodeWhy,omitempty"`
	Tolerations []any    `json:"tolerations,omitempty"` // of the app's pod: its node may be tainted
	PullSecrets []string `json:"pullSecrets,omitempty"`
	ProbePod    string   `json:"probePod"`
	PrecopyPod  string   `json:"precopyPod"`
	// Precopy: copy while the app still runs, then – frozen – only what
	// changed (off for ReadWriteOncePod PVCs: no second pod may mount them).
	Precopy bool `json:"precopy"`
	// RWX: the old volume is shared over NFS. Its pre-copy reads NFS
	// (no xattrs); frozen, the old volume is set to rwo for the copy and
	// read as a block device through SrcPV/SrcPVC, then set back. The new
	// volume is made rwo and set to rwx after the copy.
	RWX      bool     `json:"rwx,omitempty"`
	SrcPVC   string   `json:"srcPVC,omitempty"`
	SrcPV    string   `json:"srcPV,omitempty"`
	Blockers []string `json:"blockers,omitempty"`
	Warnings []string `json:"warnings,omitempty"`
}

// BuildOptions: the copy pod's image and pull secrets.
type BuildOptions struct {
	Image       string
	PullSecrets []string
	NoPrecopy   bool // copy everything while the app is down
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
func Build(ctx context.Context, c *longhorn.Client, namespace, pvc string, size int64, bo BuildOptions, now time.Time) (*Plan, error) {
	name := shrinkName(pvc, now)
	for i := 2; i <= 9 && exists(ctx, c, longhorn.GVRVolume, c.Namespace, name); i++ {
		name = shrinkName(pvc, now) + "-" + strconv.Itoa(i) // another shrink today
	}
	sw, err := switchover.BuildPending(ctx, c, namespace, pvc, name, size, false)
	if err != nil {
		return nil, err
	}
	size = (size + 2<<20 - 1) &^ (2<<20 - 1) // Longhorn sizes are multiples of 2 MiB
	sw.NewSize = size
	p := &Plan{Switch: sw, NewSize: size, Used: -1, Image: bo.Image, PullSecrets: bo.PullSecrets,
		TempPVC: tempPVCName(pvc), TempPV: "lhtx-shrink-" + name}
	h := sha256.Sum256([]byte(namespace + "/" + pvc + now.String()))
	p.CopyPod = "lhtx-shrink-copy-" + hex.EncodeToString(h[:])[:8]
	p.ProbePod = "lhtx-shrink-probe-" + hex.EncodeToString(h[:])[:8]
	p.PrecopyPod = "lhtx-shrink-pre-" + hex.EncodeToString(h[:])[:8]
	p.Precopy = !bo.NoPrecopy
	for _, m := range sw.AccessModes {
		if m == "ReadWriteOncePod" && p.Precopy {
			p.Precopy = false
			p.warn("the PVC is ReadWriteOncePod: no second pod may mount it while the app runs – no pre-copy, everything is copied while the app is down")
		}
	}
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
	if ov.Spec.AccessMode == "rwx" {
		p.RWX = true
		p.SrcPVC, p.SrcPV = srcPVCName(pvc), "lhtx-shrink-src-"+name
	}
	spec, _, _ := unstructured.NestedMap(old.Object, "spec")
	p.Volume = map[string]any{"size": strconv.FormatInt(size, 10)}
	for _, k := range keepSpec {
		if v, ok := spec[k]; ok {
			p.Volume[k] = v
		}
	}
	if p.RWX {
		p.Volume["accessMode"] = "rwo" // the copy writes the block device; rwx after it
	}
	p.VolLabels = map[string]any{}
	for k, v := range old.GetLabels() { // recurring jobs and groups: backups go on
		if strings.HasPrefix(k, "recurring-job.longhorn.io/") || strings.HasPrefix(k, "recurring-job-group.longhorn.io/") {
			p.VolLabels[k] = v
		}
	}

	// where the pods run: an old volume with data locality strict-local
	// can only attach where its replica is; otherwise where the app ran
	if ov.Spec.DataLocality == "strict-local" {
		if rs, err := c.Replicas(ctx, ov.Name); err == nil && len(rs) > 0 {
			p.Node, p.NodeWhy = rs[0].Spec.NodeID, "the old volume is strict-local: only its replica's node can attach it"
		}
		if p.Node == "" {
			p.block("the old volume is strict-local but its replica's node is not known")
		}
	} else if ov.Status.CurrentNodeID != "" {
		p.Node, p.NodeWhy = ov.Status.CurrentNodeID, "where the app runs now"
	}
	podNode := ""
	if sw.Freeze != nil && len(sw.Freeze.Pods) > 0 {
		pns, pname, _ := strings.Cut(sw.Freeze.Pods[0], "/")
		if po, err := c.Dynamic().Resource(gvrPod).Namespace(pns).Get(ctx, pname, metav1.GetOptions{}); err == nil {
			p.Tolerations, _, _ = unstructured.NestedSlice(po.Object, "spec", "tolerations")
			podNode, _, _ = unstructured.NestedString(po.Object, "spec", "nodeName")
		}
	}
	if ov.Status.LastBackup != "" || len(p.VolLabels) > 0 {
		p.warn("backups of the new volume start a new chain under its own name (the first one is a full backup); the old volume's backups stay restorable (lhtx backupvolumes)")
	}
	p.warn("snapshots of the old volume do not move – they stay with the old volume")

	// checks
	switch {
	case sw.VolumeMode == "Block":
		p.block("the PVC is a raw block device – there is no filesystem to copy files from")
	case ov.Spec.DataEngine == "v2":
		p.block("v2 data engine volumes are not supported")
	case size >= p.OldSize:
		p.block("%s is not smaller than the volume (%s) – to grow it use lhtx expand", human(size), human(p.OldSize))
	case size < 64<<20:
		p.block("%s is too small", human(size))
	}
	if exists(ctx, c, longhorn.GVRVolume, c.Namespace, name) {
		p.block("volume %s exists already (nine shrinks today?) – delete old ones first", name)
	}
	if mode := ov.Annotations[AnnAccessMode]; mode != "" {
		p.block("volume %s was set to %s by an interrupted shrink (it is %s normally): lhtx --rw shrink --abort %s/%s", ov.Name, ov.Spec.AccessMode, mode, namespace, pvc)
	}
	if p.RWX {
		p.warn("RWX: the pre-copy reads over NFS while the app runs; frozen, the old volume is set to rwo for the copy (read as a block device: xattrs, ACLs, capabilities) and back to rwx – if lhtx stops in between, lhtx --rw shrink --abort %s/%s sets it back", namespace, pvc)
		if p.SrcPVC != "" && exists(ctx, c, gvrPVC, namespace, p.SrcPVC) {
			p.block("PVC %s/%s exists already – a shrink is running or was interrupted: lhtx --rw shrink --abort %s/%s", namespace, p.SrcPVC, namespace, pvc)
		}
	}
	if exists(ctx, c, gvrPVC, namespace, p.TempPVC) {
		p.block("PVC %s/%s exists already – a shrink is running or was interrupted: lhtx --rw shrink --abort %s/%s", namespace, p.TempPVC, namespace, pvc)
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
	// (an RWX volume is attached to its share manager's node; kubelet
	// knows the usage where an app pod mounts it)
	usageNode := ov.Status.CurrentNodeID
	if p.RWX || usageNode == "" {
		usageNode = podNode
	}
	if node := usageNode; node != "" && ov.Status.State == "attached" {
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
	for _, w := range without(sw.Warnings, "older than an expansion") {
		if strings.HasPrefix(w, "encrypted: the new PV uses the same key Secret") {
			w = strings.SplitN(w, " – ", 2)[0] + " – the new volume is encrypted with it"
		}
		p.Warnings = append(p.Warnings, w)
	}
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
	where := "the node the scheduler picks"
	if p.Node != "" {
		where = "node " + p.Node
	}
	out := []string{
		"server dry run of the copy pod (Pod Security, policies, quotas) – the app still runs",
		fmt.Sprintf("create volume %s (%s instead of %s, same settings%s) with PV %s and PVC %s/%s",
			sw.NewVolume, human(p.NewSize), human(p.OldSize), enc, p.TempPV, sw.Namespace, p.TempPVC),
		fmt.Sprintf("pre-flight pod %s on %s: pulls the image, attaches and formats the new volume, checks that all replicas are placed – the app still runs; a failure removes everything",
			p.ProbePod, where),
	}
	if p.Precopy {
		out = append(out, fmt.Sprintf("pre-copy pod %s on %s: copies everything while the app still runs (mounts the PVC read-only next to it) and checks the content – the app's downtime is only the changes",
			p.PrecopyPod, where))
	}
	for _, s := range sw.Steps() {
		if strings.HasPrefix(s, "wait until volume "+sw.NewVolume) {
			continue
		}
		out = append(out, s)
		if strings.HasPrefix(s, "wait until volume "+sw.OldVolume+" is detached") {
			if p.RWX {
				out = append(out, fmt.Sprintf("set volume %s to rwo (annotation %s=rwx) and create PV %s and PVC %s/%s on it – the copy reads it as a block device",
					sw.OldVolume, AnnAccessMode, p.SrcPV, sw.Namespace, p.SrcPVC))
			}
			out = append(out,
				copyStep(p),
				fmt.Sprintf("delete the copy pod, PVC %s and PV %s (the new volume stays)", p.TempPVC, p.TempPV))
			if p.RWX {
				out = append(out, fmt.Sprintf("delete PVC %s and PV %s, set volume %s back to rwx (on any failure before, too) and the new volume %s to rwx",
					p.SrcPVC, p.SrcPV, sw.OldVolume, sw.NewVolume))
			}
		}
	}
	return out
}

func copyStep(p *Plan) string {
	if p.Precopy {
		return fmt.Sprintf("copy pod %s: copy what changed since the pre-copy (new, changed, removed entries), then compare both trees (content of what was copied again)", p.CopyPod)
	}
	return fmt.Sprintf("copy pod %s (image %s, root): copy all files from %s to the new volume, then compare both trees", p.CopyPod, p.Image, p.Switch.PVCRef())
}

// Options: Log gets progress lines; Wait bounds waits for pods/PVCs.
type Options struct {
	Log     func(format string, a ...any)
	Journal string
	Wait    time.Duration
	Poll    time.Duration
	// FreezeName / Request: as in switchover.Options (the agent names the
	// freeze after its Switch, so it can take it over after a restart).
	FreezeName string
	Request    string
	// Handover: when ctx is cancelled, leave everything as it is (no
	// rollback, no cleanup) – the lhtx agent stopping; the next agent
	// removes the leftovers and starts over.
	Handover bool
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
	if o.Log == nil {
		o.Log = func(string, ...any) {}
	}
	r := &runner{c: c, p: p, o: o, ctx: ctx, podUIDs: map[string]string{}}
	// everything that can fail without the app being down, first
	if err := r.dryRunCopyPod(); err != nil {
		return err
	}
	if err := r.createTarget(); err != nil {
		r.cleanup(true)
		return err
	}
	if err := r.preflight(); err != nil {
		r.cleanup(true)
		return fmt.Errorf("%w (nothing was stopped; the new volume was removed)", err)
	}
	if p.Precopy {
		r.precopy()
		if ctx.Err() != nil {
			if !(o.Handover) {
				r.cleanup(true)
			}
			return fmt.Errorf("%w (nothing was stopped)", ctx.Err())
		}
	}
	err := switchover.Run(ctx, c, p.Switch, switchover.Options{
		Log: o.Log, Journal: o.Journal, Wait: o.Wait, Poll: o.Poll,
		FreezeName: o.FreezeName, Request: o.Request, Handover: o.Handover,
		AfterFreeze: r.copy,
	})
	if err != nil && !r.swapped(ctx) {
		if r.cleanup(true) {
			// the switch says the target stays – here it was removed
			msg := strings.Replace(err.Error(), "volume "+p.Switch.NewVolume+" stays – delete it if not needed",
				"the new volume "+p.Switch.NewVolume+" was removed", 1)
			return fmt.Errorf("%s", msg)
		}
	}
	return err
}

type runner struct {
	c      *longhorn.Client
	p      *Plan
	o      Options
	ctx    context.Context
	volUID string
	node   string // where the probe ran: the copy pod runs there too
	// UIDs of what this run created: deletes never hit a same-named object
	// of another run
	pvUID, pvcUID       string
	srcPVUID, srcPVCUID string
	podUIDs             map[string]string
	oldRWO              bool // the old RWX volume is set to rwo (RWX copy)
}

func uidDelete(uid string) metav1.DeleteOptions {
	if uid == "" {
		return metav1.DeleteOptions{}
	}
	u := types.UID(uid)
	return metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &u}}
}

func (r *runner) createTarget() error {
	sw, p := r.p.Switch, r.p
	dyn := r.c.Dynamic()
	vol := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": longhorn.Group + "/" + longhorn.Version, "kind": "Volume",
		"metadata": map[string]any{"name": sw.NewVolume, "namespace": r.c.Namespace, "labels": p.VolLabels,
			"annotations": map[string]any{AnnShrinkOf: sw.OldVolume}},
		"spec": p.Volume,
	}}
	created, err := dyn.Resource(longhorn.GVRVolume).Namespace(r.c.Namespace).Create(r.ctx, vol, metav1.CreateOptions{FieldManager: "lhtx"})
	if err != nil {
		return fmt.Errorf("create volume %s: %w", sw.NewVolume, err)
	}
	r.volUID = string(created.GetUID())
	r.o.Log("volume %s created (%s)", sw.NewVolume, human(p.NewSize))

	pvUID, pvcUID, err := r.createClaim(r.ctx, "target", p.TempPV, p.TempPVC, sw.NewVolume, p.NewSize)
	r.pvUID, r.pvcUID = pvUID, pvcUID
	if err != nil {
		return err
	}
	r.o.Log("PV %s and PVC %s/%s created", p.TempPV, sw.Namespace, p.TempPVC)
	return nil
}

// createClaim creates a ReadWriteOnce PV for a Longhorn volume and a PVC
// bound to it (role "target": the new volume; "source": the old RWX volume
// as a block device). It returns the UIDs of what it created.
func (r *runner) createClaim(ctx context.Context, role, pvName, pvcName, volume string, size int64) (string, string, error) {
	sw := r.p.Switch
	dyn := r.c.Dynamic()
	attrs := map[string]any{}
	for k, v := range sw.PVAttributes {
		if k != "share" { // a block device here, whatever the volume is later
			attrs[k] = v
		}
	}
	csi := map[string]any{"driver": "driver.longhorn.io", "fsType": sw.FSType, "volumeHandle": volume, "volumeAttributes": attrs}
	for k, v := range sw.SecretRefs {
		csi[k] = v
	}
	pv := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": "PersistentVolume",
		"metadata": map[string]any{"name": pvName, "labels": map[string]any{LabelCopy: role}},
		"spec": map[string]any{
			"capacity":                      map[string]any{"storage": human(size)},
			"accessModes":                   []any{"ReadWriteOnce"},
			"volumeMode":                    "Filesystem",
			"persistentVolumeReclaimPolicy": "Retain",
			"storageClassName":              sw.StorageClass,
			"claimRef":                      map[string]any{"apiVersion": "v1", "kind": "PersistentVolumeClaim", "namespace": sw.Namespace, "name": pvcName},
			"csi":                           csi,
		},
	}}
	cpv, err := dyn.Resource(gvrPV).Create(ctx, pv, metav1.CreateOptions{FieldManager: "lhtx"})
	if err != nil {
		return "", "", fmt.Errorf("create PV %s: %w", pvName, err)
	}
	pvc := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": "PersistentVolumeClaim",
		"metadata": map[string]any{"name": pvcName, "namespace": sw.Namespace, "labels": map[string]any{LabelCopy: role}},
		"spec": map[string]any{
			"accessModes":      []any{"ReadWriteOnce"},
			"storageClassName": sw.StorageClass,
			"volumeMode":       "Filesystem",
			"volumeName":       pvName,
			"resources":        map[string]any{"requests": map[string]any{"storage": human(size)}},
		},
	}}
	cpvc, err := dyn.Resource(gvrPVC).Namespace(sw.Namespace).Create(ctx, pvc, metav1.CreateOptions{FieldManager: "lhtx"})
	if err != nil {
		return string(cpv.GetUID()), "", fmt.Errorf("create PVC %s: %w", pvcName, err)
	}
	return string(cpv.GetUID()), string(cpvc.GetUID()), nil
}

// podObject is the copy pod (probe=false: mounts the old PVC read-only and
// the new volume, copies) or the pre-flight probe (only the new volume).
// podObject: role "probe" (pre-flight: only the new volume), "pre" (the
// pre-copy, the app still runs) or "copy" (the final copy, frozen).
func (r *runner) podObject(role string) *unstructured.Unstructured {
	sw, p := r.p.Switch, r.p
	root := int64(0)
	mode := "full"
	if p.Precopy {
		mode = "final" // only what changed since the pre-copy (or everything, if it did not finish)
	}
	name, args := p.CopyPod, []any{"copy", "--mode", mode, "/src", "/dst"}
	probe := role == "probe"
	srcClaim := sw.PVC
	if p.RWX && role == "copy" {
		srcClaim = p.SrcPVC // the old volume as a block device (frozen, rwo)
	}
	if role == "pre" {
		name, args = p.PrecopyPod, []any{"copy", "--mode", "pre", "/src", "/dst"}
	}
	mounts := []any{
		map[string]any{"name": "src", "mountPath": "/src", "readOnly": true},
		map[string]any{"name": "dst", "mountPath": "/dst"},
	}
	vols := []any{
		map[string]any{"name": "src", "persistentVolumeClaim": map[string]any{"claimName": srcClaim, "readOnly": true}},
		map[string]any{"name": "dst", "persistentVolumeClaim": map[string]any{"claimName": p.TempPVC}},
	}
	if probe {
		name, args = p.ProbePod, []any{"copy", "--probe", "/dst"}
		mounts, vols = mounts[1:], vols[1:]
	}
	spec := map[string]any{
		"restartPolicy":                 "Never",
		"terminationGracePeriodSeconds": int64(5),
		"securityContext":               map[string]any{"runAsUser": root, "runAsGroup": root},
		"containers": []any{map[string]any{
			"name": "copy", "image": p.Image, "imagePullPolicy": "IfNotPresent",
			"args": args, "volumeMounts": mounts,
			// the content hashes of a tree with millions of files need memory
			"resources": map[string]any{
				"requests": map[string]any{"cpu": "100m", "memory": "64Mi"},
				"limits":   map[string]any{"memory": "2Gi"},
			},
		}},
		"volumes": vols,
	}
	if r.node != "" {
		spec["affinity"] = map[string]any{"nodeAffinity": map[string]any{
			"requiredDuringSchedulingIgnoredDuringExecution": map[string]any{"nodeSelectorTerms": []any{
				map[string]any{"matchFields": []any{map[string]any{"key": "metadata.name", "operator": "In", "values": []any{r.node}}}},
			}}}}
	}
	if len(p.Tolerations) > 0 {
		spec["tolerations"] = p.Tolerations
	}
	if len(p.PullSecrets) > 0 {
		var ps []any
		for _, n := range p.PullSecrets {
			ps = append(ps, map[string]any{"name": n})
		}
		spec["imagePullSecrets"] = ps
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": "Pod",
		"metadata": map[string]any{"name": name, "namespace": sw.Namespace,
			"labels": map[string]any{LabelCopy: role, "app.kubernetes.io/managed-by": "lhtx"}},
		"spec": spec,
	}}
}

// dryRunCopyPod: the API server and every admission check (Pod Security,
// policy engines, quotas, LimitRanges) see the copy pod before anything
// is created. The temporary PVC does not exist yet – admission does not
// look it up.
func (r *runner) dryRunCopyPod() error {
	r.node = r.p.Node
	pod := r.podObject("copy")
	_, err := r.c.Dynamic().Resource(gvrPod).Namespace(r.p.Switch.Namespace).Create(r.ctx, pod,
		metav1.CreateOptions{FieldManager: "lhtx", DryRun: []string{metav1.DryRunAll}})
	if err != nil {
		return fmt.Errorf("the copy pod would be rejected (nothing changed): %w", err)
	}
	r.o.Log("✔ the copy pod passes admission (server dry run)")
	return nil
}

// preflight starts a pod that only mounts the new volume, before the app
// is stopped: the image can be pulled, the pod can be scheduled (on the
// chosen node), Longhorn can place the new replicas and attach the
// volume, the filesystem (and encryption) is created and writable.
func (r *runner) preflight() error {
	sw, p := r.p.Switch, r.p
	res := r.c.Dynamic().Resource(gvrPod).Namespace(sw.Namespace)
	cp, err := res.Create(r.ctx, r.podObject("probe"), metav1.CreateOptions{FieldManager: "lhtx"})
	if err != nil {
		return fmt.Errorf("pre-flight pod: %w", err)
	}
	r.podUIDs[p.ProbePod] = string(cp.GetUID())
	defer r.deleteNamedPod(context.WithoutCancel(r.ctx), p.ProbePod)
	r.o.Log("pre-flight pod %s: pulls the image, mounts and formats the new volume (the app still runs)", p.ProbePod)
	start, lastLog, errs := time.Now(), time.Time{}, 0
	for {
		select {
		case <-r.ctx.Done():
			return r.ctx.Err()
		case <-time.After(r.o.Poll):
		}
		po, err := res.Get(r.ctx, p.ProbePod, metav1.GetOptions{})
		if err != nil {
			if errs++; errs > transientErrors || apierrors.IsNotFound(err) {
				return fmt.Errorf("pre-flight pod: %w", err)
			}
			continue
		}
		errs = 0
		phase, _, _ := unstructured.NestedString(po.Object, "status", "phase")
		switch phase {
		case "Succeeded":
			r.node, _, _ = unstructured.NestedString(po.Object, "spec", "nodeName")
			if err := r.replicasPlaced(); err != nil {
				return err
			}
			r.o.Log("✔ pre-flight passed on node %s: image, scheduling, attach, all replicas placed, the new filesystem works", r.node)
			return nil
		case "Failed":
			msg := "the pre-flight pod failed"
			if line, err := r.c.PodLogTail(r.ctx, sw.Namespace, p.ProbePod, 3); err == nil && strings.TrimSpace(line) != "" {
				msg += ": " + strings.TrimSpace(line)
			}
			return fmt.Errorf("%s", msg)
		}
		if why := hopeless(po); why != "" {
			return fmt.Errorf("the pre-flight pod cannot start – %s", why)
		}
		if time.Since(start) > r.o.Wait {
			return fmt.Errorf("the pre-flight pod did not run within %s (%s) – kubectl -n %s describe pod %s", r.o.Wait, waitingReason(po), sw.Namespace, p.ProbePod)
		}
		if time.Since(lastLog) > 15*time.Second {
			lastLog = time.Now()
			r.o.Log("… pre-flight: %s (%s)", phase, waitingReason(po))
		}
	}
}

// replicasPlaced: Longhorn could place every replica of the new volume
// (it tries when the volume is attached – the pre-flight did that). A
// shrink must not quietly leave the app with less redundancy.
func (r *runner) replicasPlaced() error {
	sw := r.p.Switch
	v, err := r.c.Volume(r.ctx, sw.NewVolume)
	if err != nil {
		return err
	}
	for _, c := range v.Status.Conditions {
		if c.Type == "Scheduled" && c.Status != "True" {
			return fmt.Errorf("Longhorn cannot place all %d replicas of the new volume: %s – free space on the disks (old volumes kept by earlier shrinks/switches?) or lower the replica count first",
				v.Spec.NumberOfReplicas, strings.TrimSpace(c.Reason+" "+c.Message))
		}
	}
	return nil
}

// transientErrors: polls in a row that may fail (API server restart,
// a network blip on the way to it) before lhtx gives up.
const transientErrors = 40

// copy runs while the PVC is frozen and its volume detached.
func (r *runner) copy(ctx context.Context) error {
	p := r.p
	if p.RWX {
		if err := r.toBlock(ctx); err != nil {
			return r.backToRWX(ctx, err)
		}
	}
	rep, err := r.runPod(ctx, "copy")
	if err != nil {
		return r.backToRWX(ctx, err)
	}
	took := (time.Duration(rep.Seconds * float64(time.Second))).Round(100 * time.Millisecond)
	if p.Precopy && rep.Stats.Unchanged > 0 {
		r.o.Log("✔ synced and verified in %s: %d entries kept from the pre-copy, %d copied again (%s), %d removed",
			took, rep.Stats.Unchanged, rep.Changed, format.Bytes(rep.Stats.Bytes), rep.Stats.Removed)
	} else {
		r.o.Log("✔ copied and verified: %d files, %d directories, %d links, %s of data in %s",
			rep.Stats.Files, rep.Stats.Dirs, rep.Stats.Symlinks+rep.Stats.Hardlinks, format.Bytes(rep.Stats.Bytes), took)
	}
	// the temporary PVC and PV go; the switch binds the PVC to the volume
	if err := r.deleteTemp(ctx); err != nil {
		return r.backToRWX(ctx, err)
	}
	if !p.RWX {
		return r.waitDetached(ctx)
	}
	if err := r.backToRWX(ctx, nil); err != nil {
		return err
	}
	if err := r.waitDetached(ctx); err != nil {
		return err
	}
	if err := r.setAccessMode(ctx, p.Switch.NewVolume, "rwx", false); err != nil {
		return fmt.Errorf("set the new volume %s to rwx: %w", p.Switch.NewVolume, err)
	}
	r.o.Log("✔ new volume %s set to rwx", p.Switch.NewVolume)
	return nil
}

// toBlock (RWX, frozen, old volume detached): the old volume is set to rwo
// – an rwx volume is always mounted over NFS, which has no xattrs – and a
// PV/PVC on it lets the copy pod mount it as a block device.
func (r *runner) toBlock(ctx context.Context) error {
	p, sw := r.p, r.p.Switch
	r.oldRWO = true // also if the patch fails: it may have arrived – setting rwx back is harmless
	if err := r.setAccessMode(ctx, sw.OldVolume, "rwo", true); err != nil {
		return fmt.Errorf("set the old volume %s to rwo for the copy: %w", sw.OldVolume, err)
	}
	r.o.Log("✔ old volume %s set to rwo for the copy (annotation %s: set back afterwards)", sw.OldVolume, AnnAccessMode)
	pvUID, pvcUID, err := r.createClaim(ctx, "source", p.SrcPV, p.SrcPVC, sw.OldVolume, p.OldSize)
	r.srcPVUID, r.srcPVCUID = pvUID, pvcUID
	if err != nil {
		return err
	}
	r.o.Log("PV %s and PVC %s/%s created on the old volume", p.SrcPV, sw.Namespace, p.SrcPVC)
	return nil
}

// setAccessMode patches a detached volume's access mode; mark: remember
// the old mode in AnnAccessMode (else the annotation is removed).
func (r *runner) setAccessMode(ctx context.Context, volume, mode string, mark bool) error {
	res := r.c.Dynamic().Resource(longhorn.GVRVolume).Namespace(r.c.Namespace)
	var last error
	for range 5 { // a conflict with longhorn-manager's own updates: read again
		v, err := res.Get(ctx, volume, metav1.GetOptions{})
		if err != nil {
			return err
		}
		if st, _, _ := unstructured.NestedString(v.Object, "status", "state"); st != "detached" {
			return fmt.Errorf("volume %s is %s, not detached", volume, st)
		}
		ann := any(nil)
		if mark {
			cur, _, _ := unstructured.NestedString(v.Object, "spec", "accessMode")
			ann = cur
		}
		patch, _ := json.Marshal(map[string]any{
			"metadata": map[string]any{"resourceVersion": v.GetResourceVersion(), "annotations": map[string]any{AnnAccessMode: ann}},
			"spec":     map[string]any{"accessMode": mode},
		})
		_, last = res.Patch(ctx, volume, types.MergePatchType, patch, metav1.PatchOptions{FieldManager: "lhtx"})
		if last == nil || !apierrors.IsConflict(last) {
			return last
		}
	}
	return last
}

// backToRWX (RWX): the copy pod is gone; delete the source PV/PVC, wait
// for the old volume to detach and set it back to rwx. cause: the error
// that ended the copy (nil: it succeeded). If the old volume cannot be set
// back the PVC stays frozen (KeepFrozen): its pods would mount a volume
// that is not shared any more.
func (r *runner) backToRWX(ctx context.Context, cause error) error {
	if !r.p.RWX || (r.srcPVCUID == "" && r.srcPVUID == "" && !r.oldRWO) {
		return cause
	}
	if r.o.Handover && r.ctx.Err() != nil {
		return cause // the next agent's abort sets it back
	}
	ctx = context.WithoutCancel(ctx)
	p, sw := r.p, r.p.Switch
	err := r.deleteClaim(ctx, p.SrcPVC, p.SrcPV, r.srcPVCUID, r.srcPVUID)
	if err == nil {
		r.srcPVCUID, r.srcPVUID = "", ""
		if r.oldRWO {
			if err = r.waitVolumeDetached(ctx, sw.OldVolume); err == nil {
				err = r.setAccessMode(ctx, sw.OldVolume, "rwx", false)
			}
		}
	}
	if err != nil {
		why := fmt.Errorf("the old volume %s could not be set back to rwx: %w – fix it, then lhtx --rw shrink --abort %s (sets it back, unfreezes)", sw.OldVolume, err, sw.PVCRef())
		if cause != nil {
			why = fmt.Errorf("%w; %w", cause, why)
		}
		return switchover.KeepFrozen(why)
	}
	if r.oldRWO {
		r.oldRWO = false
		r.o.Log("✔ old volume %s set back to rwx", sw.OldVolume)
	}
	return cause
}

func (r *runner) waitVolumeDetached(ctx context.Context, name string) error {
	deadline := time.Now().Add(r.o.Wait)
	for {
		v, err := r.c.Volume(ctx, name)
		if err == nil && v.Status.State == "detached" {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("volume %s is not detached after %s", name, r.o.Wait)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(r.o.Poll):
		}
	}
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
	Changed int64          `json:"changed"`
}

// runPod starts the pre-copy ("pre") or copy ("copy") pod, follows it to
// the end and deletes it.
func (r *runner) runPod(ctx context.Context, role string) (*copyReport, error) {
	sw := r.p.Switch
	pod := r.podObject(role)
	cp, err := r.c.Dynamic().Resource(gvrPod).Namespace(sw.Namespace).Create(ctx, pod, metav1.CreateOptions{FieldManager: "lhtx"})
	if err != nil {
		return nil, fmt.Errorf("start %s pod: %w", role, err)
	}
	r.podUIDs[pod.GetName()] = string(cp.GetUID())
	r.o.Log("%s pod %s started", map[string]string{"pre": "pre-copy", "copy": "copy"}[role], pod.GetName())
	rep, err := r.followCopy(ctx, pod.GetName())
	r.deleteNamedPod(context.WithoutCancel(ctx), pod.GetName())
	return rep, err
}

// precopy copies while the app still runs; the final copy then only takes
// what changed. A failure is not fatal: the final copy copies everything.
func (r *runner) precopy() {
	start := time.Now()
	r.o.Log("pre-copy while the app still runs (it may slow the app's disk down)")
	rep, err := r.runPod(r.ctx, "pre")
	if err != nil {
		if r.ctx.Err() == nil {
			r.o.Log("⚠ the pre-copy failed (%v) – the copy while the app is down takes everything", err)
		}
		return
	}
	r.o.Log("✔ pre-copied %s in %s (%d entries changed meanwhile – the final copy takes them)",
		format.Bytes(rep.Stats.Bytes), time.Since(start).Round(time.Second), rep.Stats.Dirty)
}

func (r *runner) followCopy(ctx context.Context, name string) (*copyReport, error) {
	sw := r.p.Switch
	start := time.Now()
	lastLog := time.Time{}
	errs := 0
	var last copyReport
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(r.o.Poll):
		}
		po, err := r.c.Dynamic().Resource(gvrPod).Namespace(sw.Namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			// the copy goes on in the cluster: a blip must not roll it back
			if errs++; errs > transientErrors || apierrors.IsNotFound(err) {
				return nil, fmt.Errorf("copy pod: %w", err)
			}
			if errs == 1 {
				r.o.Log("… cannot reach the API server (%v) – retrying, the copy goes on in the cluster", err)
			}
			continue
		}
		errs = 0
		phase, _, _ := unstructured.NestedString(po.Object, "status", "phase")
		if phase == "Running" || phase == "Succeeded" || phase == "Failed" {
			if line, err := r.c.PodLogTail(ctx, sw.Namespace, name, 2); err == nil {
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
				msg = "the copy pod failed (kubectl -n " + sw.Namespace + " logs " + name + ")"
			}
			if len(last.Diff) > 0 {
				msg += ": " + strings.Join(last.Diff, "; ")
			}
			return nil, fmt.Errorf("copy: %s", msg)
		case "Pending":
			if why := hopeless(po); why != "" {
				return nil, fmt.Errorf("the copy pod cannot start – %s", why)
			}
			if time.Since(start) > r.o.Wait {
				return nil, fmt.Errorf("the copy pod did not start within %s (%s)", r.o.Wait, waitingReason(po))
			}
		}
		if time.Since(lastLog) > 15*time.Second {
			lastLog = time.Now()
			switch {
			case phase == "Pending":
				r.o.Log("… copy pod %s: starting (%s)", name, waitingReason(po))
			case last.Phase == "check" && last.Used > 0:
				r.o.Log("… %s used, %s free in the new volume – copying", format.Bytes(last.Used), format.Bytes(last.Free))
			case last.Phase == "copy":
				pct := ""
				if last.Used > 0 {
					pct = fmt.Sprintf(" (~%d %%)", min(99, last.Stats.Bytes*100/last.Used))
				}
				r.o.Log("… copying: %d files, %s%s", last.Stats.Files, format.Bytes(last.Stats.Bytes), pct)
			case last.Phase == "verify":
				r.o.Log("… comparing both trees")
			}
		}
	}
}

// hopeless: the pod will not start by waiting longer – a bad image, a
// config error, or unschedulable for over two minutes.
func hopeless(po *unstructured.Unstructured) string {
	cs, _, _ := unstructured.NestedSlice(po.Object, "status", "containerStatuses")
	for _, x := range cs {
		if m, ok := x.(map[string]any); ok {
			w, _, _ := unstructured.NestedString(m, "state", "waiting", "reason")
			msg, _, _ := unstructured.NestedString(m, "state", "waiting", "message")
			switch w {
			case "ImagePullBackOff", "ErrImageNeverPull", "InvalidImageName", "CreateContainerConfigError", "CreateContainerError":
				return w + ": " + msg
			}
		}
	}
	conds, _, _ := unstructured.NestedSlice(po.Object, "status", "conditions")
	for _, x := range conds {
		m, _ := x.(map[string]any)
		if m["type"] == "PodScheduled" && m["status"] == "False" && m["reason"] == "Unschedulable" {
			if t, err := time.Parse(time.RFC3339, fmt.Sprint(m["lastTransitionTime"])); err == nil && time.Since(t) > 2*time.Minute {
				return fmt.Sprintf("cannot be scheduled: %v", m["message"])
			}
		}
	}
	return ""
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

func (r *runner) deletePod(ctx context.Context) { r.deleteNamedPod(ctx, r.p.CopyPod) }

// deleteNamedPod deletes a pod and waits until it is gone (its volumes
// are released).
func (r *runner) deleteNamedPod(ctx context.Context, name string) {
	if r.o.Handover && r.ctx.Err() != nil {
		return // stopping: the next run cleans up
	}
	uid := r.podUIDs[name]
	if uid == "" {
		return // not created by this run
	}
	res := r.c.Dynamic().Resource(gvrPod).Namespace(r.p.Switch.Namespace)
	if err := res.Delete(ctx, name, uidDelete(uid)); err != nil && !apierrors.IsNotFound(err) && !apierrors.IsConflict(err) {
		r.o.Log("delete pod %s: %v", name, err)
		return
	}
	deadline := time.Now().Add(r.o.Wait)
	for time.Now().Before(deadline) {
		if _, err := res.Get(ctx, name, metav1.GetOptions{}); apierrors.IsNotFound(err) {
			return
		}
		time.Sleep(r.o.Poll)
	}
	r.o.Log("pod %s is still there after %s", name, r.o.Wait)
}

func (r *runner) deleteTemp(ctx context.Context) error {
	sw, p := r.p.Switch, r.p
	if err := r.deleteClaim(ctx, p.TempPVC, p.TempPV, r.pvcUID, r.pvUID); err != nil {
		return err
	}
	r.o.Log("PVC %s and PV %s deleted (volume %s stays)", p.TempPVC, p.TempPV, sw.NewVolume)
	return nil
}

// deleteClaim deletes a temporary PVC, waits until it is gone, then its PV
// (UID preconditions: only what this run created).
func (r *runner) deleteClaim(ctx context.Context, pvcName, pvName, pvcUID, pvUID string) error {
	sw := r.p.Switch
	dyn := r.c.Dynamic()
	deadline := time.Now().Add(r.o.Wait)
	if pvcUID != "" {
		if err := dyn.Resource(gvrPVC).Namespace(sw.Namespace).Delete(ctx, pvcName, uidDelete(pvcUID)); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("delete PVC %s: %w", pvcName, err)
		}
		for {
			_, err := dyn.Resource(gvrPVC).Namespace(sw.Namespace).Get(ctx, pvcName, metav1.GetOptions{})
			if apierrors.IsNotFound(err) {
				break
			}
			if time.Now().After(deadline) {
				return fmt.Errorf("PVC %s is not gone after %s", pvcName, r.o.Wait)
			}
			time.Sleep(r.o.Poll)
		}
	}
	if pvUID == "" {
		return nil
	}
	if err := dyn.Resource(gvrPV).Delete(ctx, pvName, uidDelete(pvUID)); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete PV %s: %w", pvName, err)
	}
	for {
		_, err := dyn.Resource(gvrPV).Get(ctx, pvName, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("PV %s is not gone after %s", pvName, r.o.Wait)
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
func (r *runner) cleanup(deleteVolume bool) bool {
	if r.o.Handover && r.ctx.Err() != nil {
		return false // the next agent cleans up
	}
	ctx := context.WithoutCancel(r.ctx)
	sw, p := r.p.Switch, r.p
	r.deletePod(ctx)
	r.deleteNamedPod(ctx, p.ProbePod)
	r.deleteNamedPod(ctx, p.PrecopyPod)
	if r.pvcUID != "" {
		_ = r.c.Dynamic().Resource(gvrPVC).Namespace(sw.Namespace).Delete(ctx, p.TempPVC, uidDelete(r.pvcUID))
	}
	if r.pvUID != "" {
		_ = r.c.Dynamic().Resource(gvrPV).Delete(ctx, p.TempPV, uidDelete(r.pvUID))
	}
	if deleteVolume && r.volUID != "" {
		u := types.UID(r.volUID)
		err := r.c.Dynamic().Resource(longhorn.GVRVolume).Namespace(r.c.Namespace).Delete(ctx, sw.NewVolume,
			metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &u}})
		if err == nil || apierrors.IsNotFound(err) {
			r.o.Log("cleaned up: copy pod, PVC %s, PV %s and volume %s removed; %s is unchanged", p.TempPVC, p.TempPV, sw.NewVolume, sw.OldVolume)
			return true
		}
		r.o.Log("cleanup: delete volume %s: %v", sw.NewVolume, err)
	}
	return false
}
