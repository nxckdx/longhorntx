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
	Changed    int64          `json:"changed,omitempty"`    // final pass: entries copied again
	Diff       []string       `json:"diff,omitempty"`
	Error      string         `json:"error,omitempty"`
	Seconds    float64        `json:"seconds"`
}

func copyCmd() *cobra.Command {
	var skip []string
	var verify string
	var probe bool
	var mode string
	var jobs int
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
			var sfs unix.Statfs_t
			if err := unix.Statfs(src, &sfs); err != nil {
				return fail(fmt.Errorf("statfs %s: %w", src, err))
			}
			// an RWX volume's pre-copy reads over NFS (4.1: no xattrs); the
			// copy that counts reads the block device
			overNFS := sfs.Type == unix.NFS_SUPER_MAGIC
			if overNFS && mode != "pre" {
				return fail(fmt.Errorf("%s is NFS: extended attributes (ACLs, capabilities) cannot be read over it – only a pre-copy may read NFS", src))
			}
			statePath := filepath.Join(dst, copyStateFile)
			skip = append(skip, copyStateFile, copyStateFile+".tmp")
			noOwner := os.Geteuid() != 0
			progress := func(st copytree.Stats) { r.Stats = st; report() }
			if mode == "final" {
				prev, err := copytree.LoadState(statePath)
				if err == nil {
					// the pre-copy ran: only what changed since
					r.Phase = "sync"
					report()
					hashes := map[string][32]byte{}
					st, changed, err := copytree.Sync(src, dst, prev, copytree.Options{Skip: skip, NoOwner: noOwner, Hashes: hashes, Progress: progress, Workers: jobs})
					r.Stats = st
					if err != nil {
						return fail(err)
					}
					unix.Sync()
					r.Phase = "verify"
					report()
					diff, err := copytree.Verify(src, dst, copytree.VerifyOptions{Skip: skip, NoOwner: noOwner, Hashes: hashes, Workers: jobs})
					if err != nil {
						return fail(err)
					}
					if len(diff) > 0 {
						r.Diff = diff
						return fail(fmt.Errorf("the copy differs from the source"))
					}
					_ = os.Remove(statePath)
					unix.Sync()
					r.Changed = int64(len(changed))
					r.Phase = "done"
					report()
					return nil
				}
				// no (usable) pre-copy: a full copy into an empty target
				if err := copytree.Clear(dst, []string{"lost+found"}); err != nil {
					return fail(fmt.Errorf("clear the new volume: %w", err))
				}
			}
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
			if verify == "content" || mode == "pre" {
				hashes = map[string][32]byte{}
			}
			r.Phase = "copy"
			o := copytree.Options{Skip: skip, NoOwner: noOwner, Hashes: hashes, Progress: progress, Workers: jobs}
			var state *copytree.State
			if mode == "pre" {
				state = copytree.NewState()
				o.Live, o.State = true, state
				o.NoXattrs, state.NoXattrs = overNFS, overNFS
			}
			st, err := copytree.Copy(src, dst, o)
			r.Stats = st
			if err != nil {
				return fail(err)
			}
			unix.Sync() // on disk before anything depends on it
			if mode == "pre" {
				// what did not arrive intact is left to the final pass
				r.Phase = "verify"
				report()
				bad, err := copytree.VerifyContent(dst, hashes, jobs)
				if err != nil {
					return fail(err)
				}
				for _, rel := range bad {
					delete(state.Entries, rel)
				}
				r.Stats.Dirty += int64(len(bad))
				if err := state.Save(statePath); err != nil {
					return fail(fmt.Errorf("save the pre-copy state: %w", err))
				}
				unix.Sync()
				r.Phase = "done"
				report()
				return nil
			}
			if verify != "none" {
				r.Phase = "verify"
				report()
				last := time.Now()
				diff, err := copytree.Verify(src, dst, copytree.VerifyOptions{Skip: skip, NoOwner: noOwner, Hashes: hashes, Workers: jobs,
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
	cmd.Flags().StringVar(&mode, "mode", "full", "full: copy and verify; pre: copy a source in use, remember what arrived intact; final: copy only what changed since pre (else full)")
	cmd.Flags().IntVar(&jobs, "jobs", 16, "files copied and verified at the same time (many small files: each is a few round trips to the disk)")
	cmd.Flags().BoolVar(&probe, "probe", false, "only check that DST is mounted and writable (pre-flight)")
	return cmd
}

func bytesStr(n int64) string { return humanSize(n) }

// copyStateFile: the pre-copy's state on the target volume, removed by the
// final pass.
const copyStateFile = ".lhtx-shrink-state.json"
