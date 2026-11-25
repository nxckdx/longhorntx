package longhorn

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8stypes "k8s.io/apimachinery/pkg/types"
)

// Write actions. Everything goes through the Kubernetes API by creating,
// patching or deleting Longhorn CRs – the same objects longhorn-manager's
// own API creates (verified against v1.12.1). Longhorn's admission webhooks
// validate every request, so a server-side dry run gives a real answer
// without changing anything.
//
// Builders are pure functions: they take the objects the user looked at and
// return an Action that can be previewed, dry-run or applied.

type Verb string

const (
	VerbCreate Verb = "CREATE"
	VerbPatch  Verb = "PATCH"
	VerbDelete Verb = "DELETE"
)

type Action struct {
	Title     string
	Verb      Verb
	GVR       schema.GroupVersionResource
	Namespace string // "" = the Longhorn namespace
	// ClusterScoped objects (PersistentVolume) have no namespace.
	ClusterScoped bool
	// Header replaces the request line of the preview (batches of
	// different kinds of objects).
	Header      string
	Name        string
	UID         string         // delete precondition: never delete a recreated object
	Object      map[string]any // create
	Patch       map[string]any // JSON merge patch (incl. resourceVersion for optimistic locking)
	Changes     []Change
	Warnings    []string
	Destructive bool
	// Lines replace the request details in the preview (batch summaries).
	Lines []string
	// CopySecretFrom: create a Secret with the type and data of this one.
	// The data is read only when the action is applied (also server dry
	// run) and never shown.
	CopySecretFrom *SecretRef
	summary        string // one-line description used in batch summaries
}

// Summary is a one-line description of the action for batch previews.
func (a *Action) Summary() string {
	if a.summary != "" {
		return a.summary
	}
	return a.Title
}

// Change is a human-readable field change for the preview.
type Change struct{ Field, From, To string }

type DryRun int

const (
	DryRunNone   DryRun = iota
	DryRunClient        // only show the request
	DryRunServer        // send with dryRun=All: validated by apiserver and Longhorn webhooks, not persisted
)

func ParseDryRun(s string) (DryRun, error) {
	switch s {
	case "", "none", "false":
		return DryRunNone, nil
	case "client", "true":
		return DryRunClient, nil
	case "server":
		return DryRunServer, nil
	}
	return 0, fmt.Errorf("invalid --dry-run %q (client|server)", s)
}

// Preview renders the request like a small HTTP/kubectl summary.
func (a *Action) Preview(namespace string) string {
	var b strings.Builder
	if a.Namespace != "" {
		namespace = a.Namespace
	}
	res := a.GVR.Resource
	if a.GVR.Group != "" {
		res += "." + a.GVR.Group
	}
	if a.Header != "" {
		b.WriteString(a.Header + "\n")
	} else if a.ClusterScoped {
		fmt.Fprintf(&b, "%s %s/%s\n", a.Verb, res, a.Name)
	} else {
		fmt.Fprintf(&b, "%s %s/%s -n %s\n", a.Verb, res, a.Name, namespace)
	}
	if len(a.Lines) > 0 {
		for _, l := range a.Lines {
			b.WriteString("  " + l + "\n")
		}
		return strings.TrimSuffix(b.String(), "\n")
	}
	for _, c := range a.Changes {
		fmt.Fprintf(&b, "  %s: %s → %s\n", c.Field, dashIfEmpty(c.From), dashIfEmpty(c.To))
	}
	var body any
	switch a.Verb {
	case VerbCreate:
		body = a.Object
	case VerbPatch:
		body = a.Patch
	}
	if body != nil {
		var buf bytes.Buffer
		enc := json.NewEncoder(&buf)
		enc.SetEscapeHTML(false) // URLs with & stay readable
		enc.SetIndent("  ", "  ")
		_ = enc.Encode(body)
		b.WriteString("  " + strings.TrimSuffix(buf.String(), "\n") + "\n")
	}
	return strings.TrimSuffix(b.String(), "\n")
}

func dashIfEmpty(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// Apply executes an action. DryRunClient is a no-op (the caller shows the
// preview).
func (c *Client) Apply(ctx context.Context, a *Action, mode DryRun) error {
	if mode == DryRunClient {
		return nil
	}
	var dry []string
	if mode == DryRunServer {
		dry = []string{metav1.DryRunAll}
	}
	res := c.res(a.GVR)
	switch {
	case a.ClusterScoped:
		res = c.dyn.Resource(a.GVR)
	case a.Namespace != "":
		res = c.dyn.Resource(a.GVR).Namespace(a.Namespace)
	}
	if a.CopySecretFrom != nil {
		return c.copySecret(ctx, a, dry)
	}
	switch a.Verb {
	case VerbCreate:
		obj := &unstructured.Unstructured{Object: a.Object}
		if obj.GetNamespace() == "" && !a.ClusterScoped {
			obj.SetNamespace(c.Namespace)
		}
		_, err := res.Create(ctx, obj, metav1.CreateOptions{DryRun: dry, FieldManager: "lhtx"})
		return err
	case VerbPatch:
		data, err := json.Marshal(a.Patch)
		if err != nil {
			return err
		}
		_, err = res.Patch(ctx, a.Name, k8stypes.MergePatchType, data, metav1.PatchOptions{DryRun: dry, FieldManager: "lhtx"})
		return err
	case VerbDelete:
		opts := metav1.DeleteOptions{DryRun: dry}
		if a.UID != "" {
			uid := k8stypes.UID(a.UID)
			opts.Preconditions = &metav1.Preconditions{UID: &uid}
		}
		return res.Delete(ctx, a.Name, opts)
	}
	return fmt.Errorf("unknown verb %q", a.Verb)
}

// copySecret creates a.Namespace/a.Name with the type and data of
// a.CopySecretFrom (an encryption key for a new PVC).
func (c *Client) copySecret(ctx context.Context, a *Action, dry []string) error {
	src, err := c.dyn.Resource(gvrSecret).Namespace(a.CopySecretFrom.Namespace).Get(ctx, a.CopySecretFrom.Name, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("read Secret %s: %w", a.CopySecretFrom, err)
	}
	obj := map[string]any{"apiVersion": "v1", "kind": "Secret",
		"metadata": map[string]any{"name": a.Name, "namespace": a.Namespace,
			"annotations": map[string]any{"lhtx.io/copied-from": a.CopySecretFrom.String()}},
		"type": src.Object["type"], "data": src.Object["data"]}
	if l := src.GetLabels(); len(l) > 0 {
		obj["metadata"].(map[string]any)["labels"] = toAnyMap(l)
	}
	_, err = c.dyn.Resource(gvrSecret).Namespace(a.Namespace).Create(ctx, &unstructured.Unstructured{Object: obj},
		metav1.CreateOptions{DryRun: dry, FieldManager: "lhtx"})
	return err
}

func toAnyMap(m map[string]string) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func lhObject(kind, name string, labels map[string]string, spec map[string]any) map[string]any {
	md := map[string]any{"name": name}
	if len(labels) > 0 {
		l := map[string]any{}
		for k, v := range labels {
			l[k] = v
		}
		md["labels"] = l
	}
	return map[string]any{
		"apiVersion": Group + "/" + Version,
		"kind":       kind,
		"metadata":   md,
		"spec":       spec,
	}
}

// patchWithRV adds metadata.resourceVersion, so the patch fails with a
// conflict if the object changed since the user looked at it.
func patchWithRV(rv string, spec map[string]any, extra ...map[string]any) map[string]any {
	p := map[string]any{"metadata": map[string]any{"resourceVersion": rv}}
	if spec != nil {
		p["spec"] = spec
	}
	for _, e := range extra {
		for k, v := range e {
			p[k] = v
		}
	}
	return p
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func checkVolumeWritable(v *Volume) error {
	if v.Spec.MigrationNodeID != "" {
		return fmt.Errorf("volume %s is being migrated", v.Name)
	}
	if v.Status.IsStandby {
		return fmt.Errorf("volume %s is a standby (DR) volume", v.Name)
	}
	return nil
}

func volLabel(v *Volume) string {
	if p := v.PVC(); p != "" {
		return p
	}
	return v.Name
}

// ---------------------------------------------------------------- snapshots

// GenerateSnapshotName returns a name in Longhorn's style, prefixed lhtx.
func GenerateSnapshotName() string { return "lhtx-" + randomHex(8) }

// GenerateBackupName mirrors longhorn-manager: "backup-" + 16 hex chars.
func GenerateBackupName() string { return "backup-" + randomHex(8) }

func CreateSnapshot(v *Volume, name string) (*Action, error) {
	if err := checkVolumeWritable(v); err != nil {
		return nil, err
	}
	if name == "" {
		name = GenerateSnapshotName()
	}
	a := &Action{
		Title: fmt.Sprintf("Create snapshot %s of %s", name, volLabel(v)),
		Verb:  VerbCreate, GVR: GVRSnapshot, Name: name,
		Object: lhObject("Snapshot", name, nil, map[string]any{
			"volume":         v.Name,
			"createSnapshot": true,
		}),
	}
	if v.Status.State != "attached" {
		a.Warnings = append(a.Warnings, fmt.Sprintf("volume is %s: Longhorn takes snapshots only of attached volumes, the request will stay pending", v.Status.State))
	}
	return a, nil
}

func DeleteSnapshot(s *VolumeSnapshot) (*Action, error) {
	a := &Action{
		Title: fmt.Sprintf("Delete snapshot %s of volume %s", s.Name, s.Spec.Volume),
		Verb:  VerbDelete, GVR: GVRSnapshot, Name: s.Name, UID: string(s.UID),
		Destructive: true,
	}
	a.Warnings = append(a.Warnings, "the snapshot is marked removed and its data merged/purged by Longhorn; this cannot be undone")
	if SnapshotKind(s) == "backup" {
		a.Warnings = append(a.Warnings, "this snapshot was taken for a backup; the backup itself stays in the backup target")
	}
	return a, nil
}

// ---------------------------------------------------------------- backups

func CreateBackup(v *Volume, snapshot string) (*Action, error) {
	if err := checkVolumeWritable(v); err != nil {
		return nil, err
	}
	if snapshot == "" {
		return nil, fmt.Errorf("snapshot name required")
	}
	target := v.Spec.BackupTargetName
	if target == "" {
		return nil, fmt.Errorf("volume %s has no backup target (spec.backupTargetName)", v.Name)
	}
	name := GenerateBackupName()
	return &Action{
		Title: fmt.Sprintf("Back up snapshot %s of %s to target %q", snapshot, volLabel(v), target),
		Verb:  VerbCreate, GVR: GVRBackup, Name: name,
		Object: lhObject("Backup", name,
			map[string]string{"backup-target": target, LabelBackupVolume: v.Name},
			map[string]any{"snapshotName": snapshot}),
	}, nil
}

// ---------------------------------------------------------------- replicas

// availableReplica mirrors longhorn-manager's IsAvailableHealthyReplica.
func availableReplica(r *Replica) bool {
	return r.DeletionTimestamp == nil && r.Spec.FailedAt == "" && r.Spec.HealthyAt != "" && !r.Spec.EvictionRequested
}

// DeleteReplica refuses to delete the last available replica (Longhorn's
// webhook does the same) and explains the consequences otherwise.
func DeleteReplica(r *Replica, volume *Volume, siblings []*Replica) (*Action, error) {
	var avail []string
	for _, s := range siblings {
		if availableReplica(s) {
			avail = append(avail, s.Name)
		}
	}
	if len(avail) == 1 && avail[0] == r.Name {
		return nil, fmt.Errorf("%s is the only healthy replica of %s – deleting it would lose data", r.Name, r.Spec.VolumeName)
	}
	a := &Action{
		Title: fmt.Sprintf("Delete replica %s (node %s, %s)", r.Name, r.Spec.NodeID, r.Spec.DiskPath),
		Verb:  VerbDelete, GVR: GVRReplica, Name: r.Name, UID: string(r.UID),
		Destructive: true,
	}
	if volume != nil && len(siblings) <= volume.Spec.NumberOfReplicas {
		a.Warnings = append(a.Warnings, fmt.Sprintf("the volume has %d of %d replicas; Longhorn will rebuild a new one (full copy of up to %s)",
			len(siblings), volume.Spec.NumberOfReplicas, humanBytes(int64(volume.Spec.Size))))
	}
	if !availableReplica(r) {
		a.Warnings = append(a.Warnings, "this replica is not healthy")
	}
	return a, nil
}

func SetReplicaCount(v *Volume, n int) (*Action, error) {
	if err := checkVolumeWritable(v); err != nil {
		return nil, err
	}
	if n < 1 || n > 20 {
		return nil, fmt.Errorf("replica count must be between 1 and 20")
	}
	if n == v.Spec.NumberOfReplicas {
		return nil, fmt.Errorf("volume already has %d replicas", n)
	}
	if v.Status.State != "attached" {
		return nil, fmt.Errorf("volume must be attached to change the replica count (Longhorn's own API requires this)")
	}
	a := &Action{
		Title: fmt.Sprintf("Set replica count of %s to %d", volLabel(v), n),
		Verb:  VerbPatch, GVR: GVRVolume, Name: v.Name,
		Patch:   patchWithRV(v.ResourceVersion, map[string]any{"numberOfReplicas": n}),
		Changes: []Change{{"spec.numberOfReplicas", fmt.Sprint(v.Spec.NumberOfReplicas), fmt.Sprint(n)}},
	}
	if n > v.Spec.NumberOfReplicas {
		a.Warnings = append(a.Warnings, fmt.Sprintf("Longhorn will build %d new replica(s), each a full copy (%s)", n-v.Spec.NumberOfReplicas, humanBytes(int64(v.Spec.Size))))
	} else {
		a.Destructive = true
		a.Warnings = append(a.Warnings, "Longhorn will remove surplus replicas")
	}
	if n == 1 {
		a.Warnings = append(a.Warnings, "with 1 replica the volume has no redundancy")
	}
	return a, nil
}

// ---------------------------------------------------------------- nodes / disks

// SetScheduling enables or disables replica scheduling on a node or, with
// disk != "", on one of its disks. Enabling also cancels a pending eviction,
// because Longhorn rejects scheduling while eviction is requested.
func SetScheduling(n *Node, disk string, enable bool) (*Action, error) {
	what := "node " + n.Name
	var spec map[string]any
	var changes []Change
	if disk == "" {
		if n.Spec.AllowScheduling == enable && (!enable || !n.Spec.EvictionRequested) {
			return nil, fmt.Errorf("scheduling on %s is already %s", what, onOff(enable))
		}
		spec = map[string]any{"allowScheduling": enable}
		changes = append(changes, Change{"spec.allowScheduling", fmt.Sprint(n.Spec.AllowScheduling), fmt.Sprint(enable)})
		if enable && n.Spec.EvictionRequested {
			spec["evictionRequested"] = false
			changes = append(changes, Change{"spec.evictionRequested", "true", "false"})
		}
	} else {
		d, ok := n.Spec.Disks[disk]
		if !ok {
			return nil, fmt.Errorf("node %s has no disk %q", n.Name, disk)
		}
		what = fmt.Sprintf("disk %s (%s) on node %s", disk, d.Path, n.Name)
		if d.AllowScheduling == enable && (!enable || !d.EvictionRequested) {
			return nil, fmt.Errorf("scheduling on %s is already %s", what, onOff(enable))
		}
		dp := map[string]any{"allowScheduling": enable}
		changes = append(changes, Change{"spec.disks." + disk + ".allowScheduling", fmt.Sprint(d.AllowScheduling), fmt.Sprint(enable)})
		if enable && d.EvictionRequested {
			dp["evictionRequested"] = false
			changes = append(changes, Change{"spec.disks." + disk + ".evictionRequested", "true", "false"})
		}
		spec = map[string]any{"disks": map[string]any{disk: dp}}
	}
	verb := "Disable"
	if enable {
		verb = "Enable"
	}
	a := &Action{
		Title: fmt.Sprintf("%s replica scheduling on %s", verb, what),
		Verb:  VerbPatch, GVR: GVRNode, Name: n.Name,
		Patch: patchWithRV(n.ResourceVersion, spec), Changes: changes,
	}
	if !enable {
		a.Warnings = append(a.Warnings, "existing replicas stay; no new replicas will be placed here")
	}
	return a, nil
}

// SetEviction requests (or cancels) eviction of all replicas from a node or
// disk. Requesting eviction also disables scheduling (Longhorn requires it).
func SetEviction(n *Node, disk string, evict bool) (*Action, error) {
	what := "node " + n.Name
	var spec map[string]any
	var changes []Change
	if disk == "" {
		if n.Spec.EvictionRequested == evict {
			return nil, fmt.Errorf("eviction on %s is already %s", what, onOff(evict))
		}
		spec = map[string]any{"evictionRequested": evict}
		changes = append(changes, Change{"spec.evictionRequested", fmt.Sprint(n.Spec.EvictionRequested), fmt.Sprint(evict)})
		if evict && n.Spec.AllowScheduling {
			spec["allowScheduling"] = false
			changes = append(changes, Change{"spec.allowScheduling", "true", "false"})
		}
	} else {
		d, ok := n.Spec.Disks[disk]
		if !ok {
			return nil, fmt.Errorf("node %s has no disk %q", n.Name, disk)
		}
		what = fmt.Sprintf("disk %s (%s) on node %s", disk, d.Path, n.Name)
		if d.EvictionRequested == evict {
			return nil, fmt.Errorf("eviction on %s is already %s", what, onOff(evict))
		}
		dp := map[string]any{"evictionRequested": evict}
		changes = append(changes, Change{"spec.disks." + disk + ".evictionRequested", fmt.Sprint(d.EvictionRequested), fmt.Sprint(evict)})
		if evict && d.AllowScheduling {
			dp["allowScheduling"] = false
			changes = append(changes, Change{"spec.disks." + disk + ".allowScheduling", "true", "false"})
		}
		spec = map[string]any{"disks": map[string]any{disk: dp}}
	}
	title := "Evict all replicas from " + what
	if !evict {
		title = "Cancel eviction on " + what + " (scheduling stays disabled)"
	}
	a := &Action{
		Title: title, Verb: VerbPatch, GVR: GVRNode, Name: n.Name,
		Patch: patchWithRV(n.ResourceVersion, spec), Changes: changes,
	}
	if evict {
		a.Destructive = true
		a.Warnings = append(a.Warnings, "Longhorn rebuilds every replica on it elsewhere – this moves a lot of data")
	}
	return a, nil
}

func onOff(b bool) string {
	if b {
		return "on"
	}
	return "off"
}

// ---------------------------------------------------------------- settings

func SetSetting(s *Setting, value string) (*Action, error) {
	if s.Value == value {
		return nil, fmt.Errorf("setting %s is already %q", s.Name, value)
	}
	return &Action{
		Title: fmt.Sprintf("Change setting %s", s.Name),
		Verb:  VerbPatch, GVR: GVRSetting, Name: s.Name,
		Patch:    patchWithRV(s.ResourceVersion, nil, map[string]any{"value": value}),
		Changes:  []Change{{"value", s.Value, value}},
		Warnings: gitopsWarning(s),
	}, nil
}

// ---------------------------------------------------------------- composite: snapshot + backup

// SnapshotAndBackup creates a snapshot, waits until Longhorn reports it
// ready, then backs it up – what the web UI's "Create Backup" does.
// progress receives human-readable steps.
func (c *Client) SnapshotAndBackup(ctx context.Context, v *Volume, progress func(string)) (backup string, err error) {
	snapA, err := CreateSnapshot(v, "")
	if err != nil {
		return "", err
	}
	if err := c.Apply(ctx, snapA, DryRunNone); err != nil {
		return "", fmt.Errorf("create snapshot: %w", err)
	}
	progress("snapshot " + snapA.Name + " requested, waiting until ready …")
	deadline := time.Now().Add(2 * time.Minute)
	for {
		u, err := c.res(GVRSnapshot).Get(ctx, snapA.Name, metav1.GetOptions{})
		if err == nil {
			s, cerr := convert[VolumeSnapshot](u)
			if cerr == nil && s.Status.ReadyToUse {
				break
			}
			if cerr == nil && s.Status.Error != "" {
				return "", fmt.Errorf("snapshot %s: %s", snapA.Name, s.Status.Error)
			}
		}
		if time.Now().After(deadline) {
			return "", fmt.Errorf("snapshot %s not ready after 2 minutes", snapA.Name)
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(time.Second):
		}
	}
	bkA, err := CreateBackup(v, snapA.Name)
	if err != nil {
		return "", err
	}
	if err := c.Apply(ctx, bkA, DryRunNone); err != nil {
		return "", fmt.Errorf("create backup: %w", err)
	}
	progress("backup " + bkA.Name + " created")
	return bkA.Name, nil
}

// SortedSettings returns settings sorted by name.
func SortedSettings(s []*Setting) []*Setting {
	sort.Slice(s, func(i, j int) bool { return s[i].Name < s[j].Name })
	return s
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

// ReplicaCountWarning warns when a replica count exceeds the number of
// nodes that accept replicas (Longhorn then needs soft anti-affinity or
// leaves replicas unscheduled).
func ReplicaCountWarning(n int, nodes []*Node) string {
	sched := 0
	for _, nd := range nodes {
		if nd.Spec.AllowScheduling && nd.Ready() && !nd.Spec.EvictionRequested {
			sched++
		}
	}
	if len(nodes) > 0 && n > sched {
		return fmt.Sprintf("only %d node(s) accept replicas: %d replicas need replica-soft-anti-affinity, otherwise some stay unscheduled", sched, n)
	}
	return ""
}

// SetJobAssignment adds or removes recurring job (or group) labels on a
// volume in one patch, like longhorn-manager's volume recurringJobAdd/Delete
// actions. pvcLabels are the labels of the volume's PVC (nil if none): with
// recurring-job.longhorn.io/source=enabled the PVC's labels win and Longhorn
// overwrites changes on the volume.
func SetJobAssignment(v *Volume, refs []JobRef, assign bool, known []*RecurringJob, pvcLabels map[string]string) (*Action, error) {
	if len(refs) == 0 {
		return nil, fmt.Errorf("no job given")
	}
	labels := map[string]any{}
	var changes []Change
	var names, warns []string
	for _, ref := range refs {
		if ref.Name == "" {
			return nil, fmt.Errorf("empty job name")
		}
		if ref.Assigned(v) == assign {
			if assign {
				return nil, fmt.Errorf("%s is already assigned to %s", ref, volLabel(v))
			}
			return nil, fmt.Errorf("%s is not assigned to %s directly (jobs via a group: remove the group)", ref, volLabel(v))
		}
		found := false
		for _, j := range known {
			if (!ref.Group && j.Name == ref.Name) || (ref.Group && contains(j.Spec.Groups, ref.Name)) {
				found = true
			}
		}
		if assign && !found {
			if ref.Group {
				warns = append(warns, "no recurring job is in group "+ref.Name+" (yet)")
			} else {
				warns = append(warns, "there is no recurring job "+ref.Name)
			}
		}
		if assign {
			labels[ref.label()] = "enabled"
			changes = append(changes, Change{"metadata.labels." + ref.label(), "", "enabled"})
		} else {
			labels[ref.label()] = nil
			changes = append(changes, Change{"metadata.labels." + ref.label(), "enabled", ""})
		}
		names = append(names, ref.String())
	}
	title := fmt.Sprintf("Assign %s to %s", strings.Join(names, ", "), volLabel(v))
	if !assign {
		title = fmt.Sprintf("Remove %s from %s", strings.Join(names, ", "), volLabel(v))
	}
	a := &Action{
		Title: title, Verb: VerbPatch, GVR: GVRVolume, Name: v.Name,
		Patch:    map[string]any{"metadata": map[string]any{"resourceVersion": v.ResourceVersion, "labels": labels}},
		Changes:  changes,
		Warnings: warns,
	}
	if pvcLabels["recurring-job.longhorn.io/source"] == "enabled" {
		a.Warnings = append(a.Warnings, "the PVC has recurring-job.longhorn.io/source=enabled: Longhorn syncs job labels from the PVC and will undo this – change the PVC labels instead")
	}
	return a, nil
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
