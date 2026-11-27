package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/nxckdx/longhorntx/internal/longhorn"
	"github.com/nxckdx/longhorntx/internal/shrink"
	"github.com/nxckdx/longhorntx/internal/switchover"
)

// A Switch with spec.shrinkTo: the agent shrinks the PVC (lhtx shrink) –
// in the cluster, so closing the laptop during the copy changes nothing.
// After an agent restart: past the swap it continues the switch; before
// it, it removes what the interrupted run left (copy pod, temporary
// PVC/PV, new volume – the PVC stays frozen) and starts over.

func (a *Agent) runShrink(ctx context.Context, lg *switchLog, s *Switch) {
	fail := func(err error) {
		a.op("switch", "failed")
		lg.add("✘ %v", err)
		lg.flush(ctx, map[string]any{"phase": SwitchFailed, "message": err.Error()})
	}
	q, err := resource.ParseQuantity(s.Spec.ShrinkTo)
	if err != nil {
		fail(fmt.Errorf("spec.shrinkTo %q: %w", s.Spec.ShrinkTo, err))
		return
	}
	o := switchover.Options{Log: lg.add, Wait: durationOr(s.Spec.Wait, 10*time.Minute), Poll: a.Poll,
		FreezeName: switchFreezeName(s), Request: s.Namespace + "/" + s.Name, Handover: true}
	stop := lg.autoFlush(ctx)
	defer stop()

	if s.Status.Phase == SwitchRunning && s.Status.Plan != nil {
		err := switchover.Continue(ctx, a.c, s.Status.Plan, o)
		switch {
		case err == nil:
			a.op("switch", "done")
			lg.flush(ctx, map[string]any{"phase": SwitchDone, "message": ""})
			return
		case errors.Is(err, switchover.ErrStartOver):
			l, ferr := shrink.FindLeftovers(ctx, a.c, s.Namespace, s.Spec.PVC)
			if ferr != nil {
				fail(ferr)
				return
			}
			l.Freezes = nil // kept: the new run takes it over
			if !l.Empty() {
				lg.add("the copy was interrupted – removing what it left and starting over")
				if err := shrink.Abort(ctx, a.c, l, lg.add); err != nil {
					fail(err)
					return
				}
			}
		default:
			a.finishSwitch(ctx, lg, s, err)
			return
		}
	}

	image, err := a.ownImage(ctx)
	if err != nil {
		fail(err)
		return
	}
	p, err := shrink.Build(ctx, a.c, s.Namespace, s.Spec.PVC, q.Value(), shrink.BuildOptions{Image: image, PullSecrets: s.Spec.ImagePullSecrets}, time.Now())
	if err != nil {
		fail(err)
		return
	}
	if len(p.Blockers) > 0 {
		fail(fmt.Errorf("blocked: %s", strings.Join(p.Blockers, "; ")))
		return
	}
	for _, w := range p.Warnings {
		lg.add("⚠ %s", w)
	}
	lg.flush(ctx, map[string]any{"phase": SwitchRunning, "plan": p.Switch, "targetVolume": p.Switch.NewVolume})
	err = shrink.Run(ctx, a.c, p, shrink.Options{Log: lg.add, Wait: o.Wait, Poll: o.Poll, FreezeName: o.FreezeName, Request: o.Request, Handover: true})
	if ctx.Err() != nil {
		return // agent stopping: the next one continues
	}
	a.finishSwitch(ctx, lg, s, err)
}

// ownImage: the image the agent runs – the copy pod uses the same, so it
// comes from wherever the agent's came from (a mirror, a private registry).
func (a *Agent) ownImage(ctx context.Context) (string, error) {
	if img := os.Getenv("LHTX_SHRINK_IMAGE"); img != "" {
		return img, nil
	}
	name, ns := os.Getenv("POD_NAME"), os.Getenv("POD_NAMESPACE")
	if name != "" && ns != "" {
		if po, err := a.c.Dynamic().Resource(longhorn.GVRPod).Namespace(ns).Get(ctx, name, metav1.GetOptions{}); err == nil {
			cs, _, _ := unstructured.NestedSlice(po.Object, "spec", "containers")
			if len(cs) > 0 {
				if img, ok := cs[0].(map[string]any)["image"].(string); ok && img != "" {
					return img, nil
				}
			}
		}
	}
	if v := a.metrics.version; v != "" && v != "dev" {
		return "ghcr.io/nxckdx/lhtx:" + strings.TrimPrefix(v, "v"), nil
	}
	return "", fmt.Errorf("the agent cannot tell its own image (POD_NAME/POD_NAMESPACE not set, development build) – set LHTX_SHRINK_IMAGE")
}
