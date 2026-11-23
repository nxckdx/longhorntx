package cli

import (
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/nxckdx/longhorntx/internal/longhorn"
)

func matrixCmd(g *globals) *cobra.Command {
	var problems bool
	cmd := &cobra.Command{
		Use:     "matrix",
		Aliases: []string{"spread"},
		Short:   "Replica distribution: volumes × nodes",
		Long: `One row per volume, one column per node: ● healthy, ◐ rebuilding,
✖ failed, ○ other (stopped, unknown); a digit if a node holds several
replicas of the volume. Volumes whose replicas share a disk or a node come
first (losing that disk or node loses several copies). The last row counts
the replicas per node. With more than 8 nodes the columns are numbered.

Traffic: one LIST of volumes, replicas and nodes.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := g.checkOutput(); err != nil {
				return err
			}
			c, err := g.client()
			if err != nil {
				return err
			}
			defer g.reportTraffic(c)
			st, err := loadState(cmd, c)
			if err != nil {
				return err
			}
			var nodes []string
			for _, n := range st.Nodes {
				nodes = append(nodes, n.Name)
			}
			mx := longhorn.BuildMatrix(longhorn.Build(st), nodes)
			if problems {
				var keep []longhorn.MatrixRow
				for _, r := range mx.Rows {
					if r.Issue != "" {
						keep = append(keep, r)
					}
				}
				mx.Rows = keep
			}
			if g.output == "json" {
				type jrow struct {
					Volume string         `json:"volume"`
					PVC    string         `json:"pvc,omitempty"`
					Nodes  map[string]int `json:"replicasPerNode"`
					Issue  string         `json:"issue,omitempty"`
				}
				out := []jrow{}
				for _, r := range mx.Rows {
					j := jrow{Volume: r.View.Name, PVC: r.View.PVC, Nodes: map[string]int{}, Issue: r.Issue}
					for i, cell := range r.Cells {
						if cell.Total() > 0 {
							j.Nodes[mx.Nodes[i]] = cell.Total()
						}
					}
					out = append(out, j)
				}
				return writeJSON(os.Stdout, out)
			}
			if problems && len(mx.Rows) == 0 {
				fmt.Println("✔ no two replicas of a volume share a node or disk")
				return nil
			}
			numbered := len(mx.Nodes) > 8
			head := func(i int) string {
				if numbered {
					return fmt.Sprint(i + 1)
				}
				return mx.Nodes[i]
			}
			w := newTab(os.Stdout)
			hdr := []string{"PVC"}
			for i := range mx.Nodes {
				hdr = append(hdr, head(i))
			}
			fmt.Fprintln(w, strings.Join(append(hdr, "ISSUE"), "\t"))
			for _, r := range mx.Rows {
				cells := []string{dash(r.View.PVC)}
				if r.View.PVC == "" {
					cells[0] = r.View.Name
				}
				for _, cell := range r.Cells {
					cells = append(cells, MatrixSymbol(cell))
				}
				fmt.Fprintln(w, strings.Join(append(cells, dash(r.Issue)), "\t"))
			}
			tot := []string{"(replicas)"}
			for _, n := range mx.Totals {
				tot = append(tot, fmt.Sprint(n))
			}
			fmt.Fprintln(w, strings.Join(append(tot, ""), "\t"))
			if err := w.Flush(); err != nil {
				return err
			}
			if numbered {
				var legend []string
				for i, n := range mx.Nodes {
					legend = append(legend, fmt.Sprintf("%d=%s", i+1, n))
				}
				fmt.Println("\nnodes: " + strings.Join(legend, "  "))
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&problems, "problems", false, "only volumes whose replicas share a node or disk")
	return cmd
}

// MatrixSymbol renders one cell: "·" none, ●◐✖○ for one replica, else the
// count.
func MatrixSymbol(c longhorn.MatrixCell) string {
	switch {
	case c.Total() == 0:
		return "·"
	case c.Total() > 1:
		return fmt.Sprint(c.Total())
	case c.Healthy == 1:
		return "●"
	case c.Rebuilding == 1:
		return "◐"
	case c.Failed == 1:
		return "✖"
	}
	return "○"
}
