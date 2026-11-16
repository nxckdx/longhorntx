package longhorn

import (
	"context"
	"fmt"
	"sync"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/tools/cache"
)

// Store keeps live caches of Longhorn objects via watches (no polling).
//
// Traffic strategy:
//   - volumes, replicas, nodes are watched permanently. Replicas and nodes
//     are quiet; volumes update on writes (status.actualSize).
//   - engines are the chattiest objects (every write updates the volume-head
//     snapshot size) and are only watched per volume, on demand:
//     while a caller holds a reference (e.g. the TUI detail view) or while a
//     replica of that volume looks like it is rebuilding.
type Store struct {
	c *Client

	volumes, replicas, nodes cache.SharedIndexInformer

	mu      sync.Mutex
	engines map[string]*engineWatch // by volume
	backups map[string]*trackedBackup
	lastErr error

	startedAt time.Time
	synced    map[string]time.Duration // resource -> time to initial sync

	// WatchBackups enables the watch-from-now on backups (see activity.go).
	// Set before Start.
	WatchBackups bool

	ctx     context.Context
	cancel  context.CancelFunc
	changed chan struct{}
}

type engineWatch struct {
	inf    cache.SharedIndexInformer
	cancel context.CancelFunc
	refs   int  // explicit references
	auto   bool // held because of a rebuild
}

func NewStore(c *Client) *Store {
	s := &Store{
		c:       c,
		engines: map[string]*engineWatch{},
		backups: map[string]*trackedBackup{},
		synced:  map[string]time.Duration{},
		changed: make(chan struct{}, 1),
	}
	s.volumes = newInformer[Volume](s, GVRVolume, "")
	s.replicas = newInformer[Replica](s, GVRReplica, "")
	s.nodes = newInformer[Node](s, GVRNode, "")
	return s
}

// Changed delivers a (coalesced) signal whenever any cached object changed.
func (s *Store) Changed() <-chan struct{} { return s.changed }

func (s *Store) notify() {
	select {
	case s.changed <- struct{}{}:
	default:
	}
}

// Resources watched permanently, in the order they are shown while loading.
const (
	ResVolumes  = "volumes"
	ResReplicas = "replicas"
	ResNodes    = "nodes"
)

var StoreResources = []string{ResVolumes, ResReplicas, ResNodes}

// Start runs the informers and returns immediately. Each resource becomes
// available on its own (see Synced); every completed initial list triggers
// a Changed signal, so a UI can show volumes before replicas have arrived.
func (s *Store) Start(ctx context.Context) {
	s.ctx, s.cancel = context.WithCancel(ctx)
	s.startedAt = time.Now()
	// On a slow link all lists share the bandwidth. Volumes are what the
	// user sees first, so replicas (the largest list) start only once the
	// volumes are in; nodes are small and run alongside.
	volumesDone := make(chan struct{})
	run := func(name string, inf cache.SharedIndexInformer, after <-chan struct{}, done chan struct{}) {
		if after != nil {
			select {
			case <-after:
			case <-s.ctx.Done():
				return
			}
		}
		go inf.RunWithContext(s.ctx)
		if !cache.WaitForCacheSync(s.ctx.Done(), inf.HasSynced) {
			return
		}
		s.mu.Lock()
		s.synced[name] = time.Since(s.startedAt)
		s.mu.Unlock()
		if done != nil {
			close(done)
		}
		if name == ResReplicas {
			s.reconcileAutoEngines()
			// backups come last: nice to have, should not slow down startup
			if s.WatchBackups {
				go s.runBackupWatch(s.ctx)
			}
		}
		s.notify()
	}
	go run(ResVolumes, s.volumes, nil, volumesDone)
	go run(ResNodes, s.nodes, nil, nil)
	go run(ResReplicas, s.replicas, volumesDone, nil)
}

func (s *Store) informers() map[string]cache.SharedIndexInformer {
	return map[string]cache.SharedIndexInformer{ResVolumes: s.volumes, ResReplicas: s.replicas, ResNodes: s.nodes}
}

// Synced reports whether the initial list of a resource has completed and
// how long it took.
func (s *Store) Synced(res string) (time.Duration, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, ok := s.synced[res]
	return d, ok
}

// AllSynced reports whether volumes, replicas and nodes are loaded.
func (s *Store) AllSynced() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.synced) == len(StoreResources)
}

// StartedAt is when Start was called.
func (s *Store) StartedAt() time.Time { return s.startedAt }

// WaitSynced blocks until all permanent resources are loaded (for the CLI).
func (s *Store) WaitSynced(ctx context.Context, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	for !s.AllSynced() {
		select {
		case <-ctx.Done():
			if err := s.Err(); err != nil {
				return err
			}
			return fmt.Errorf("timed out waiting for Longhorn objects (is Longhorn installed in %q?)", s.c.Namespace)
		case <-time.After(50 * time.Millisecond):
		}
	}
	return nil
}

func (s *Store) Stop() {
	if s.cancel != nil {
		s.cancel()
	}
}

// Err returns the last watch error, if any (e.g. RBAC, connection loss).
func (s *Store) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastErr
}

func (s *Store) setErr(err error) {
	s.mu.Lock()
	s.lastErr = err
	s.mu.Unlock()
	s.notify()
}

// AcquireEngine starts watching the engine of a volume (if not already) and
// returns a release func.
func (s *Store) AcquireEngine(volume string) (release func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	w := s.engineLocked(volume)
	w.refs++
	var once sync.Once
	return func() {
		once.Do(func() {
			s.mu.Lock()
			defer s.mu.Unlock()
			w.refs--
			s.maybeStopLocked(volume, w)
		})
	}
}

// WatchedEngines returns the volumes whose engines are currently watched.
func (s *Store) WatchedEngines() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.engines))
	for v := range s.engines {
		out = append(out, v)
	}
	return out
}

func (s *Store) engineLocked(volume string) *engineWatch {
	if w, ok := s.engines[volume]; ok {
		return w
	}
	ctx, cancel := context.WithCancel(s.ctx)
	w := &engineWatch{inf: newInformer[Engine](s, GVREngine, LabelVolume+"="+volume), cancel: cancel}
	s.engines[volume] = w
	go w.inf.RunWithContext(ctx)
	return w
}

func (s *Store) maybeStopLocked(volume string, w *engineWatch) {
	if w.refs <= 0 && !w.auto {
		w.cancel()
		delete(s.engines, volume)
	}
}

// reconcileAutoEngines watches engines of volumes that appear to rebuild.
func (s *Store) reconcileAutoEngines() {
	want := map[string]bool{}
	for _, o := range s.replicas.GetStore().List() {
		r, ok := o.(*Replica)
		if ok && r.LikelyRebuilding() {
			want[r.Spec.VolumeName] = true
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ctx == nil {
		return
	}
	for v := range want {
		w := s.engineLocked(v)
		w.auto = true
	}
	for v, w := range s.engines {
		if w.auto && !want[v] {
			w.auto = false
			s.maybeStopLocked(v, w)
		}
	}
}

// State returns the current cache contents.
func (s *Store) State() State {
	snap := State{
		Volumes:  listStore[Volume](s.volumes),
		Replicas: listStore[Replica](s.replicas),
		Nodes:    listStore[Node](s.nodes),
	}
	s.mu.Lock()
	for _, w := range s.engines {
		snap.Engines = append(snap.Engines, listStore[Engine](w.inf)...)
	}
	s.mu.Unlock()
	return snap
}

func listStore[T any](inf cache.SharedIndexInformer) []*T {
	items := inf.GetStore().List()
	out := make([]*T, 0, len(items))
	for _, o := range items {
		if t, ok := o.(*T); ok {
			out = append(out, t)
		}
	}
	return out
}

func newInformer[T any](s *Store, g schema.GroupVersionResource, selector string) cache.SharedIndexInformer {
	res := s.c.res(g)
	lw := &cache.ListWatch{
		ListWithContextFunc: func(ctx context.Context, o metav1.ListOptions) (runtime.Object, error) {
			o.LabelSelector = selector
			return res.List(ctx, o)
		},
		WatchFuncWithContext: func(ctx context.Context, o metav1.ListOptions) (watch.Interface, error) {
			o.LabelSelector = selector
			return res.Watch(ctx, o)
		},
	}
	inf := cache.NewSharedIndexInformer(&plainListWatch{lw}, &unstructured.Unstructured{}, 0, cache.Indexers{})
	// Convert to slim structs right away: the cache then holds only the
	// fields we model instead of full unstructured maps (incl. managedFields).
	_ = inf.SetTransform(func(obj any) (any, error) {
		if u, ok := obj.(*unstructured.Unstructured); ok {
			return convert[T](u)
		}
		return obj, nil
	})
	_ = inf.SetWatchErrorHandler(func(_ *cache.Reflector, err error) {
		s.setErr(fmt.Errorf("watch %s: %w", g.Resource, err))
	})
	isReplica := g == GVRReplica
	onChange := func() {
		s.mu.Lock()
		s.lastErr = nil // events flow again
		s.mu.Unlock()
		if isReplica {
			s.reconcileAutoEngines()
		}
		s.notify()
	}
	_, _ = inf.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(any) { onChange() },
		UpdateFunc: func(any, any) { onChange() },
		DeleteFunc: func(any) { onChange() },
	})
	return inf
}

// plainListWatch opts out of the WatchList feature (streaming the initial
// state over a watch, default since client-go 1.35). Watch streams are not
// gzip-compressed by the apiserver while a plain LIST is, which makes the
// initial sync several times cheaper on the wire.
type plainListWatch struct{ *cache.ListWatch }

func (plainListWatch) IsWatchListSemanticsUnSupported() bool { return true }
