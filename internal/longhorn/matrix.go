package longhorn

import (
	"fmt"
	"sort"
	"strings"
)

// Replica distribution: volumes × nodes. Shows at a glance where replicas
// are, whether two replicas share a node or even a disk (no protection if
// it fails), and how many replicas each node carries.

type MatrixCell struct {
	Healthy, Rebuilding, Failed, Other int
}

func (c MatrixCell) Total() int { return c.Healthy + c.Rebuilding + c.Failed + c.Other }

type MatrixRow struct {
	View  *VolumeView
	Cells []MatrixCell // index = Matrix.Nodes
	// Issue: replicas sharing a disk or a node, or more replicas than nodes
	// can hold apart; "" if the spread is fine.
	Issue string
	rank  int // 2 = shared disk, 1 = shared node
}

type Matrix struct {
	Nodes  []string
	Rows   []MatrixRow
	Totals []int // replicas per node
}

// BuildMatrix lays out all volumes over the given nodes (replicas on
// unknown nodes are added as extra columns). Rows with an issue come first.
func BuildMatrix(views []VolumeView, nodes []string) Matrix {
	m := Matrix{Nodes: append([]string(nil), nodes...)}
	idx := map[string]int{}
	for i, n := range m.Nodes {
		idx[n] = i
	}
	for _, vv := range views {
		for _, r := range vv.Replicas {
			if _, ok := idx[r.Node]; !ok && r.Node != "" {
				idx[r.Node] = len(m.Nodes)
				m.Nodes = append(m.Nodes, r.Node)
			}
		}
	}
	m.Totals = make([]int, len(m.Nodes))
	for i := range views {
		vv := &views[i]
		row := MatrixRow{View: vv, Cells: make([]MatrixCell, len(m.Nodes))}
		disks := map[string]int{}
		for _, r := range vv.Replicas {
			if r.Node == "" {
				continue // not scheduled yet
			}
			c := &row.Cells[idx[r.Node]]
			switch r.Health {
			case HealthHealthy:
				c.Healthy++
			case HealthRebuilding:
				c.Rebuilding++
			case HealthFailed:
				c.Failed++
			default:
				c.Other++
			}
			m.Totals[idx[r.Node]]++
			if r.Disk.UUID != "" && r.Health != HealthFailed {
				disks[r.Node+"\x00"+r.Disk.UUID]++
			}
		}
		row.Issue, row.rank = spreadIssue(row, disks, m.Nodes)
		m.Rows = append(m.Rows, row)
	}
	sort.SliceStable(m.Rows, func(i, j int) bool {
		a, b := m.Rows[i], m.Rows[j]
		if a.rank != b.rank {
			return a.rank > b.rank
		}
		return volKey(a.View) < volKey(b.View)
	})
	return m
}

func volKey(v *VolumeView) string {
	if v.PVC != "" {
		return v.PVC
	}
	return v.Name
}

// spreadIssue ignores failed replicas (Longhorn cleans them up).
func spreadIssue(row MatrixRow, disks map[string]int, nodes []string) (string, int) {
	keys := make([]string, 0, len(disks))
	for k := range disks {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if n := disks[k]; n > 1 {
			node, _, _ := strings.Cut(k, "\x00")
			return fmt.Sprintf("%d replicas on one disk of %s", n, node), 2
		}
	}
	for i, c := range row.Cells {
		if n := c.Total() - c.Failed; n > 1 {
			return fmt.Sprintf("%d replicas on %s", n, nodes[i]), 1
		}
	}
	return "", 0
}
