package tui

import (
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/nxckdx/longhorntx/internal/longhorn"
)

// A form dialog with several fields (recurring jobs, backup targets,
// restore). enter builds the action; if that fails the error is shown in
// the form and nothing is lost, otherwise the usual confirm dialog opens.

type formField struct {
	key, label string
	value      string
	hint       string   // dim text after the field
	options    []string // choice field: ←/→ or space cycle, no typing
	readOnly   bool     // shown, not editable (e.g. the name when editing)
}

type form struct {
	title  string
	fields []formField
	cursor int
	err    string
	// live shows extra lines under the fields (e.g. the next cron runs)
	live   func(vals map[string]string) []string
	submit func(vals map[string]string) (*longhorn.Action, error)
}

func (f *form) values() map[string]string {
	out := make(map[string]string, len(f.fields))
	for _, fl := range f.fields {
		out[fl.key] = fl.value
	}
	return out
}

func (m *model) openForm(f *form) {
	for f.cursor < len(f.fields) && f.fields[f.cursor].readOnly {
		f.cursor++
	}
	m.modal = &modal{kind: modalForm, form: f}
}

func (f *form) move(d int) {
	for range len(f.fields) {
		f.cursor = (f.cursor + d + len(f.fields)) % len(f.fields)
		if !f.fields[f.cursor].readOnly {
			return
		}
	}
}

func (m *model) formKey(k tea.KeyPressMsg) tea.Cmd {
	f := m.modal.form
	fl := &f.fields[f.cursor]
	switch key := k.String(); key {
	case "esc":
		m.modal = nil
	case "tab", "down":
		f.move(1)
	case "shift+tab", "up":
		f.move(-1)
	case "enter":
		a, err := f.submit(f.values())
		if err != nil {
			f.err = err.Error()
			return nil
		}
		m.modal = nil
		m.confirm(a, nil)
	case "left", "right", "space":
		if len(fl.options) > 0 {
			i := indexOf(fl.options, fl.value)
			if key == "left" {
				i = (i - 1 + len(fl.options)) % len(fl.options)
			} else {
				i = (i + 1) % len(fl.options)
			}
			fl.value = fl.options[i]
			f.err = ""
		} else if key == "space" {
			fl.value += " "
		}
	case "backspace":
		if len(fl.options) == 0 {
			if r := []rune(fl.value); len(r) > 0 {
				fl.value = string(r[:len(r)-1])
			}
		}
	case "ctrl+u":
		if len(fl.options) == 0 {
			fl.value = ""
		}
	default:
		if k.Text != "" && len(fl.options) == 0 {
			fl.value += k.Text
			f.err = ""
		}
	}
	return nil
}

func indexOf(ss []string, s string) int {
	for i, x := range ss {
		if x == s {
			return i
		}
	}
	return 0
}

func (m *model) viewForm(f *form, w int) string {
	var b strings.Builder
	b.WriteString(sBold.Render(f.title) + "\n" + sDim.Render("context "+m.ctxLabel()) + "\n\n")
	lw := 0
	for _, fl := range f.fields {
		lw = max(lw, len(fl.label))
	}
	for i, fl := range f.fields {
		label := fl.label + strings.Repeat(" ", lw-len(fl.label))
		val := fl.value
		switch {
		case fl.readOnly:
			val = sDim.Render(val)
		case len(fl.options) > 0:
			val = "‹ " + val + " ›"
			if i == f.cursor {
				val = sKey.Render(val)
			}
		case i == f.cursor:
			val += "█"
		}
		mark := "  "
		if i == f.cursor {
			mark = sKey.Render("▶ ")
			label = sBold.Render(label)
		} else {
			label = sDim.Render(label)
		}
		line := mark + label + "  " + val
		if fl.hint != "" {
			line += "  " + sDim.Render(fl.hint)
		}
		b.WriteString(fit(line, w-6) + "\n")
	}
	if f.live != nil {
		if lines := f.live(f.values()); len(lines) > 0 {
			b.WriteString("\n")
			for _, l := range lines {
				c := cInfo
				if strings.HasPrefix(l, "✖") {
					c = cErr
				}
				b.WriteString(fg(c, fit(l, w-6)) + "\n")
			}
		}
	}
	if f.err != "" {
		b.WriteString("\n" + sErr.Render("✖ "+f.err) + "\n")
	}
	b.WriteString("\n" + sDim.Render("[tab/↑↓] field  [←→] choose  [enter] preview  [esc] cancel"))
	return b.String()
}
