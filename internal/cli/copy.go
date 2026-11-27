package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/sys/unix"

	"github.com/nxckdx/longhorntx/internal/copytree"
)

// CopyReport is a line lhtx copy prints (JSON): what lhtx shrink reads
// from the copy pod's log.
type CopyReport struct {
	Phase      string         `json:"phase"` // probe | check | copy | verify | done | error
	Stats      copytree.Stats `json:"stats"`
	Used       int64          `json:"used,omitempty"`       // bytes used in SRC
	Free       int64          `json:"free,omitempty"`       // bytes free in DST
	UsedInodes int64          `json:"usedInodes,omitempty"` // in SRC
	FreeInodes int64          `json:"freeInodes,omitempty"` // in DST
	Checked    int64          `json:"checked,omitempty"`    // entries verified
	Diff       []string       `json:"diff,omitempty"`
	Error      string         `json:"error,omitempty"`
	Seconds    float64        `json:"seconds"`
}

func copyCmd() *cobra.Command {
	var skip []string
	var verify string
	var probe bool
	cmd := &cobra.Command{
		Use:    "copy SRC DST | copy --probe DST",
		Short:  "Copy a directory tree keeping owners, modes, links, xattrs and holes, then verify (lhtx shrink's copy pod)",
		Hidden: true,
		Args:   cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			start := time.Now()
			enc := json.NewEncoder(os.Stdout)
			var r CopyReport
			report := func() {
				r.Seconds = time.Since(start).Seconds()
				_ = enc.Encode(r)
			}
			fail := func(err error) error {
				r.Phase, r.Error = "error", err.Error()
				report()
				return err
			}
			if probe {
				// the pre-flight pod: the target is mounted, writable, empty
				dst := args[len(args)-1]
				var d unix.Statfs_t
				if err := unix.Statfs(dst, &d); err != nil {
					return fail(fmt.Errorf("statfs %s: %w", dst, err))
				}
				r.Free, r.FreeInodes = int64(d.Bavail)*d.Bsize, int64(d.Ffree)
				f := filepath.Join(dst, ".lhtx-probe")
				if err := os.WriteFile(f, []byte("ok"), 0o600); err != nil {
					return fail(fmt.Errorf("the new volume is not writable: %w", err))
				}
				_ = os.Remove(f)
				r.Phase = "probe"
				report()
				return nil
			}
			if len(args) != 2 {
				return fmt.Errorf("give SRC and DST")
			}
			src, dst := args[0], args[1]
			var s, d unix.Statfs_t
			if err := unix.Statfs(src, &s); err != nil {
				return fail(fmt.Errorf("statfs %s: %w", src, err))
			}
			if err := unix.Statfs(dst, &d); err != nil {
				return fail(fmt.Errorf("statfs %s: %w", dst, err))
			}
			r.Used = int64(s.Blocks-s.Bfree) * s.Bsize
			r.Free = int64(d.Bavail) * d.Bsize
			r.UsedInodes = int64(s.Files - s.Ffree)
			r.FreeInodes = int64(d.Ffree)
			r.Phase = "check"
			report()
			if r.Used > r.Free {
				return fail(fmt.Errorf("%s used in the old volume do not fit into %s free in the new one", bytesStr(r.Used), bytesStr(r.Free)))
			}
			if s.Files > 0 && d.Files > 0 && r.UsedInodes > r.FreeInodes {
				return fail(fmt.Errorf("%d files and directories in the old volume, the new filesystem has room for %d – choose a larger size", r.UsedInodes, r.FreeInodes))
			}
			var hashes map[string][32]byte
			if verify == "content" {
				hashes = map[string][32]byte{}
			}
			r.Phase = "copy"
			st, err := copytree.Copy(src, dst, copytree.Options{Skip: skip, NoOwner: os.Geteuid() != 0, Hashes: hashes,
				Progress: func(st copytree.Stats) { r.Stats = st; report() }})
			r.Stats = st
			if err != nil {
				return fail(err)
			}
			unix.Sync() // on disk before anything depends on it
			if verify != "none" {
				r.Phase = "verify"
				report()
				last := time.Now()
				diff, err := copytree.Verify(src, dst, copytree.VerifyOptions{Skip: skip, NoOwner: os.Geteuid() != 0, Hashes: hashes,
					Progress: func(n int64) {
						if time.Since(last) > 5*time.Second {
							last = time.Now()
							r.Checked = n
							report()
						}
					}})
				if err != nil {
					return fail(err)
				}
				if len(diff) > 0 {
					r.Diff = diff
					return fail(fmt.Errorf("the copy differs from the source"))
				}
			}
			r.Phase = "done"
			report()
			return nil
		},
	}
	cmd.Flags().StringSliceVar(&skip, "skip", []string{"lost+found"}, "entries of the source root to leave out")
	cmd.Flags().StringVar(&verify, "verify", "content", "compare afterwards: content (metadata + SHA-256 of every file), meta, none")
	cmd.Flags().BoolVar(&probe, "probe", false, "only check that DST is mounted and writable (pre-flight)")
	return cmd
}

func bytesStr(n int64) string { return humanSize(n) }
