package longhorn

import (
	"context"
	"fmt"
	"sort"
	"strconv"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

var GVREngineImage = gvr("engineimages")

type EngineImage struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata"`
	Spec              struct {
		Image string `json:"image"`
	} `json:"spec"`
	Status struct {
		State             string          `json:"state"` // deployed | deploying | incompatible | error
		Version           string          `json:"version"`
		RefCount          int             `json:"refCount"`
		Incompatible      bool            `json:"incompatible"`
		NodeDeploymentMap map[string]bool `json:"nodeDeploymentMap"`
		BuildDate         string          `json:"buildDate"`
	} `json:"status"`
}

func (o *EngineImage) DeepCopyObject() runtime.Object { c := *o; return &c }

// EngineUpgradeEnv is the cluster-wide context for engine upgrades.
type EngineUpgradeEnv struct {
	Images       []*EngineImage
	DefaultImage string // setting default-engine-image
	AutoLimit    int    // setting concurrent-automatic-engine-upgrade-per-node-limit
}

func (c *Client) EngineImages(ctx context.Context) ([]*EngineImage, error) {
	imgs, err := list[EngineImage](ctx, c, GVREngineImage, "")
	sort.Slice(imgs, func(i, j int) bool { return imgs[i].Spec.Image > imgs[j].Spec.Image })
	return imgs, err
}

// LoadEngineUpgradeEnv reads engine images and the two relevant settings.
func (c *Client) LoadEngineUpgradeEnv(ctx context.Context) (EngineUpgradeEnv, error) {
	var env EngineUpgradeEnv
	var err error
	if env.Images, err = c.EngineImages(ctx); err != nil {
		return env, err
	}
	env.DefaultImage, _ = c.Setting(ctx, "default-engine-image")
	if v, err := c.Setting(ctx, "concurrent-automatic-engine-upgrade-per-node-limit"); err == nil {
		env.AutoLimit, _ = strconv.Atoi(v)
	}
	return env, nil
}

func (env EngineUpgradeEnv) image(img string) *EngineImage {
	for _, e := range env.Images {
		if e.Spec.Image == img {
			return e
		}
	}
	return nil
}

// UpgradeEngine switches a volume to another engine image, with the checks
// longhorn-manager's own API does (manager/volume.go EngineUpgrade, v1.12.1).
// replicas are the volume's replicas (for the node deployment check).
func UpgradeEngine(v *Volume, replicas []*Replica, image string, env EngineUpgradeEnv) (*Action, error) {
	switch {
	case v.Spec.DataEngine == "v2":
		return nil, fmt.Errorf("%s uses the v2 data engine", volLabel(v))
	case v.Spec.Image == image && v.Status.CurrentImage == image:
		return nil, fmt.Errorf("%s already uses this image", volLabel(v))
	case v.Spec.Image == image:
		return nil, fmt.Errorf("%s is already being upgraded to this image", volLabel(v))
	case v.Spec.Image != v.Status.CurrentImage && image != v.Status.CurrentImage:
		return nil, fmt.Errorf("%s: an upgrade %s → %s is in progress; only a rollback to the current image is possible",
			volLabel(v), v.Status.CurrentImage, v.Spec.Image)
	case v.Spec.MigrationNodeID != "":
		return nil, fmt.Errorf("%s is being migrated", volLabel(v))
	}
	if env.AutoLimit > 0 && env.DefaultImage != "" && image != env.DefaultImage {
		return nil, fmt.Errorf("automatic engine upgrade is enabled: only the default image %s is allowed", env.DefaultImage)
	}
	ei := env.image(image)
	if ei == nil {
		return nil, fmt.Errorf("engine image %s not found", image)
	}
	if ei.Status.Incompatible || ei.Status.State == "incompatible" {
		return nil, fmt.Errorf("engine image %s is incompatible with this Longhorn version", image)
	}
	nodes := map[string]bool{}
	if v.Status.CurrentNodeID != "" {
		nodes[v.Status.CurrentNodeID] = true
	}
	for _, r := range replicas {
		if r.Spec.NodeID != "" {
			nodes[r.Spec.NodeID] = true
		}
	}
	for n := range nodes {
		if !ei.Status.NodeDeploymentMap[n] {
			return nil, fmt.Errorf("engine image %s is not deployed on node %s", ei.Status.Version, n)
		}
	}
	live := v.Status.State == "attached"
	if live && v.Status.Robustness != "healthy" {
		return nil, fmt.Errorf("%s is %s: live upgrade only for healthy volumes", volLabel(v), v.Status.Robustness)
	}
	if live && v.Spec.DataLocality == "strict-local" {
		return nil, fmt.Errorf("%s is strict-local: no live upgrade while attached", volLabel(v))
	}
	from := v.Spec.Image
	if cur := env.image(from); cur != nil && cur.Status.Version != "" {
		from = cur.Status.Version + " (" + from + ")"
	}
	a := &Action{
		Title: fmt.Sprintf("Upgrade engine of %s to %s", volLabel(v), dashIfEmpty(ei.Status.Version)),
		Verb:  VerbPatch, GVR: GVRVolume, Name: v.Name,
		Patch:   patchWithRV(v.ResourceVersion, map[string]any{"image": image}),
		Changes: []Change{{"spec.image", from, image}},
		summary: fmt.Sprintf("%-40s %s → %s", volLabel(v), versionOf(env, v.Spec.Image), dashIfEmpty(ei.Status.Version)),
	}
	if image == v.Status.CurrentImage && v.Spec.Image != v.Status.CurrentImage {
		a.Title = fmt.Sprintf("Roll back engine upgrade of %s to %s", volLabel(v), dashIfEmpty(ei.Status.Version))
	}
	if live {
		a.Warnings = append(a.Warnings, "live upgrade: the volume stays attached, Longhorn swaps engine and replicas one by one")
	} else {
		a.Warnings = append(a.Warnings, "offline upgrade: the new image is used the next time the volume is attached")
	}
	return a, nil
}

func versionOf(env EngineUpgradeEnv, img string) string {
	if e := env.image(img); e != nil && e.Status.Version != "" {
		return e.Status.Version
	}
	return img
}
