package agent

import (
	"context"
	"fmt"
	"time"

	"github.com/nxckdx/longhorntx/internal/format"
	"github.com/nxckdx/longhorntx/internal/longhorn"
)

// Mount check: every Interval the agent looks for pods that wait for a
// Longhorn volume (lhtx mounts: one field-selected LIST of pending pods;
// more only if some wait). A pod waiting longer than After goes into the
// volume's history as mount-stuck (notification, alert via
// lhtx_agent_mounts_waiting); when it no longer waits, "mounted".
// Read-only: the agent never restarts anything.

// MountCheck configures it; Interval 0 = off.
type MountCheck struct {
	Interval time.Duration
	After    time.Duration // report pods waiting longer than this (default 5m)
	Kubelet  bool          // ask kubelet via the node proxy (needs nodes/proxy)
}

type mountWait struct {
	since    time.Time
	reported bool
	s        longhorn.StuckMount
}

func (a *Agent) runMountCheck(ctx context.Context) {
	mc := a.Mounts
	if mc.Interval <= 0 {
		return
	}
	if mc.After <= 0 {
		mc.After = 5 * time.Minute
	}
	a.Logf("checking for stuck mounts every %s (reporting after %s)", mc.Interval, mc.After)
	waits := map[string]*mountWait{}
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(mc.Interval):
		}
		rep, err := a.c.Mounts(ctx, nil, nil, longhorn.MountOptions{MinWait: mc.After, Kubelet: mc.Kubelet})
		if err != nil {
			if ctx.Err() == nil {
				a.Logf("mount check: %v", err)
			}
			continue
		}
		a.mountCheck(ctx, rep, waits, time.Now())
	}
}

// mountCheck records what changed since the last check.
func (a *Agent) mountCheck(ctx context.Context, rep *longhorn.MountReport, waits map[string]*mountWait, now time.Time) {
	gauge := map[[2]string]int{}
	seen := map[string]bool{}
	for _, s := range rep.Stuck {
		k := s.Pod + "|" + s.Volume
		seen[k] = true
		gauge[[2]string{s.Node, s.Stage}]++
		w := waits[k]
		if w == nil {
			w = &mountWait{since: now.Add(-s.Waiting)}
			waits[k] = w
		}
		w.s = s
		if !w.reported {
			w.reported = true
			text := fmt.Sprintf("pod %s waits %s on node %s (%s)", s.Pod, format.Duration(s.Waiting), s.Node, s.Stage)
			if s.Stage == longhorn.StageMount {
				text += " – attached, not mounted: kubelet / CSI plugin on the node (lhtx mounts)"
			} else if s.Detail != "" {
				text += ": " + s.Detail
			}
			a.recordMount(ctx, s.Volume, longhorn.HistoryEvent{At: now.UTC(), Kind: longhorn.HistMountStuck, Text: text})
		}
	}
	for k, w := range waits {
		if seen[k] {
			continue
		}
		delete(waits, k)
		a.recordMount(ctx, w.s.Volume, longhorn.HistoryEvent{At: now.UTC(), Kind: longhorn.HistMounted,
			Took: now.Sub(w.since).Round(time.Second),
			Text: fmt.Sprintf("pod %s no longer waits on node %s (mounted, or the pod is gone)", w.s.Pod, w.s.Node)})
	}
	a.metrics.mountsWaiting.Store(&gauge)
}

func (a *Agent) recordMount(ctx context.Context, volume string, e longhorn.HistoryEvent) {
	a.Logf("mount check: %s", e.Text)
	vol := &longhorn.Volume{}
	vol.Name = volume
	if a.c != nil {
		if v, err := a.c.Volume(ctx, volume); err == nil {
			vol = v
		}
	}
	if r := a.history.Load(); r != nil {
		r.record(vol, []longhorn.HistoryEvent{e})
	} else if notifiable(e) {
		a.notifier.add(notice{At: e.At, Kind: e.Kind, Volume: vol.Name, PVC: vol.PVC(), Text: e.Text, Bad: e.Bad()})
	}
}
