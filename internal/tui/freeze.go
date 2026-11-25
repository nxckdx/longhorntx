package tui

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/nxckdx/longhorntx/internal/agent"
	"github.com/nxckdx/longhorntx/internal/longhorn"
	"github.com/nxckdx/longhorntx/internal/switchover"
)

// Freeze from the TUI (Z): stop everything that uses the chosen Longhorn
// volumes with an admission policy – selected volumes, all volumes on a
// node, or all (Longhorn upgrade) – and end freezes. The header shows
// "❄ frozen" while a freeze is active (checked on connect, every 5 minutes
// and after each run: one LIST of policies, a few hundred bytes).

type freezesMsg struct {
	client *longhorn.Client
	list   []switchover.Freeze
	err    error
}

type freezePlanMsg struct {
	client *longhorn.Client
	plan   *switchover.FreezePlan
	label  string
	agent  *agent.Info // the lhtx agent carries the freeze out (nil: lhtx itself)
	err    error
	// with the agent: end the freeze on its own after this ("" = never);
	// asked once before the confirmation
	autoUnfreeze string
	asked        bool
}

func (m *model) loadFreezes() tea.Cmd {
	if m.client == nil {
		return nil
	}
	c, ctx := m.client, m.ctx
	return func() tea.Msg {
		fs, err := switchover.ActiveFreezes(ctx, c.Dynamic())
		return freezesMsg{client: c, list: fs, err: err}
	}
}

func (m *model) applyFreezes(msg freezesMsg) {
	if msg.client != m.client {
		return
	}
	if msg.err == nil {
		m.freezes, m.freezesAt = msg.list, time.Now()
	}
}

// freezeBadge for the header.
func (m *model) freezeBadge() string {
	if len(m.freezes) == 0 {
		return ""
	}
	n := 0
	for _, f := range m.freezes {
		n += len(f.PVCs)
	}
	// left in the header, next to RW: it must not be cut off
	return " " + lipgloss.NewStyle().Background(cInfo).Foreground(cBadgeFg).Bold(true).Padding(0, 1).Render(fmt.Sprintf("❄ %d frozen", n))
}

// freezeKey opens the choice of what to freeze or unfreeze.
func (m *model) freezeKey(p *page) tea.Cmd {
	if !m.requireRW() {
		return nil
	}
	var opts []pickOption
	if len(m.marked) > 0 {
		opts = append(opts, pickOption{label: fmt.Sprintf("freeze the %d selected volumes", len(m.marked)), value: "marked"})
	}
	var cur *longhorn.VolumeView
	if p.kind == pDetail {
		cur = m.findView(p.vol)
	} else if rows := m.rowsFor(p); p.kind == pVolumes && p.cursor < len(rows) {
		if r, ok := rows[p.cursor].ref.(volRef); ok {
			cur = m.findView(string(r))
		}
	}
	if cur != nil && cur.Volume.BoundPVC() != "" {
		opts = append(opts, pickOption{label: "freeze " + cur.Volume.BoundPVC(), value: "one:" + cur.Volume.BoundPVC()})
		if cur.Node != "" {
			opts = append(opts, pickOption{label: "freeze all volumes attached on " + cur.Node, value: "node:" + cur.Node})
		}
	}
	opts = append(opts, pickOption{label: "freeze ALL volumes (e.g. for a Longhorn upgrade)", value: "all"})
	for _, f := range m.freezes {
		opts = append(opts, pickOption{label: fmt.Sprintf("unfreeze %s: %d PVCs since %s%s", f.Name, len(f.PVCs),
			f.Created.Local().Format("15:04"), dashPrefix(f.Reason)), value: "un:" + f.Name})
	}
	m.modal = &modal{kind: modalPick, label: "Freeze: stop every pod that uses the volumes (admission policy, no scaling)", options: opts,
		onPick: func(v string) {
			switch {
			case strings.HasPrefix(v, "un:"):
				m.confirmUnfreeze(strings.TrimPrefix(v, "un:"))
			default:
				m.afterSubmit = m.planFreeze(v)
			}
		}}
	return nil
}

func dashPrefix(s string) string {
	if s == "" {
		return ""
	}
	return " – " + s
}

// planFreeze resolves the choice to PVCs and plans (async: pods per namespace).
func (m *model) planFreeze(choice string) tea.Cmd {
	var pvcs []string
	label := ""
	switch {
	case choice == "marked":
		for n := range m.marked {
			if vv := m.findView(n); vv != nil && vv.Volume.BoundPVC() != "" {
				pvcs = append(pvcs, vv.Volume.BoundPVC())
			}
		}
		label = fmt.Sprintf("%d selected volumes", len(pvcs))
	case strings.HasPrefix(choice, "one:"):
		pvcs = []string{strings.TrimPrefix(choice, "one:")}
		label = pvcs[0]
	case strings.HasPrefix(choice, "node:"), choice == "all":
		node := strings.TrimPrefix(choice, "node:")
		for _, v := range m.state.Volumes {
			if p := v.BoundPVC(); p != "" && (choice == "all" || (v.Status.State == "attached" && v.Status.CurrentNodeID == node)) {
				pvcs = append(pvcs, p)
			}
		}
		label = "all volumes"
		if choice != "all" {
			label = "volumes on " + node
		}
	}
	sort.Strings(pvcs)
	if len(pvcs) == 0 {
		m.setFlash("no PVC to freeze", true)
		return nil
	}
	c, ctx := m.client, m.ctx
	m.setFlash("planning the freeze …", false)
	return func() tea.Msg {
		p, err := switchover.PlanFreeze(ctx, c, pvcs, "lhtx TUI: "+label)
		var ag *agent.Info
		if err == nil {
			ag = agent.DetectRW(ctx, c)
		}
		return freezePlanMsg{client: c, plan: p, label: label, agent: ag, err: err}
	}
}

func (m *model) confirmFreeze(msg freezePlanMsg) {
	if msg.client != m.client {
		return
	}
	if msg.err != nil {
		m.setFlash("freeze: "+msg.err.Error(), true)
		return
	}
	m.flash = ""
	if msg.agent != nil && !msg.asked {
		opts := []pickOption{{label: "never – until I unfreeze", value: ""}}
		for _, d := range []string{"1h", "4h", "12h", "24h"} {
			opts = append(opts, pickOption{label: "after " + d, value: d})
		}
		m.modal = &modal{kind: modalPick, label: "End the freeze on its own (a watchdog in the lhtx agent)?", options: opts,
			onPick: func(v string) {
				msg.autoUnfreeze, msg.asked = v, true
				m.confirmFreeze(msg)
			}}
		return
	}
	p := msg.plan
	lines := []string{
		fmt.Sprintf("PVCs:    %d – %s", len(p.PVCs), strings.Join(firstNS(p.PVCs, 4), ", ")),
		fmt.Sprintf("pods:    %d deleted, cannot start again until unfreeze", len(p.Pods)),
		fmt.Sprintf("owners:  %d (Deployments, StatefulSets, operators … – their specs stay unchanged)", len(p.Owners)),
		fmt.Sprintf("volumes: %d attached now – lhtx waits until they are detached", len(p.Attached)),
		"policy:  no pod may mount them; only " + p.User + " may create/delete these PVCs",
	}
	if msg.agent != nil {
		lines[len(lines)-1] = "policy:  no pod may mount them; only the agent may create/delete these PVCs"
		lines = append(lines, "agent:   carried out by the lhtx agent "+msg.agent.Holder+" – x only stops following")
		if msg.autoUnfreeze != "" {
			lines = append(lines, "auto:    the agent ends the freeze on its own "+msg.autoUnfreeze+" after it is frozen")
		}
	}
	lines = append(lines, "apps (in parallel, each stopped in order with its grace period):")
	for _, l := range p.GroupLines() {
		lines = append(lines, "  "+l)
	}
	first := "until unfreeze (Z) these workloads stay down – also if lhtx stops"
	if msg.agent != nil {
		first = "until unfreeze (Z) these workloads stay down"
	}
	warns := append([]string{first}, p.Warnings...)
	if with, without := p.HookCount(); without > 0 && with+without > 1 {
		warns = append(warns, fmt.Sprintf("%d of %d apps have no pre-freeze/post-unfreeze hooks (annotations lhtx.io/…, see lhtx freeze --help)", without, with+without))
	}
	sum := &longhorn.Action{Title: "Freeze " + msg.label, Verb: longhorn.VerbCreate,
		Header: "ValidatingAdmissionPolicy + delete pods", Lines: lines, Warnings: warns, Destructive: true}
	c := m.client
	if ag := msg.agent; ag != nil {
		m.modal = &modal{kind: modalConfirm, action: sum, onConfirm: func() tea.Cmd {
			return m.startRun("Freezing "+msg.label+" (lhtx agent)", msg.label+" frozen by the agent – Z unfreezes", "x stop following (the agent goes on)",
				func(ctx context.Context, log func(string, ...any)) error {
					name, err := agent.Request(ctx, c, agent.Spec{PVCs: p.PVCs, Reason: p.Reason, AutoUnfreezeAfter: msg.autoUnfreeze}, p.User)
					if err != nil {
						return err
					}
					log("requested Freeze %s from the agent %s", name, ag.Holder)
					fr, _, err := agent.Follow(ctx, c, name, true, log, agent.Settled)
					if err == nil && (fr == nil || fr.Status.Phase == agent.PhaseFailed) {
						msg := "the Freeze was removed"
						if fr != nil {
							msg = fr.Status.Message
							_ = agent.End(context.WithoutCancel(ctx), c, name)
						}
						err = fmt.Errorf("the agent could not freeze: %s", msg)
					}
					return err
				})
		}}
		return
	}
	m.modal = &modal{kind: modalConfirm, action: sum, onConfirm: func() tea.Cmd {
		return m.startRun("Freezing "+msg.label, msg.label+" frozen – Z unfreezes", "x stop waiting (the freeze stays)",
			func(ctx context.Context, log func(string, ...any)) error {
				f, err := switchover.ApplyFreeze(ctx, c, p, switchover.FreezeOptions{Log: log})
				if f != nil {
					log("frozen as %s – Z in the volume list unfreezes it", f.Name)
				}
				return err
			})
	}}
}

func (m *model) confirmUnfreeze(name string) {
	var f *switchover.Freeze
	for i := range m.freezes {
		if m.freezes[i].Name == name {
			f = &m.freezes[i]
		}
	}
	if f == nil {
		return
	}
	var owners []string
	for _, o := range f.Owners {
		owners = append(owners, o.Kind+" "+o.Namespace+"/"+o.Name)
	}
	header := "remove the policy, nudge the owners"
	if f.Request != "" {
		header = "delete Freeze " + f.Request + " – the lhtx agent ends it (lhtx itself if no agent runs)"
	}
	sum := &longhorn.Action{Title: "Unfreeze " + f.Name, Verb: longhorn.VerbDelete, Header: header,
		Lines: []string{
			fmt.Sprintf("PVCs:   %d – %s", len(f.PVCs), strings.Join(firstNS(f.PVCs, 4), ", ")),
			fmt.Sprintf("owners: %d nudged so they start their pods now: %s", len(f.Owners), strings.Join(firstNS(owners, 3), ", ")),
			fmt.Sprintf("then wait until the %d volumes that were attached are attached again", len(f.Attached)),
		}}
	c, fr := m.client, *f
	m.modal = &modal{kind: modalConfirm, action: sum, onConfirm: func() tea.Cmd {
		stop := "x stop waiting (already unfrozen)"
		if fr.Request != "" {
			stop = "x stop following (the agent goes on)"
		}
		return m.startRun("Unfreezing "+fr.Name, fr.Name+" ended", stop,
			func(ctx context.Context, log func(string, ...any)) error {
				return agent.Unfreeze(ctx, c, fr, switchover.FreezeOptions{}, log, log)
			})
	}}
}

func firstNS(s []string, n int) []string {
	if len(s) <= n {
		return s
	}
	return append(append([]string{}, s[:n]...), fmt.Sprintf("… %d more", len(s)-n))
}
