package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/nxckdx/longhorntx/internal/format"
	"github.com/nxckdx/longhorntx/internal/longhorn"
)

// Initial lists arrive independently. Each page only waits for what it
// needs; columns that depend on missing data show "…".

func (m *model) synced(res string) bool {
	if m.store == nil {
		return false
	}
	_, ok := m.store.Synced(res)
	return ok
}

// needs lists the resources a page requires before it can render.
func needs(k pageKind) []string {
	switch k {
	case pVolumes:
		return []string{longhorn.ResVolumes}
	case pNodes:
		return []string{longhorn.ResNodes}
	case pDisk, pActivity, pDetail, pHealth, pDrain, pOrphans:
		return []string{longhorn.ResVolumes, longhorn.ResReplicas, longhorn.ResNodes}
	}
	return nil
}

func (m *model) pageReady(p *page) bool {
	for _, r := range needs(p.kind) {
		if !m.synced(r) {
			return false
		}
	}
	return true
}

// viewLoading shows the progress of the initial lists.
func (m *model) viewLoading() string {
	var b strings.Builder
	b.WriteString("\n " + "loading from " + contextStyle(m.ctxLabel()).Render(m.ctxLabel()) + "\n\n")
	elapsed := time.Since(m.store.StartedAt())
	counts := map[string]int{
		longhorn.ResVolumes:  len(m.state.Volumes),
		longhorn.ResReplicas: len(m.state.Replicas),
		longhorn.ResNodes:    len(m.state.Nodes),
	}
	for _, r := range longhorn.StoreResources {
		if d, ok := m.store.Synced(r); ok {
			b.WriteString(fmt.Sprintf("   %s %-9s %5d  %s\n", fg(cOK, "✔"), r, counts[r], sDim.Render(fmt.Sprintf("%.1fs", d.Seconds()))))
		} else {
			b.WriteString(fmt.Sprintf("   %s %-9s %5s  %s\n", fg(cInfo, spinner(elapsed)), r, "", sDim.Render(fmt.Sprintf("%.1fs", elapsed.Seconds()))))
		}
	}
	b.WriteString("\n   " + sDim.Render(fmt.Sprintf("↓ %s received", format.Bytes(m.client.Cluster.Traffic.RX()))))
	if err := m.store.Err(); err != nil {
		b.WriteString("\n\n   " + sErr.Render("⚠ "+err.Error()))
	}
	return b.String()
}

// loadingHint is shown in the header while some lists are still missing.
func (m *model) loadingHint() string {
	if m.store == nil || m.store.AllSynced() {
		return ""
	}
	var miss []string
	for _, r := range longhorn.StoreResources {
		if !m.synced(r) {
			miss = append(miss, r)
		}
	}
	return fg(cInfo, spinner(time.Since(m.store.StartedAt()))+" loading "+strings.Join(miss, ", ")) + "  "
}

func spinner(d time.Duration) string {
	frames := []string{"◐", "◓", "◑", "◒"}
	return frames[int(d/(250*time.Millisecond))%len(frames)]
}
