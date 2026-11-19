package longhorn

import "testing"

func TestUpgradeEngine(t *testing.T) {
	ei := func(img, ver string, nodes ...string) *EngineImage {
		e := &EngineImage{}
		e.Spec.Image, e.Status.Version, e.Status.State = img, ver, "deployed"
		e.Status.NodeDeploymentMap = map[string]bool{}
		for _, n := range nodes {
			e.Status.NodeDeploymentMap[n] = true
		}
		return e
	}
	env := EngineUpgradeEnv{Images: []*EngineImage{ei("old", "v1.11.0", "n1", "n2"), ei("new", "v1.12.1", "n1", "n2"), ei("partial", "v1.12.0", "n1")}}
	vol := func(state, robust string) *Volume {
		v := &Volume{}
		v.Name, v.ResourceVersion = "vol", "7"
		v.Spec.Image, v.Status.CurrentImage = "old", "old"
		v.Status.State, v.Status.Robustness, v.Status.CurrentNodeID = state, robust, "n1"
		return v
	}
	r := &Replica{}
	r.Spec.NodeID = "n2"
	reps := []*Replica{r}

	a, err := UpgradeEngine(vol("attached", "healthy"), reps, "new", env)
	if err != nil {
		t.Fatal(err)
	}
	if a.Patch["spec"].(map[string]any)["image"] != "new" || a.Patch["metadata"].(map[string]any)["resourceVersion"] != "7" {
		t.Errorf("patch %v", a.Patch)
	}
	for name, c := range map[string]struct {
		v     *Volume
		image string
		env   EngineUpgradeEnv
	}{
		"degraded live":     {vol("attached", "degraded"), "new", env},
		"not deployed":      {vol("detached", "unknown"), "partial", env},
		"same image":        {vol("attached", "healthy"), "old", env},
		"auto upgrade only": {vol("attached", "healthy"), "partial", EngineUpgradeEnv{Images: env.Images, AutoLimit: 1, DefaultImage: "new"}},
	} {
		if _, err := UpgradeEngine(c.v, reps, c.image, c.env); err == nil {
			t.Errorf("%s: expected refusal", name)
		}
	}
	// upgrade in progress: only rollback allowed
	v := vol("attached", "healthy")
	v.Spec.Image = "new"
	if _, err := UpgradeEngine(v, reps, "partial", env); err == nil {
		t.Error("switching during an upgrade must be refused")
	}
	if a, err := UpgradeEngine(v, reps, "old", env); err != nil || a.Title[:4] != "Roll" {
		t.Errorf("rollback: %v %v", a, err)
	}
	// detached degraded volume: offline upgrade is fine
	if _, err := UpgradeEngine(vol("detached", "unknown"), reps, "new", env); err != nil {
		t.Errorf("offline upgrade: %v", err)
	}
}
