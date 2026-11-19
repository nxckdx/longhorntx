// Package update checks GitHub releases for a newer lhtx and replaces the
// running binary (for installs from the release archives; Nix installs are
// updated through Nix).
package update

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

const (
	Repo     = "nxckdx/longhorntx"
	cacheTTL = 24 * time.Hour
)

// Release is the subset of the GitHub release API we use.
type Release struct {
	Tag    string  `json:"tag_name"`
	URL    string  `json:"html_url"`
	Assets []Asset `json:"assets"`
}

type Asset struct {
	Name string `json:"name"`
	URL  string `json:"browser_download_url"`
}

// Version returns the release version without the leading "v".
func (r *Release) Version() string { return strings.TrimPrefix(r.Tag, "v") }

var client = &http.Client{Timeout: 20 * time.Second}

func get(ctx context.Context, url, current string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "lhtx/"+current)
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	return resp, nil
}

// Latest fetches the latest release (one small API request).
func Latest(ctx context.Context, current string) (*Release, error) {
	resp, err := get(ctx, "https://api.github.com/repos/"+Repo+"/releases/latest", current)
	if err != nil {
		if strings.Contains(err.Error(), "404") {
			return nil, errors.New("no release published yet")
		}
		return nil, err
	}
	defer resp.Body.Close()
	var r Release
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return nil, err
	}
	if r.Tag == "" {
		return nil, errors.New("no release found")
	}
	return &r, nil
}

// Newer reports whether version a is newer than b ("v1.2.3" or "1.2.3").
func Newer(a, b string) bool {
	pa, pb := parse(a), parse(b)
	for i := range pa {
		if pa[i] != pb[i] {
			return pa[i] > pb[i]
		}
	}
	return false
}

func parse(v string) [3]int {
	v = strings.TrimPrefix(v, "v")
	if i := strings.IndexAny(v, "-+"); i >= 0 {
		v = v[:i]
	}
	var out [3]int
	for i, p := range strings.SplitN(v, ".", 3) {
		out[i], _ = strconv.Atoi(p)
	}
	return out
}

// Disabled reports whether automatic checks are off: dev builds, Nix
// installs (updated by Nix) or LHTX_NO_UPDATE_CHECK set.
func Disabled(current string) bool {
	if current == "" || current == "dev" || os.Getenv("LHTX_NO_UPDATE_CHECK") != "" {
		return true
	}
	return InNixStore()
}

// InNixStore reports whether the running binary lives in /nix/store.
func InNixStore() bool {
	exe, err := os.Executable()
	if err != nil {
		return false
	}
	if p, err := filepath.EvalSymlinks(exe); err == nil {
		exe = p
	}
	return strings.HasPrefix(exe, "/nix/store/")
}

type cache struct {
	CheckedAt time.Time `json:"checkedAt"`
	Latest    string    `json:"latest"`
	URL       string    `json:"url"`
}

func cachePath() string {
	dir, err := os.UserCacheDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "lhtx", "update.json")
}

// Check returns the latest version if it is newer than current. It asks
// GitHub at most once per day (result cached in ~/.cache/lhtx) and never
// returns an error – a failed check simply reports nothing.
func Check(ctx context.Context, current string) (latest, url string, newer bool) {
	if Disabled(current) {
		return "", "", false
	}
	path := cachePath()
	var c cache
	if path != "" {
		if b, err := os.ReadFile(path); err == nil {
			_ = json.Unmarshal(b, &c)
		}
	}
	if time.Since(c.CheckedAt) > cacheTTL {
		r, err := Latest(ctx, current)
		c.CheckedAt = time.Now() // also back off after failures
		if err == nil {
			c.Latest, c.URL = r.Version(), r.URL
		}
		if path != "" {
			_ = os.MkdirAll(filepath.Dir(path), 0o700)
			if b, err := json.Marshal(c); err == nil {
				_ = os.WriteFile(path, b, 0o600)
			}
		}
	}
	if c.Latest != "" && Newer(c.Latest, current) {
		return c.Latest, c.URL, true
	}
	return "", "", false
}

// AssetName is the archive goreleaser builds for this platform.
func AssetName(version string) string {
	ext := "tar.gz"
	if runtime.GOOS == "windows" {
		ext = "zip"
	}
	return fmt.Sprintf("lhtx_%s_%s_%s.%s", strings.TrimPrefix(version, "v"), runtime.GOOS, runtime.GOARCH, ext)
}

// Install downloads the release archive for this platform, verifies its
// SHA256 against checksums.txt and replaces the running binary.
func Install(ctx context.Context, r *Release, current string, log func(string)) error {
	if InNixStore() {
		return errors.New("lhtx is installed through Nix: update with `nix profile upgrade` or your flake inputs")
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if p, err := filepath.EvalSymlinks(exe); err == nil {
		exe = p
	}
	name := AssetName(r.Tag)
	var archiveURL, sumsURL string
	for _, a := range r.Assets {
		switch a.Name {
		case name:
			archiveURL = a.URL
		case "checksums.txt":
			sumsURL = a.URL
		}
	}
	if archiveURL == "" || sumsURL == "" {
		return fmt.Errorf("release %s has no %s (or checksums.txt)", r.Tag, name)
	}

	log("downloading " + name)
	archive, err := download(ctx, archiveURL, current)
	if err != nil {
		return err
	}
	sums, err := download(ctx, sumsURL, current)
	if err != nil {
		return err
	}
	want := ""
	for line := range strings.SplitSeq(string(sums), "\n") {
		if f := strings.Fields(line); len(f) == 2 && f[1] == name {
			want = f[0]
		}
	}
	got := sha256.Sum256(archive)
	if want == "" || hex.EncodeToString(got[:]) != want {
		return fmt.Errorf("checksum mismatch for %s – not installing", name)
	}
	log("checksum ok")

	bin, err := extract(archive, name)
	if err != nil {
		return err
	}
	return replace(exe, bin)
}

func download(ctx context.Context, url, current string) ([]byte, error) {
	resp, err := get(ctx, url, current)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(io.LimitReader(resp.Body, 200<<20))
}

func extract(archive []byte, name string) ([]byte, error) {
	want := "lhtx"
	if strings.HasSuffix(name, ".zip") {
		want = "lhtx.exe"
		zr, err := zip.NewReader(bytes.NewReader(archive), int64(len(archive)))
		if err != nil {
			return nil, err
		}
		for _, f := range zr.File {
			if filepath.Base(f.Name) == want {
				rc, err := f.Open()
				if err != nil {
					return nil, err
				}
				defer rc.Close()
				return io.ReadAll(rc)
			}
		}
		return nil, fmt.Errorf("%s not found in %s", want, name)
	}
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return nil, err
	}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err != nil {
			return nil, fmt.Errorf("%s not found in %s: %w", want, name, err)
		}
		if filepath.Base(h.Name) == want && h.Typeflag == tar.TypeReg {
			return io.ReadAll(tr)
		}
	}
}

// replace writes the new binary next to the old one and renames it over
// it (atomic on Unix). On Windows the running exe is moved aside first.
func replace(exe string, bin []byte) error {
	dir := filepath.Dir(exe)
	tmp, err := os.CreateTemp(dir, ".lhtx-update-*")
	if err != nil {
		return fmt.Errorf("cannot write to %s (%w) – re-run with permissions for that directory", dir, err)
	}
	if _, err := tmp.Write(bin); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o755); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	if runtime.GOOS == "windows" {
		old := exe + ".old"
		_ = os.Remove(old)
		if err := os.Rename(exe, old); err != nil {
			os.Remove(tmp.Name())
			return err
		}
	}
	if err := os.Rename(tmp.Name(), exe); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return nil
}
