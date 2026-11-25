package switchover

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/nxckdx/longhorntx/internal/longhorn"
)

// JournalDir is where switch journals go: $XDG_STATE_HOME/lhtx/switch.
func JournalDir() string {
	dir := os.Getenv("XDG_STATE_HOME")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		dir = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(dir, "lhtx", "switch")
}

// NewJournalPath names the journal of a run that starts now.
func NewJournalPath(p *Plan) string {
	d := JournalDir()
	if d == "" {
		return ""
	}
	return filepath.Join(d, fmt.Sprintf("%s-%s-%s.json", time.Now().Format("20060102-150405"), p.Namespace, p.PVC))
}

// JournalEntry summarizes one journal file.
type JournalEntry struct {
	Path       string
	Context    string
	RolledBack bool
	PVC        string
	From, To   string
	At         time.Time
	Finished   bool
	Error      string
	LastStep   string
}

// Journals lists the journals, newest first.
func Journals(dir string) []JournalEntry {
	files, _ := filepath.Glob(filepath.Join(dir, "*.json"))
	var out []JournalEntry
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		var j journal
		if json.Unmarshal(b, &j) != nil || j.Plan == nil {
			continue
		}
		e := JournalEntry{Path: f, Context: j.Context, RolledBack: j.RolledBack, PVC: j.Plan.PVCRef(),
			From: j.Plan.OldVolume, To: j.Plan.NewVolume, At: j.At, Error: j.Error}
		if n := len(j.Steps); n > 0 {
			e.LastStep = j.Steps[n-1]
			e.Finished = j.Steps[n-1] == "finished"
		}
		out = append(out, e)
	}
	sort.Slice(out, func(i, k int) bool { return out[i].At.After(out[k].At) })
	return out
}

// Checks are findings for doctor / the health tab: active freezes (pods of
// frozen PVCs cannot start) and switches of this machine that did not
// finish.
func Checks(ctx context.Context, c *longhorn.Client, journalDir string) []longhorn.Finding {
	var f []longhorn.Finding
	for _, e := range Journals(journalDir) {
		if e.Context != c.Cluster.Context || e.Finished || e.RolledBack || time.Since(e.At) > 30*24*time.Hour {
			continue
		}
		if e.Error != "" && e.LastStep == "" {
			continue // failed before it changed anything
		}
		msg := "switch to " + e.To + " did not finish"
		if e.Error != "" {
			msg += ": " + firstLine(e.Error)
		} else if e.LastStep != "" {
			msg += " (last step: " + e.LastStep + ")"
		}
		f = append(f, longhorn.Finding{Severity: longhorn.SevWarning, Check: "switch", Object: e.PVC, Message: msg,
			Hint: "see " + e.Path + "; check the PVC and active freezes (lhtx freeze --status)"})
	}
	// active freezes: pods of these PVCs cannot start until unfrozen
	if fs, err := ActiveFreezes(ctx, c.Dynamic()); err == nil {
		for _, fr := range fs {
			f = append(f, longhorn.Finding{Severity: longhorn.SevWarning, Check: "freeze", Object: fr.Name,
				Message: fmt.Sprintf("%d PVCs frozen since %s (%s): their pods cannot start", len(fr.PVCs),
					fr.Created.Local().Format("2006-01-02 15:04"), dashIf(fr.Reason)),
				Hint: "lhtx unfreeze " + fr.Name})
		}
	}
	return f
}

func firstLine(s string) string {
	if i := strings.IndexAny(s, "\n"); i >= 0 {
		s = s[:i]
	}
	if len(s) > 140 {
		s = s[:137] + "…"
	}
	return s
}

func dashIf(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
