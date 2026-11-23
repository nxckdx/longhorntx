package longhorn

import (
	"context"
	"fmt"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/rest"

	"github.com/nxckdx/longhorntx/internal/kube"
)

func fakeVolume(name, size string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": Group + "/" + Version, "kind": "Volume",
		"metadata": map[string]any{"name": name, "namespace": DefaultNamespace},
		"spec":     map[string]any{"numberOfReplicas": int64(2)},
		"status":   map[string]any{"state": "attached", "actualSize": size},
	}}
	return u
}

// A busy volume watch makes auto mode switch to polling; the volumes stay.
func TestAutoSwitchToPoll(t *testing.T) {
	kinds := map[schema.GroupVersionResource]string{}
	for _, g := range []schema.GroupVersionResource{GVRVolume, GVRReplica, GVRNode, GVROrphan} {
		kinds[g] = map[string]string{"volumes": "Volume", "replicas": "Replica", "nodes": "Node", "orphans": "Orphan"}[g.Resource] + "List"
	}
	dyn := dynfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), kinds,
		fakeVolume("v1", "1"), fakeVolume("v2", "1"))
	c := NewClientFrom(&kube.Cluster{Context: "t", Config: &rest.Config{}, Traffic: &kube.Counter{}}, DefaultNamespace, dyn, nil)
	s := NewStore(c)
	s.AutoPollRate, s.autoWindow = 60, 200*time.Millisecond // more than 1 update per 200 ms
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.Start(ctx)
	if err := s.WaitSynced(ctx, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	if s.RefreshInterval() != 0 {
		t.Fatalf("auto with 2 volumes should start live, got %v", s.RefreshInterval())
	}
	res := dyn.Resource(GVRVolume).Namespace(DefaultNamespace)
	deadline := time.Now().Add(3 * time.Second)
	for i := 0; s.AutoSwitched() == 0 && time.Now().Before(deadline); i++ {
		if _, err := res.Update(ctx, fakeVolume("v1", fmt.Sprint(i+2)), metav1.UpdateOptions{}); err != nil {
			t.Fatal(err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if s.AutoSwitched() == 0 || s.RefreshInterval() != DefaultPollInterval {
		t.Fatalf("no switch: rate %d interval %v", s.AutoSwitched(), s.RefreshInterval())
	}
	if n := len(s.State().Volumes); n != 2 {
		t.Errorf("volumes after switch: %d", n)
	}
}
