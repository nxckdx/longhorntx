package tui

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/nxckdx/longhorntx/internal/longhorn"
)

// Replica matrix (nodes view → m): volumes × nodes from the live store,
// no extra traffic. Volumes whose replicas share a disk or node first.

func (m *model) matrix() longhorn.Matrix {
	nodes := make([]string, 0, len(m.nodes))
	for _, n := range m.nodes {
		nodes = append(nodes, n.Name)
	}
	return longhorn.BuildMatrix(m.views, nodes)
}

// matrixNumbered: node columns get numbers when the names do not fit.
func (m *model) matrixNumbered(mx longhorn.Matrix) bool {
	w := 0
	for _, n := range mx.Nodes {
		w += min(len(n), 12) + 1
	}
	return w > m.width-50
}

func (m *model) matrixCols() []col {
	mx := m.matrix()
	cols := []col{{"PVC", 0, 3}}
	numbered := m.matrixNumbered(mx)
	for i, n := range mx.Nodes {
		if numbered {
			cols = append(cols, col{fmt.Sprint(i + 1), 3, 0})
		} else {
			cols = append(cols, col{n, max(min(len(n), 12), 3), 0})
		}
	}
	return append(cols, col{"ISSUE", 0, 2})
}

func matrixCell(c longhorn.MatrixCell) string {
	switch {
	case c.Total() == 0:
		return fg(cFaint, "·")
	case c.Total() > 1:
		return fg(cWarn, fmt.Sprint(c.Total()))
	case c.Healthy == 1:
		return fg(cOK, "●")
	case c.Rebuilding == 1:
		return fg(cInfo, "◐")
	case c.Failed == 1:
		return sErr.Render("✖")
	}
	return sDim.Render("○")
}

func (m *model) matrixRows() []row {
	mx := m.matrix()
	rows := make([]row, 0, len(mx.Rows)+1)
	for _, r := range mx.Rows {
		name := r.View.PVC
		if name == "" {
			name = r.View.Name
		}
		cells := []string{name}
		for _, c := range r.Cells {
			cells = append(cells, matrixCell(c))
		}
		cells = append(cells, fg(cWarn, r.Issue))
		vol := r.View.Name
		rows = append(rows, row{cells: cells, open: func() tea.Cmd { return m.openDetail(vol) }})
	}
	tot := []string{sDim.Render("(replicas per node)")}
	for _, n := range mx.Totals {
		tot = append(tot, sDim.Render(fmt.Sprint(n)))
	}
	rows = append(rows, row{cells: append(tot, "")})
	return rows
}

func (m *model) matrixTitle() string {
	mx := m.matrix()
	issues := 0
	for _, r := range mx.Rows {
		if r.Issue != "" {
			issues++
		}
	}
	t := " " + sBold.Render("Replica matrix") + sDim.Render("  ● healthy ◐ rebuilding ✖ failed ○ other · digit = replicas on that node")
	if issues > 0 {
		what := fmt.Sprintf("%d volumes share a node or disk", issues)
		if issues == 1 {
			what = "1 volume shares a node or disk"
		}
		t += "  " + fg(cWarn, what)
	} else if len(mx.Rows) > 0 {
		t += "  " + fg(cOK, "✔ spread")
	}
	if m.matrixNumbered(mx) {
		var legend []string
		for i, n := range mx.Nodes {
			legend = append(legend, fmt.Sprintf("%d=%s", i+1, n))
		}
		t += "\n " + sDim.Render(fit(strings.Join(legend, " "), m.width-2))
	}
	return t
}
