package demo

import (
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"

	lh "github.com/nxckdx/longhorntx/internal/longhorn"
)

const engineImage = "longhornio/longhorn-engine:v1.12.1"

type volDef struct {
	ns, pvc     string
	sizeGi      int64
	usedPct     int64 // actual size in % of the size
	replicas    int
	node        string // attached to ("" = detached)
	backupAge   time.Duration
	rwx         bool
	noRebuild   bool // not used for the simulated rebuilds
	lastFailure bool
	oneNode     bool // all replicas on one node (shows up in the replica matrix)
}

var nodeNames = []string{"worker-1", "worker-2", "worker-3", "worker-4"}

var volDefs = []volDef{
	{ns: "databases", pvc: "data-postgres-0", sizeGi: 50, usedPct: 62, replicas: 3, node: "worker-1", backupAge: 3 * time.Hour},
	{ns: "databases", pvc: "data-postgres-1", sizeGi: 50, usedPct: 61, replicas: 3, node: "worker-2", backupAge: 3 * time.Hour},
	{ns: "databases", pvc: "valkey-data", sizeGi: 8, usedPct: 35, replicas: 2, node: "worker-3", backupAge: 3 * time.Hour},
	{ns: "monitoring", pvc: "prometheus-db-prometheus-0", sizeGi: 100, usedPct: 78, replicas: 2, node: "worker-4", backupAge: 27 * time.Hour},
	{ns: "monitoring", pvc: "storage-loki-0", sizeGi: 40, usedPct: 55, replicas: 2, node: "worker-1", backupAge: 27 * time.Hour},
	{ns: "monitoring", pvc: "grafana", sizeGi: 5, usedPct: 12, replicas: 3, node: "worker-3", backupAge: 3 * time.Hour},
	{ns: "git", pvc: "gitea-shared-storage", sizeGi: 30, usedPct: 41, replicas: 3, node: "worker-2", backupAge: 3 * time.Hour},
	{ns: "cloud", pvc: "nextcloud-data", sizeGi: 200, usedPct: 70, replicas: 3, node: "worker-4", backupAge: 3 * time.Hour, rwx: true},
	{ns: "cloud", pvc: "nextcloud-config", sizeGi: 2, usedPct: 20, replicas: 3, node: "worker-4", backupAge: 3 * time.Hour, rwx: true},
	{ns: "media", pvc: "jellyfin-config", sizeGi: 20, usedPct: 48, replicas: 2, node: "worker-3", backupAge: 9 * 24 * time.Hour},
	{ns: "registry", pvc: "registry-data", sizeGi: 80, usedPct: 33, replicas: 3, node: "worker-1", backupAge: 3 * time.Hour},
	{ns: "ci", pvc: "runner-cache", sizeGi: 50, usedPct: 90, replicas: 1, node: "worker-2", noRebuild: true},
	{ns: "auth", pvc: "keycloak-db", sizeGi: 10, usedPct: 25, replicas: 3, node: "worker-1", backupAge: 3 * time.Hour},
	{ns: "tools", pvc: "scratch", sizeGi: 15, usedPct: 30, replicas: 2, node: "worker-3", noRebuild: true, oneNode: true},
	{ns: "archive", pvc: "old-backups", sizeGi: 150, usedPct: 85, replicas: 2, node: "", backupAge: 30 * 24 * time.Hour, noRebuild: true},
}

type rebuild struct {
	vol      *lh.Volume
	engine   *lh.Engine
	replica  *lh.Replica
	from     []string
	progress int
}

type data struct {
	now      time.Time
	volumes  []*lh.Volume
	replicas map[string][]*lh.Replica // by volume
	engines  map[string]*lh.Engine
	nodes    []*lh.Node
	diskUUID map[string]string // node → main disk UUID
	rest     []runtime.Object

	rebuild *rebuild
	next    int // rotation index into volumes
	serial  int
}

func (d *data) ts(ago time.Duration) string { return d.now.Add(-ago).UTC().Format(time.RFC3339) }

func meta(name string, labels map[string]string, created time.Time) metav1.ObjectMeta {
	return metav1.ObjectMeta{Name: name, Namespace: ns, Labels: labels, UID: types.UID("demo-" + hex8(name)),
		CreationTimestamp: metav1.NewTime(created)}
}

func build(now time.Time) *data {
	d := &data{now: now, replicas: map[string][]*lh.Replica{}, engines: map[string]*lh.Engine{}, diskUUID: map[string]string{}}
	d.buildNodes()
	for i, def := range volDefs {
		d.buildVolume(i, def)
	}
	d.schedule()
	d.buildRest()
	// start with a rebuild that is already under way, on a small volume so
	// the simulated speed stays believable
	d.next = 5
	if v := d.nextRebuildVolume(); v != nil {
		d.replaceReplica(v)
		d.rebuild.progress = 23
		d.setRebuild(d.rebuild)
	}
	return d
}

func (d *data) buildNodes() {
	for i, name := range nodeNames {
		n := &lh.Node{ObjectMeta: meta(name, nil, d.now.Add(-400*24*time.Hour))}
		n.Spec.AllowScheduling = true
		n.Status.Conditions = []lh.Condition{{Type: "Ready", Status: "True"}, {Type: "Schedulable", Status: "True"}}
		n.Spec.Disks = map[string]lh.DiskSpec{}
		n.Status.DiskStatus = map[string]lh.DiskStatus{}
		addDisk := func(dname, path string, maxGi int64, nvme bool) {
			id := uuid(name + dname)
			n.Spec.Disks[dname] = lh.DiskSpec{Path: path, DiskType: "filesystem", AllowScheduling: true, StorageReserved: lh.Quantity(maxGi * gi / 10)}
			ds := lh.DiskStatus{DiskName: dname, DiskUUID: id, DiskPath: path, DiskType: "filesystem", DiskDriver: "auto", FilesystemType: "ext4",
				StorageMaximum: lh.Quantity(maxGi * gi), StorageAvailable: lh.Quantity(maxGi * gi * int64(55-i*7) / 100),
				Conditions: []lh.Condition{{Type: "Ready", Status: "True"}, {Type: "Schedulable", Status: "True"}}}
			if nvme {
				h := lh.DiskHealth{DiskName: "nvme0n1", ModelName: "Samsung SSD 980 PRO 2TB", HealthStatus: "PASSED", Temperature: 41 + i*3}
				h.Attributes = append(h.Attributes, struct {
					Name     string `json:"name"`
					RawValue int64  `json:"rawValue"`
					Value    int    `json:"value"`
				}{Name: "Percentage Used", RawValue: int64(4 + i*6), Value: 96 - i*6})
				ds.HealthData = map[string]lh.DiskHealth{"/dev/nvme0n1": h}
			}
			n.Status.DiskStatus[dname] = ds
		}
		addDisk("default-disk-"+hex8(name)[:4], "/var/lib/longhorn", 900, false)
		d.diskUUID[name] = uuid(name + "default-disk-" + hex8(name)[:4])
		if i >= 2 {
			addDisk("nvme", "/mnt/nvme0/longhorn", 1800, true)
		}
		d.nodes = append(d.nodes, n)
	}
	// worker-4 is cordoned for maintenance
	d.nodes[3].Spec.AllowScheduling = false
}

func (d *data) newReplica(v *lh.Volume, node string, idx int) *lh.Replica {
	d.serial++
	name := fmt.Sprintf("%s-r-%s", v.Name, hex8(fmt.Sprintf("%s%d", v.Name, d.serial)))
	r := &lh.Replica{ObjectMeta: meta(name, map[string]string{lh.LabelVolume: v.Name, lh.LabelNode: node}, d.now.Add(-90*24*time.Hour))}
	r.Spec.VolumeName = v.Name
	r.Spec.EngineName = v.Name + "-e-0"
	r.Spec.NodeID = node
	r.Spec.DiskID = d.diskUUID[node]
	r.Spec.DiskPath = "/var/lib/longhorn"
	r.Spec.DataDirectoryName = v.Name + "-" + hex8(name)
	r.Spec.DataEngine = "v1"
	r.Spec.Active = true
	r.Spec.HealthyAt = d.ts(90 * 24 * time.Hour)
	r.Spec.DesireState = "stopped"
	r.Status.CurrentState = "stopped"
	if v.Status.State == "attached" {
		r.Spec.DesireState, r.Status.CurrentState = "running", "running"
	}
	ni := 0
	for i, n := range nodeNames {
		if n == node {
			ni = i
		}
	}
	r.Status.IP = fmt.Sprintf("10.42.%d.%d", ni+1, 10+idx+d.serial%200)
	r.Status.StorageIP = r.Status.IP
	r.Status.Port = 10000 + 15*(d.serial%60)
	return r
}

func (d *data) buildVolume(i int, def volDef) {
	name := "pvc-" + uuid(def.ns+"/"+def.pvc)
	v := &lh.Volume{ObjectMeta: meta(name, map[string]string{"recurring-job-group.longhorn.io/default": "enabled"}, d.now.Add(-time.Duration(120+i*9)*24*time.Hour))}
	v.Spec.Size = lh.Quantity(def.sizeGi * gi)
	v.Spec.NumberOfReplicas = def.replicas
	v.Spec.DataEngine = "v1"
	v.Spec.AccessMode = "rwo"
	if def.rwx {
		v.Spec.AccessMode = "rwx"
		v.Status.ShareState = "running"
	}
	v.Spec.Frontend = "blockdev"
	v.Spec.DataLocality = "disabled"
	v.Spec.BackupTargetName = "default"
	v.Spec.Image = engineImage
	v.Spec.NodeID = def.node
	v.Status.CurrentImage = engineImage
	v.Status.ActualSize = lh.Quantity(def.sizeGi * gi * def.usedPct / 100)
	v.Status.KubernetesStatus = lh.KubernetesStatus{PVName: name, PVStatus: "Bound", Namespace: def.ns, PVCName: def.pvc}
	v.Status.State, v.Status.Robustness = "detached", "unknown"
	if def.node != "" {
		v.Status.State, v.Status.Robustness, v.Status.CurrentNodeID = "attached", "healthy", def.node
	}
	if def.backupAge > 0 {
		v.Status.LastBackupAt = d.ts(def.backupAge)
		v.Status.LastBackup = "backup-" + hex8(name + "b")[:8] + hex8(name + "c")[:8]
	}
	d.volumes = append(d.volumes, v)

	e := &lh.Engine{ObjectMeta: meta(name+"-e-0", map[string]string{lh.LabelVolume: name, lh.LabelNode: def.node}, v.CreationTimestamp.Time)}
	e.Spec.VolumeName, e.Spec.VolumeSize, e.Spec.NodeID, e.Spec.DataEngine, e.Spec.Active = name, v.Spec.Size, def.node, "v1", true
	e.Status.CurrentState = "stopped"
	e.Status.CurrentReplicaAddressMap = map[string]string{}
	e.Status.ReplicaModeMap = map[string]string{}
	e.Status.ReplicaTransitionTimeMap = map[string]string{}
	e.Status.RebuildStatus = map[string]lh.RebuildStatus{}
	if def.node != "" {
		e.Status.CurrentState = "running"
	}
	d.engines[name] = e

	// spread replicas, starting next to the attached node
	start := i % len(nodeNames)
	for k := 0; k < def.replicas; k++ {
		node := nodeNames[(start+k)%len(nodeNames)]
		if def.oneNode {
			node = nodeNames[start]
		}
		r := d.newReplica(v, node, k)
		d.replicas[name] = append(d.replicas[name], r)
		if def.node != "" {
			e.Status.CurrentReplicaAddressMap[r.Name] = fmt.Sprintf("%s:%d", r.Status.StorageIP, r.Status.Port)
			e.Status.ReplicaModeMap[r.Name] = "RW"
			e.Status.ReplicaTransitionTimeMap[r.Name] = r.Spec.HealthyAt
		}
	}
}

// schedule books the replicas on their disks.
func (d *data) schedule() {
	for _, n := range d.nodes {
		for k, ds := range n.Status.DiskStatus {
			if ds.DiskUUID != d.diskUUID[n.Name] {
				continue
			}
			ds.ScheduledReplica = map[string]lh.Quantity{}
			for _, v := range d.volumes {
				for _, r := range d.replicas[v.Name] {
					if r.Spec.NodeID == n.Name {
						ds.ScheduledReplica[r.Name] = v.Spec.Size
						ds.StorageScheduled += v.Spec.Size
					}
				}
			}
			n.Status.DiskStatus[k] = ds
		}
	}
}

// nextRebuildVolume picks the next attached volume with ≥ 2 replicas.
func (d *data) nextRebuildVolume() *lh.Volume {
	for range d.volumes {
		i := d.next % len(d.volumes)
		d.next++
		if def := volDefs[i]; def.node != "" && def.replicas >= 2 && !def.noRebuild {
			return d.volumes[i]
		}
	}
	return nil
}

// replaceReplica swaps the last replica of v for a new, empty one on the
// same node and starts rebuilding it. Returns the old and the new replica.
func (d *data) replaceReplica(v *lh.Volume) (old, repl *lh.Replica) {
	rs := d.replicas[v.Name]
	old = rs[len(rs)-1]
	repl = d.newReplica(v, old.Spec.NodeID, len(rs))
	repl.CreationTimestamp = metav1.NewTime(time.Now())
	repl.Spec.HealthyAt = ""
	repl.Spec.LastFailedAt = time.Now().Add(-5 * time.Second).UTC().Format(time.RFC3339)
	rs[len(rs)-1] = repl

	e := d.engines[v.Name]
	var from []string
	for _, r := range rs[:len(rs)-1] {
		from = append(from, "tcp://"+e.Status.CurrentReplicaAddressMap[r.Name])
	}
	delete(e.Status.CurrentReplicaAddressMap, old.Name)
	delete(e.Status.ReplicaModeMap, old.Name)
	delete(e.Status.ReplicaTransitionTimeMap, old.Name)
	e.Status.CurrentReplicaAddressMap[repl.Name] = fmt.Sprintf("%s:%d", repl.Status.StorageIP, repl.Status.Port)
	e.Status.ReplicaModeMap[repl.Name] = "WO"
	e.Status.ReplicaTransitionTimeMap[repl.Name] = time.Now().UTC().Format(time.RFC3339)
	v.Status.Robustness = "degraded"
	v.Status.LastDegradedAt = time.Now().UTC().Format(time.RFC3339)
	d.rebuild = &rebuild{vol: v, engine: e, replica: repl, from: from}
	d.setRebuild(d.rebuild)
	return old, repl
}

func (d *data) setRebuild(r *rebuild) {
	addr := "tcp://" + r.engine.Status.CurrentReplicaAddressMap[r.replica.Name]
	r.engine.Status.RebuildStatus = map[string]lh.RebuildStatus{addr: {
		IsRebuilding: true, Progress: min(r.progress, 99), State: "in_progress",
		FromReplicaAddressList: r.from, AppliedRebuildingMBps: 120,
	}}
}

func (d *data) buildRest() {
	add := func(o runtime.Object) { d.rest = append(d.rest, o) }
	created := d.now.Add(-400 * 24 * time.Hour)
	for k, v := range map[string]string{
		"current-longhorn-version":                           "v1.12.1",
		"default-engine-image":                               engineImage,
		"concurrent-automatic-engine-upgrade-per-node-limit": "0",
		"default-replica-count":                              `{"v1":"3","v2":"3"}`,
		"storage-over-provisioning-percentage":               "100",
		"storage-minimal-available-percentage":               "25",
		"replica-auto-balance":                               "best-effort",
		"node-drain-policy":                                  "block-if-contains-last-replica",
		"concurrent-replica-rebuild-per-node-limit":          "5",
		"backup-concurrent-limit":                            "2",
		"auto-delete-pod-when-volume-detached-unexpectedly":  "true",
		"orphan-resource-auto-deletion":                      "",
	} {
		add(&lh.Setting{ObjectMeta: meta(k, nil, created), Value: v})
	}

	ei := &lh.EngineImage{ObjectMeta: meta("ei-"+hex8(engineImage), nil, created)}
	ei.Spec.Image = engineImage
	ei.Status.State, ei.Status.Version, ei.Status.RefCount = "deployed", "v1.12.1", len(d.volumes)
	ei.Status.NodeDeploymentMap = map[string]bool{}
	for _, n := range nodeNames {
		ei.Status.NodeDeploymentMap[n] = true
	}
	add(ei)

	bt := &lh.BackupTarget{ObjectMeta: meta("default", nil, created)}
	bt.Spec.BackupTargetURL = "s3://longhorn-backups@us-east-1/"
	bt.Spec.CredentialSecret = "backup-s3"
	bt.Spec.PollInterval = "5m0s"
	bt.Status.Available = true
	bt.Status.LastSyncedAt = d.ts(2 * time.Minute)
	add(bt)
	// a second target (Longhorn ≥ 1.8) that is currently unreachable
	off := &lh.BackupTarget{ObjectMeta: meta("offsite", map[string]string{
		"kustomize.toolkit.fluxcd.io/name": "infra", "kustomize.toolkit.fluxcd.io/namespace": "flux-system"}, created)}
	off.Spec.BackupTargetURL = "nfs://backup-nas.example:/export/longhorn"
	off.Spec.PollInterval = "30m0s"
	off.Status.LastSyncedAt = d.ts(3 * time.Hour)
	off.Status.Conditions = []lh.Condition{{Type: "Unavailable", Status: "True", Reason: "Unavailable",
		Message: "failed to mount nfs://backup-nas.example:/export/longhorn: connection timed out"}}
	add(off)

	rj := &lh.RecurringJob{ObjectMeta: meta("daily-backup", nil, created)}
	rj.Spec.Task, rj.Spec.Cron, rj.Spec.Retain, rj.Spec.Concurrency = "backup", "0 2 * * *", 7, 2
	rj.Spec.Groups = []string{"default"}
	rj.Status.ExecutionCount = 395
	add(rj)
	sj := &lh.RecurringJob{ObjectMeta: meta("hourly-snapshot", nil, created)}
	sj.Spec.Task, sj.Spec.Cron, sj.Spec.Retain, sj.Spec.Concurrency = "snapshot", "0 * * * *", 6, 4
	sj.Spec.Groups = []string{"default"}
	sj.Status.ExecutionCount = 9480
	add(sj)
	wj := &lh.RecurringJob{ObjectMeta: meta("weekly-full-backup", nil, created)}
	wj.Spec.Task, wj.Spec.Cron, wj.Spec.Retain, wj.Spec.Concurrency = "backup-force-create", "0 4 * * 0", 4, 1
	wj.Status.ExecutionCount = 52
	add(wj)

	for vi, v := range d.volumes {
		def := volDefs[vi]
		// snapshot chain (hourly); data-postgres-0 was reverted to the
		// snapshot from 4h ago, so the newer old line is a branch
		mk := func(id string, ago time.Duration, parent string, k int) *lh.VolumeSnapshot {
			name := "snap-" + hex8(v.Name+id)
			s := &lh.VolumeSnapshot{ObjectMeta: meta(name, map[string]string{lh.LabelVolume: v.Name}, d.now.Add(-ago))}
			s.Spec.Volume = v.Name
			s.Status.Parent = parent
			s.Status.CreationTime = d.ts(ago)
			s.Status.Size = lh.Quantity(int64(v.Status.ActualSize) / int64(20+k*5))
			s.Status.ReadyToUse, s.Status.UserCreated = true, true
			s.Status.Labels = map[string]string{"type": "snap"}
			s.Status.Children = map[string]bool{}
			return s
		}
		var snaps []*lh.VolumeSnapshot
		parent := ""
		for k := 5; k >= 0; k-- {
			s := mk(fmt.Sprint(k), time.Duration(k)*time.Hour+2*time.Minute, parent, k)
			snaps = append(snaps, s)
			parent = s.Name
		}
		if def.pvc == "data-postgres-0" {
			// revert after snapshot 4: snapshots 3..0 were taken on the new
			// line, two older ones (manual) remain on the abandoned line
			fork := snaps[1].Name // 4h ago
			b1 := mk("old1", 4*time.Hour-20*time.Minute, fork, 3)
			b2 := mk("old2", 4*time.Hour-40*time.Minute, b1.Name, 3)
			b1.Status.Labels, b2.Status.Labels = nil, nil
			snaps = append(snaps, b1, b2)
		}
		byName := map[string]*lh.VolumeSnapshot{}
		for _, s := range snaps {
			byName[s.Name] = s
		}
		for _, s := range snaps {
			if p := byName[s.Status.Parent]; p != nil {
				p.Status.Children[s.Name] = true
			}
		}
		byName[parent].Status.Children["volume-head"] = true
		for _, s := range snaps {
			add(s)
		}
		if def.backupAge > 0 {
			d.addBackups(add, v.Name, def.ns, def.pvc, int64(v.Spec.Size), int64(v.Status.ActualSize), def.backupAge)
		}
	}
	// a volume deleted two weeks ago whose backups remain (restore candidate)
	d.addBackups(add, "pvc-"+uuid("legacy-wiki"), "legacy", "wiki-data", 20<<30, 6<<30, 14*24*time.Hour)

	// leftovers of a volume deleted while worker-2 was down (risky: maybe
	// the last copy) and of an old replica of a healthy volume (safe)
	var healthy string
	for _, v := range d.volumes {
		if v.Status.Robustness == "healthy" && healthy == "" {
			healthy = v.Name
		}
	}
	for k, gone := range []string{"pvc-" + uuid("deleted0"), "pvc-" + uuid("deleted1"), healthy} {
		age := time.Duration(6-k) * 24 * time.Hour
		o := &lh.Orphan{ObjectMeta: meta("orphan-"+hex8(gone)+hex8(gone+"x"), map[string]string{lh.LabelNode: "worker-2"}, d.now.Add(-age))}
		o.Spec.NodeID, o.Spec.OrphanType, o.Spec.DataEngine = "worker-2", "replica-data", "v1"
		o.Spec.Parameters = map[string]string{"DataName": gone + "-" + hex8(gone), "DiskName": "default-disk-" + hex8("worker-2")[:4],
			"DiskPath": "/var/lib/longhorn", "DiskUUID": d.diskUUID["worker-2"]}
		o.Status.OwnerID = "worker-2"
		o.Status.Conditions = []lh.Condition{{Type: "DataCleanable", Status: "True"}}
		add(o)
	}
}

func (d *data) objects() []runtime.Object {
	var out []runtime.Object
	for _, n := range d.nodes {
		out = append(out, toU(lh.GVRNode, n))
	}
	for _, v := range d.volumes {
		va := &lh.VolumeAttachment{ObjectMeta: meta(v.Name, map[string]string{lh.LabelVolume: v.Name}, v.CreationTimestamp.Time)}
		va.Spec.Volume = v.Name
		va.Spec.AttachmentTickets = map[string]*lh.AttachmentTicket{}
		if n := v.Status.CurrentNodeID; n != "" {
			id := "csi-" + hex8(v.Name+"csi") + hex8(v.Name+"csi2")
			va.Spec.AttachmentTickets[id] = &lh.AttachmentTicket{ID: id, Type: "csi-attacher", NodeID: n,
				Parameters: map[string]string{"disableFrontend": "false", "lastAttachedBy": ""}}
		}
		out = append(out, toU(lh.GVRVolumeAttachment, va))
		out = append(out, toU(lh.GVRVolume, v), toU(lh.GVREngine, d.engines[v.Name]))
		for _, r := range d.replicas[v.Name] {
			out = append(out, toU(lh.GVRReplica, r))
		}
	}
	for _, o := range d.rest {
		out = append(out, toU(gvrOf(o), o))
	}
	// PVCs and the StorageClass (core objects, for expansion)
	out = append(out, &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "storage.k8s.io/v1", "kind": "StorageClass",
		"metadata":             map[string]any{"name": "longhorn"},
		"provisioner":          "driver.longhorn.io",
		"allowVolumeExpansion": true,
	}})
	for _, v := range d.volumes {
		ks := v.Status.KubernetesStatus
		mode := "ReadWriteOnce"
		if v.Spec.AccessMode == "rwx" {
			mode = "ReadWriteMany"
		}
		size := resource.NewQuantity(int64(v.Spec.Size), resource.BinarySI).String()
		out = append(out, &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "v1", "kind": "PersistentVolumeClaim",
			"metadata": map[string]any{"name": ks.PVCName, "namespace": ks.Namespace, "resourceVersion": "100",
				"labels": fluxLabels(workloadOf(ks.PVCName))},
			"spec": map[string]any{
				"accessModes": []any{mode}, "storageClassName": "longhorn", "volumeName": v.Name,
				"resources": map[string]any{"requests": map[string]any{"storage": size}},
			},
			"status": map[string]any{"phase": "Bound", "capacity": map[string]any{"storage": size}},
		}})
	}
	return append(out, d.workloadObjects()...)
}

// addBackups adds 4 daily backups and the BackupVolume of a volume.
func (d *data) addBackups(add func(runtime.Object), vol, ns, pvc string, size, actual int64, newest time.Duration) {
	var last *lh.Backup
	for k := 3; k >= 0; k-- {
		age := newest + time.Duration(k)*24*time.Hour
		name := "backup-" + hex8(fmt.Sprintf("%sb%d", vol, k)) + hex8(fmt.Sprintf("%sc%d", vol, k))
		b := &lh.Backup{ObjectMeta: meta(name, map[string]string{lh.LabelBackupVolume: vol, "backup-target": "default"}, d.now.Add(-age))}
		b.Spec.SnapshotName = "snap-" + hex8(name)
		b.Status.SnapshotName = b.Spec.SnapshotName
		b.Status.State, b.Status.Progress = "Completed", 100
		b.Status.VolumeName, b.Status.VolumeSize = vol, lh.Quantity(size)
		b.Status.Size = lh.Quantity(actual * 9 / 10)
		b.Status.NewlyUploadDataSize = lh.Quantity(actual / 40)
		b.Status.SnapshotCreatedAt, b.Status.BackupCreatedAt = d.ts(age), d.ts(age-3*time.Minute)
		b.Status.BackupTargetName = "default"
		b.Status.URL = "s3://longhorn-backups@us-east-1/?backup=" + name + "&volume=" + vol
		add(b)
		last = b
	}
	bv := &lh.BackupVolume{ObjectMeta: meta(vol+"-"+hex8(vol+"default"), map[string]string{lh.LabelBackupVolume: vol, "backup-target": "default"}, d.now.Add(-newest-60*24*time.Hour))}
	bv.Spec.BackupTargetName, bv.Spec.VolumeName = "default", vol
	bv.Status.Size, bv.Status.DataStored = lh.Quantity(size), lh.Quantity(actual*11/10)
	bv.Status.LastBackupName, bv.Status.LastBackupAt = last.Name, last.Status.BackupCreatedAt
	bv.Status.CreatedAt, bv.Status.LastSyncedAt = d.ts(newest+60*24*time.Hour), d.ts(2*time.Minute)
	bv.Status.StorageClassName = "longhorn"
	bv.Status.Labels = map[string]string{"type": "bak",
		"KubernetesStatus": fmt.Sprintf(`{"pvName":%q,"pvStatus":"Bound","namespace":%q,"pvcName":%q}`, vol, ns, pvc)}
	add(bv)
}

func gvrOf(o runtime.Object) schema.GroupVersionResource {
	switch o.(type) {
	case *lh.BackupVolume:
		return lh.GVRBackupVolume
	case *lh.Setting:
		return lh.GVRSetting
	case *lh.EngineImage:
		return lh.GVREngineImage
	case *lh.BackupTarget:
		return lh.GVRBackupTarget
	case *lh.RecurringJob:
		return lh.GVRRecurringJob
	case *lh.VolumeSnapshot:
		return lh.GVRSnapshot
	case *lh.Backup:
		return lh.GVRBackup
	case *lh.Orphan:
		return lh.GVROrphan
	}
	panic(fmt.Sprintf("demo: no GVR for %T", o))
}
