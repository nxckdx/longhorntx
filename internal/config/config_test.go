package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const sample = `
defaults:
  refresh: watch
  trafficBudget: 0
contexts:
  homelab:
    readOnly: false
    color: 28
  "customer-*":
    color: red
    refresh: 30s
    trafficBudget: 200MiB
  "customer-prod-*":
    refresh: 1m
  customer-prod-eu:
    overview: false
    namespace: storage
`

func TestFor(t *testing.T) {
	var f File
	if err := Parse([]byte(sample), &f); err != nil {
		t.Fatal(err)
	}
	e := f.For("other")
	if e.Refresh != "watch" || !e.ReadOnly || e.Color != "" || e.TrafficBudget != 0 || !e.Overview ||
		e.Namespace != DefaultNamespace || e.Source["refresh"] != "defaults" || e.Source["readOnly"] != "built-in" {
		t.Errorf("other: %+v", e)
	}
	e = f.For("homelab")
	if e.ReadOnly || e.Color != "28" || e.Source["readOnly"] != "contexts.homelab" {
		t.Errorf("homelab: %+v", e)
	}
	e = f.For("customer-test")
	if e.Refresh != "30s" || e.Color != "red" || e.TrafficBudget != 200<<20 {
		t.Errorf("customer-test: %+v", e)
	}
	// more specific glob wins, exact name on top
	e = f.For("customer-prod-eu")
	if e.Refresh != "1m" || e.Source["refresh"] != "contexts.customer-prod-*" || e.Color != "red" ||
		e.Overview || e.Namespace != "storage" {
		t.Errorf("customer-prod-eu: %+v", e)
	}
}

func TestParseErrors(t *testing.T) {
	ValidRefresh = func(s string) error {
		if s == "never" {
			return os.ErrInvalid
		}
		return nil
	}
	defer func() { ValidRefresh = func(string) error { return nil } }()
	for _, bad := range []string{
		"defaults:\n  refesh: watch\n", // typo
		"defaults:\n  color: pink\n",
		"defaults:\n  trafficBudget: lots\n",
		"contexts:\n  x:\n    refresh: never\n",
		"contexts:\n  \"[\":\n    color: red\n",
	} {
		var f File
		if err := Parse([]byte(bad), &f); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}

func TestLoad(t *testing.T) {
	dir := t.TempDir()
	f, err := Load(filepath.Join(dir, "missing.yaml"))
	if err != nil || f.Path != "" || f.For("x").Refresh != DefaultRefresh {
		t.Fatalf("missing file: %+v %v", f, err)
	}
	p := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(p, []byte(Example), 0o600); err != nil {
		t.Fatal(err)
	}
	if f, err = Load(p); err != nil || f.Path != p {
		t.Fatalf("example: %v", err)
	}
	if err := os.WriteFile(p, []byte("defaults: [\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(p); err == nil || !strings.Contains(err.Error(), p) {
		t.Errorf("broken file: %v", err)
	}
}

func TestParseBytes(t *testing.T) {
	for in, want := range map[string]int64{"0": 0, "": 0, "200MiB": 200 << 20, "1Gi": 1 << 30, "500M": 500e6, "1.5GiB": 3 << 29} {
		if got, err := ParseBytes(in); err != nil || got != want {
			t.Errorf("%q: %d %v", in, got, err)
		}
	}
}
