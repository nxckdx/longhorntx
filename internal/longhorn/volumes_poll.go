package longhorn

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/cache"
)

// Volume refresh modes.
//
// Measured on a large cluster (878 volumes): the volume watch costs ~10 MiB/min
// because every write to a volume updates status.actualSize (plus the
// managedFields timestamp) and each event carries the whole object
// (~6 KiB, uncompressed). A gzip-compressed LIST of all volumes is ~430 KiB,
// so re-listing every 30 s costs ~0.85 MiB/min.
//
// In poll mode lhtx still reacts immediately to what matters:
//   - replicas stay watched (practically silent); a replica change triggers a
//     GET of just that volume
//   - a volume a caller holds (detail view, see AcquireEngine) gets its own
//     watch with a name field selector
const (
	RefreshAuto  time.Duration = -1 // decide by volume count
	RefreshWatch time.Duration = 0
	// AutoPollThreshold: from this many volumes on, auto mode polls.
	AutoPollThreshold = 150
	// DefaultPollInterval is used by auto mode.
	DefaultPollInterval = 30 * time.Second
	// maxQueuedGets: above this many changed volumes at once, a full
	// re-list is cheaper than individual GETs.
	maxQueuedGets = 20
)

// ParseRefresh parses "auto", "watch" or a duration ("30s", "2m").
func ParseRefresh(s string) (time.Duration, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "auto":
		return RefreshAuto, nil
	case "watch", "live", "0":
		return RefreshWatch, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil || d < 5*time.Second {
		return 0, fmt.Errorf("invalid refresh %q: auto, watch or a duration ≥ 5s (e.g. 30s, 2m)", s)
	}
	return d, nil
}

// decideRefresh resolves RefreshAuto with a cheap LIST limit=1, whose
// metadata.remainingItemCount tells the collection size.
func (s *Store) decideRefresh(ctx context.Context) time.Duration {
	if s.Refresh != RefreshAuto {
		return s.Refresh
	}
	ul, err := s.c.res(GVRVolume).List(ctx, metav1.ListOptions{Limit: 1})
	if err != nil {
		return RefreshWatch // the informer will surface the error
	}
	n := int64(len(ul.Items))
	if r := ul.GetRemainingItemCount(); r != nil {
		n += *r
	}
	if n >= AutoPollThreshold {
		return DefaultPollInterval
	}
	return RefreshWatch
}

// RefreshInterval returns the effective volume refresh (0 = live watch);
// valid after the volumes are synced.
func (s *Store) RefreshInterval() time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.refresh
}

// LastRefresh is when the volume list was last loaded (poll mode).
func (s *Store) LastRefresh() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastPoll
}

// RefreshNow triggers an immediate re-list of the volumes (poll mode).
func (s *Store) RefreshNow() {
	select {
	case s.pollNow <- struct{}{}:
	default:
	}
}

func (s *Store) runVolumePoll(interval time.Duration, done chan struct{}) {
	res := s.c.res(GVRVolume)
	first := true
	for s.ctx.Err() == nil {
		ul, err := res.List(s.ctx, metav1.ListOptions{ResourceVersion: "0"})
		wait := interval
		if err != nil {
			s.setErr(fmt.Errorf("list volumes: %w", err))
			wait = min(interval, 10*time.Second)
		} else {
			items := make([]any, 0, len(ul.Items))
			for i := range ul.Items {
				if v, err := convert[Volume](&ul.Items[i]); err == nil {
					items = append(items, v)
				}
			}
			s.mergeVolumeList(items, ul.GetResourceVersion())
			s.mu.Lock()
			s.lastPoll = time.Now()
			if first {
				s.synced[ResVolumes] = time.Since(s.startedAt)
			}
			s.lastErr = nil
			s.mu.Unlock()
			if first {
				close(done)
				first = false
			}
			s.notify()
		}
		select {
		case <-s.ctx.Done():
			return
		case <-time.After(wait):
		case <-s.pollNow:
		}
	}
}

// mergeVolumeList replaces the volume cache, but keeps objects that are
// newer in the cache (from a GET or a single-volume watch) than in a list
// that may have been served slightly stale from the apiserver cache.
func (s *Store) mergeVolumeList(items []any, rv string) {
	for i, it := range items {
		v := it.(*Volume)
		if cur, ok, _ := s.volStore.GetByKey(v.Namespace + "/" + v.Name); ok {
			if c := cur.(*Volume); newerRV(c.ResourceVersion, v.ResourceVersion) {
				items[i] = c
			}
		}
	}
	_ = s.volStore.Replace(items, rv)
}

// newerRV compares resourceVersions numerically (etcd revisions). Kubernetes
// says RVs are opaque, but for etcd-backed resources they are integers; on
// anything unexpected we treat the list as authoritative.
func newerRV(a, b string) bool {
	if len(a) != len(b) {
		return len(a) > len(b)
	}
	return a > b
}

// queueVolumeGet schedules a GET of a volume after a replica change
// (poll mode). GETs are coalesced for a second.
func (s *Store) queueVolumeGet(vol string) {
	if vol == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.refresh == RefreshWatch || s.ctx == nil {
		return
	}
	if s.pendingGets == nil {
		s.pendingGets = map[string]bool{}
		time.AfterFunc(time.Second, s.flushVolumeGets)
	}
	s.pendingGets[vol] = true
}

func (s *Store) flushVolumeGets() {
	s.mu.Lock()
	vols := s.pendingGets
	s.pendingGets = nil
	s.mu.Unlock()
	if len(vols) > maxQueuedGets {
		s.RefreshNow()
		return
	}
	names := make([]string, 0, len(vols))
	for v := range vols {
		names = append(names, v)
	}
	sort.Strings(names)
	for _, name := range names {
		v, err := s.c.Volume(s.ctx, name)
		if err != nil {
			if s.ctx.Err() != nil {
				return
			}
			continue // deleted volumes disappear with the next list
		}
		_ = s.volStore.Update(v)
	}
	s.notify()
}

// startSingleVolumeWatch keeps one volume live in poll mode (detail view).
func (s *Store) startSingleVolumeWatch(ctx context.Context, volume string) {
	inf := newInformerSel[Volume](s, GVRVolume, "", "metadata.name="+volume)
	upd := func(obj any) {
		if v, ok := obj.(*Volume); ok {
			_ = s.volStore.Update(v)
			s.notify()
		}
	}
	_, _ = inf.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    upd,
		UpdateFunc: func(_, n any) { upd(n) },
	})
	go inf.RunWithContext(ctx)
}
