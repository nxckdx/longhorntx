package copytree

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestPrecopyThenSync(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	w := func(name, content string) {
		p := filepath.Join(src, name)
		_ = os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	w("keep.txt", "unchanged")
	w("same-size.txt", "aaaa")
	w("gone.txt", "bye")
	w("old-name.txt", "moved")
	w("mode.txt", "m")
	w("becomes-dir", "file")
	w("d/inner.txt", "inner")

	// pre-copy while "the app runs"
	st := NewState()
	hashes := map[string][32]byte{}
	if _, err := Copy(src, dst, Options{NoOwner: true, Live: true, State: st, Hashes: hashes, Workers: 4}); err != nil {
		t.Fatal(err)
	}
	if bad, err := VerifyContent(dst, hashes, 4); err != nil || len(bad) != 0 {
		t.Fatalf("pre-copy content: %v %v", bad, err)
	}
	var keepIno syscall.Stat_t
	_ = syscall.Stat(filepath.Join(dst, "keep.txt"), &keepIno)

	// the app keeps writing until the freeze
	time.Sleep(10 * time.Millisecond) // a ctime tick
	p := filepath.Join(src, "same-size.txt")
	fi, _ := os.Stat(p)
	_ = os.WriteFile(p, []byte("bbbb"), 0o644)
	_ = os.Chtimes(p, fi.ModTime(), fi.ModTime()) // same size, same mtime: only the ctime tells
	_ = os.Remove(filepath.Join(src, "gone.txt"))
	_ = os.Rename(filepath.Join(src, "old-name.txt"), filepath.Join(src, "new-name.txt"))
	_ = os.Chmod(filepath.Join(src, "mode.txt"), 0o600)
	_ = os.Link(filepath.Join(src, "keep.txt"), filepath.Join(src, "keep-link.txt"))
	_ = os.Remove(filepath.Join(src, "becomes-dir"))
	w("becomes-dir/x", "now a dir")
	w("new.txt", "new")

	// frozen: only the changes
	h2 := map[string][32]byte{}
	stats, changed, err := Sync(src, dst, st, Options{NoOwner: true, Hashes: h2, Workers: 4})
	if err != nil {
		t.Fatal(err)
	}
	if stats.Removed < 2 { // gone.txt, old-name.txt (becomes-dir is replaced in place)
		t.Errorf("removed %d", stats.Removed)
	}
	if stats.Unchanged < 1 {
		t.Errorf("nothing kept from the pre-copy: %+v", stats)
	}
	for _, want := range []string{"same-size.txt", "new-name.txt", "mode.txt", "new.txt", "keep-link.txt", "becomes-dir/x"} {
		found := false
		for _, c := range changed {
			found = found || c == want
		}
		if !found {
			t.Errorf("%s not copied again (changed: %v)", want, changed)
		}
	}
	if diff, err := Verify(src, dst, VerifyOptions{NoOwner: true, Hashes: h2}); err != nil || len(diff) != 0 {
		t.Errorf("after sync: %v %v", diff, err)
	}
	if b, _ := os.ReadFile(filepath.Join(dst, "same-size.txt")); string(b) != "bbbb" {
		t.Errorf("same-size content %q", b)
	}
	var a, b syscall.Stat_t
	_ = syscall.Stat(filepath.Join(dst, "keep.txt"), &a)
	_ = syscall.Stat(filepath.Join(dst, "keep-link.txt"), &b)
	if a.Ino != b.Ino {
		t.Error("new hard link not kept")
	}
}

// A file that changes while the pre-copy reads it is not recorded: Sync
// copies it.
func TestLiveCopySkipsChanging(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	_ = os.WriteFile(filepath.Join(src, "a"), []byte("1"), 0o644)
	st := NewState()
	// simulate: the file vanishes between walk and copy
	_ = os.WriteFile(filepath.Join(src, "b"), []byte("2"), 0o644)
	s, err := Copy(src, dst, Options{NoOwner: true, Live: true, State: st, Progress: func(Stats) {}, Every: time.Nanosecond})
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Entries) != 2 || s.Dirty != 0 {
		t.Errorf("state %v stats %+v", st.Entries, s)
	}
	f := filepath.Join(t.TempDir(), "state.json")
	if err := st.Save(f); err != nil {
		t.Fatal(err)
	}
	if l, err := LoadState(f); err != nil || len(l.Entries) != 2 {
		t.Errorf("load: %v %v", l, err)
	}
}

// A pre-copy that could not read xattrs (over NFS): Verify notices them
// missing, Sync sets them on unchanged entries too.
func TestSyncAddsXattrsAfterNFSPrecopy(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	f := filepath.Join(src, "f")
	_ = os.WriteFile(f, []byte("x"), 0o644)
	if err := unix.Setxattr(f, "user.k", []byte("v"), 0); err != nil {
		t.Skipf("no user xattrs here: %v", err)
	}
	st := NewState()
	st.NoXattrs = true
	if _, err := Copy(src, dst, Options{NoOwner: true, Live: true, State: st, NoXattrs: true, Workers: 2}); err != nil {
		t.Fatal(err)
	}
	if diff, _ := Verify(src, dst, VerifyOptions{NoOwner: true}); len(diff) != 1 || !strings.Contains(diff[0], "user.k missing") {
		t.Errorf("verify after the pre-copy: %v", diff)
	}
	s, changed, err := Sync(src, dst, st, Options{NoOwner: true, Workers: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(changed) != 0 || s.Unchanged != 1 {
		t.Errorf("sync copied again: %v %+v", changed, s)
	}
	if diff, err := Verify(src, dst, VerifyOptions{NoOwner: true}); err != nil || len(diff) != 0 {
		t.Errorf("verify after sync: %v %v", diff, err)
	}
	// another value is found too
	_ = unix.Setxattr(filepath.Join(dst, "f"), "user.k", []byte("w"), 0)
	if diff, _ := Verify(src, dst, VerifyOptions{NoOwner: true}); len(diff) != 1 || !strings.Contains(diff[0], "another value") {
		t.Errorf("verify, other value: %v", diff)
	}
}
