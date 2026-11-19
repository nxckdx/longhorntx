package update

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"testing"
)

func TestNewer(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"0.2.0", "0.1.0", true}, {"v0.10.0", "0.9.9", true}, {"0.1.0", "0.1.0", false},
		{"0.1.0", "0.2.0", false}, {"1.0.0", "0.99.0", true}, {"0.2.0-rc1", "0.1.5", true},
	}
	for _, c := range cases {
		if got := Newer(c.a, c.b); got != c.want {
			t.Errorf("Newer(%s, %s) = %v", c.a, c.b, got)
		}
	}
}

func TestDisabledForDevBuilds(t *testing.T) {
	if !Disabled("dev") || !Disabled("") {
		t.Error("dev builds must not check for updates")
	}
	t.Setenv("LHTX_NO_UPDATE_CHECK", "1")
	if !Disabled("0.1.0") {
		t.Error("LHTX_NO_UPDATE_CHECK must disable the check")
	}
}

func TestExtractAndReplace(t *testing.T) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, body := range map[string]string{"README.md": "readme", "lhtx": "NEW BINARY"} {
		_ = tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(body)), Typeflag: tar.TypeReg})
		_, _ = tw.Write([]byte(body))
	}
	_ = tw.Close()
	_ = gz.Close()

	bin, err := extract(buf.Bytes(), "lhtx_0.2.0_linux_amd64.tar.gz")
	if err != nil || string(bin) != "NEW BINARY" {
		t.Fatalf("extract: %q %v", bin, err)
	}
	exe := filepath.Join(t.TempDir(), "lhtx")
	_ = os.WriteFile(exe, []byte("OLD"), 0o755)
	if err := replace(exe, bin); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(exe)
	if string(got) != "NEW BINARY" {
		t.Errorf("binary not replaced: %q", got)
	}
	if st, _ := os.Stat(exe); st.Mode().Perm()&0o100 == 0 {
		t.Error("new binary must be executable")
	}
}
