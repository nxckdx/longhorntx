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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

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

// Checks are findings for doctor / the health tab: Flux objects that are
// suspended (maybe left over) and switches of this machine that did not
// finish. Flux being absent is not an error.
func Checks(ctx context.Context, c *longhorn.Client, journalDir string) []longhorn.Finding {
	var f []longhorn.Finding
	for _, e := range Journals(journalDir) {
		if e.Context != c.Cluster.Context || e.Finished || e.RolledBack || time.Since(e.At) > 30*24*time.Hour {
			continue
		}
		msg := "switch to " + e.To + " did not finish"
		if e.Error != "" {
			msg += ": " + firstLine(e.Error)
		} else if e.LastStep != "" {
			msg += " (last step: " + e.LastStep + ")"
		}
		f = append(f, longhorn.Finding{Severity: longhorn.SevWarning, Check: "switch", Object: e.PVC, Message: msg,
			Hint: "see " + e.Path + "; check the workload's replicas and suspended Flux objects"})
	}
	dyn := c.Dynamic()
	for _, kind := range []string{"Kustomization", "HelmRelease"} {
		l, err := dyn.Resource(FluxRef{Kind: kind}.gvr()).List(ctx, metav1.ListOptions{})
		if err != nil {
			continue // Flux not installed or not allowed
		}
		for _, it := range l.Items {
			if s, _, _ := unstructured.NestedBool(it.Object, "spec", "suspend"); s {
				f = append(f, longhorn.Finding{Severity: longhorn.SevInfo, Check: "flux",
					Object:  kind + " " + it.GetNamespace() + "/" + it.GetName(),
					Message: "suspended: Flux applies no changes from Git",
					Hint:    "intended? else: flux resume " + strings.ToLower(kind) + " " + it.GetName() + " -n " + it.GetNamespace()})
			}
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
