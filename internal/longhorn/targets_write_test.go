package longhorn

import (
	"strings"
	"testing"
	"time"
)

func TestTargetValidate(t *testing.T) {
	other := &BackupTarget{}
	other.Name, other.Spec.BackupTargetURL = "default", "nfs://nas:/backups"
	ok := []TargetSpec{
		{Name: "offsite", URL: "s3://lh-backups@eu-central-1/", Secret: "s3-creds", PollInterval: "5m"},
		{Name: "nas2", URL: "nfs://10.0.0.2:/export/lh", PollInterval: "0"},
		{Name: "smb", URL: "cifs://fs/share/lh", Secret: "cifs"},
		{Name: "az", URL: "azblob://lh@core.windows.net/", Secret: "az"},
	}
	secrets := []string{"s3-creds", "cifs", "az"}
	for _, s := range ok {
		if err := s.Validate([]*BackupTarget{other}, secrets); err != nil {
			t.Errorf("%s: %v", s.Name, err)
		}
	}
	for name, s := range map[string]TargetSpec{
		"scheme":    {Name: "x", URL: "ftp://a/b"},
		"s3 bucket": {Name: "x", URL: "s3://eu-central-1/"},
		"nfs path":  {Name: "x", URL: "nfs://nas/backups"},
		"poll":      {Name: "x", URL: "nfs://nas:/x", PollInterval: "often"},
		"same url":  {Name: "x", URL: "nfs://nas:/backups"},
		"no secret": {Name: "x", URL: "s3://b@r/", Secret: "missing"},
		"bad name":  {Name: "X", URL: "nfs://nas:/x"},
	} {
		if s.Validate([]*BackupTarget{other}, secrets) == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestTargetActions(t *testing.T) {
	tg := &BackupTarget{}
	tg.Name, tg.ResourceVersion, tg.UID = "offsite", "7", "u"
	tg.Spec.BackupTargetURL, tg.Spec.PollInterval = "s3://a@r/", "5m0s"
	tg.Labels = map[string]string{"kustomize.toolkit.fluxcd.io/name": "infra"}

	a, err := CreateTarget(TargetSpec{Name: "new", URL: "s3://b@r/"}, []*BackupTarget{tg}, nil)
	if err != nil || a.Object["spec"].(map[string]any)["pollInterval"] != "5m0s" {
		t.Fatalf("%v %+v", err, a)
	}
	if !strings.Contains(strings.Join(a.Warnings, " "), "instance roles") {
		t.Errorf("%v", a.Warnings)
	}

	s := TargetSpecOf(tg)
	s.URL = "s3://c@r/"
	a, err = UpdateTarget(tg, s, []*BackupTarget{tg}, nil)
	if err != nil {
		t.Fatal(err)
	}
	w := strings.Join(a.Warnings, "\n")
	if !strings.Contains(w, "Flux") || !strings.Contains(w, "disappear") || a.Patch["spec"].(map[string]any)["backupTargetURL"] != "s3://c@r/" {
		t.Errorf("%v %v", a.Patch, w)
	}

	v := &Volume{}
	v.Name, v.Spec.BackupTargetName = "v1", "offsite"
	d, _ := DeleteTarget(tg, []*Volume{v})
	if !d.Destructive || !strings.Contains(strings.Join(d.Warnings, " "), "1 volumes back up") {
		t.Errorf("%v", d.Warnings)
	}
	def := &BackupTarget{}
	def.Name = "default"
	if _, err := DeleteTarget(def, nil); err == nil {
		t.Error("default deletable")
	}
	sy := SyncTarget(tg, time.Date(2026, 11, 24, 10, 0, 0, 0, time.UTC))
	if sy.Patch["spec"].(map[string]any)["syncRequestedAt"] != "2026-11-24T10:00:00Z" || sy.Patch["metadata"] != nil {
		t.Errorf("sync is a request, no resourceVersion: %v", sy.Patch)
	}
	// Longhorn stores durations as 1m0s: sent that way, and "1m" vs "1m0s"
	// is no change
	a, _ = CreateTarget(TargetSpec{Name: "n2", URL: "nfs://x:/y", PollInterval: "1m"}, nil, nil)
	if p := a.Object["spec"].(map[string]any)["pollInterval"]; p != "1m0s" {
		t.Errorf("create poll %v", p)
	}
	tg.Spec.PollInterval = "1m"
	s = TargetSpecOf(tg)
	s.PollInterval = "60s"
	if _, err := UpdateTarget(tg, s, nil, nil); err == nil || !strings.Contains(err.Error(), "nothing changed") {
		t.Errorf("1m → 60s: %v", err)
	}
	s.PollInterval = "2m"
	if a, err := UpdateTarget(tg, s, nil, nil); err != nil || a.Patch["spec"].(map[string]any)["pollInterval"] != "2m0s" {
		t.Errorf("1m → 2m: %v %v", err, a)
	}
}
