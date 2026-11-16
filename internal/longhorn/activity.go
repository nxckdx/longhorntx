package longhorn

import (
	"context"
	"sort"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/watch"
)

// Backups are by far the largest collection (one CR per backup, kept for the
// whole retention: 700 backups ≈ 2.4 MB here) and almost all of them are
// Completed. Instead of an informer we watch "from now": a LIST with limit=1
// yields the collection's resourceVersion, and a watch starting there only
// delivers changes. Running backups update their progress continuously, so
// they show up within seconds even if they started before lhtx.

// finishedLinger is how long a completed/failed backup stays visible.
const finishedLinger = 2 * time.Minute

type trackedBackup struct {
	b          *Backup
	finishedAt time.Time
}

func (s *Store) runBackupWatch(ctx context.Context) {
	res := s.c.res(GVRBackup)
	backoff := time.Second
	for ctx.Err() == nil {
		ul, err := res.List(ctx, metav1.ListOptions{Limit: 1})
		if err != nil {
			s.setErr(err)
			sleepCtx(ctx, backoff)
			backoff = min(backoff*2, time.Minute)
			continue
		}
		rv := ul.GetResourceVersion()
		w, err := res.Watch(ctx, metav1.ListOptions{ResourceVersion: rv, AllowWatchBookmarks: true})
		if err != nil {
			s.setErr(err)
			sleepCtx(ctx, backoff)
			backoff = min(backoff*2, time.Minute)
			continue
		}
		backoff = time.Second
		s.consumeBackupWatch(w)
		w.Stop()
	}
}

func (s *Store) consumeBackupWatch(w watch.Interface) {
	for ev := range w.ResultChan() {
		switch ev.Type {
		case watch.Added, watch.Modified:
			u, ok := ev.Object.(*unstructured.Unstructured)
			if !ok {
				continue
			}
			b, err := convert[Backup](u)
			if err != nil {
				continue
			}
			s.mu.Lock()
			t := s.backups[b.Name]
			if t == nil {
				t = &trackedBackup{}
				s.backups[b.Name] = t
			}
			t.b = b
			if !b.Running() && t.finishedAt.IsZero() {
				t.finishedAt = time.Now()
			}
			s.mu.Unlock()
			s.notify()
		case watch.Deleted:
			if u, ok := ev.Object.(*unstructured.Unstructured); ok {
				s.mu.Lock()
				delete(s.backups, u.GetName())
				s.mu.Unlock()
				s.notify()
			}
		case watch.Error:
			// 410 Gone etc.: restart from a fresh resourceVersion
			if st, ok := ev.Object.(*metav1.Status); ok && !apierrors.IsResourceExpired(&apierrors.StatusError{ErrStatus: *st}) {
				s.setErr(&apierrors.StatusError{ErrStatus: *st})
			}
			return
		}
	}
}

// ActiveBackups returns backups that are running or finished recently,
// newest first.
func (s *Store) ActiveBackups() []*Backup {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	var out []*Backup
	for name, t := range s.backups {
		if !t.finishedAt.IsZero() && now.Sub(t.finishedAt) > finishedLinger {
			delete(s.backups, name)
			continue
		}
		out = append(out, t.b)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreationTimestamp.After(out[j].CreationTimestamp.Time) })
	return out
}

func sleepCtx(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}

// ---------------------------------------------------------------- activity view

// Activity is one long-running operation shown in the activity view.
type Activity struct {
	Kind     string `json:"kind"` // rebuild | purge | restore | clone | backup
	Volume   string `json:"volume"`
	PVC      string `json:"pvc,omitempty"`
	Target   string `json:"target"` // replica, backup name, ...
	Node     string `json:"node,omitempty"`
	Disk     string `json:"disk,omitempty"`
	Progress int    `json:"progress"` // -1 if unknown
	State    string `json:"state,omitempty"`
	Error    string `json:"error,omitempty"`
	Total    int64  `json:"totalBytes"` // basis for rate/ETA
	Done     bool   `json:"done"`
}

// Key identifies an activity across updates (for rate tracking).
func (a Activity) Key() string { return a.Kind + "/" + a.Target }

// BuildActivities collects rebuilds, purges, restores and clones (from the
// engines lhtx is watching) plus backups.
func BuildActivities(views []VolumeView, engines []*Engine, backups []*Backup) []Activity {
	var out []Activity
	byVol := map[string]*VolumeView{}
	for i := range views {
		byVol[views[i].Name] = &views[i]
	}
	for _, v := range views {
		for _, r := range v.Replicas {
			if r.Health != HealthRebuilding {
				continue
			}
			a := Activity{Kind: "rebuild", Volume: v.Name, PVC: v.PVC, Target: r.Name, Node: r.Node, Disk: r.Disk.Path, Progress: -1, Total: v.Size}
			if r.Rebuild != nil {
				a.Progress, a.State, a.Error = r.Rebuild.Progress, r.Rebuild.State, r.Rebuild.Error
			}
			out = append(out, a)
		}
	}
	for _, e := range ActiveEngines(engines) {
		vv := byVol[e.Spec.VolumeName]
		pvc, size := "", int64(e.Spec.VolumeSize)
		if vv != nil {
			pvc = vv.PVC
		}
		for addr, p := range e.Status.PurgeStatus {
			if p.IsPurging {
				out = append(out, Activity{Kind: "purge", Volume: e.Spec.VolumeName, PVC: pvc, Target: addr, Progress: p.Progress, State: p.State, Error: p.Error, Total: size})
			}
		}
		for addr, p := range e.Status.RestoreStatus {
			if p.IsRestoring {
				out = append(out, Activity{Kind: "restore", Volume: e.Spec.VolumeName, PVC: pvc, Target: addr, Progress: p.Progress, State: p.State, Error: p.Error, Total: size})
			}
		}
		for addr, p := range e.Status.CloneStatus {
			if p.IsCloning {
				out = append(out, Activity{Kind: "clone", Volume: e.Spec.VolumeName, PVC: pvc, Target: addr, Progress: p.Progress, State: p.State, Error: p.Error, Total: size})
			}
		}
	}
	for _, b := range backups {
		a := Activity{Kind: "backup", Volume: b.Volume(), Target: b.Name, Progress: b.Status.Progress, State: b.Status.State, Error: b.Status.Error, Total: int64(b.Status.VolumeSize), Done: !b.Running()}
		if vv := byVol[a.Volume]; vv != nil {
			a.PVC = vv.PVC
			if a.Total == 0 {
				a.Total = vv.Size
			}
		}
		out = append(out, a)
	}
	return out
}
