package longhorn

import (
	"strings"
	"testing"
)

func TestParseSize(t *testing.T) {
	gi := int64(1) << 30
	for in, want := range map[string]int64{"20Gi": 20 * gi, "+5Gi": 15 * gi, "1073741824": gi} {
		got, err := ParseSize(in, 10*gi)
		if err != nil || got != want {
			t.Errorf("ParseSize(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	if _, err := ParseSize("lots", 0); err == nil {
		t.Error("expected an error for an invalid size")
	}
}

func TestExpandVolume(t *testing.T) {
	gi := int64(1) << 30
	v := &Volume{}
	v.Name, v.ResourceVersion = "pvc-1", "7"
	v.Spec.Size = Quantity(10 * gi)
	v.Status.State, v.Status.Robustness = "attached", "healthy"

	if _, err := ExpandVolume(v, nil, 5*gi); err == nil {
		t.Error("shrinking must be refused")
	}

	// without PVC: spec.size, aligned to 2 MiB
	a, err := ExpandVolume(v, nil, 20*gi+1)
	if err != nil {
		t.Fatal(err)
	}
	if a.GVR != GVRVolume || a.Patch["spec"].(map[string]any)["size"] != "21476933632" {
		t.Errorf("unexpected volume patch: %v", a.Patch)
	}

	// with PVC: patch the PVC in its namespace
	pvc := &PVCInfo{Namespace: "app", Name: "data", ResourceVersion: "3", Requested: 10 * gi, StorageClass: "longhorn", AllowExpansion: true}
	a, err = ExpandVolume(v, pvc, 20*gi)
	if err != nil {
		t.Fatal(err)
	}
	if a.GVR != GVRPVC || a.Namespace != "app" || !strings.Contains(a.Preview("longhorn-system"), `"storage": "20Gi"`) {
		t.Errorf("unexpected PVC action: %s", a.Preview("longhorn-system"))
	}

	pvc.AllowExpansion = false
	if _, err := ExpandVolume(v, pvc, 20*gi); err == nil || !strings.Contains(err.Error(), "allowVolumeExpansion") {
		t.Errorf("StorageClass without allowVolumeExpansion must be refused, got %v", err)
	}
}
