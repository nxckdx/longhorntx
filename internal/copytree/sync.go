package copytree

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"

	"golang.org/x/sys/unix"
)

// A shrink copies twice: a pre-copy while the app runs (Copy with Live and
// State), then – frozen – Sync, which copies only what changed since. An
// entry counts as unchanged if its fingerprint is the same; the ctime is
// part of it, and every change to a file (content, owner, mode, xattrs,
// links) moves the ctime – nobody can set it back.

// Fingerprint of an entry in the source.
type Fingerprint struct {
	Ino   uint64 `json:"i"`
	Ctime int64  `json:"c"`
	Mtime int64  `json:"m"`
	Size  int64  `json:"s"`
	Mode  uint32 `json:"o"`
	Nlink uint64 `json:"n"`
}

func fingerprintOf(st *unix.Stat_t) Fingerprint {
	return Fingerprint{Ino: st.Ino, Ctime: st.Ctim.Nano(), Mtime: st.Mtim.Nano(), Size: st.Size, Mode: st.Mode, Nlink: uint64(st.Nlink)}
}

// State: the fingerprints of what the pre-copy took consistently.
type State struct {
	Entries map[string]Fingerprint `json:"entries"`
	// NoXattrs: the pre-copy could not read extended attributes (over
	// NFS) – Sync sets them on every entry.
	NoXattrs bool `json:"noXattrs,omitempty"`
}

// NewState is an empty State.
func NewState() *State { return &State{Entries: map[string]Fingerprint{}} }

// Save / LoadState: the state travels from the pre-copy pod to the final
// one in a file on the target volume (left out of the copy).
func (s *State) Save(path string) error {
	b, err := json.Marshal(s)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func LoadState(path string) (*State, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	s := NewState()
	if err := json.Unmarshal(b, s); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return s, nil
}

// VerifyContent reads every file of hashes in dst and returns those whose
// content differs (a pre-copy drops them from its state: Sync copies them
// again). workers: files read at the same time.
func VerifyContent(dst string, hashes map[string][32]byte, workers int) ([]string, error) {
	var mu sync.Mutex
	var bad []string
	pool := newPool(workers)
	for rel, want := range hashes {
		err := pool.do(func(buf []byte) error {
			p := filepath.Join(dst, rel)
			var st unix.Stat_t
			if err := unix.Lstat(p, &st); err != nil {
				mu.Lock()
				bad = append(bad, rel)
				mu.Unlock()
				return nil
			}
			got, err := fileHash(p, st.Size, buf)
			if err != nil {
				return err
			}
			if got != want {
				mu.Lock()
				bad = append(bad, rel)
				mu.Unlock()
			}
			return nil
		})
		if err != nil {
			break
		}
	}
	err := pool.wait()
	return bad, err
}

// Sync makes dst equal to src, copying only entries whose fingerprint
// changed since prev (or that are new), and removing what is gone. Hard
// linked files are always copied again (their link structure may have
// changed). It returns the copied entries (for a content check of them).
// It runs while the app is stopped: with Workers, the entries are compared
// (two lstats each) and copied in parallel.
func Sync(src, dst string, prev *State, o Options) (Stats, []string, error) {
	c := newCopier(o)
	skip := skipSet(o.Skip)
	var changed []string
	one := func(p, rel string, buf []byte) error {
		var st, dt unix.Stat_t
		if err := unix.Lstat(p, &st); err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		dErr := unix.Lstat(target, &dt)
		if st.Mode&unix.S_IFMT == unix.S_IFDIR {
			if dErr == nil && dt.Mode&unix.S_IFMT != unix.S_IFDIR {
				if err := os.RemoveAll(target); err != nil {
					return err
				}
			}
			if err := c.entry(p, target, rel, buf); err != nil { // mkdir (exists is fine), xattrs, meta later
				return fmt.Errorf("%s: %w", rel, err)
			}
			return nil
		}
		old, known := prev.Entries[rel]
		if known && dErr == nil && old == fingerprintOf(&st) && st.Nlink == 1 && dt.Mode&unix.S_IFMT == st.Mode&unix.S_IFMT {
			if prev.NoXattrs {
				// the content is there, the attributes are not yet
				if err := c.xattrs(p, target); err != nil {
					return fmt.Errorf("%s: %w", rel, err)
				}
				if err := copyXattr(p, target, "security.capability"); err != nil && !errors.Is(err, unix.ENODATA) && !errors.Is(err, unix.ENOTSUP) {
					return fmt.Errorf("%s: %w", rel, err)
				}
			}
			c.add(func(s *Stats) { s.Unchanged++ })
			return nil
		}
		if dErr == nil {
			if err := os.RemoveAll(target); err != nil {
				return err
			}
		}
		if err := c.entry(p, target, rel, buf); err != nil {
			return fmt.Errorf("%s: %w", rel, err)
		}
		c.mu.Lock()
		changed = append(changed, rel)
		c.mu.Unlock()
		return nil
	}
	err := filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		if rel == "." {
			return nil
		}
		if skip[rel] {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		c.progress()
		if d.IsDir() {
			return one(p, rel, c.pool.buf)
		}
		return c.pool.do(func(buf []byte) error { return one(p, rel, buf) })
	})
	if err == nil {
		err = c.pool.wait()
	}
	if err != nil {
		_ = c.pool.wait()
		return c.stats(), changed, err
	}
	// what is gone from the source goes from the target
	c.pool = newPool(o.Workers)
	err = filepath.WalkDir(dst, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		rel, _ := filepath.Rel(dst, p)
		if rel == "." {
			return nil
		}
		if skip[rel] {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		gone := func() (bool, error) {
			var st unix.Stat_t
			err := unix.Lstat(filepath.Join(src, rel), &st)
			if errors.Is(err, unix.ENOENT) {
				c.add(func(s *Stats) { s.Removed++ })
				return true, os.RemoveAll(p)
			}
			return false, nil
		}
		if d.IsDir() {
			g, err := gone()
			if g && err == nil {
				return filepath.SkipDir
			}
			return err
		}
		return c.pool.do(func([]byte) error { _, err := gone(); return err })
	})
	if err != nil {
		_ = c.pool.wait()
		return c.stats(), changed, err
	}
	err = c.finish(src, dst)
	return c.stats(), changed, err
}

// Clear removes everything in dir except skip (a full copy after a failed
// pre-copy starts from an empty target).
func Clear(dir string, skip []string) error {
	sk := skipSet(skip)
	ents, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range ents {
		if sk[e.Name()] {
			continue
		}
		if err := os.RemoveAll(filepath.Join(dir, e.Name())); err != nil {
			return err
		}
	}
	return nil
}
