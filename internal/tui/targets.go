package tui

import (
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/nxckdx/longhorntx/internal/format"
	"github.com/nxckdx/longhorntx/internal/longhorn"
)

// Backup targets page (Backups tab 6 → t). Write mode: n new, enter edit,
// X delete, s sync now. The credential secret is picked from the secret
// names of the Longhorn namespace (metadata only, no secret data).

type targetsMsg struct {
	client  *longhorn.Client
	targets []*longhorn.BackupTarget
	secrets []string // nil if not allowed to list
	err     error
}

func (m *model) loadTargets() tea.Cmd {
	if m.client == nil {
		return nil
	}
	c, ctx := m.client, m.ctx
	m.tgLoading = true
	return func() tea.Msg {
		ts, err := c.BackupTargets(ctx)
		var secrets []string
		if err == nil {
			secrets, _ = c.SecretNames(ctx)
		}
		return targetsMsg{client: c, targets: ts, secrets: secrets, err: err}
	}
}

func (m *model) applyTargets(msg targetsMsg) {
	if msg.client != m.client {
		return
	}
	m.tgLoading, m.tgErr = false, msg.err
	if msg.err == nil {
		m.tgRaw, m.tgSecrets, m.tgAt = msg.targets, msg.secrets, time.Now()
	}
	m.clamp()
}

func targetMessage(t *longhorn.BackupTarget) string {
	for _, c := range t.Status.Conditions {
		if c.Status == "True" && c.Message != "" {
			return c.Message
		}
	}
	return ""
}

func (m *model) targetRows() []row {
	now := time.Now()
	rows := make([]row, 0, len(m.tgRaw))
	for _, t := range m.tgRaw {
		avail := fg(cOK, "✔ available")
		if !t.Status.Available {
			avail = sErr.Render("✖ unavailable")
		}
		if t.Spec.BackupTargetURL == "" {
			avail = sDim.Render("disabled")
		}
		users := 0
		for _, v := range m.state.Volumes {
			if v.Spec.BackupTargetName == t.Name || (v.Spec.BackupTargetName == "" && t.Name == "default") {
				users++
			}
		}
		managed := ""
		if by := longhorn.ManagedBy(t); by != "" {
			managed = sDim.Render(strings.SplitN(by, " ", 2)[0])
		}
		t := t
		rows = append(rows, row{
			cells: []string{t.Name, dash(t.Spec.BackupTargetURL), avail, dash(t.Spec.CredentialSecret), dash(format.Interval(t.Spec.PollInterval)),
				dash(format.Age(t.Status.LastSyncedAt, now)), fmt.Sprint(users), dash(managed), fg(cWarn, targetMessage(t))},
			ref: t,
			open: func() tea.Cmd {
				if m.requireRW() {
					m.targetForm(t)
				}
				return nil
			},
		})
	}
	return rows
}

func (m *model) targetsTitle() string {
	t := " " + sBold.Render(fmt.Sprintf("Backup targets %d", len(m.tgRaw))) +
		sDim.Render("  where backups go · VOLUMES = volumes backing up there · BY = GitOps owner")
	switch {
	case m.tgLoading:
		t += sDim.Render(" · loading …")
	case m.tgErr != nil:
		t += "  " + sErr.Render(m.tgErr.Error())
	case !m.tgAt.IsZero():
		t += sDim.Render(fmt.Sprintf(" · loaded %s ago", format.Age(m.tgAt.Format(time.RFC3339), time.Now())))
	}
	if m.rw {
		t += sDim.Render(" · n new · enter edit · s sync now · X delete")
	} else {
		t += sDim.Render(" · W: write mode to change")
	}
	return t
}

func (m *model) targetForm(t *longhorn.BackupTarget) {
	s := longhorn.TargetSpec{PollInterval: "5m0s"}
	title := "New backup target"
	if t != nil {
		s = longhorn.TargetSpecOf(t)
		title = "Edit backup target " + t.Name
	}
	secret := formField{key: "secret", label: "credential secret", value: s.Secret,
		hint: "secret in the Longhorn namespace, empty for none"}
	if m.tgSecrets != nil {
		secret.options = append([]string{""}, m.tgSecrets...)
		secret.hint = "←→ choose (names only; empty = none)"
	}
	f := &form{title: title, fields: []formField{
		{key: "name", label: "name", value: s.Name, readOnly: t != nil},
		{key: "url", label: "URL", value: s.URL},
		secret,
		{key: "poll", label: "poll interval", value: format.Interval(s.PollInterval), hint: "how often Longhorn reads the store, 0 = off"},
	}}
	f.live = func(map[string]string) []string { return []string{"URL formats: " + longhorn.TargetURLHelp} }
	f.submit = func(v map[string]string) (*longhorn.Action, error) {
		ns := longhorn.TargetSpec{Name: strings.TrimSpace(v["name"]), URL: strings.TrimSpace(v["url"]),
			Secret: strings.TrimSpace(v["secret"]), PollInterval: strings.TrimSpace(v["poll"])}
		if t == nil {
			return longhorn.CreateTarget(ns, m.tgRaw, m.tgSecrets)
		}
		return longhorn.UpdateTarget(t, ns, m.tgRaw, m.tgSecrets)
	}
	m.openForm(f)
}

func (m *model) targetsKey(p *page, key string) (tea.Cmd, bool) {
	if p.kind != pTargets {
		return nil, false
	}
	var cur *longhorn.BackupTarget
	if rows := m.rowsFor(p); p.cursor < len(rows) {
		cur, _ = rows[p.cursor].ref.(*longhorn.BackupTarget)
	}
	switch key {
	case "n":
		if m.requireRW() {
			m.targetForm(nil)
		}
	case "e":
		if cur != nil && m.requireRW() {
			m.targetForm(cur)
		}
	case "s":
		if cur != nil && m.requireRW() {
			m.confirm(longhorn.SyncTarget(cur, time.Now()), nil)
		}
	case "X":
		if cur != nil && m.requireRW() {
			m.confirm(longhorn.DeleteTarget(cur, m.state.Volumes))
		}
	case "ctrl+r":
		return m.loadTargets(), true
	default:
		return nil, false
	}
	return nil, true
}
