package agent

import (
	"context"
	"sync"
	"time"

	"golang.org/x/time/rate"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"

	"github.com/nxckdx/longhorntx/internal/longhorn"
)

// runDigests keeps a VolumeDigest per volume (see longhorn/digest.go): the
// volumes are watched here, inside the cluster, where the churn costs
// nothing; a digest is written only when something lhtx shows changed.
// Writes are limited (DigestQPS) so a burst – a node failure degrading
// hundreds of volumes – does not hammer the API server.
func (a *Agent) runDigests(ctx context.Context) error {
	dyn := a.c.Dynamic()
	res := dyn.Resource(longhorn.GVRVolumeDigest).Namespace(a.c.Namespace)
	if _, err := res.List(ctx, metav1.ListOptions{Limit: 1}); err != nil {
		a.Logf("volumedigests.lhtx.io not available (%v) – no digests", err)
		return nil
	}
	// what is written now, from the existing digests
	type entry struct {
		v  *longhorn.Volume
		rv string // of the digest
	}
	written := map[string]entry{}
	var mu sync.Mutex
	l, err := res.List(ctx, metav1.ListOptions{})
	if err != nil {
		return err
	}
	for i := range l.Items {
		if v, err := longhorn.VolumeFromDigest(&l.Items[i]); err == nil {
			written[l.Items[i].GetName()] = entry{v, l.Items[i].GetResourceVersion()}
		}
	}

	f := dynamicinformer.NewFilteredDynamicSharedInformerFactory(dyn, 0, a.c.Namespace, nil)
	inf := f.ForResource(longhorn.GVRVolume).Informer()
	q := workqueue.NewTypedRateLimitingQueue(workqueue.NewTypedMaxOfRateLimiter(
		workqueue.NewTypedItemExponentialFailureRateLimiter[string](time.Second, 5*time.Minute),
		&workqueue.TypedBucketRateLimiter[string]{Limiter: newLimiter(a.DigestQPS)},
	))
	defer q.ShutDown()
	push := func(obj any) {
		if u, ok := obj.(*unstructured.Unstructured); ok {
			q.AddRateLimited(u.GetName())
		}
	}
	if _, err := inf.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: push, UpdateFunc: func(_, n any) { a.metrics.volumeEvents.Add(1); push(n) },
	}); err != nil {
		return err
	}
	// a digest deleted by someone is written again at once
	dinf := f.ForResource(longhorn.GVRVolumeDigest).Informer()
	if _, err := dinf.AddEventHandler(cache.ResourceEventHandlerFuncs{
		DeleteFunc: func(obj any) {
			u, ok := obj.(*unstructured.Unstructured)
			if t, isT := obj.(cache.DeletedFinalStateUnknown); isT {
				u, ok = t.Obj.(*unstructured.Unstructured)
			}
			if !ok {
				return
			}
			mu.Lock()
			delete(written, u.GetName())
			mu.Unlock()
			q.Add(u.GetName())
		},
	}); err != nil {
		return err
	}
	f.Start(ctx.Done())
	if !cache.WaitForCacheSync(ctx.Done(), inf.HasSynced, dinf.HasSynced) {
		return nil
	}
	a.Logf("keeping volume digests (%d volumes)", len(inf.GetStore().ListKeys()))

	sync := func(name string) error {
		o, ok, err := inf.GetStore().GetByKey(a.c.Namespace + "/" + name)
		if err != nil || !ok {
			mu.Lock()
			delete(written, name) // the owner reference removes the digest
			a.metrics.digestVolumes.Store(int64(len(written)))
			mu.Unlock()
			return err
		}
		v, err := longhorn.ConvertVolume(o.(*unstructured.Unstructured))
		if err != nil {
			return err
		}
		mu.Lock()
		old, had := written[name]
		mu.Unlock()
		if had && !longhorn.DigestChanged(old.v, v) {
			return nil
		}
		obj, err := longhorn.DigestObject(v)
		if err != nil {
			return err
		}
		var out *unstructured.Unstructured
		if had {
			obj.SetResourceVersion(old.rv)
			out, err = res.Update(ctx, obj, metav1.UpdateOptions{FieldManager: "lhtx-agent"})
			if apierrors.IsConflict(err) { // changed elsewhere: take its version
				if cur, gerr := res.Get(ctx, name, metav1.GetOptions{}); gerr == nil {
					obj.SetResourceVersion(cur.GetResourceVersion())
					out, err = res.Update(ctx, obj, metav1.UpdateOptions{FieldManager: "lhtx-agent"})
				}
			}
		}
		if !had || apierrors.IsNotFound(err) {
			obj.SetResourceVersion("")
			out, err = res.Create(ctx, obj, metav1.CreateOptions{FieldManager: "lhtx-agent"})
			if apierrors.IsAlreadyExists(err) {
				if cur, gerr := res.Get(ctx, name, metav1.GetOptions{}); gerr == nil {
					obj.SetResourceVersion(cur.GetResourceVersion())
					out, err = res.Update(ctx, obj, metav1.UpdateOptions{FieldManager: "lhtx-agent"})
				}
			}
		}
		if err != nil {
			return err
		}
		mu.Lock()
		written[name] = entry{v, out.GetResourceVersion()}
		a.metrics.digestVolumes.Store(int64(len(written)))
		mu.Unlock()
		a.digestWrites.Add(1)
		return nil
	}
	go func() {
		for {
			name, quit := q.Get()
			if quit {
				return
			}
			if err := sync(name); err != nil && ctx.Err() == nil {
				a.Logf("digest %s: %v", name, err)
				q.AddRateLimited(name)
			} else {
				q.Forget(name)
			}
			q.Done(name)
		}
	}()
	<-ctx.Done()
	return nil
}

func newLimiter(qps float64) *rate.Limiter {
	if qps <= 0 {
		qps = 10
	}
	return rate.NewLimiter(rate.Limit(qps), int(qps)+1)
}
