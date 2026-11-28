package tui

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/nxckdx/longhorntx/internal/agent"
	"github.com/nxckdx/longhorntx/internal/longhorn"
	"github.com/nxckdx/longhorntx/internal/shrink"
	"github.com/nxckdx/longhorntx/internal/switchover"
)

// Shrink in the TUI: K in the volume details asks for the new size, plans
// it (nothing changes), shows the steps and warnings, and after Y runs it
// in the background with the live log of a switch. x cancels – which rolls
// back until the old PVC is deleted.

type shrinkPlanMsg struct {
	client   *longhorn.Client
	plan     *shrink.Plan
	err      error
	imageErr error // no image for a local run (development build)
}

// ShrinkImage: the copy pod's image – env LHTX_SHRINK_IMAGE, else the image
// of this lhtx version.
func ShrinkImage(version string) (string, error) {
	if img := os.Getenv("LHTX_SHRINK_IMAGE"); img != "" {
		return img, nil
	}
	if version == "" || version == "dev" {
		return "", fmt.Errorf("a development build does not know its image: set LHTX_SHRINK_IMAGE")
	}
	return "ghcr.io/nxckdx/lhtx:" + strings.TrimPrefix(version, "v"), nil
}

func (m *model) shrinkKey(vol string) tea.Cmd {
	if !m.requireRW() {
		return nil
	}
	vv := m.findView(vol)
	if vv == nil {
		return nil
	}
	if vv.PVC == "" {
		m.setFlash("this volume has no PVC – shrink works on a PVC", true)
		return nil
	}
	image, imageErr := ShrinkImage(m.opts.Version) // only needed without an agent
	ns, pvc, _ := strings.Cut(vv.PVC, "/")
	m.prompt(fmt.Sprintf("Shrink %s (now %s) to which size? e.g. 5Gi – its app is down while the data is copied", vv.PVC, humanQ(vv.Size)), "", func(in string) error {
		q, err := resource.ParseQuantity(strings.TrimSpace(in))
		if err != nil {
			return fmt.Errorf("size: %v", err)
		}
		c, ctx := m.client, m.ctx
		m.modal = &modal{kind: modalBusy, busyText: "planning the shrink (PVC, pods, used space) …"}
		m.afterSubmit = func() tea.Msg {
			p, err := shrink.Build(ctx, c, ns, pvc, q.Value(), shrink.BuildOptions{Image: image}, time.Now())
			return shrinkPlanMsg{client: c, plan: p, err: err, imageErr: imageErr}
		}
		return nil
	})
	return nil
}

func humanQ(n int64) string { return resource.NewQuantity(n, resource.BinarySI).String() }

func (m *model) onShrinkPlan(msg shrinkPlanMsg) {
	if msg.client != m.client {
		return
	}
	if msg.err != nil {
		m.modal = nil
		m.setFlash("shrink: "+msg.err.Error(), true)
		return
	}
	p := msg.plan
	var lines []string
	if p.Used >= 0 {
		lines = append(lines, fmt.Sprintf("used: %s (%s)", humanQ(p.Used), p.UsedFrom))
	}
	if p.Node != "" {
		lines = append(lines, fmt.Sprintf("copy on node %s (%s)", p.Node, p.NodeWhy))
	}
	for i, s := range p.Steps() {
		lines = append(lines, fmt.Sprintf("%2d. %s", i+1, s))
	}
	warns := append([]string{}, p.Warnings...)
	for _, b := range p.Blockers {
		warns = append(warns, "✖ NOT POSSIBLE: "+b)
	}
	sw := p.Switch
	sum := &longhorn.Action{Title: fmt.Sprintf("Shrink PVC %s from %s to %s", sw.PVCRef(), humanQ(p.OldSize), humanQ(p.NewSize)),
		Verb: longhorn.VerbPatch, Header: "checks run first while the app still runs; it is down from the freeze to the end",
		Lines: lines, Warnings: warns, Destructive: true}
	m.modal = &modal{kind: modalConfirm, action: sum, onConfirm: func() tea.Cmd {
		if len(p.Blockers) > 0 {
			m.setFlash("the shrink is not possible: "+p.Blockers[0], true)
			return nil
		}
		c := m.client
		return m.startRun(fmt.Sprintf("Shrinking %s to %s", sw.PVCRef(), humanQ(p.NewSize)),
			fmt.Sprintf("%s now uses %s (%s); the old volume %s is kept", sw.PVCRef(), sw.NewVolume, humanQ(p.NewSize), sw.OldVolume),
			"x cancel (rolls back until the old PVC is deleted)",
			func(ctx context.Context, log func(string, ...any)) error {
				// an lhtx agent that can do switches carries it out (with its own image)
				if ag := agent.DetectRW(ctx, c); ag != nil && agent.SwitchesAvailable(ctx, c, sw.Namespace) {
					name, err := agent.RequestSwitch(ctx, c, sw.Namespace, agent.SwitchSpec{PVC: sw.PVC, ShrinkTo: humanQ(p.NewSize)})
					if err != nil {
						return err
					}
					log("handed to the lhtx agent %s as Switch %s/%s", ag.Holder, sw.Namespace, name)
					res, err := agent.FollowSwitch(ctx, c, sw.Namespace, name, log)
					if err != nil {
						return err
					}
					if res.Status.Phase != agent.SwitchDone {
						return fmt.Errorf("shrink %s: %s", res.Status.Phase, res.Status.Message)
					}
					return nil
				}
				if msg.imageErr != nil {
					return msg.imageErr
				}
				return shrink.Run(ctx, c, p, shrink.Options{Journal: switchover.NewJournalPath(sw), Log: log})
			})
	}}
}
