package agent

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/tools/cache"

	"github.com/nxckdx/longhorntx/internal/format"
	"github.com/nxckdx/longhorntx/internal/longhorn"
)

// The agent's history recorder (see longhorn/history.go): it watches
// volumes, replicas and backups inside the cluster and turns changes into
// history events. Only changes seen while it runs are recorded (an agent
// that was down misses what happened meanwhile).

// volumeEvents: robustness changes (only those involving degraded/faulted –
// healthy ↔ unknown is just attach/detach), expansions, engine upgrades.
func volumeEvents(old, new *longhorn.Volume, now time.Time) []longhorn.HistoryEvent {
	var out []longhorn.HistoryEvent
	add := func(kind, text string) {
		out = append(out, longhorn.HistoryEvent{At: now, Kind: kind, Text: text})
	}
	or, nr := old.Status.Robustness, new.Status.Robustness
	bad := func(r string) bool { return r == "degraded" || r == "faulted" }
	if or != nr && (bad(or) || bad(nr)) {
		add(longhorn.HistRobustness, fmt.Sprintf("%s → %s", orDash(or), nr))
	}
	if os, ns := int64(old.Spec.Size), int64(new.Spec.Size); ns > os && os > 0 {
		add(longhorn.HistExpanded, fmt.Sprintf("%s → %s", format.Bytes(os), format.Bytes(ns)))
	}
	if oi, ni := old.Status.CurrentImage, new.Status.CurrentImage; oi != ni && oi != "" && ni != "" {
		add(longhorn.HistEngine, fmt.Sprintf("engine %s → %s", shortImage(oi), shortImage(ni)))
	}
	return out
}

// replicaEvents: a rebuild started (a new replica of a volume that existed
// before), finished (healthyAt set, with the duration), a replica failed.
func replicaEvents(old, new *longhorn.Replica, vol *longhorn.Volume, now time.Time) []longhorn.HistoryEvent {
	where := new.Spec.NodeID
	if new.Spec.DiskPath != "" {
		where += ":" + new.Spec.DiskPath
	}
	// replicas created with the volume are not rebuilds – unless they failed
	// later and are rebuilt in place (Longhorn reuses a replica after a node
	// restart: same object, lastFailedAt set)
	failed, _ := time.Parse(time.RFC3339, new.Spec.LastFailedAt)
	reused := !failed.IsZero() && failed.After(new.CreationTimestamp.Time)
	rebuild := vol == nil || reused || new.CreationTimestamp.Sub(vol.CreationTimestamp.Time) > 2*time.Minute
	var out []longhorn.HistoryEvent
	// the start: once the new replica is scheduled (it has no node when
	// it is created)
	scheduled := new.Spec.NodeID != "" && (old == nil || old.Spec.NodeID == "")
	switch {
	case scheduled && new.Spec.HealthyAt == "":
		if rebuild && vol != nil {
			out = append(out, longhorn.HistoryEvent{At: now, Kind: longhorn.HistRebuildStart, Text: "rebuilding a replica on " + where})
		}
	case old == nil:
	case old.Spec.FailedAt != "" && new.Spec.FailedAt == "" && new.Spec.HealthyAt == "" && new.Spec.NodeID != "":
		// a failed replica is being rebuilt in place
		out = append(out, longhorn.HistoryEvent{At: now, Kind: longhorn.HistRebuildStart, Text: "rebuilding the replica on " + where})
	case old.Spec.HealthyAt == "" && new.Spec.HealthyAt != "" && rebuild:
		e := longhorn.HistoryEvent{At: now, Kind: longhorn.HistRebuildDone, Text: "replica on " + where + " rebuilt"}
		// from its creation, or – rebuilt in place – from its failure
		start := new.CreationTimestamp.Time
		if reused {
			start = failed
		}
		if t, err := time.Parse(time.RFC3339, new.Spec.HealthyAt); err == nil && !start.IsZero() && t.After(start) {
			e.Took = t.Sub(start).Round(time.Second)
		}
		out = append(out, e)
	}
	if old != nil && old.Spec.FailedAt == "" && new.Spec.FailedAt != "" {
		out = append(out, longhorn.HistoryEvent{At: now, Kind: longhorn.HistReplicaFailed, Text: "replica on " + where + " failed"})
	}
	return out
}

// backupEvents: a backup completed or failed.
func backupEvents(old, new *longhorn.Backup, now time.Time) []longhorn.HistoryEvent {
	if old.Status.State == new.Status.State {
		return nil
	}
	var took time.Duration
	if !new.CreationTimestamp.IsZero() {
		took = now.Sub(new.CreationTimestamp.Time).Round(time.Second)
	}
	switch new.Status.State {
	case "Completed":
		return []longhorn.HistoryEvent{{At: now, Kind: longhorn.HistBackup, Took: took,
			Text: fmt.Sprintf("backup %s completed (%s, %s uploaded)", new.Name, format.Bytes(int64(new.Status.Size)),
				format.Bytes(int64(new.Status.NewlyUploadDataSize)))}}
	case "Error":
		msg := new.Status.Error
		if len(msg) > 160 {
			msg = msg[:157] + "…"
		}
		return []longhorn.HistoryEvent{{At: now, Kind: longhorn.HistBackupFailed, Took: took,
			Text: fmt.Sprintf("backup %s failed: %s", new.Name, orDash(msg))}}
	}
	return nil
}

func shortImage(s string) string {
	if i := lastIndex(s, ':'); i >= 0 {
		return s[i+1:]
	}
	return s
}

func lastIndex(s string, c byte) int {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == c {
			return i
		}
	}
	return -1
}

// recorder buffers events and writes them (at most every flushEvery).
type recorder struct {
	a       *Agent
	mu      sync.Mutex
	pending map[string][]longhorn.HistoryEvent // volume → new events
	cluster []longhorn.HistoryEvent
	known   map[string]histEntry // what is written, by object name
	vols    map[string]*longhorn.Volume
}

type histEntry struct {
	events []longhorn.HistoryEvent
	rv     string
}

func (r *recorder) record(vol *longhorn.Volume, evs []longhorn.HistoryEvent) {
	if len(evs) == 0 || vol == nil || vol.Labels[longhorn.LabelRestoreProbe] != "" {
		return // the restore probe's test volumes have no history
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, e := range evs {
		if notifiable(e) {
			r.a.notifier.add(notice{At: e.At, Kind: e.Kind, Volume: vol.Name, PVC: vol.PVC(), Text: e.Text, Bad: e.Bad()})
		}
		r.pending[vol.Name] = append(r.pending[vol.Name], e)
		if e.Notable() {
			c := e
			c.Volume, c.PVC = vol.Name, vol.PVC()
			r.cluster = append(r.cluster, c)
		}
	}
	r.vols[vol.Name] = vol
	r.a.metrics.historyEvents.Add(int64(len(evs)))
	for _, e := range evs {
		c, _ := r.a.metrics.historyKinds.LoadOrStore(e.Kind, new(atomic.Int64))
		c.(*atomic.Int64).Add(1)
		if e.Kind == longhorn.HistRebuildDone && e.Took > 0 {
			r.a.metrics.rebuildSecs.Add(int64(e.Took.Seconds()))
			r.a.metrics.rebuilds.Add(1)
		}
	}
}

// flush writes the pending events: one update per volume touched, and the
// cluster history once.
func (r *recorder) flush(ctx context.Context) {
	r.mu.Lock()
	pending, cluster, vols := r.pending, r.cluster, r.vols
	r.pending, r.cluster, r.vols = map[string][]longhorn.HistoryEvent{}, nil, map[string]*longhorn.Volume{}
	r.mu.Unlock()
	names := make([]string, 0, len(pending))
	for n := range pending {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		if err := r.write(ctx, n, vols[n], pending[n], r.a.historyVolumeMax()); err != nil {
			r.a.Logf("history %s: %v", n, err)
		}
	}
	if len(cluster) > 0 {
		if err := r.write(ctx, longhorn.ClusterHistory, nil, cluster, r.a.historyClusterMax()); err != nil {
			r.a.Logf("cluster history: %v", err)
		}
	}
}

func (r *recorder) write(ctx context.Context, name string, owner *longhorn.Volume, add []longhorn.HistoryEvent, max int) error {
	res := r.a.c.Dynamic().Resource(longhorn.GVRVolumeHistory).Namespace(r.a.c.Namespace)
	r.mu.Lock()
	cur, had := r.known[name]
	r.mu.Unlock()
	events := r.a.trimHistory(append(append([]longhorn.HistoryEvent{}, cur.events...), add...), max)
	obj, err := longhorn.HistoryObject(name, r.a.c.Namespace, owner, events, max)
	if err != nil {
		return err
	}
	var out *unstructured.Unstructured
	if had {
		obj.SetResourceVersion(cur.rv)
		out, err = res.Update(ctx, obj, metav1.UpdateOptions{FieldManager: "lhtx-agent"})
	}
	if !had || apierrors.IsNotFound(err) {
		obj.SetResourceVersion("")
		out, err = res.Create(ctx, obj, metav1.CreateOptions{FieldManager: "lhtx-agent"})
	}
	if apierrors.IsConflict(err) || apierrors.IsAlreadyExists(err) {
		// someone else wrote it: start from what is there
		if u, gerr := res.Get(ctx, name, metav1.GetOptions{}); gerr == nil {
			old, _ := longhorn.DecodeHistory(u)
			events = r.a.trimHistory(append(old, add...), max)
			if obj, err = longhorn.HistoryObject(name, r.a.c.Namespace, owner, events, max); err == nil {
				obj.SetResourceVersion(u.GetResourceVersion())
				out, err = res.Update(ctx, obj, metav1.UpdateOptions{FieldManager: "lhtx-agent"})
			}
		}
	}
	if err != nil {
		return err
	}
	r.mu.Lock()
	r.known[name] = histEntry{events: events, rv: out.GetResourceVersion()}
	r.mu.Unlock()
	return nil
}

// runHistory watches volumes, replicas and backups and records history.
func (a *Agent) runHistory(ctx context.Context) error {
	dyn := a.c.Dynamic()
	res := dyn.Resource(longhorn.GVRVolumeHistory).Namespace(a.c.Namespace)
	l, err := res.List(ctx, metav1.ListOptions{})
	if err != nil {
		a.Logf("volumehistories.lhtx.io not available (%v) – no history", err)
		return nil
	}
	r := &recorder{a: a, pending: map[string][]longhorn.HistoryEvent{}, known: map[string]histEntry{}, vols: map[string]*longhorn.Volume{}}
	a.history.Store(r)
	defer a.history.Store(nil)
	for i := range l.Items {
		if ev, err := longhorn.DecodeHistory(&l.Items[i]); err == nil {
			r.known[l.Items[i].GetName()] = histEntry{events: ev, rv: l.Items[i].GetResourceVersion()}
		}
	}

	f := dynamicinformer.NewFilteredDynamicSharedInformerFactory(dyn, 0, a.c.Namespace, nil)
	vinf := f.ForResource(longhorn.GVRVolume).Informer()
	rinf := f.ForResource(longhorn.GVRReplica).Informer()
	binf := f.ForResource(longhorn.GVRBackup).Informer()
	volume := func(name string) *longhorn.Volume {
		o, ok, _ := vinf.GetStore().GetByKey(a.c.Namespace + "/" + name)
		if !ok {
			return nil
		}
		v, _ := longhorn.ConvertVolume(o.(*unstructured.Unstructured))
		return v
	}
	var synced bool
	var syncMu sync.RWMutex
	ready := func() bool { syncMu.RLock(); defer syncMu.RUnlock(); return synced }

	_, _ = vinf.AddEventHandler(cache.ResourceEventHandlerFuncs{UpdateFunc: func(o, n any) {
		if !ready() {
			return
		}
		ov, err1 := longhorn.ConvertVolume(o.(*unstructured.Unstructured))
		nv, err2 := longhorn.ConvertVolume(n.(*unstructured.Unstructured))
		if err1 == nil && err2 == nil {
			r.record(nv, volumeEvents(ov, nv, time.Now()))
		}
	}})
	_, _ = rinf.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(n any) {
			if !ready() {
				return
			}
			if nr, err := longhorn.ConvertReplica(n.(*unstructured.Unstructured)); err == nil {
				vol := volume(nr.Spec.VolumeName)
				r.record(vol, replicaEvents(nil, nr, vol, time.Now()))
			}
		},
		UpdateFunc: func(o, n any) {
			if !ready() {
				return
			}
			or, err1 := longhorn.ConvertReplica(o.(*unstructured.Unstructured))
			nr, err2 := longhorn.ConvertReplica(n.(*unstructured.Unstructured))
			if err1 == nil && err2 == nil {
				vol := volume(nr.Spec.VolumeName)
				r.record(vol, replicaEvents(or, nr, vol, time.Now()))
			}
		},
	})
	_, _ = binf.AddEventHandler(cache.ResourceEventHandlerFuncs{UpdateFunc: func(o, n any) {
		if !ready() {
			return
		}
		ob, err1 := longhorn.ConvertBackup(o.(*unstructured.Unstructured))
		nb, err2 := longhorn.ConvertBackup(n.(*unstructured.Unstructured))
		if err1 == nil && err2 == nil {
			r.record(volume(nb.Volume()), backupEvents(ob, nb, time.Now()))
		}
	}})
	f.Start(ctx.Done())
	if !cache.WaitForCacheSync(ctx.Done(), vinf.HasSynced, rinf.HasSynced, binf.HasSynced) {
		return nil
	}
	syncMu.Lock()
	synced = true // what exists now is not history: only changes from here on
	syncMu.Unlock()
	a.Logf("recording history")

	t := time.NewTicker(a.historyFlush())
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			r.flush(context.WithoutCancel(ctx))
			return nil
		case <-t.C:
			r.flush(ctx)
		}
	}
}

func (a *Agent) historyFlush() time.Duration {
	if a.HistoryFlush > 0 {
		return a.HistoryFlush
	}
	return 5 * time.Second
}

// Upper bounds for the configurable history sizes: a VolumeHistory must stay
// a small object (etcd limit ~1.5 MB; an event is ~40 bytes gzipped).
const (
	maxHistoryVolumeLimit  = 1000
	maxHistoryClusterLimit = 5000
)

func clampHistory(n, def, limit int) int {
	switch {
	case n <= 0:
		return def
	case n > limit:
		return limit
	}
	return n
}

func (a *Agent) historyVolumeMax() int {
	return clampHistory(a.HistoryVolumeMax, longhorn.MaxVolumeHistory, maxHistoryVolumeLimit)
}

func (a *Agent) historyClusterMax() int {
	return clampHistory(a.HistoryClusterMax, longhorn.MaxClusterHistory, maxHistoryClusterLimit)
}

// trimHistory keeps the newest max events (events are oldest first) and,
// with HistoryMaxAge, none older than that.
func (a *Agent) trimHistory(events []longhorn.HistoryEvent, max int) []longhorn.HistoryEvent {
	if a.HistoryMaxAge > 0 {
		cut := time.Now().Add(-a.HistoryMaxAge)
		kept := events[:0:0]
		for _, e := range events {
			if !e.At.Before(cut) {
				kept = append(kept, e)
			}
		}
		events = kept
	}
	if len(events) > max {
		events = events[len(events)-max:]
	}
	return events
}
