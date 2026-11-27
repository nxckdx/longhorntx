package shrink

import (
	"fmt"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/nxckdx/longhorntx/internal/switchover"
	"strings"
	"testing"
	"time"

	"github.com/nxckdx/longhorntx/internal/longhorn"
)

func TestShrinkName(t *testing.T) {
	now := time.Date(2026, 11, 27, 0, 0, 0, 0, time.UTC)
	if n := shrinkName("data", now); n != "data-shrink-1127" {
		t.Errorf("name = %s", n)
	}
	long := shrinkName(strings.Repeat("very-long-pvc-name-", 5), now)
	if len(long) > longhorn.MaxVolumeName || !strings.HasSuffix(long, "-shrink-1127") || strings.Contains(long, "--") {
		t.Errorf("long name = %s (%d)", long, len(long))
	}
}

func TestWithout(t *testing.T) {
	got := without([]string{"a encrypted b", "keep", "x older than an expansion"}, "encrypted")
	if len(got) != 2 || got[0] != "keep" {
		t.Errorf("got %v", got)
	}
}

func TestLeftoverChecks(t *testing.T) {
	vol := func(name, of, ns, pvc string) *longhorn.Volume {
		v := &longhorn.Volume{}
		v.Name = name
		if of != "" {
			v.Annotations = map[string]string{AnnShrinkOf: of}
		}
		v.Status.KubernetesStatus = longhorn.KubernetesStatus{Namespace: ns, PVCName: pvc}
		return v
	}
	f := LeftoverChecks([]*longhorn.Volume{
		vol("old", "", "shop", "data"),                    // the app's volume
		vol("done", "old-before", "shop", "other"),        // a finished shrink: in use
		vol("orphan", "old", "", ""),                      // nobody uses it
		vol("copying", "old", "shop", "lhtx-shrink-data"), // still on the temporary PVC
		vol("plain", "", "", ""),                          // not made by a shrink
	})
	if len(f) != 2 || f[0].Object != "volume/orphan" || !strings.Contains(f[0].Hint, "--abort shop/data") || f[1].Object != "volume/copying" {
		t.Errorf("findings = %+v", f)
	}
}

func TestPodObject(t *testing.T) {
	p := &Plan{CopyPod: "c", ProbePod: "p", TempPVC: "tmp", Image: "img", PullSecrets: []string{"reg"},
		Tolerations: []any{map[string]any{"key": "dedicated", "operator": "Exists"}}}
	p.Switch = &switchover.Plan{Namespace: "shop", PVC: "data"}
	r := &runner{p: p, node: "n1", podUIDs: map[string]string{}}
	pod := r.podObject("copy").Object
	spec := pod["spec"].(map[string]any)
	if len(spec["volumes"].([]any)) != 2 || spec["tolerations"] == nil || spec["imagePullSecrets"] == nil {
		t.Errorf("copy pod: %+v", spec)
	}
	if !strings.Contains(fmt.Sprint(spec["affinity"]), "n1") {
		t.Errorf("not pinned: %v", spec["affinity"])
	}
	probe := r.podObject("probe").Object["spec"].(map[string]any)
	if vs := probe["volumes"].([]any); len(vs) != 1 || !strings.Contains(fmt.Sprint(vs[0]), "tmp") {
		t.Errorf("probe mounts %v", vs)
	}
	if args := fmt.Sprint(probe["containers"].([]any)[0].(map[string]any)["args"]); !strings.Contains(args, "--probe") {
		t.Errorf("probe args %s", args)
	}
}

func TestHopeless(t *testing.T) {
	pod := func(status map[string]any) *unstructured.Unstructured {
		return &unstructured.Unstructured{Object: map[string]any{"status": status}}
	}
	waiting := func(reason string) map[string]any {
		return map[string]any{"containerStatuses": []any{map[string]any{"state": map[string]any{"waiting": map[string]any{"reason": reason, "message": "m"}}}}}
	}
	if hopeless(pod(waiting("ContainerCreating"))) != "" || hopeless(pod(waiting("ErrImagePull"))) != "" {
		t.Error("still starting is not hopeless")
	}
	if !strings.HasPrefix(hopeless(pod(waiting("ImagePullBackOff"))), "ImagePullBackOff") {
		t.Error("back-off is hopeless")
	}
	unsched := func(ago time.Duration) map[string]any {
		return map[string]any{"conditions": []any{map[string]any{"type": "PodScheduled", "status": "False", "reason": "Unschedulable",
			"message": "0/3 nodes", "lastTransitionTime": time.Now().Add(-ago).UTC().Format(time.RFC3339)}}}
	}
	if hopeless(pod(unsched(time.Minute))) != "" || !strings.Contains(hopeless(pod(unsched(3*time.Minute))), "0/3 nodes") {
		t.Error("unschedulable: hopeless only after two minutes")
	}
}

func TestPodObjectPrecopy(t *testing.T) {
	p := &Plan{CopyPod: "c", PrecopyPod: "pre", ProbePod: "p", TempPVC: "tmp", Image: "img", Precopy: true}
	p.Switch = &switchover.Plan{Namespace: "shop", PVC: "data"}
	r := &runner{p: p}
	args := func(role string) string {
		return fmt.Sprint(r.podObject(role).Object["spec"].(map[string]any)["containers"].([]any)[0].(map[string]any)["args"])
	}
	if a := args("pre"); !strings.Contains(a, "--mode pre") {
		t.Errorf("pre args %s", a)
	}
	if a := args("copy"); !strings.Contains(a, "--mode final") {
		t.Errorf("copy args %s", a)
	}
	p.Precopy = false
	if a := args("copy"); !strings.Contains(a, "--mode full") {
		t.Errorf("no pre-copy: %s", a)
	}
}
