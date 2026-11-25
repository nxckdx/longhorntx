package longhorn

import (
	"strings"
	"testing"
)

func encBackup() *Backup {
	b := &Backup{}
	b.Name = "backup-1"
	b.Status.State, b.Status.URL, b.Status.VolumeSize = "Completed", "nfs://x?backup=1", Quantity(1<<30)
	b.Status.Labels = map[string]string{"volume-encrypted": "enabled"}
	return b
}

func TestRestoreEncrypted(t *testing.T) {
	none := func(string) bool { return false }
	s := RestoreSpec{Name: "vault-restore", Replicas: 2, StorageClass: "longhorn-static", PVCNamespace: "apps", PVCName: "vault-restore",
		Encryption: &Encryption{Secret: SecretRef{"apps", "vault"}, PerPVC: true}}
	acts, err := RestoreBackup(encBackup(), nil, s, none)
	if err != nil {
		t.Fatal(err)
	}
	if len(acts) != 4 || acts[1].CopySecretFrom == nil || acts[1].Name != "vault-restore" || acts[1].Namespace != "apps" {
		t.Fatalf("want volume, secret copy, PV, PVC: %+v", acts)
	}
	if acts[0].Object["spec"].(map[string]any)["encrypted"] != true {
		t.Error("restored volume not encrypted")
	}
	csi := acts[2].Object["spec"].(map[string]any)["csi"].(map[string]any)
	if r := csi["nodeStageSecretRef"].(map[string]any); r["name"] != "vault-restore" || csi["volumeAttributes"].(map[string]any)["encrypted"] != "true" {
		t.Errorf("PV csi %v", csi)
	}
	if strings.Contains(acts[1].Preview("x"), "data") && !strings.Contains(acts[1].Preview("x"), "not shown") {
		t.Error("preview may show secret data")
	}

	// one Secret for all: no copy, same refs
	s.Encryption = &Encryption{Secret: SecretRef{"longhorn-system", "longhorn-crypto"}}
	acts, _ = RestoreBackup(encBackup(), nil, s, none)
	if len(acts) != 3 {
		t.Fatalf("global secret: %d actions", len(acts))
	}
	// the key Secret is unknown (source PV gone): refuse with a hint
	s.Encryption = nil
	if _, err := RestoreBackup(encBackup(), nil, s, none); err == nil || !strings.Contains(err.Error(), "--secret") {
		t.Errorf("unknown key accepted: %v", err)
	}
	// the copy is skipped if the Secret already exists
	s.Encryption = &Encryption{Secret: SecretRef{"apps", "vault"}, PerPVC: true}
	acts, _ = RestoreBackup(encBackup(), nil, s, func(k string) bool { return k == "secret" })
	if len(acts) != 3 {
		t.Errorf("existing secret copied again: %d actions", len(acts))
	}
}
