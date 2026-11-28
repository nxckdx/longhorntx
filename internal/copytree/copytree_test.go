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

	hashes := map[string][32]byte{}
	st, err := Copy(src, dst, Options{Skip: []string{"lost+found"}, NoOwner: true, Hashes: hashes})
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
	vo := VerifyOptions{Skip: []string{"lost+found"}, NoOwner: true, Hashes: hashes}
	diff, err := Verify(src, dst, vo)
	if err != nil || len(diff) != 0 {
		t.Errorf("verify: %v %v", diff, err)
	}
	if len(hashes) != 3 {
		t.Errorf("hashes: %d", len(hashes))
	}
	// same size and times, other content: only the hash finds it
	p := filepath.Join(dst, "exec.sh")
	fi, _ := os.Stat(p)
	_ = os.WriteFile(p, []byte("#!/bin/XX\n"), 0o755)
	_ = os.Chtimes(p, fi.ModTime(), fi.ModTime())
	diff, _ = Verify(src, dst, vo)
	if len(diff) != 1 || !strings.Contains(diff[0], "content differs: exec.sh") {
		t.Errorf("verify after change: %v", diff)
	}
	// an extra entry in the target
	_ = os.WriteFile(filepath.Join(dst, "extra"), nil, 0o600)
	diff, _ = Verify(src, dst, VerifyOptions{Skip: []string{"lost+found"}, NoOwner: true})
	if len(diff) != 1 || !strings.Contains(diff[0], "entries") {
		t.Errorf("extra entry: %v", diff)
	}
}

// A large sparse file is not read where it has holes, and its content
// hash covers the holes.
func TestCopySparseHash(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	f, _ := os.Create(filepath.Join(src, "img"))
	_, _ = f.WriteAt([]byte("start"), 0)
	_, _ = f.WriteAt([]byte("end"), 4<<30) // 4 GiB apart
	f.Close()
	hashes := map[string][32]byte{}
	start := time.Now()
	st, err := Copy(src, dst, Options{NoOwner: true, Hashes: hashes})
	if err != nil {
		t.Fatal(err)
	}
	if st.Bytes > 8<<20 {
		t.Errorf("read %d bytes of a mostly empty file", st.Bytes)
	}
	if time.Since(start) > 30*time.Second {
		t.Errorf("took %s", time.Since(start))
	}
	if diff, err := Verify(src, dst, VerifyOptions{NoOwner: true, Hashes: hashes}); err != nil || len(diff) != 0 {
		t.Errorf("verify: %v %v", diff, err)
	}
}
