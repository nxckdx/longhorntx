// Package copytree copies a directory tree with everything a filesystem
// copy must keep for an application not to notice: modes (incl. setuid),
// owners, timestamps, hard links, symlinks, device files and FIFOs,
// extended attributes (incl. POSIX ACLs and capabilities), and holes in
// sparse files. It is what `lhtx copy` runs inside the copy pod of a
// shrink, so no foreign rsync image is needed.
package copytree

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"time"

	"golang.org/x/sys/unix"
)

// Stats counts what was copied.
type Stats struct {
	Dirs      int64 `json:"dirs"`
	Files     int64 `json:"files"`
	Symlinks  int64 `json:"symlinks"`
	Hardlinks int64 `json:"hardlinks"`
	Special   int64 `json:"special"`
	Bytes     int64 `json:"bytes"` // file content written (holes not counted)
	Xattrs    int64 `json:"xattrs"`
}

// Options: Progress is called now and then with the stats so far; Skip
// names entries of the source root to leave out (lost+found).
type Options struct {
	Progress func(Stats)
	Every    time.Duration // progress interval (default 5s)
	Skip     []string
	// NoOwner: do not chown (tests as non-root).
	NoOwner bool
}

type inodeKey struct{ dev, ino uint64 }

type copier struct {
	o     Options
	st    Stats
	links map[inodeKey]string // first copied path of a multiply linked file
	last  time.Time
	dirs  []dirMeta // set after their contents
}

type dirMeta struct {
	path string
	info unix.Stat_t
}

// Copy copies the contents of src into dst (both existing directories).
func Copy(src, dst string, o Options) (Stats, error) {
	if o.Every <= 0 {
		o.Every = 5 * time.Second
	}
	c := &copier{o: o, links: map[inodeKey]string{}, last: time.Now()}
	skip := map[string]bool{}
	for _, s := range o.Skip {
		skip[s] = true
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
		if err := c.entry(p, filepath.Join(dst, rel)); err != nil {
			return fmt.Errorf("%s: %w", rel, err)
		}
		if o.Progress != nil && time.Since(c.last) >= o.Every {
			c.last = time.Now()
			o.Progress(c.st)
		}
		return nil
	})
	if err != nil {
		return c.st, err
	}
	// directories last-to-first: their times change while filling them
	for i := len(c.dirs) - 1; i >= 0; i-- {
		if err := c.meta(c.dirs[i].path, &c.dirs[i].info, false); err != nil {
			return c.st, err
		}
	}
	var root unix.Stat_t
	if err := unix.Lstat(src, &root); err == nil {
		_ = c.meta(dst, &root, false)
	}
	return c.st, nil
}

func (c *copier) entry(src, dst string) error {
	var st unix.Stat_t
	if err := unix.Lstat(src, &st); err != nil {
		return err
	}
	switch st.Mode & unix.S_IFMT {
	case unix.S_IFDIR:
		if err := os.Mkdir(dst, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
			return err
		}
		if err := c.xattrs(src, dst); err != nil {
			return err
		}
		c.st.Dirs++
		c.dirs = append(c.dirs, dirMeta{dst, st})
		return nil
	case unix.S_IFLNK:
		target, err := os.Readlink(src)
		if err != nil {
			return err
		}
		if err := os.Symlink(target, dst); err != nil {
			return err
		}
		c.st.Symlinks++
		if err := c.xattrs(src, dst); err != nil {
			return err
		}
		return c.meta(dst, &st, true)
	}
	// a file (or special) with more links: link to the first copy
	if st.Nlink > 1 {
		k := inodeKey{uint64(st.Dev), st.Ino}
		if first, ok := c.links[k]; ok {
			c.st.Hardlinks++
			return os.Link(first, dst)
		}
		c.links[k] = dst
	}
	switch st.Mode & unix.S_IFMT {
	case unix.S_IFREG:
		if err := c.file(src, dst, st.Size); err != nil {
			return err
		}
		c.st.Files++
	case unix.S_IFCHR, unix.S_IFBLK, unix.S_IFIFO:
		if err := unix.Mknod(dst, st.Mode, int(st.Rdev)); err != nil {
			return err
		}
		c.st.Special++
	case unix.S_IFSOCK:
		return nil // sockets are created by their server, not copied
	default:
		return fmt.Errorf("unknown file type %o", st.Mode&unix.S_IFMT)
	}
	if err := c.xattrs(src, dst); err != nil {
		return err
	}
	if err := c.meta(dst, &st, false); err != nil {
		return err
	}
	// file capabilities last: chown in meta would drop them
	if err := copyXattr(src, dst, "security.capability"); err != nil && !errors.Is(err, unix.ENODATA) && !errors.Is(err, unix.ENOTSUP) {
		return err
	}
	return nil
}

const block = 1 << 16

var zeros = make([]byte, block)

// file copies content; all-zero blocks become holes (the file is sparse
// in the copy even if it was not – the content is the same).
func (c *copier) file(src, dst string, size int64) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	buf := make([]byte, block)
	var off int64
	for {
		n, err := io.ReadFull(in, buf)
		if n > 0 {
			if bytes.Equal(buf[:n], zeros[:n]) {
				off += int64(n)
			} else {
				if _, werr := out.WriteAt(buf[:n], off); werr != nil {
					out.Close()
					return werr
				}
				off += int64(n)
				c.st.Bytes += int64(n)
			}
		}
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			break
		}
		if err != nil {
			out.Close()
			return err
		}
	}
	if off != size {
		out.Close()
		return fmt.Errorf("read %d bytes, expected %d (file changed while copying?)", off, size)
	}
	if err := out.Truncate(size); err != nil { // trailing hole
		out.Close()
		return err
	}
	return out.Close()
}

// xattrs copies extended attributes (POSIX ACLs are system.posix_acl_*,
// file capabilities security.capability – set after chown, see meta).
func (c *copier) xattrs(src, dst string) error {
	sz, err := unix.Llistxattr(src, nil)
	if err != nil {
		if errors.Is(err, unix.ENOTSUP) {
			return nil
		}
		return err
	}
	if sz == 0 {
		return nil
	}
	names := make([]byte, sz)
	sz, err = unix.Llistxattr(src, names)
	if err != nil {
		return err
	}
	for name := range bytes.SplitSeq(names[:sz], []byte{0}) {
		if len(name) == 0 || string(name) == "security.capability" {
			continue // capabilities are dropped by chown: set in meta
		}
		if err := copyXattr(src, dst, string(name)); err != nil {
			return err
		}
		c.st.Xattrs++
	}
	return nil
}

func copyXattr(src, dst, name string) error {
	sz, err := unix.Lgetxattr(src, name, nil)
	if err != nil {
		return err
	}
	val := make([]byte, sz)
	if sz, err = unix.Lgetxattr(src, name, val); err != nil {
		return err
	}
	if err := unix.Lsetxattr(dst, name, val[:sz], 0); err != nil && !errors.Is(err, unix.ENOTSUP) {
		return fmt.Errorf("xattr %s: %w", name, err)
	}
	return nil
}

// meta sets owner, mode, capabilities and times – in that order: chown
// clears setuid bits and capabilities.
func (c *copier) meta(dst string, st *unix.Stat_t, link bool) error {
	if !c.o.NoOwner {
		if err := unix.Lchown(dst, int(st.Uid), int(st.Gid)); err != nil {
			return err
		}
	}
	if !link {
		if err := unix.Chmod(dst, st.Mode&0o7777); err != nil {
			return err
		}
	}
	ts := []unix.Timespec{st.Atim, st.Mtim}
	return unix.UtimesNanoAt(unix.AT_FDCWD, dst, ts, unix.AT_SYMLINK_NOFOLLOW)
}

// ---------------------------------------------------------------- verify

// Entry is what Verify compares.
type Entry struct {
	Mode     uint32
	UID, GID uint32
	Size     int64
	Link     string
	MtimeNs  int64
}

// Verify compares two trees: the same entries with the same type, mode,
// owner, size, symlink target and modification time. It returns the
// differences (at most max).
func Verify(src, dst string, skip []string, noOwner bool, max int) ([]string, error) {
	a, err := list(src, skip)
	if err != nil {
		return nil, err
	}
	b, err := list(dst, skip)
	if err != nil {
		return nil, err
	}
	var diff []string
	add := func(f string, args ...any) {
		if len(diff) < max {
			diff = append(diff, fmt.Sprintf(f, args...))
		}
	}
	var names []string
	for n := range a {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		x := a[n]
		y, ok := b[n]
		if !ok {
			add("missing: %s", n)
			continue
		}
		if noOwner {
			x.UID, x.GID, y.UID, y.GID = 0, 0, 0, 0
		}
		if x.Mode&unix.S_IFMT == unix.S_IFDIR {
			x.Size, y.Size = 0, 0 // directory sizes depend on the filesystem
		}
		if x != y {
			add("differs: %s (%+v vs %+v)", n, x, y)
		}
	}
	for n := range b {
		if _, ok := a[n]; !ok {
			add("extra: %s", n)
		}
	}
	return diff, nil
}

func list(root string, skip []string) (map[string]Entry, error) {
	sk := map[string]bool{}
	for _, s := range skip {
		sk[s] = true
	}
	out := map[string]Entry{}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		if rel == "." {
			return nil
		}
		if sk[rel] {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		var st unix.Stat_t
		if err := unix.Lstat(p, &st); err != nil {
			return err
		}
		if st.Mode&unix.S_IFMT == unix.S_IFSOCK {
			return nil
		}
		e := Entry{Mode: st.Mode, UID: st.Uid, GID: st.Gid, Size: st.Size, MtimeNs: st.Mtim.Nano()}
		if st.Mode&unix.S_IFMT == unix.S_IFLNK {
			e.Link, _ = os.Readlink(p)
			e.MtimeNs = 0 // not every filesystem keeps symlink times exactly
		}
		out[rel] = e
		return nil
	})
	return out, err
}
