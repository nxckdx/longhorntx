package agent

import (
	"context"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/nxckdx/longhorntx/internal/longhorn"
)

// runCapacity samples the Longhorn disks every CapacityInterval into the
// VolumeHistory lhtx-capacity (lhtx capacity, doctor's forecast). After a
// restart it samples only once the last sample is older than the interval.
func (a *Agent) runCapacity(ctx context.Context) {
	every := a.CapacityInterval
	if every <= 0 {
		return
	}
	a.Logf("sampling disk capacity every %s", every)
	for {
		wait := every
		if last, err := a.c.Capacity(ctx); err == nil && len(last) > 0 {
			if age := time.Since(last[len(last)-1].At); age < every {
				wait = every - age
			} else {
				wait = 0
			}
		} else {
			wait = 0
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		if err := a.sampleCapacity(ctx); err != nil && ctx.Err() == nil {
			a.Logf("capacity: %v", err)
			select { // try again later, not in a loop
			case <-ctx.Done():
				return
			case <-time.After(10 * time.Minute):
			}
		}
	}
}

func (a *Agent) sampleCapacity(ctx context.Context) error {
	nodes, err := a.c.Nodes(ctx)
	if err != nil {
		return err
	}
	res := a.c.Dynamic().Resource(longhorn.GVRVolumeHistory).Namespace(a.c.Namespace)
	samples, err := a.c.Capacity(ctx)
	if err != nil && err != longhorn.ErrNoCapacity {
		return err
	}
	samples = append(samples, longhorn.SampleCapacity(nodes, time.Now()))
	obj, err := longhorn.CapacityObject(a.c.Namespace, samples, longhorn.MaxCapacitySamples)
	if err != nil {
		return err
	}
	cur, err := res.Get(ctx, longhorn.CapacityHistory, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		_, err = res.Create(ctx, obj, metav1.CreateOptions{FieldManager: "lhtx-agent"})
	case err == nil:
		obj.SetResourceVersion(cur.GetResourceVersion())
		_, err = res.Update(ctx, obj, metav1.UpdateOptions{FieldManager: "lhtx-agent"})
	}
	return err
}
