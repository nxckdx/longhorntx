package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSet(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.yaml")

	// new file
	if err := Set(p, "homelab", "readOnly", "false"); err != nil {
		t.Fatal(err)
	}
	f, err := Load(p)
	if err != nil || f.For("homelab").ReadOnly {
		t.Fatalf("%v %+v", err, f.For("homelab"))
	}

	// example file: comments survive, flow "contexts: {}" becomes block
	if err := os.WriteFile(p, []byte(Example), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, kv := range [][3]string{{"", "refresh", "30s"}, {"kunde-prod", "color", "red"}, {"kunde-prod", "autoPollRate", "60"}} {
		if err := Set(p, kv[0], kv[1], kv[2]); err != nil {
			t.Fatalf("%v: %v", kv, err)
		}
	}
	b, _ := os.ReadFile(p)
	s := string(b)
	for _, want := range []string{"# lhtx configuration", "# volume list updates", "refresh: 30s", "kunde-prod:", "color: red", "autoPollRate: 60"} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in:\n%s", want, s)
		}
	}
	if f, err = Load(p); err != nil {
		t.Fatal(err)
	}
	if e := f.For("kunde-prod"); e.Color != "red" || e.Refresh != "30s" || e.AutoPollRate != 60 {
		t.Errorf("%+v", e)
	}

	// remove → inherited again
	if err := Set(p, "kunde-prod", "color", ""); err != nil {
		t.Fatal(err)
	}
	if f, _ = Load(p); f.For("kunde-prod").Color != "auto" {
		t.Errorf("color after remove: %q", f.For("kunde-prod").Color)
	}
	// invalid values never reach the file
	ValidRefresh = func(v string) error {
		if v == "often" {
			return os.ErrInvalid
		}
		return nil
	}
	before, _ := os.ReadFile(p)
	if err := Set(p, "", "refresh", "often"); err == nil {
		t.Error("invalid refresh written")
	}
	ValidRefresh = func(string) error { return nil }
	after, _ := os.ReadFile(p)
	if string(before) != string(after) {
		t.Error("file changed on error")
	}
	if err := Set(p, "", "nope", "1"); err == nil {
		t.Error("unknown key accepted")
	}
}
