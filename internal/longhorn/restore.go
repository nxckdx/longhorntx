package longhorn

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// Restore a backup into a NEW volume, optionally with a PV and PVC, like
// the web UI's "Restore" plus "Create PV/PVC" – in one confirmed step. The
// original volume and its workload are not touched (switching a workload
// over is a separate, later step).

var GVRPV = schema.GroupVersionResource{Version: "v1", Resource: "persistentvolumes"}

// RestoreSpec describes the new volume.
type RestoreSpec struct {
	Name         string // new Longhorn volume (also the PV name)
	Replicas     int
	PVCNamespace string // optional: create PV + PVC
	PVCName      string
	StorageClass string // for PV/PVC; Longhorn's "static" class by default
	FSType       string
}

var volNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*[a-z0-9]$`)

// RestoreName suggests a name for the restored volume: "<pvc>-restore-<MMDD>".
func RestoreName(pvc, volume string, now time.Time) string {
	base := volume
	if _, n, ok := strings.Cut(pvc, "/"); ok && n != "" {
		base = n
	}
	suffix := "-restore-" + now.Format("0102")
	if len(base)+len(suffix) > 63 {
		base = strings.TrimRight(base[:63-len(suffix)], "-")
	}
	return base + suffix
}

// DefaultReplicas reads the default-replica-count setting ("3" before v1.8,
// {"v1":"3","v2":"3"} since); 3 if it cannot be parsed.
func DefaultReplicas(value string) int {
	if n, err := strconv.Atoi(strings.TrimSpace(value)); err == nil && n > 0 {
		return n
	}
	var m map[string]string
	if json.Unmarshal([]byte(value), &m) == nil {
		if n, err := strconv.Atoi(m["v1"]); err == nil && n > 0 {
			return n
		}
	}
	return 3
}

// accessModeOf returns rwo/rwx as recorded with the backup volume.
func accessModeOf(bv *BackupVolume) string {
	if bv != nil && bv.Status.Labels["longhorn.io/volume-access-mode"] == "rwx" {
		return "rwx"
	}
	return "rwo"
}

// RestoreBackup builds the create actions: Volume (spec.fromBackup), then
// optionally PersistentVolume and PersistentVolumeClaim. exists reports
// whether a volume/PV/PVC of that name is already there (the caller GETs).
func RestoreBackup(b *Backup, bv *BackupVolume, s RestoreSpec, exists func(kind string) bool) ([]*Action, error) {
	if b.Status.State != "Completed" {
		return nil, fmt.Errorf("backup %s is %s, only completed backups can be restored", b.Name, dashIfEmpty(b.Status.State))
	}
	if b.Status.URL == "" {
		return nil, fmt.Errorf("backup %s has no URL yet (the backup target was not synced)", b.Name)
	}
	if !volNameRE.MatchString(s.Name) || len(s.Name) > 63 {
		return nil, fmt.Errorf("volume name %q: lowercase letters, digits and '-', at most 63 characters", s.Name)
	}
	if exists("volume") {
		return nil, fmt.Errorf("volume %s exists – choose another name", s.Name)
	}
	if s.Replicas < 1 || s.Replicas > 20 {
		return nil, fmt.Errorf("replicas must be 1–20")
	}
	size := int64(b.Status.VolumeSize)
	if size <= 0 {
		return nil, fmt.Errorf("backup %s does not record the volume size", b.Name)
	}
	mode := accessModeOf(bv)
	src := b.Volume()
	if bv != nil && bv.LastPVC() != "" {
		src = bv.LastPVC()
	}
	vol := &Action{
		Title: fmt.Sprintf("Restore backup %s of %s into new volume %s", b.Name, src, s.Name),
		Verb:  VerbCreate, GVR: GVRVolume, Name: s.Name,
		Object: lhObject("Volume", s.Name, nil, map[string]any{
			"fromBackup":       b.Status.URL,
			"size":             strconv.FormatInt(size, 10),
			"numberOfReplicas": int64(s.Replicas),
			"accessMode":       mode,
			"dataEngine":       "v1",
			"frontend":         "blockdev",
			"backupTargetName": b.Status.BackupTargetName,
		}),
		summary: fmt.Sprintf("volume %s from %s (%s, %d replicas)", s.Name, b.Name, humanBytes(size), s.Replicas),
	}
	vol.Warnings = append(vol.Warnings,
		fmt.Sprintf("restores %s (backup of %s from %s) into a new volume; the original is not touched", humanBytes(int64(b.Status.Size)), src, backupAge(b)),
		"the new volume stays in state \"restoring\" until all data is copied; it cannot be attached before")
	acts := []*Action{vol}
	if s.PVCName == "" {
		vol.Warnings = append(vol.Warnings, "no PVC: create one later (Longhorn UI \"Create PV/PVC\" or lhtx restore --pvc on a new restore)")
		return acts, nil
	}
	more, err := pvAndPVC(s, size, mode, exists)
	if err != nil {
		return nil, err
	}
	vol.Warnings = append(vol.Warnings, fmt.Sprintf("PV reclaim policy Retain: deleting PVC %s/%s keeps the volume", s.PVCNamespace, s.PVCName))
	return append(acts, more...), nil
}

// pvAndPVC builds the PV (volumeHandle = the Longhorn volume) and a PVC
// bound to it, like the web UI's "Create PV/PVC".
func pvAndPVC(s RestoreSpec, size int64, mode string, exists func(string) bool) ([]*Action, error) {
	if s.PVCNamespace == "" {
		return nil, fmt.Errorf("PVC needs a namespace (namespace/name)")
	}
	if exists("pv") {
		return nil, fmt.Errorf("PersistentVolume %s exists", s.Name)
	}
	if exists("pvc") {
		return nil, fmt.Errorf("PVC %s/%s exists – the restore creates a new PVC; pick a new name (switching an existing workload is not done here)", s.PVCNamespace, s.PVCName)
	}
	fs := s.FSType
	if fs == "" {
		fs = "ext4"
	}
	sizeQ := resource.NewQuantity(size, resource.BinarySI).String()
	k8sMode := "ReadWriteOnce"
	if mode == "rwx" {
		k8sMode = "ReadWriteMany"
	}
	pv := &Action{
		Title: "Create PersistentVolume " + s.Name, Verb: VerbCreate, GVR: GVRPV, Name: s.Name, ClusterScoped: true,
		Object: map[string]any{
			"apiVersion": "v1", "kind": "PersistentVolume",
			"metadata": map[string]any{"name": s.Name},
			"spec": map[string]any{
				"capacity":                      map[string]any{"storage": sizeQ},
				"volumeMode":                    "Filesystem",
				"accessModes":                   []any{k8sMode},
				"persistentVolumeReclaimPolicy": "Retain",
				"storageClassName":              s.StorageClass,
				"csi": map[string]any{
					"driver": "driver.longhorn.io", "fsType": fs, "volumeHandle": s.Name,
					"volumeAttributes": map[string]any{"numberOfReplicas": strconv.Itoa(s.Replicas), "staleReplicaTimeout": "30"},
				},
			},
		},
		summary: fmt.Sprintf("PV %s (%s, %s, reclaim Retain)", s.Name, sizeQ, s.StorageClass),
	}
	pvc := &Action{
		Title: fmt.Sprintf("Create PVC %s/%s", s.PVCNamespace, s.PVCName), Verb: VerbCreate, GVR: GVRPVC,
		Namespace: s.PVCNamespace, Name: s.PVCName,
		Object: map[string]any{
			"apiVersion": "v1", "kind": "PersistentVolumeClaim",
			"metadata": map[string]any{"name": s.PVCName, "namespace": s.PVCNamespace},
			"spec": map[string]any{
				"accessModes":      []any{k8sMode},
				"resources":        map[string]any{"requests": map[string]any{"storage": sizeQ}},
				"storageClassName": s.StorageClass,
				"volumeName":       s.Name,
			},
		},
		summary: fmt.Sprintf("PVC %s/%s bound to %s", s.PVCNamespace, s.PVCName, s.Name),
	}
	return []*Action{pv, pvc}, nil
}

func backupAge(b *Backup) string {
	t, err := time.Parse(time.RFC3339, BackupTime(b))
	if err != nil {
		return "an unknown time"
	}
	return t.Local().Format("2006-01-02 15:04")
}

// LatestBackup picks the newest completed backup.
func LatestBackup(bks []*Backup) *Backup {
	var best *Backup
	for _, b := range bks {
		if b.Status.State != "Completed" {
			continue
		}
		if best == nil || BackupTime(b) > BackupTime(best) {
			best = b
		}
	}
	return best
}

// Exists reports whether an object is there (404 = false). namespace ""
// with cluster=false means the Longhorn namespace.
func (c *Client) Exists(ctx context.Context, g schema.GroupVersionResource, namespace, name string, cluster bool) (bool, error) {
	res := c.res(g)
	switch {
	case cluster:
		res = c.dyn.Resource(g)
	case namespace != "":
		res = c.dyn.Resource(g).Namespace(namespace)
	}
	_, err := res.Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	return err == nil, err
}

func (c *Client) Backup(ctx context.Context, name string) (*Backup, error) {
	return get[Backup](ctx, c, GVRBackup, name)
}

// RestorePlan gathers what RestoreBackup needs for one backup: its backup
// volume, defaults from the settings and a name suggestion.
type RestorePlan struct {
	Backup       *Backup
	BackupVolume *BackupVolume
	Spec         RestoreSpec
}

func (c *Client) PlanRestore(ctx context.Context, b *Backup, now time.Time) (*RestorePlan, error) {
	p := &RestorePlan{Backup: b}
	bvs, err := c.BackupVolumes(ctx)
	if err != nil {
		return nil, err
	}
	for _, bv := range bvs {
		if bv.Volume() == b.Volume() && (b.Status.BackupTargetName == "" || bv.Target() == b.Status.BackupTargetName) {
			p.BackupVolume = bv
		}
	}
	pvc := ""
	if p.BackupVolume != nil {
		pvc = p.BackupVolume.LastPVC()
	}
	rc, _ := c.Setting(ctx, "default-replica-count")
	sc, _ := c.Setting(ctx, "default-longhorn-static-storage-class")
	if sc == "" {
		sc = "longhorn-static"
	}
	p.Spec = RestoreSpec{Name: RestoreName(pvc, b.Volume(), now), Replicas: DefaultReplicas(rc), StorageClass: sc, FSType: "ext4"}
	if ns, _, ok := strings.Cut(pvc, "/"); ok {
		p.Spec.PVCNamespace = ns
	}
	return p, nil
}

// Build checks for existing objects and returns the actions.
func (c *Client) BuildRestore(ctx context.Context, p *RestorePlan) ([]*Action, error) {
	exists, lookupErr := c.existsFn(ctx, p.Spec)
	acts, err := RestoreBackup(p.Backup, p.BackupVolume, p.Spec, exists)
	if err != nil {
		return nil, err
	}
	return acts, *lookupErr
}

// BuildClone checks for existing objects and returns the clone actions.
func (c *Client) BuildClone(ctx context.Context, src *Volume, snapshot string, s RestoreSpec) ([]*Action, error) {
	exists, lookupErr := c.existsFn(ctx, s)
	acts, err := CloneVolume(src, snapshot, s, exists)
	if err != nil {
		return nil, err
	}
	return acts, *lookupErr
}

// CloneDefaults: same replica count as the source, the static class, the
// source's namespace for a PVC.
func (c *Client) CloneDefaults(ctx context.Context, src *Volume, now time.Time) RestoreSpec {
	sc, _ := c.Setting(ctx, "default-longhorn-static-storage-class")
	if sc == "" {
		sc = "longhorn-static"
	}
	return RestoreSpec{Name: CloneName(src, now), Replicas: max(src.Spec.NumberOfReplicas, 1), StorageClass: sc,
		FSType: "ext4", PVCNamespace: src.Status.KubernetesStatus.Namespace}
}

func (c *Client) existsFn(ctx context.Context, spec RestoreSpec) (func(string) bool, *error) {
	var lookupErr error
	p := struct{ Spec RestoreSpec }{spec}
	return func(kind string) bool {
		var ok bool
		var err error
		switch kind {
		case "volume":
			ok, err = c.Exists(ctx, GVRVolume, "", p.Spec.Name, false)
		case "pv":
			ok, err = c.Exists(ctx, GVRPV, "", p.Spec.Name, true)
		case "pvc":
			ok, err = c.Exists(ctx, GVRPVC, p.Spec.PVCNamespace, p.Spec.PVCName, false)
		}
		if err != nil && lookupErr == nil {
			lookupErr = fmt.Errorf("checking %s: %w", kind, err)
		}
		return ok
	}, &lookupErr
}
