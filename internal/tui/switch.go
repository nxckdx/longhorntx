package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/nxckdx/longhorntx/internal/longhorn"
	"github.com/nxckdx/longhorntx/internal/switchover"
)

// Restore + switch in the TUI: the restore form's "switch" choice plans
// the switch (nothing created yet), shows the steps, and after Y runs
// restore and switch in the background with a live log. x cancels – which
// rolls back until the old PVC is deleted. lhtx does not quit while a
// switch runs.

// switchRun is a long background run with a live log: a switch, a freeze
// or an unfreeze.
type switchRun struct {
	title      string // "Switching a → b"
	okMsg      string // flash when done
	cancelHint string // what x does
	lines      []string
	done       bool
	err        error
	cancel     context.CancelFunc
	ch         chan string
	start      time.Time
}

type switchLogMsg struct {
	run  *switchRun
	line string
}

type switchDoneMsg struct {
	run *switchRun
	err error
}

// confirmSwitch shows the plan as a confirm dialog; Y starts it.
func (m *model) confirmSwitch(p *switchover.Plan, restore *longhorn.Action) {
	var lines []string
	n := 1
	if restore != nil {
		lines = append(lines, fmt.Sprintf("%2d. %s", n, restore.Summary()))
		n++
	}
	for _, s := range p.Steps() {
		lines = append(lines, fmt.Sprintf("%2d. %s", n, s))
		n++
	}
	warns := append([]string{}, p.Warnings...)
	for _, b := range p.Blockers {
		warns = append(warns, "✖ NOT POSSIBLE: "+b)
	}
	title := fmt.Sprintf("Switch PVC %s to volume %s", p.PVCRef(), p.NewVolume)
	if restore != nil {
		title = fmt.Sprintf("Restore %s and switch PVC %s to it", p.NewVolume, p.PVCRef())
	}
	sum := &longhorn.Action{Title: title, Verb: longhorn.VerbPatch, Header: fmt.Sprintf("%d steps, the workload is down in between", len(lines)),
		Lines: lines, Warnings: warns, Destructive: true}
	m.modal = &modal{kind: modalConfirm, action: sum, onConfirm: func() tea.Cmd {
		if len(p.Blockers) > 0 {
			m.setFlash("the switch is not possible: "+p.Blockers[0], true)
			return nil
		}
		return m.startSwitch(p, restore)
	}}
}

func (m *model) startSwitch(p *switchover.Plan, restore *longhorn.Action) tea.Cmd {
	c := m.client
	return m.startRun(fmt.Sprintf("Switching %s → %s", p.PVCRef(), p.NewVolume), p.PVCRef()+" now uses "+p.NewVolume,
		"x cancel (rolls back until the old PVC is deleted)",
		func(ctx context.Context, log func(string, ...any)) error {
			if restore != nil {
				if err := c.Apply(ctx, restore, longhorn.DryRunNone); err != nil {
					return err
				}
				log("✔ %s", restore.Title)
			}
			return switchover.Run(ctx, c, p, switchover.Options{Journal: journalPath(p), Log: log})
		})
}

// startRun runs fn in the background; its log lines stream into the
// dialog. lhtx does not quit while it runs.
func (m *model) startRun(title, okMsg, cancelHint string, fn func(context.Context, func(string, ...any)) error) tea.Cmd {
	ctx, cancel := context.WithCancel(context.WithoutCancel(m.ctx))
	r := &switchRun{title: title, okMsg: okMsg, cancelHint: cancelHint, cancel: cancel, ch: make(chan string, 64), start: time.Now()}
	m.switching = r
	m.modal = nil
	run := func() tea.Msg {
		defer close(r.ch)
		err := fn(ctx, func(f string, a ...any) { r.ch <- fmt.Sprintf(f, a...) })
		return switchDoneMsg{run: r, err: err}
	}
	return tea.Batch(run, m.nextSwitchLine(r))
}

func (m *model) nextSwitchLine(r *switchRun) tea.Cmd {
	return func() tea.Msg {
		line, ok := <-r.ch
		if !ok {
			return nil
		}
		return switchLogMsg{run: r, line: line}
	}
}

func (m *model) onSwitchLog(msg switchLogMsg) tea.Cmd {
	msg.run.lines = append(msg.run.lines, fmt.Sprintf("%5s  %s", time.Since(msg.run.start).Round(time.Second), msg.line))
	return m.nextSwitchLine(msg.run)
}

func (m *model) onSwitchDone(msg switchDoneMsg) tea.Cmd {
	msg.run.done, msg.run.err = true, msg.err
	if msg.err != nil {
		m.setFlash(msg.err.Error(), true)
	} else {
		m.setFlash("✔ "+msg.run.okMsg, false)
	}
	return m.loadFreezes() // the header shows what is still frozen
}

// switchKey: x cancels (rollback), esc closes a finished run.
func (m *model) switchKey(key string) tea.Cmd {
	r := m.switching
	switch {
	case r.done && (key == "esc" || key == "enter" || key == "q"):
		m.switching = nil
	case !r.done && key == "x":
		r.cancel()
		r.lines = append(r.lines, "… cancelling")
	case !r.done && (key == "q" || key == "ctrl+c"):
		m.setFlash("still running – "+r.cancelHint+"; lhtx does not quit meanwhile", true)
	}
	return nil
}

func (m *model) viewSwitch(w, h int) string {
	r := m.switching
	var b strings.Builder
	title := r.title
	switch {
	case r.done && r.err == nil:
		title = fg(cOK, "✔ "+title+" – done")
	case r.done:
		title = sErr.Render("✖ " + title + " – stopped")
	default:
		title = fg(cInfo, spinner(time.Since(r.start))+" "+title)
	}
	b.WriteString(sBold.Render(title) + "\n\n")
	lines := r.lines
	if max := h - 8; len(lines) > max && max > 0 {
		lines = lines[len(lines)-max:]
	}
	for _, l := range lines {
		c := sDim
		switch {
		case strings.Contains(l, "✖"):
			c = sErr
		case strings.Contains(l, "✔"):
			c = sBold
		}
		b.WriteString(c.Render(fit(l, w-6)) + "\n")
	}
	if r.err != nil {
		b.WriteString("\n" + sErr.Render(fit(r.err.Error(), (w-6)*4)) + "\n")
	}
	b.WriteString("\n")
	if r.done {
		b.WriteString(sDim.Render("[esc] close"))
	} else {
		b.WriteString(sDim.Render("[" + r.cancelHint + "] – lhtx stays open until it is done"))
	}
	return b.String()
}

func journalPath(p *switchover.Plan) string { return switchover.NewJournalPath(p) }

func (m *model) viewSwitchBox(h int) string {
	w := min(max(m.width-8, 40), 120)
	box := lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(cBorder).Padding(1, 2).Width(w).Render(m.viewSwitch(w, h))
	return lipgloss.Place(m.width, h, lipgloss.Center, lipgloss.Center, box)
}

// ---------------------------------------------------------------- w: switch a PVC to this volume

type switchPlanMsg struct {
	client *longhorn.Client
	plan   *switchover.Plan
	err    error
}

// switchToVolumeKey: w in the volume details – point a PVC at this volume
// (switching back to an old volume, or to a restored/cloned one). The PVC
// comes from Longhorn's record of the volume's last PVC, else it is asked.
func (m *model) switchToVolumeKey(vol string) tea.Cmd {
	if !m.requireRW() {
		return nil
	}
	vv := m.findView(vol)
	if vv == nil {
		return nil
	}
	if vv.State == "attached" {
		m.setFlash("this volume is attached – a PVC can only be switched to a detached volume", true)
		return nil
	}
	initial := vv.PVC
	m.prompt("Switch which PVC to volume "+vol+"? (namespace/name; its workload is stopped meanwhile)", initial, func(s string) error {
		ns, pvc, ok := strings.Cut(strings.TrimSpace(s), "/")
		if !ok || ns == "" || pvc == "" {
			return fmt.Errorf("namespace/name")
		}
		c, ctx := m.client, m.ctx
		m.setFlash("planning the switch …", false)
		m.afterSubmit = func() tea.Msg {
			p, err := switchover.Build(ctx, c, ns, pvc, vol)
			return switchPlanMsg{client: c, plan: p, err: err}
		}
		return nil
	})
	return nil
}

func (m *model) onSwitchPlan(msg switchPlanMsg) {
	if msg.client != m.client {
		return
	}
	if msg.err != nil {
		m.setFlash("switch: "+msg.err.Error(), true)
		return
	}
	m.flash = ""
	m.confirmSwitch(msg.plan, nil)
}
