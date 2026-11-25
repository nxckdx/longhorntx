package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"slices"
	"sync"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic/dynamicinformer"
	coordinationv1 "k8s.io/client-go/kubernetes/typed/coordination/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"

	"github.com/nxckdx/longhorntx/internal/longhorn"
	"github.com/nxckdx/longhorntx/internal/switchover"
)

// Agent reconciles Freeze resources.
type Agent struct {
	c        *longhorn.Client
	identity string
	// Poll/Resync are shortened in tests.
	Poll   time.Duration
	Resync time.Duration
	Logf   func(format string, a ...any)

	mu    sync.Mutex
	tasks map[string]*task // Freeze name → running work
	kick  chan string
}

type task struct {
	unfreeze bool
	cancel   context.CancelFunc
	done     chan struct{}
}

// New returns an agent; identity names it in status and the lease.
func New(c *longhorn.Client, identity string) *Agent {
	if identity == "" {
		identity, _ = os.Hostname()
	}
	return &Agent{c: c, identity: identity, Resync: 30 * time.Second, Logf: log.Printf,
		tasks: map[string]*task{}, kick: make(chan string, 64)}
}

// RunWithLeaderElection runs the agent while it holds the lease (one
// active agent; the lease is also the heartbeat lhtx looks at).
func (a *Agent) RunWithLeaderElection(ctx context.Context, namespace string) error {
	cc, err := coordinationv1.NewForConfig(a.c.Cluster.Config)
	if err != nil {
		return err
	}
	lock := &resourcelock.LeaseLock{
		LeaseMeta:  metav1.ObjectMeta{Name: LeaseName, Namespace: namespace},
		Client:     cc,
		LockConfig: resourcelock.ResourceLockConfig{Identity: a.identity},
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var runErr error
	leaderelection.RunOrDie(ctx, leaderelection.LeaderElectionConfig{
		Lock: lock, ReleaseOnCancel: true,
		LeaseDuration: 30 * time.Second, RenewDeadline: 20 * time.Second, RetryPeriod: 5 * time.Second,
		Callbacks: leaderelection.LeaderCallbacks{
			OnStartedLeading: func(ctx context.Context) {
				a.Logf("leading as %s", a.identity)
				runErr = a.Run(ctx)
				cancel()
			},
			OnStoppedLeading: func() {
				a.Logf("lost the lease – stopping")
				cancel()
			},
		},
	})
	return runErr
}

// Run watches Freeze resources until ctx ends.
func (a *Agent) Run(ctx context.Context) error {
	f := dynamicinformer.NewDynamicSharedInformerFactory(a.c.Dynamic(), 0)
	inf := f.ForResource(GVRFreeze).Informer()
	push := func(obj any) {
		if u, ok := obj.(*unstructured.Unstructured); ok {
			a.enqueue(u.GetName())
		} else if t, ok := obj.(cache.DeletedFinalStateUnknown); ok {
			if u, ok := t.Obj.(*unstructured.Unstructured); ok {
				a.enqueue(u.GetName())
			}
		}
	}
	if _, err := inf.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: push, UpdateFunc: func(_, n any) { push(n) }, DeleteFunc: push,
	}); err != nil {
		return err
	}
	f.Start(ctx.Done())
	if !cache.WaitForCacheSync(ctx.Done(), inf.HasSynced) {
		return fmt.Errorf("Freeze resources did not sync (is the CRD installed?)")
	}
	a.Logf("watching freezes.lhtx.io")
	tick := time.NewTicker(a.Resync)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			a.stopAll()
			return nil
		case n := <-a.kick:
			a.reconcile(ctx, n)
		case <-tick.C:
			// watchdog (autoUnfreezeAfter) and retries
			for _, o := range inf.GetStore().List() {
				a.reconcile(ctx, o.(*unstructured.Unstructured).GetName())
			}
		}
	}
}

func (a *Agent) enqueue(name string) {
	select {
	case a.kick <- name:
	default: // full: the next resync catches it
	}
}

func (a *Agent) stopAll() {
	a.mu.Lock()
	ts := make([]*task, 0, len(a.tasks))
	for _, t := range a.tasks {
		t.cancel()
		ts = append(ts, t)
	}
	a.mu.Unlock()
	for _, t := range ts {
		<-t.done
	}
}

func (a *Agent) get(ctx context.Context, name string) (*Freeze, error) {
	u, err := a.c.Dynamic().Resource(GVRFreeze).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	return fromU(u)
}

// reconcile decides what a Freeze needs; the work itself runs in a task.
func (a *Agent) reconcile(ctx context.Context, name string) {
	fr, err := a.get(ctx, name)
	if apierrors.IsNotFound(err) {
		return
	}
	if err != nil {
		a.Logf("%s: %v", name, err)
		return
	}
	a.mu.Lock()
	t := a.tasks[name]
	a.mu.Unlock()
	switch {
	case fr.Deleting:
		if !slices.Contains(fr.Finalizers, Finalizer) {
			return
		}
		if t != nil && !t.unfreeze {
			t.cancel() // freezing: stop, then unfreeze what is there
			<-t.done
			t = nil
		}
		if t == nil {
			a.start(ctx, fr, true)
		}
	case t != nil:
	case fr.Status.Phase == "", fr.Status.Phase == PhasePending, fr.Status.Phase == PhaseFreezing:
		a.start(ctx, fr, false)
	case fr.Status.Phase == PhaseFrozen && fr.Spec.AutoUnfreezeAfter != "" && fr.Status.FrozenAt != nil:
		d := durationOr(fr.Spec.AutoUnfreezeAfter, 0)
		if d > 0 && time.Since(fr.Status.FrozenAt.Time) > d {
			a.Logf("%s: frozen for %s – auto-unfreeze", name, d)
			lg := a.statusLog(fr)
			lg.add("⏰ autoUnfreezeAfter %s reached – ending the freeze", d)
			_ = lg.flush(ctx, nil)
			if err := a.c.Dynamic().Resource(GVRFreeze).Delete(ctx, name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
				a.Logf("%s: auto-unfreeze: %v", name, err)
			}
		}
	}
}

func (a *Agent) start(ctx context.Context, fr *Freeze, unfreeze bool) {
	tctx, cancel := context.WithCancel(ctx)
	t := &task{unfreeze: unfreeze, cancel: cancel, done: make(chan struct{})}
	a.mu.Lock()
	a.tasks[fr.Name] = t
	a.mu.Unlock()
	go func() {
		defer close(t.done)
		defer cancel()
		if unfreeze {
			a.unfreeze(tctx, fr)
		} else {
			a.freeze(tctx, fr)
		}
		a.mu.Lock()
		if a.tasks[fr.Name] == t {
			delete(a.tasks, fr.Name)
		}
		a.mu.Unlock()
		a.enqueue(fr.Name)
	}()
}

func (a *Agent) freeze(ctx context.Context, fr *Freeze) {
	dyn := a.c.Dynamic()
	if !slices.Contains(fr.Finalizers, Finalizer) {
		patch, _ := json.Marshal(map[string]any{"metadata": map[string]any{
			"finalizers": append(slices.Clone(fr.Finalizers), Finalizer)}})
		if _, err := dyn.Resource(GVRFreeze).Patch(ctx, fr.Name, types.MergePatchType, patch, metav1.PatchOptions{FieldManager: "lhtx-agent"}); err != nil {
			a.Logf("%s: add finalizer: %v", fr.Name, err)
			return
		}
	}
	lg := a.statusLog(fr)
	resumed := fr.Status.Phase == PhaseFreezing
	if resumed {
		lg.add("agent %s resumes the freeze", a.identity)
	} else {
		lg.add("agent %s plans the freeze of %d PVCs", a.identity, len(fr.Spec.PVCs))
	}
	_ = lg.flush(ctx, map[string]any{"phase": PhaseFreezing, "policy": PolicyName(fr.Name), "agent": a.identity, "message": ""})
	reason := fr.Spec.Reason
	if reason == "" {
		reason = "Freeze " + fr.Name
	}
	p, err := switchover.PlanFreeze(ctx, a.c, fr.Spec.PVCs, reason)
	if err != nil {
		a.fail(ctx, lg, err)
		return
	}
	stop := lg.autoFlush(ctx)
	f, err := switchover.ApplyFreeze(ctx, a.c, p, switchover.FreezeOptions{
		Name: PolicyName(fr.Name), Request: fr.Name, Log: lg.add,
		Wait: durationOr(fr.Spec.Wait, 10*time.Minute), Poll: a.Poll})
	stop()
	if ctx.Err() != nil {
		return // deleted while freezing (unfreeze follows) or agent stopping (resumes)
	}
	if f == nil {
		a.fail(ctx, lg, err)
		return
	}
	msg := ""
	if err != nil {
		msg = err.Error()
		lg.add("⚠ %s", err)
	}
	lg.add("❄ frozen – delete the Freeze %s to end it (lhtx unfreeze)", fr.Name)
	now := metav1.Now()
	_ = lg.flush(context.WithoutCancel(ctx), map[string]any{"phase": PhaseFrozen, "message": msg, "frozenAt": now, "freeze": f})
}

func (a *Agent) fail(ctx context.Context, lg *statusLog, err error) {
	lg.add("✘ %v", err)
	_ = lg.flush(context.WithoutCancel(ctx), map[string]any{"phase": PhaseFailed, "message": err.Error()})
}

func (a *Agent) unfreeze(ctx context.Context, fr *Freeze) {
	dyn := a.c.Dynamic()
	lg := a.statusLog(fr)
	lg.add("agent %s ends the freeze", a.identity)
	_ = lg.flush(ctx, map[string]any{"phase": PhaseUnfreezing, "agent": a.identity})
	f, err := switchover.GetFreeze(ctx, dyn, PolicyName(fr.Name))
	if err != nil {
		lg.add("✘ %v – retrying", err)
		_ = lg.flush(ctx, nil)
		return
	}
	if f == nil {
		f = fr.Status.Freeze // policy already removed (agent restarted while unfreezing)
	}
	if f != nil {
		stop := lg.autoFlush(ctx)
		err = switchover.Unfreeze(ctx, a.c, *f, switchover.FreezeOptions{Log: lg.add,
			Wait: durationOr(fr.Spec.Wait, 10*time.Minute), Poll: a.Poll})
		stop()
		if ctx.Err() != nil {
			return
		}
		if still, gerr := switchover.GetFreeze(ctx, dyn, PolicyName(fr.Name)); gerr != nil || still != nil {
			lg.add("✘ policy still there (%v) – retrying", firstErr(err, gerr))
			_ = lg.flush(ctx, map[string]any{"message": fmt.Sprint(firstErr(err, gerr))})
			return
		}
		if err != nil {
			lg.add("⚠ %v", err)
		}
	}
	lg.add("✔ unfrozen")
	_ = lg.flush(ctx, nil)
	// the finalizer goes last: the Freeze disappears when everything is done
	cur, err := dyn.Resource(GVRFreeze).Get(ctx, fr.Name, metav1.GetOptions{})
	if err != nil {
		return
	}
	fins := slices.DeleteFunc(cur.GetFinalizers(), func(s string) bool { return s == Finalizer })
	patch, _ := json.Marshal(map[string]any{"metadata": map[string]any{"finalizers": fins, "resourceVersion": cur.GetResourceVersion()}})
	if _, err := dyn.Resource(GVRFreeze).Patch(ctx, fr.Name, types.MergePatchType, patch, metav1.PatchOptions{FieldManager: "lhtx-agent"}); err != nil {
		a.Logf("%s: remove finalizer: %v", fr.Name, err)
	}
}

func firstErr(a, b error) error {
	if a != nil {
		return a
	}
	return b
}

// statusLog collects log lines and writes them to the Freeze status: in
// one patch with a phase change, else at most every 2 s (a status patch
// sends the whole object to everyone watching it).
type statusLog struct {
	a     *Agent
	name  string
	mu    sync.Mutex
	lines []string
	count int
	dirty bool
}

func (a *Agent) statusLog(fr *Freeze) *statusLog {
	return &statusLog{a: a, name: fr.Name, lines: slices.Clone(fr.Status.Log), count: fr.Status.LogCount}
}

func (l *statusLog) add(format string, args ...any) {
	line := time.Now().UTC().Format("15:04:05") + " " + fmt.Sprintf(format, args...)
	l.a.Logf("%s: %s", l.name, line[9:])
	l.mu.Lock()
	l.lines = append(l.lines, line)
	if len(l.lines) > maxLog {
		l.lines = l.lines[len(l.lines)-maxLog:]
	}
	l.count++
	l.dirty = true
	l.mu.Unlock()
}

func (l *statusLog) flush(ctx context.Context, fields map[string]any) error {
	l.mu.Lock()
	if !l.dirty && fields == nil {
		l.mu.Unlock()
		return nil
	}
	st := map[string]any{"log": slices.Clone(l.lines), "logCount": l.count}
	l.dirty = false
	l.mu.Unlock()
	for k, v := range fields {
		st[k] = v
	}
	patch, err := json.Marshal(map[string]any{"status": st})
	if err != nil {
		return err
	}
	_, err = l.a.c.Dynamic().Resource(GVRFreeze).Patch(ctx, l.name, types.MergePatchType, patch,
		metav1.PatchOptions{FieldManager: "lhtx-agent"}, "status")
	if err != nil && !apierrors.IsNotFound(err) {
		l.a.Logf("%s: status: %v", l.name, err)
	}
	return err
}

// autoFlush writes new lines every 2 s until stop is called.
func (l *statusLog) autoFlush(ctx context.Context) (stop func()) {
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		t := time.NewTicker(2 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-t.C:
				_ = l.flush(ctx, nil)
			}
		}
	}()
	return func() { close(done); wg.Wait() }
}
