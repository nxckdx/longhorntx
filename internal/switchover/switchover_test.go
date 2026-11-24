package switchover

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/rest"
	k8stesting "k8s.io/client-go/testing"

	"github.com/nxckdx/longhorntx/internal/kube"
	"github.com/nxckdx/longhorntx/internal/longhorn"
)

const lhNS = "longhorn-system"

func obj(apiVersion, kind, ns, name string, labels map[string]string, fields map[string]any) *unstructured.Unstructured {
	md := map[string]any{"name": name}
	if ns != "" {
		md["namespace"] = ns
	}
	u := &unstructured.Unstructured{Object: map[string]any{"apiVersion": apiVersion, "kind": kind, "metadata": md}}
	for k, v := range fields {
		u.Object[k] = v
	}
	if labels != nil {
		u.SetLabels(labels)
	}
	return u
}

func ownedBy(u *unstructured.Unstructured, apiVersion, kind, name string) *unstructured.Unstructured {
	t := true
	u.SetOwnerReferences([]metav1.OwnerReference{{APIVersion: apiVersion, Kind: kind, Name: name, UID: "x", Controller: &t}})
	return u
}

func volume(name, state string) *unstructured.Unstructured {
	return obj("longhorn.io/v1beta2", "Volume", lhNS, name, nil, map[string]any{
		"spec":   map[string]any{"size": "10737418240", "numberOfReplicas": int64(2), "dataEngine": "v1"},
		"status": map[string]any{"state": state, "robustness": "healthy"},
	})
}

var fluxLabels = map[string]string{"kustomize.toolkit.fluxcd.io/name": "apps", "kustomize.toolkit.fluxcd.io/namespace": "flux-system"}

// world: app/data (PVC) → PV pvc-old → Longhorn volume pvc-old, mounted by
// Deployment app (2 replicas) managed by the Flux Kustomization apps;
// target volume pvc-new restored and detached.
func world(extra ...runtime.Object) (*longhorn.Client, *dynfake.FakeDynamicClient) {
	kinds := map[schema.GroupVersionResource]string{
		gvrPod: "PodList", gvrPVC: "PersistentVolumeClaimList", gvrPV: "PersistentVolumeList",
		gvrReplicaSet: "ReplicaSetList", gvrDeployment: "DeploymentList", gvrStatefulSet: "StatefulSetList",
		gvrHPA: "HorizontalPodAutoscalerList", GVRKustomization: "KustomizationList", GVRHelmRelease: "HelmReleaseList",
		longhorn.GVRVolume: "VolumeList", longhorn.GVREngine: "EngineList",
	}
	objs := []runtime.Object{
		obj("v1", "PersistentVolumeClaim", "app", "data", fluxLabels, map[string]any{
			"spec": map[string]any{"volumeName": "pvc-old", "storageClassName": "longhorn", "accessModes": []any{"ReadWriteOnce"},
				"volumeMode": "Filesystem", "resources": map[string]any{"requests": map[string]any{"storage": "10Gi"}}},
			"status": map[string]any{"phase": "Bound"},
		}),
		obj("v1", "PersistentVolume", "", "pvc-old", nil, map[string]any{
			"spec": map[string]any{"persistentVolumeReclaimPolicy": "Delete", "storageClassName": "longhorn",
				"csi": map[string]any{"driver": "driver.longhorn.io", "volumeHandle": "pvc-old", "fsType": "ext4",
					"volumeAttributes": map[string]any{"numberOfReplicas": "2", "fromBackup": "", "storage.kubernetes.io/csiProvisionerIdentity": "x"}}},
			"status": map[string]any{"phase": "Bound"},
		}),
		volume("pvc-old", "attached"),
		volume("pvc-new", "detached"),
		ownedBy(obj("v1", "Pod", "app", "app-123-abc", nil, map[string]any{
			"spec": map[string]any{"volumes": []any{map[string]any{"name": "d", "persistentVolumeClaim": map[string]any{"claimName": "data"}}}},
		}), "apps/v1", "ReplicaSet", "app-123"),
		ownedBy(obj("apps/v1", "ReplicaSet", "app", "app-123", nil, nil), "apps/v1", "Deployment", "app"),
		obj("apps/v1", "Deployment", "app", "app", fluxLabels, map[string]any{
			"spec": map[string]any{"replicas": int64(2)}, "status": map[string]any{"readyReplicas": int64(2)}}),
		obj("kustomize.toolkit.fluxcd.io/v1", "Kustomization", "flux-system", "apps", nil, map[string]any{"spec": map[string]any{"interval": "1h"}}),
	}
	dyn := dynfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), kinds, append(objs, extra...)...)
	ctx := context.Background()
	// scaling the Deployment to 0 removes its pod and detaches the volume
	dyn.PrependReactor("patch", "deployments", func(a k8stesting.Action) (bool, runtime.Object, error) {
		if strings.Contains(string(a.(k8stesting.PatchActionImpl).GetPatch()), `"replicas":0`) {
			go func() {
				_ = dyn.Resource(gvrPod).Namespace("app").Delete(ctx, "app-123-abc", metav1.DeleteOptions{})
				v, _ := dyn.Resource(longhorn.GVRVolume).Namespace(lhNS).Get(ctx, "pvc-old", metav1.GetOptions{})
				_ = unstructured.SetNestedField(v.Object, "detached", "status", "state")
				_, _ = dyn.Resource(longhorn.GVRVolume).Namespace(lhNS).Update(ctx, v, metav1.UpdateOptions{})
			}()
		}
		return false, nil, nil
	})
	// a new PVC binds at once
	dyn.PrependReactor("create", "persistentvolumeclaims", func(a k8stesting.Action) (bool, runtime.Object, error) {
		u := a.(k8stesting.CreateActionImpl).GetObject().(*unstructured.Unstructured)
		_ = unstructured.SetNestedField(u.Object, "Bound", "status", "phase")
		return false, nil, nil
	})
	cl := &kube.Cluster{Context: "t", Config: &rest.Config{}, Traffic: &kube.Counter{}}
	return longhorn.NewClientFrom(cl, lhNS, dyn, nil), dyn
}

func get(t *testing.T, dyn *dynfake.FakeDynamicClient, g schema.GroupVersionResource, ns, name string) *unstructured.Unstructured {
	t.Helper()
	r := dyn.Resource(g)
	var u *unstructured.Unstructured
	var err error
	if ns != "" {
		u, err = r.Namespace(ns).Get(context.Background(), name, metav1.GetOptions{})
	} else {
		u, err = r.Get(context.Background(), name, metav1.GetOptions{})
	}
	if err != nil {
		t.Fatalf("%s %s: %v", g.Resource, name, err)
	}
	return u
}

func TestSwitch(t *testing.T) {
	c, dyn := world()
	ctx := context.Background()
	p, err := Build(ctx, c, "app", "data", "pvc-new")
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Blockers) > 0 || p.OldVolume != "pvc-old" || len(p.Workloads) != 1 || p.Workloads[0].Replicas != 2 ||
		len(p.Flux) != 1 || p.Flux[0].Name != "apps" || p.PVAttributes["fromBackup"] != "" {
		t.Fatalf("plan: %+v", p)
	}
	if _, ok := p.PVAttributes["storage.kubernetes.io/csiProvisionerIdentity"]; ok {
		t.Error("provisioner identity copied")
	}
	journal := filepath.Join(t.TempDir(), "j.json")
	var log []string
	err = Run(ctx, c, p, Options{Poll: 10 * time.Millisecond, Wait: 2 * time.Second, Journal: journal,
		Log: func(f string, a ...any) { log = append(log, f) }})
	if err != nil {
		t.Fatalf("%v\n%s", err, strings.Join(log, "\n"))
	}
	pvc := get(t, dyn, gvrPVC, "app", "data")
	if vn, _, _ := unstructured.NestedString(pvc.Object, "spec", "volumeName"); vn != "pvc-new" {
		t.Errorf("PVC bound to %s", vn)
	}
	if pvc.GetLabels()["kustomize.toolkit.fluxcd.io/name"] != "apps" {
		t.Error("Flux labels lost")
	}
	pv := get(t, dyn, gvrPV, "", "pvc-new")
	if h, _, _ := unstructured.NestedString(pv.Object, "spec", "csi", "volumeHandle"); h != "pvc-new" {
		t.Errorf("PV handle %s", h)
	}
	if rp, _, _ := unstructured.NestedString(get(t, dyn, gvrPV, "", "pvc-old").Object, "spec", "persistentVolumeReclaimPolicy"); rp != "Retain" {
		t.Errorf("old PV reclaim %s", rp)
	}
	if n, _, _ := unstructured.NestedInt64(get(t, dyn, gvrDeployment, "app", "app").Object, "spec", "replicas"); n != 2 {
		t.Errorf("replicas %d", n)
	}
	ks := get(t, dyn, GVRKustomization, "flux-system", "apps")
	if s, found, _ := unstructured.NestedBool(ks.Object, "spec", "suspend"); found && s {
		t.Error("Kustomization still suspended")
	}
	if ks.GetAnnotations()["reconcile.fluxcd.io/requestedAt"] == "" {
		t.Error("no reconcile requested")
	}
	if b, _ := os.ReadFile(journal); !strings.Contains(string(b), "finished") {
		t.Errorf("journal: %s", b)
	}

	// switching back reuses the old (Released) PV
	old := get(t, dyn, gvrPV, "", "pvc-old")
	_ = unstructured.SetNestedField(old.Object, "Released", "status", "phase")
	_ = unstructured.SetNestedMap(old.Object, map[string]any{"namespace": "app", "name": "data", "uid": "old"}, "spec", "claimRef")
	_, _ = dyn.Resource(gvrPV).Update(ctx, old, metav1.UpdateOptions{})
	v := get(t, dyn, longhorn.GVRVolume, lhNS, "pvc-old")
	_ = unstructured.SetNestedField(v.Object, "pvc-old", "status", "kubernetesStatus", "pvName")
	_, _ = dyn.Resource(longhorn.GVRVolume).Namespace(lhNS).Update(ctx, v, metav1.UpdateOptions{})
	// the PVC is bound to pvc-new now; the new PV has to report Bound for Build
	back, err := Build(ctx, c, "app", "data", "pvc-old")
	if err != nil {
		t.Fatal(err)
	}
	if back.ReusePV != "pvc-old" || back.OldVolume != "pvc-new" {
		t.Errorf("switch back plan: reuse %q old %q blockers %v", back.ReusePV, back.OldVolume, back.Blockers)
	}
}

func TestSwitchRollback(t *testing.T) {
	c, dyn := world()
	dyn.PrependReactor("delete", "persistentvolumeclaims", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("forbidden")
	})
	ctx := context.Background()
	p, err := Build(ctx, c, "app", "data", "pvc-new")
	if err != nil {
		t.Fatal(err)
	}
	err = Run(ctx, c, p, Options{Poll: 10 * time.Millisecond, Wait: 2 * time.Second})
	if err == nil || !strings.Contains(err.Error(), "rolled back") {
		t.Fatalf("want rollback, got %v", err)
	}
	if n, _, _ := unstructured.NestedInt64(get(t, dyn, gvrDeployment, "app", "app").Object, "spec", "replicas"); n != 2 {
		t.Errorf("replicas after rollback %d", n)
	}
	if s, found, _ := unstructured.NestedBool(get(t, dyn, GVRKustomization, "flux-system", "apps").Object, "spec", "suspend"); found && s {
		t.Error("still suspended after rollback")
	}
}

func TestSwitchBlockers(t *testing.T) {
	ctx := context.Background()
	cnpgPod := ownedBy(obj("v1", "Pod", "app", "db-1", nil, map[string]any{
		"spec": map[string]any{"volumes": []any{map[string]any{"persistentVolumeClaim": map[string]any{"claimName": "data"}}}},
	}), "postgresql.cnpg.io/v1", "Cluster", "db")
	hpa := obj("autoscaling/v2", "HorizontalPodAutoscaler", "app", "app", nil, map[string]any{
		"spec": map[string]any{"scaleTargetRef": map[string]any{"kind": "Deployment", "name": "app"}}})
	for name, extra := range map[string]runtime.Object{"CloudNativePG": cnpgPod, "HorizontalPodAutoscaler": hpa} {
		c, _ := world(extra)
		p, err := Build(ctx, c, "app", "data", "pvc-new")
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(strings.Join(p.Blockers, " "), name) {
			t.Errorf("%s: blockers %v", name, p.Blockers)
		}
		if err := Run(ctx, c, p, Options{}); !errors.Is(err, ErrBlocked) {
			t.Errorf("%s: ran anyway: %v", name, err)
		}
	}
	// same volume, missing target
	c, _ := world()
	if p, _ := Build(ctx, c, "app", "data", "pvc-old"); len(p.Blockers) == 0 {
		t.Error("switch to the current volume not blocked")
	}
	if _, err := Build(ctx, c, "app", "data", "pvc-nope"); err == nil {
		t.Error("missing target volume accepted")
	}
}
