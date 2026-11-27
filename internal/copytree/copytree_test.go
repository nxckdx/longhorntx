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

func TestCopyTree(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	w := func(name, content string, mode os.FileMode) {
		p := filepath.Join(src, name)
		_ = os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(content), mode); err != nil {
			t.Fatal(err)
		}
	}
	w("a/b/file.txt", "hello", 0o640)
	w("exec.sh", "#!/bin/sh\n", 0o755)
	w("lost+found/junk", "x", 0o600)
	_ = os.Chmod(filepath.Join(src, "a"), 0o750)
	if err := os.Link(filepath.Join(src, "a/b/file.txt"), filepath.Join(src, "hard.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("a/b/file.txt", filepath.Join(src, "link")); err != nil {
		t.Fatal(err)
	}
	if err := unix.Mkfifo(filepath.Join(src, "fifo"), 0o620); err != nil {
		t.Fatal(err)
	}
	// sparse: 1 MiB hole, 5 bytes, 1 MiB trailing hole
	f, _ := os.Create(filepath.Join(src, "sparse"))
	_, _ = f.WriteAt([]byte("data!"), 1<<20)
	_ = f.Truncate(2 << 20)
	f.Close()
	_ = unix.Setxattr(filepath.Join(src, "exec.sh"), "user.test", []byte("v"), 0) // tmpfs may not support user xattrs
	old := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)
	_ = os.Chtimes(filepath.Join(src, "a/b"), old, old)

	st, err := Copy(src, dst, Options{Skip: []string{"lost+found"}, NoOwner: true})
	if err != nil {
		t.Fatal(err)
	}
	if st.Files != 3 || st.Hardlinks != 1 || st.Symlinks != 1 || st.Special != 1 || st.Dirs != 2 {
		t.Errorf("stats = %+v", st)
	}
	if st.Bytes > 1<<17 {
		t.Errorf("holes were written: %d bytes", st.Bytes)
	}
	if _, err := os.Stat(filepath.Join(dst, "lost+found")); !os.IsNotExist(err) {
		t.Error("lost+found copied")
	}
	var a, b syscall.Stat_t
	_ = syscall.Stat(filepath.Join(dst, "a/b/file.txt"), &a)
	_ = syscall.Stat(filepath.Join(dst, "hard.txt"), &b)
	if a.Ino != b.Ino {
		t.Error("hard link not kept")
	}
	if fi, _ := os.Stat(filepath.Join(dst, "a/b")); !fi.ModTime().Equal(old) {
		t.Errorf("dir mtime %v", fi.ModTime())
	}
	var sp syscall.Stat_t
	_ = syscall.Stat(filepath.Join(dst, "sparse"), &sp)
	if sp.Size != 2<<20 || sp.Blocks*512 >= 1<<20 {
		t.Errorf("sparse: size %d, allocated %d", sp.Size, sp.Blocks*512)
	}
	diff, err := Verify(src, dst, []string{"lost+found"}, true, 10)
	if err != nil || len(diff) != 0 {
		t.Errorf("verify: %v %v", diff, err)
	}
	// a change is found
	_ = os.WriteFile(filepath.Join(dst, "exec.sh"), []byte("changed!!!"), 0o755)
	diff, _ = Verify(src, dst, []string{"lost+found"}, true, 10)
	if len(diff) != 1 || !strings.Contains(diff[0], "exec.sh") {
		t.Errorf("verify after change: %v", diff)
	}
}
