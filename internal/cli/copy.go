package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/sys/unix"

	"github.com/nxckdx/longhorntx/internal/copytree"
)

// CopyReport is the last line lhtx copy prints (JSON): what lhtx shrink
// reads from the copy pod's log.
type CopyReport struct {
	Phase   string         `json:"phase"` // check | copy | verify | done | error
	Stats   copytree.Stats `json:"stats"`
	Used    int64          `json:"used,omitempty"` // bytes used in SRC
	Free    int64          `json:"free,omitempty"` // bytes free in DST
	Diff    []string       `json:"diff,omitempty"`
	Error   string         `json:"error,omitempty"`
	Seconds float64        `json:"seconds"`
}

func copyCmd() *cobra.Command {
	var skip []string
	var verify bool
	cmd := &cobra.Command{
		Use:    "copy SRC DST",
		Short:  "Copy a directory tree keeping owners, modes, links, xattrs and holes (used by lhtx shrink in its copy pod)",
		Hidden: true,
		Args:   cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			start := time.Now()
			enc := json.NewEncoder(os.Stdout)
			report := func(r CopyReport) {
				r.Seconds = time.Since(start).Seconds()
				_ = enc.Encode(r)
			}
			fail := func(r CopyReport, err error) error {
				r.Phase, r.Error = "error", err.Error()
				report(r)
				return err
			}
			var r CopyReport
			var s, d unix.Statfs_t
			if err := unix.Statfs(args[0], &s); err != nil {
				return fail(r, fmt.Errorf("statfs %s: %w", args[0], err))
			}
			if err := unix.Statfs(args[1], &d); err != nil {
				return fail(r, fmt.Errorf("statfs %s: %w", args[1], err))
			}
			r.Used = int64(s.Blocks-s.Bfree) * s.Bsize
			r.Free = int64(d.Bavail) * d.Bsize
			r.Phase = "check"
			report(r)
			if r.Used > r.Free {
				return fail(r, fmt.Errorf("%d bytes used in the source do not fit into %d bytes free in the target", r.Used, r.Free))
			}
			r.Phase = "copy"
			st, err := copytree.Copy(args[0], args[1], copytree.Options{Skip: skip, NoOwner: os.Geteuid() != 0,
				Progress: func(st copytree.Stats) { r.Stats = st; report(r) }})
			r.Stats = st
			if err != nil {
				return fail(r, err)
			}
			if verify {
				r.Phase = "verify"
				report(r)
				diff, err := copytree.Verify(args[0], args[1], skip, os.Geteuid() != 0, 20)
				if err != nil {
					return fail(r, err)
				}
				if len(diff) > 0 {
					r.Diff = diff
					return fail(r, fmt.Errorf("the copy differs from the source (%d+ entries)", len(diff)))
				}
			}
			r.Phase = "done"
			report(r)
			return nil
		},
	}
	cmd.Flags().StringSliceVar(&skip, "skip", []string{"lost+found"}, "entries of the source root to leave out")
	cmd.Flags().BoolVar(&verify, "verify", true, "compare both trees afterwards (type, mode, owner, size, mtime)")
	return cmd
}
