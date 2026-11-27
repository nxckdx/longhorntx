package copytree

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

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
// again).
func VerifyContent(dst string, hashes map[string][32]byte) ([]string, error) {
	buf := make([]byte, 1<<20)
	var bad []string
	for rel, want := range hashes {
		p := filepath.Join(dst, rel)
		var st unix.Stat_t
		if err := unix.Lstat(p, &st); err != nil {
			bad = append(bad, rel)
			continue
		}
		got, err := fileHash(p, st.Size, buf)
		if err != nil {
			return bad, err
		}
		if got != want {
			bad = append(bad, rel)
		}
	}
	return bad, nil
}

// Sync makes dst equal to src, copying only entries whose fingerprint
// changed since prev (or that are new), and removing what is gone. Hard
// linked files are always copied again (their link structure may have
// changed). It returns the copied entries (for a content check of them).
func Sync(src, dst string, prev *State, o Options) (Stats, []string, error) {
	if o.Every <= 0 {
		o.Every = 5 * time.Second
	}
	c := &copier{o: o, links: map[inodeKey]string{}, buf: make([]byte, 1<<20)}
	skip := skipSet(o.Skip)
	var changed []string
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
		var st, dt unix.Stat_t
		if err := unix.Lstat(p, &st); err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		dErr := unix.Lstat(target, &dt)
		isDir := st.Mode&unix.S_IFMT == unix.S_IFDIR
		if isDir {
			if dErr == nil && dt.Mode&unix.S_IFMT != unix.S_IFDIR {
				if err := os.RemoveAll(target); err != nil {
					return err
				}
			}
			if err := c.entry(p, target, rel); err != nil { // mkdir (exists is fine), xattrs, meta later
				return fmt.Errorf("%s: %w", rel, err)
			}
			return nil
		}
		old, known := prev.Entries[rel]
		if known && dErr == nil && old == fingerprintOf(&st) && st.Nlink == 1 && dt.Mode&unix.S_IFMT == st.Mode&unix.S_IFMT {
			c.st.Unchanged++
			return nil
		}
		if dErr == nil {
			if err := os.RemoveAll(target); err != nil {
				return err
			}
		}
		if err := c.entry(p, target, rel); err != nil {
			return fmt.Errorf("%s: %w", rel, err)
		}
		changed = append(changed, rel)
		if o.Progress != nil {
			o.Progress(c.st)
		}
		return nil
	})
	if err != nil {
		return c.st, changed, err
	}
	// what is gone from the source goes from the target
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
		var st unix.Stat_t
		if err := unix.Lstat(filepath.Join(src, rel), &st); errors.Is(err, unix.ENOENT) {
			c.st.Removed++
			if err := os.RemoveAll(p); err != nil {
				return err
			}
			if d.IsDir() {
				return filepath.SkipDir
			}
		}
		return nil
	})
	if err != nil {
		return c.st, changed, err
	}
	for i := len(c.dirs) - 1; i >= 0; i-- {
		if err := c.meta(c.dirs[i].path, &c.dirs[i].info, false); err != nil {
			return c.st, changed, err
		}
	}
	var root unix.Stat_t
	if err := unix.Lstat(src, &root); err == nil {
		_ = c.xattrs(src, dst)
		_ = c.meta(dst, &root, false)
	}
	return c.st, changed, nil
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
