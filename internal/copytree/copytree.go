// Package copytree copies a directory tree with everything a filesystem
// copy must keep for an application not to notice: modes (incl. setuid),
// owners, timestamps, hard links, symlinks, device files and FIFOs,
// extended attributes (incl. POSIX ACLs and capabilities), and holes in
// sparse files. It is what `lhtx copy` runs inside the copy pod of a
// shrink, so no foreign rsync image is needed.
package copytree

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
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
	Bytes     int64 `json:"bytes"` // file content read (holes not counted)
	Xattrs    int64 `json:"xattrs"`
	// XattrsSkipped: attributes the target refused (SELinux labels under a
	// context= mount, trusted.* without CAP_SYS_ADMIN) – see Skipped.
	XattrsSkipped int64    `json:"xattrsSkipped,omitempty"`
	Skipped       []string `json:"skipped,omitempty"` // first few, "path: attr: error"
	// Dirty: entries a live copy could not take consistently (changed or
	// vanished while copying) – the final pass copies them.
	Dirty int64 `json:"dirty,omitempty"`
	// Removed: entries the final pass deleted (gone from the source).
	Removed int64 `json:"removed,omitempty"`
	// Unchanged: entries the final pass kept from the pre-copy.
	Unchanged int64 `json:"unchanged,omitempty"`
}

// Options: Progress is called now and then with the stats so far; Skip
// names entries of the source root to leave out (lost+found).
type Options struct {
	Progress func(Stats)
	Every    time.Duration // progress interval (default 5s)
	Skip     []string
	// NoOwner: do not chown (tests as non-root).
	NoOwner bool
	// Hashes: record a SHA-256 of every regular file's content as it is
	// read (for Verify, without reading the source twice).
	Hashes map[string][32]byte
	// Live: the source is in use (the pre-copy of a shrink): an entry that
	// changes or vanishes while it is copied is skipped (Stats.Dirty), not
	// an error.
	Live bool
	// State: record each entry's fingerprint for a later Sync.
	State *State
	// Workers: entries copied at the same time (default 1). Directories
	// are always made by the walk itself, in order.
	Workers int
	// NoXattrs: copy no extended attributes – the source cannot give them
	// (NFS before 4.2: the pre-copy of an RWX volume). The State records
	// it: Sync then sets them on every entry, also unchanged ones.
	NoXattrs bool
}

type inodeKey struct{ dev, ino uint64 }

type copier struct {
	o     Options
	mu    sync.Mutex // st, links, o.Hashes, o.State (workers)
	st    Stats
	links map[inodeKey]*linkTarget // first copy of a multiply linked file
	last  time.Time
	dirs  []dirMeta // set after their contents (walk only)
	pool  *pool
}

func newCopier(o Options) *copier {
	if o.Every <= 0 {
		o.Every = 5 * time.Second
	}
	return &copier{o: o, links: map[inodeKey]*linkTarget{}, last: time.Now(), pool: newPool(o.Workers)}
}

func (c *copier) add(f func(*Stats)) {
	c.mu.Lock()
	f(&c.st)
	c.mu.Unlock()
}

// stats: a copy of the counts so far.
func (c *copier) stats() Stats {
	c.mu.Lock()
	defer c.mu.Unlock()
	st := c.st
	st.Skipped = append([]string(nil), c.st.Skipped...)
	return st
}

func (c *copier) setHash(rel string, sum *[32]byte) {
	if c.o.Hashes == nil {
		return
	}
	c.mu.Lock()
	if sum == nil {
		delete(c.o.Hashes, rel)
	} else {
		c.o.Hashes[rel] = *sum
	}
	c.mu.Unlock()
}

func (c *copier) progress() {
	if c.o.Progress == nil || time.Since(c.last) < c.o.Every {
		return
	}
	c.last = time.Now()
	c.o.Progress(c.stats())
}

// finish waits for the workers, then sets the directories' metadata (last
// to first: their times change while filling them) and the root's.
func (c *copier) finish(src, dst string) error {
	if err := c.pool.wait(); err != nil {
		return err
	}
	for i := len(c.dirs) - 1; i >= 0; i-- {
		if err := c.meta(c.dirs[i].path, &c.dirs[i].info, false); err != nil {
			return err
		}
	}
	var root unix.Stat_t
	if err := unix.Lstat(src, &root); err == nil {
		_ = c.xattrs(src, dst)
		_ = c.meta(dst, &root, false)
	}
	return nil
}

type dirMeta struct {
	path string
	info unix.Stat_t
}

// Copy copies the contents of src into dst (both existing directories).
func Copy(src, dst string, o Options) (Stats, error) {
	c := newCopier(o)
	skip := skipSet(o.Skip)
	// one entry; dirty: what a live copy leaves to the final pass
	one := func(p, rel string, isDir bool, buf []byte) error {
		target := filepath.Join(dst, rel)
		dirty := func() error {
			c.add(func(s *Stats) { s.Dirty++ })
			if !isDir {
				_ = os.Remove(target) // a partial copy
			}
			c.setHash(rel, nil)
			return nil
		}
		var before unix.Stat_t
		if o.State != nil {
			if err := unix.Lstat(p, &before); err != nil {
				if o.Live {
					return dirty()
				}
				return err
			}
		}
		if err := c.entry(p, target, rel, buf); err != nil {
			if o.Live {
				return dirty()
			}
			return fmt.Errorf("%s: %w", rel, err)
		}
		if o.State != nil {
			var after unix.Stat_t
			if err := unix.Lstat(p, &after); err == nil && fingerprintOf(&before) == fingerprintOf(&after) {
				c.mu.Lock()
				o.State.Entries[rel] = fingerprintOf(&before)
				c.mu.Unlock()
			} else {
				c.add(func(s *Stats) { s.Dirty++ }) // changed while copying
				c.setHash(rel, nil)
			}
		}
		return nil
	}
	err := filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if o.Live && p != src {
				c.add(func(s *Stats) { s.Dirty++ }) // vanished while walking
				return nil
			}
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
			// made here, before what is in it
			return one(p, rel, true, c.pool.buf)
		}
		return c.pool.do(func(buf []byte) error { return one(p, rel, false, buf) })
	})
	if err != nil {
		_ = c.pool.wait()
		return c.stats(), err
	}
	err = c.finish(src, dst)
	return c.stats(), err
}

func skipSet(s []string) map[string]bool {
	m := map[string]bool{}
	for _, x := range s {
		m[x] = true
	}
	return m
}

func (c *copier) entry(src, dst, rel string, buf []byte) (err error) {
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
		c.add(func(s *Stats) { s.Dirs++ })
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
		c.add(func(s *Stats) { s.Symlinks++ })
		if err := c.xattrs(src, dst); err != nil {
			return err
		}
		return c.meta(dst, &st, true)
	}
	// a file (or special) with more links: link to the first copy (that
	// another worker may still be making)
	if st.Nlink > 1 {
		k := inodeKey{uint64(st.Dev), st.Ino}
		c.mu.Lock()
		first, ok := c.links[k]
		if !ok {
			first = &linkTarget{path: dst, done: make(chan struct{})}
			c.links[k] = first
		}
		c.mu.Unlock()
		if ok {
			<-first.done
			if first.err != nil {
				return fmt.Errorf("its other link %s was not copied: %w", first.path, first.err)
			}
			c.add(func(s *Stats) { s.Hardlinks++ })
			return os.Link(first.path, dst)
		}
		defer func() { first.err = err; close(first.done) }()
	}
	switch st.Mode & unix.S_IFMT {
	case unix.S_IFREG:
		var h hash.Hash
		if c.o.Hashes != nil {
			h = sha256.New()
		}
		if err := c.file(src, dst, st.Size, h, buf); err != nil {
			return err
		}
		if h != nil {
			var sum [32]byte
			copy(sum[:], h.Sum(nil))
			c.setHash(rel, &sum)
		}
		c.add(func(s *Stats) { s.Files++ })
	case unix.S_IFCHR, unix.S_IFBLK, unix.S_IFIFO:
		if err := unix.Mknod(dst, st.Mode, int(st.Rdev)); err != nil {
			return err
		}
		c.add(func(s *Stats) { s.Special++ })
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
	if c.o.NoXattrs {
		return nil
	}
	if err := copyXattr(src, dst, "security.capability"); err != nil && !errors.Is(err, unix.ENODATA) && !errors.Is(err, unix.ENOTSUP) {
		return err
	}
	return nil
}

const zeroBlock = 1 << 16

var zeros = make([]byte, zeroBlock)

// scanData calls fn for every 64 KiB block of f that is not all zeros, in
// order. Holes (SEEK_DATA/SEEK_HOLE) are not read at all; the blocks are
// aligned to 64 KiB whatever the hole layout, so source and copy give the
// same blocks.
func scanData(f *os.File, size int64, buf []byte, fn func(off int64, block []byte) error) (read int64, err error) {
	fd := int(f.Fd())
	off := int64(0)
	for off < size {
		data, err := unix.Seek(fd, off, unix.SEEK_DATA)
		if errors.Is(err, unix.ENXIO) {
			break // only a hole up to the end
		}
		if err != nil { // no SEEK_DATA support: everything is data
			data = off
		}
		hole, err := unix.Seek(fd, data, unix.SEEK_HOLE)
		if err != nil || hole > size {
			hole = size
		}
		data = max(off, data&^(zeroBlock-1))
		hole = min(size, (hole+zeroBlock-1)&^(zeroBlock-1))
		for pos := data; pos < hole; {
			n, err := f.ReadAt(buf[:min(int64(len(buf)), hole-pos)], pos)
			if n == 0 && err != nil {
				if err == io.EOF {
					return read, fmt.Errorf("file shrank while reading (%d of %d bytes)", pos, size)
				}
				return read, err
			}
			read += int64(n)
			for i := 0; i < n; i += zeroBlock {
				part := buf[i:min(i+zeroBlock, n)]
				if !bytes.Equal(part, zeros[:len(part)]) {
					if err := fn(pos+int64(i), part); err != nil {
						return read, err
					}
				}
			}
			pos += int64(n)
		}
		off = hole
	}
	return read, nil
}

// blockHash: the content hash Copy records and Verify compares – size plus
// every non-zero 64 KiB block with its offset (holes cost nothing).
func blockHash(h hash.Hash, off int64, block []byte) {
	var o [8]byte
	for i := range 8 {
		o[i] = byte(off >> (8 * i))
	}
	h.Write(o[:])
	h.Write(block)
}

// file copies the content of src: holes stay holes, all-zero blocks become
// holes. h, if set, gets the block hash.
func (c *copier) file(src, dst string, size int64, h hash.Hash, buf []byte) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	fail := func(err error) error { out.Close(); return err }
	if h != nil {
		blockHash(h, size, nil)
	}
	read, err := scanData(in, size, buf, func(off int64, block []byte) error {
		if h != nil {
			blockHash(h, off, block)
		}
		_, err := out.WriteAt(block, off)
		return err
	})
	c.add(func(s *Stats) { s.Bytes += read })
	if err != nil {
		return fail(err)
	}
	if err := out.Truncate(size); err != nil { // trailing hole
		return fail(err)
	}
	if fi, err := in.Stat(); err == nil && fi.Size() != size {
		return fail(fmt.Errorf("file changed size while copying (%d → %d)", size, fi.Size()))
	}
	return out.Close()
}

// xattrs copies extended attributes (POSIX ACLs are system.posix_acl_*,
// file capabilities security.capability – set after chown, see entry).
// The target refusing a security.* or trusted.* attribute (SELinux under
// a context= mount, trusted.* without CAP_SYS_ADMIN) is counted, not fatal.
func (c *copier) xattrs(src, dst string) error {
	if c.o.NoXattrs {
		return nil
	}
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
		n := string(name)
		if n == "" || n == "security.capability" {
			continue // capabilities are dropped by chown: set in entry
		}
		err := copyXattr(src, dst, n)
		switch {
		case err == nil:
			c.add(func(s *Stats) { s.Xattrs++ })
		case strings.HasPrefix(n, "security.") || strings.HasPrefix(n, "trusted."):
			c.add(func(s *Stats) {
				s.XattrsSkipped++
				if len(s.Skipped) < 10 {
					s.Skipped = append(s.Skipped, fmt.Sprintf("%s: %s: %v", dst, n, err))
				}
			})
		default:
			return err
		}
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

// meta sets owner, mode and times – in that order: chown clears setuid
// bits.
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
	Nlink    uint64
}

func entryOf(p string, st *unix.Stat_t) Entry {
	e := Entry{Mode: st.Mode, UID: st.Uid, GID: st.Gid, Size: st.Size, MtimeNs: st.Mtim.Nano(), Nlink: uint64(st.Nlink)}
	switch st.Mode & unix.S_IFMT {
	case unix.S_IFLNK:
		e.Link, _ = os.Readlink(p)
		e.MtimeNs = 0 // not every filesystem keeps symlink times exactly
	case unix.S_IFDIR:
		e.Size = 0  // directory sizes depend on the filesystem
		e.Nlink = 0 // … and so may the link count
	}
	return e
}

// VerifyOptions: Hashes (from Copy) makes Verify read every target file
// and compare its content too.
type VerifyOptions struct {
	Skip     []string
	NoOwner  bool
	Max      int // differences to collect (default 20)
	Hashes   map[string][32]byte
	Progress func(checked int64)
	Workers  int // entries compared at the same time (default 1)
}

// Verify compares two trees walking the source (call sync first: the
// target's content is then read from the disk, see fileHash) and looking up each entry
// in the target (memory does not grow with the tree): the same entries with
// the same type, mode, owner, size, symlink target, link count and
// modification time – and, with Hashes, the same content. It then counts
// the target's entries to find extra ones. It returns the differences.
func Verify(src, dst string, o VerifyOptions) ([]string, error) {
	if o.Max <= 0 {
		o.Max = 20
	}
	skip := skipSet(o.Skip)
	var mu sync.Mutex // diff, n
	var diff []string
	add := func(f string, args ...any) {
		mu.Lock()
		if len(diff) < o.Max {
			diff = append(diff, fmt.Sprintf(f, args...))
		}
		mu.Unlock()
	}
	var n int64
	pool := newPool(o.Workers)
	one := func(p, rel string, buf []byte) error {
		var a, b unix.Stat_t
		if err := unix.Lstat(p, &a); err != nil {
			return err
		}
		if a.Mode&unix.S_IFMT == unix.S_IFSOCK {
			return nil
		}
		mu.Lock()
		n++
		if o.Progress != nil && n%1000 == 0 {
			o.Progress(n)
		}
		mu.Unlock()
		q := filepath.Join(dst, rel)
		if err := unix.Lstat(q, &b); err != nil {
			add("missing: %s", rel)
			return nil
		}
		x, y := entryOf(p, &a), entryOf(q, &b)
		if o.NoOwner {
			x.UID, x.GID, y.UID, y.GID = 0, 0, 0, 0
		}
		if x != y {
			add("differs: %s (%+v vs %+v)", rel, x, y)
			return nil
		}
		if d := xattrDiff(p, q); d != "" {
			add("xattrs differ: %s (%s)", rel, d)
			return nil
		}
		if want, ok := o.Hashes[rel]; ok && a.Mode&unix.S_IFMT == unix.S_IFREG {
			got, err := fileHash(q, b.Size, buf)
			if err != nil {
				return err
			}
			if got != want {
				add("content differs: %s", rel)
			}
		}
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
		if d.IsDir() {
			return one(p, rel, pool.buf)
		}
		return pool.do(func(buf []byte) error { return one(p, rel, buf) })
	})
	if werr := pool.wait(); err == nil {
		err = werr
	}
	if err != nil {
		return diff, err
	}
	var m int64
	err = filepath.WalkDir(dst, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
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
		if d.Type()&fs.ModeSocket == 0 {
			m++
		}
		return nil
	})
	if err == nil && m != n {
		add("the target has %d entries, the source %d", m, n)
	}
	return diff, err
}

// listXattrs: the names of p's extended attributes (none where the
// filesystem has none).
func listXattrs(p string) ([]string, error) {
	sz, err := unix.Llistxattr(p, nil)
	if err != nil || sz == 0 {
		if errors.Is(err, unix.ENOTSUP) {
			err = nil
		}
		return nil, err
	}
	buf := make([]byte, sz)
	if sz, err = unix.Llistxattr(p, buf); err != nil {
		return nil, err
	}
	var out []string
	for n := range bytes.SplitSeq(buf[:sz], []byte{0}) {
		if len(n) > 0 {
			out = append(out, string(n))
		}
	}
	return out, nil
}

func getXattr(p, name string) ([]byte, error) {
	sz, err := unix.Lgetxattr(p, name, nil)
	if err != nil {
		return nil, err
	}
	val := make([]byte, sz)
	sz, err = unix.Lgetxattr(p, name, val)
	return val[:sz], err
}

// xattrDiff compares the extended attributes of a (source) and b (copy).
// Those the target may refuse (security.* but capabilities, trusted.* –
// counted as skipped by the copy) are compared only if the copy has them.
func xattrDiff(a, b string) string {
	names, err := listXattrs(a)
	if err != nil {
		return err.Error()
	}
	for _, n := range names {
		va, err := getXattr(a, n)
		if err != nil {
			continue // gone meanwhile (cannot happen frozen)
		}
		vb, err := getXattr(b, n)
		mayLack := (strings.HasPrefix(n, "security.") && n != "security.capability") || strings.HasPrefix(n, "trusted.")
		switch {
		case err != nil && mayLack:
		case err != nil:
			return n + " missing"
		case !bytes.Equal(va, vb):
			return n + " has another value"
		}
	}
	return ""
}

func fileHash(p string, size int64, buf []byte) ([32]byte, error) {
	var sum [32]byte
	f, err := os.Open(p)
	if err != nil {
		return sum, err
	}
	defer f.Close()
	// read what is on the disk, not what is still in the page cache from
	// writing it (the caller synced: the pages are clean and can be dropped)
	_ = unix.Fadvise(int(f.Fd()), 0, 0, unix.FADV_DONTNEED)
	h := sha256.New()
	blockHash(h, size, nil)
	if _, err := scanData(f, size, buf, func(off int64, block []byte) error {
		blockHash(h, off, block)
		return nil
	}); err != nil {
		return sum, err
	}
	copy(sum[:], h.Sum(nil))
	return sum, nil
}
