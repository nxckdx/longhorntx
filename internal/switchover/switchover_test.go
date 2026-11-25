package switchover

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
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

var gvrRS = schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "replicasets"}

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

func pod(name, claim string, ownerKind, ownerName string) *unstructured.Unstructured {
	return ownedBy(obj("v1", "Pod", "app", name, nil, map[string]any{
		"spec":   map[string]any{"volumes": []any{map[string]any{"name": "d", "persistentVolumeClaim": map[string]any{"claimName": claim}}}},
		"status": map[string]any{"conditions": []any{map[string]any{"type": "Ready", "status": "True"}}},
	}), "apps/v1", ownerKind, ownerName)
}

// cluster simulates what lhtx relies on: the API server enforcing the
// freeze policy (also in dry runs), Longhorn detaching volumes when pods go,
// and controllers re-creating pods when nudged.
type cluster struct {
	mu     sync.Mutex
	frozen map[string]map[string]bool // policy → ns/pvc
	dyn    *dynfake.FakeDynamicClient
	c      *longhorn.Client
}

var fluxLabels = map[string]string{"kustomize.toolkit.fluxcd.io/name": "apps", "kustomize.toolkit.fluxcd.io/namespace": "flux-system"}

// world: app/data (PVC) → PV pvc-old → Longhorn volume pvc-old, mounted by
// a pod of ReplicaSet app-123 (Deployment app); target pvc-new restored.
func world(extra ...runtime.Object) *cluster {
	kinds := map[schema.GroupVersionResource]string{
		gvrPod: "PodList", gvrPVC: "PersistentVolumeClaimList", gvrPV: "PersistentVolumeList", gvrRS: "ReplicaSetList",
		GVRPolicy: "ValidatingAdmissionPolicyList", GVRBinding: "ValidatingAdmissionPolicyBindingList",
		longhorn.GVRVolume: "VolumeList", longhorn.GVREngine: "EngineList", gvrConfigMap: "ConfigMapList",
		{Group: "postgresql.cnpg.io", Version: "v1", Resource: "clusters"}: "ClusterList",
		gvrSecret: "SecretList",
	}
	objs := []runtime.Object{
		obj("v1", "PersistentVolumeClaim", "app", "data", fluxLabels, map[string]any{
			"spec": map[string]any{"volumeName": "pvc-old", "storageClassName": "longhorn", "accessModes": []any{"ReadWriteOnce"},
				"volumeMode": "Filesystem", "resources": map[string]any{"requests": map[string]any{"storage": "10Gi"}}},
			"status": map[string]any{"phase": "Bound"},
		}),
		obj("v1", "PersistentVolume", "", "pvc-old", nil, map[string]any{
			"spec": map[string]any{"persistentVolumeReclaimPolicy": "Delete", "storageClassName": "longhorn",
				"claimRef": map[string]any{"namespace": "app", "name": "data"},
				"csi": map[string]any{"driver": "driver.longhorn.io", "volumeHandle": "pvc-old", "fsType": "ext4",
					"volumeAttributes": map[string]any{"numberOfReplicas": "2", "fromBackup": "", "storage.kubernetes.io/csiProvisionerIdentity": "x"}}},
			"status": map[string]any{"phase": "Bound"},
		}),
		obj("longhorn.io/v1beta2", "Volume", lhNS, "pvc-old", nil, map[string]any{
			"spec": map[string]any{"size": "10737418240", "numberOfReplicas": int64(2), "dataEngine": "v1"},
			"status": map[string]any{"state": "attached", "robustness": "healthy",
				"kubernetesStatus": map[string]any{"namespace": "app", "pvcName": "data", "pvName": "pvc-old"}},
		}),
		obj("longhorn.io/v1beta2", "Volume", lhNS, "pvc-new", nil, map[string]any{
			"spec": map[string]any{"size": "10737418240", "dataEngine": "v1"}, "status": map[string]any{"state": "detached"}}),
		pod("app-123-abc", "data", "ReplicaSet", "app-123"),
		ownedBy(obj("apps/v1", "ReplicaSet", "app", "app-123", nil, nil), "apps/v1", "Deployment", "app"),
	}
	k := &cluster{frozen: map[string]map[string]bool{}}
	dyn := dynfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), kinds, append(objs, extra...)...)
	k.dyn = dyn
	ctx := context.Background()

	dyn.PrependReactor("create", "selfsubjectreviews", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, obj("authentication.k8s.io/v1", "SelfSubjectReview", "", "", nil,
			map[string]any{"status": map[string]any{"userInfo": map[string]any{"username": "tester"}}}), nil
	})
	// the API server enforces the policy
	dyn.PrependReactor("create", "validatingadmissionpolicies", func(a k8stesting.Action) (bool, runtime.Object, error) {
		u := a.(k8stesting.CreateActionImpl).GetObject().(*unstructured.Unstructured)
		set := map[string]bool{}
		for _, x := range splitList(u.GetAnnotations()[annPVCs]) {
			set[x] = true
		}
		k.mu.Lock()
		k.frozen[u.GetName()] = set
		k.mu.Unlock()
		return false, nil, nil
	})
	dyn.PrependReactor("delete", "validatingadmissionpolicies", func(a k8stesting.Action) (bool, runtime.Object, error) {
		k.mu.Lock()
		delete(k.frozen, a.(k8stesting.DeleteActionImpl).GetName())
		k.mu.Unlock()
		return false, nil, nil
	})
	dyn.PrependReactor("create", "pods", func(a k8stesting.Action) (bool, runtime.Object, error) {
		ca := a.(k8stesting.CreateActionImpl)
		u := ca.GetObject().(*unstructured.Unstructured)
		k.mu.Lock()
		defer k.mu.Unlock()
		for _, cl := range claimsOf(u) {
			for name, set := range k.frozen {
				if set[a.GetNamespace()+"/"+cl] {
					return true, nil, apierrors.NewForbidden(gvrPod.GroupResource(), "", errors.New(denyMarker+" ("+name+")"))
				}
			}
		}
		if len(ca.CreateOptions.DryRun) > 0 {
			return true, u, nil
		}
		return false, nil, nil
	})
	// Longhorn detaches when the pod is gone
	dyn.PrependReactor("delete", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		go k.setState("app", "data", "detached")
		return false, nil, nil
	})
	// a nudged ReplicaSet starts its pod again (rejected while frozen)
	dyn.PrependReactor("patch", "replicasets", func(k8stesting.Action) (bool, runtime.Object, error) {
		go func() {
			np := pod("app-123-new", "data", "ReplicaSet", "app-123")
			np.SetLabels(map[string]string{"app": "web"})
			_ = unstructured.SetNestedSlice(np.Object, []any{map[string]any{"type": "Ready", "status": "True"}}, "status", "conditions")
			if _, err := dyn.Resource(gvrPod).Namespace("app").Create(ctx, np, metav1.CreateOptions{}); err == nil {
				k.setState("app", "data", "attached")
			}
		}()
		return false, nil, nil
	})
	dyn.PrependReactor("create", "persistentvolumeclaims", func(a k8stesting.Action) (bool, runtime.Object, error) {
		u := a.(k8stesting.CreateActionImpl).GetObject().(*unstructured.Unstructured)
		_ = unstructured.SetNestedField(u.Object, "Bound", "status", "phase")
		vol, _, _ := unstructured.NestedString(u.Object, "spec", "volumeName")
		go func() { // Longhorn records the PVC on the volume
			time.Sleep(5 * time.Millisecond)
			v, err := dyn.Resource(longhorn.GVRVolume).Namespace(lhNS).Get(ctx, vol, metav1.GetOptions{})
			if err == nil {
				_ = unstructured.SetNestedMap(v.Object, map[string]any{"namespace": "app", "pvcName": "data", "pvName": vol}, "status", "kubernetesStatus")
				_, _ = dyn.Resource(longhorn.GVRVolume).Namespace(lhNS).Update(ctx, v, metav1.UpdateOptions{})
			}
		}()
		return false, nil, nil
	})
	dyn.PrependReactor("delete", "persistentvolumeclaims", func(a k8stesting.Action) (bool, runtime.Object, error) {
		// the PV of the deleted claim (not one bound to a re-created claim later)
		bound := ""
		if o, err := dyn.Tracker().Get(gvrPVC, a.GetNamespace(), a.(k8stesting.DeleteActionImpl).GetName()); err == nil {
			bound, _, _ = unstructured.NestedString(o.(*unstructured.Unstructured).Object, "spec", "volumeName")
		}
		go func() { // the PV controller releases the PV
			time.Sleep(5 * time.Millisecond)
			pvs, _ := dyn.Resource(gvrPV).List(ctx, metav1.ListOptions{})
			for _, pv := range pvs.Items {
				if n, _, _ := unstructured.NestedString(pv.Object, "spec", "claimRef", "name"); n == "data" && pv.GetName() == bound {
					if ph, _, _ := unstructured.NestedString(pv.Object, "status", "phase"); ph == "Bound" || ph == "" {
						_ = unstructured.SetNestedField(pv.Object, "Released", "status", "phase")
						_, _ = dyn.Resource(gvrPV).Update(ctx, &pv, metav1.UpdateOptions{})
						// Longhorn notes that the PVC no longer uses the volume
						vol := field(&pv, "spec", "csi", "volumeHandle")
						if v, err := dyn.Resource(longhorn.GVRVolume).Namespace(lhNS).Get(ctx, vol, metav1.GetOptions{}); err == nil {
							_ = unstructured.SetNestedField(v.Object, "now", "status", "kubernetesStatus", "lastPVCRefAt")
							_, _ = dyn.Resource(longhorn.GVRVolume).Namespace(lhNS).Update(ctx, v, metav1.UpdateOptions{})
						}
					}
				}
			}
		}()
		return false, nil, nil
	})
	cl := &kube.Cluster{Context: "t", Config: &rest.Config{}, Traffic: &kube.Counter{}}
	k.c = longhorn.NewClientFrom(cl, lhNS, dyn, nil)
	return k
}

// setState sets the state of the Longhorn volume the PVC is bound to now.
func (k *cluster) setState(ns, pvc, state string) {
	ctx := context.Background()
	p, err := k.dyn.Resource(gvrPVC).Namespace(ns).Get(ctx, pvc, metav1.GetOptions{})
	if err != nil {
		return
	}
	vol, _, _ := unstructured.NestedString(p.Object, "spec", "volumeName")
	v, err := k.dyn.Resource(longhorn.GVRVolume).Namespace(lhNS).Get(ctx, vol, metav1.GetOptions{})
	if err != nil {
		return
	}
	_ = unstructured.SetNestedField(v.Object, state, "status", "state")
	_, _ = k.dyn.Resource(longhorn.GVRVolume).Namespace(lhNS).Update(ctx, v, metav1.UpdateOptions{})
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

func field(u *unstructured.Unstructured, path ...string) string {
	s, _, _ := unstructured.NestedString(u.Object, path...)
	return s
}

var fast = Options{Poll: 10 * time.Millisecond, Wait: 3 * time.Second}
var fastF = FreezeOptions{Poll: 10 * time.Millisecond, Wait: 3 * time.Second}

func TestSwitch(t *testing.T) {
	k := world()
	ctx := context.Background()
	p, err := Build(ctx, k.c, "app", "data", "pvc-new")
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Blockers) > 0 || p.OldVolume != "pvc-old" || len(p.Freeze.Pods) != 1 || len(p.Freeze.Owners) != 1 ||
		p.Freeze.Owners[0].Kind != "ReplicaSet" || p.Freeze.User != "tester" {
		t.Fatalf("plan: %+v / %+v", p, p.Freeze)
	}
	if _, ok := p.PVAttributes["storage.kubernetes.io/csiProvisionerIdentity"]; ok {
		t.Error("provisioner identity copied")
	}
	o := fast
	o.Journal = filepath.Join(t.TempDir(), "j.json")
	var log []string
	o.Log = func(f string, a ...any) { log = append(log, f) }
	if err := Run(ctx, k.c, p, o); err != nil {
		t.Fatalf("%v\n%s", err, strings.Join(log, "\n"))
	}
	pvc := get(t, k.dyn, gvrPVC, "app", "data")
	if vn := field(pvc, "spec", "volumeName"); vn != "pvc-new" {
		t.Errorf("PVC bound to %s", vn)
	}
	if pvc.GetLabels()["kustomize.toolkit.fluxcd.io/name"] != "apps" {
		t.Error("labels lost")
	}
	if rp := field(get(t, k.dyn, gvrPV, "", "pvc-old"), "spec", "persistentVolumeReclaimPolicy"); rp != "Retain" {
		t.Errorf("old PV reclaim %s", rp)
	}
	if fs, _ := ActiveFreezes(ctx, k.dyn); len(fs) != 0 {
		t.Errorf("still frozen: %+v", fs)
	}
	if s := field(get(t, k.dyn, longhorn.GVRVolume, lhNS, "pvc-new"), "status", "state"); s != "attached" {
		t.Errorf("new volume %s, want attached by the restarted pod", s)
	}
	if b, _ := os.ReadFile(o.Journal); !strings.Contains(string(b), "finished") {
		t.Errorf("journal: %s", b)
	}

	// switching back reuses the old, released PV
	time.Sleep(50 * time.Millisecond)
	back, err := Build(ctx, k.c, "app", "data", "pvc-old")
	if err != nil {
		t.Fatal(err)
	}
	if back.ReusePV != "pvc-old" || back.OldVolume != "pvc-new" || len(back.Blockers) > 0 {
		t.Fatalf("switch back plan: reuse %q old %q blockers %v", back.ReusePV, back.OldVolume, back.Blockers)
	}
	if err := Run(ctx, k.c, back, fast); err != nil {
		t.Fatal(err)
	}
	if vn := field(get(t, k.dyn, gvrPVC, "app", "data"), "spec", "volumeName"); vn != "pvc-old" {
		t.Errorf("after switch back PVC bound to %s", vn)
	}
}

func TestSwitchRollback(t *testing.T) {
	k := world()
	k.dyn.PrependReactor("delete", "persistentvolumeclaims", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("forbidden")
	})
	ctx := context.Background()
	p, err := Build(ctx, k.c, "app", "data", "pvc-new")
	if err != nil {
		t.Fatal(err)
	}
	err = Run(ctx, k.c, p, fast)
	if err == nil || !strings.Contains(err.Error(), "rolled back:") {
		t.Fatalf("want rollback, got %v", err)
	}
	if fs, _ := ActiveFreezes(ctx, k.dyn); len(fs) != 0 {
		t.Error("still frozen after rollback")
	}
	if _, err := k.dyn.Resource(gvrPod).Namespace("app").Get(ctx, "app-123-new", metav1.GetOptions{}); err != nil {
		t.Errorf("pod not started again: %v", err)
	}
}

func TestFreezeUnfreeze(t *testing.T) {
	k := world()
	ctx := context.Background()
	p, err := PlanFreeze(ctx, k.c, []string{"app/data"}, "longhorn upgrade")
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Pods) != 1 || len(p.Attached) != 1 {
		t.Fatalf("%+v", p)
	}
	pol, _ := policyObjects("x", p, time.Now(), "")
	vals, _, _ := unstructured.NestedSlice(pol.Object, "spec", "validations")
	if e := vals[1].(map[string]any)["expression"].(string); !strings.Contains(e, "'tester'") {
		t.Errorf("own user not exempt: %s", e)
	}
	if _, err := ApplyFreeze(ctx, k.c, p, fastF); err != nil {
		t.Fatal(err)
	}
	// nothing can start on the PVC now
	if _, err := k.dyn.Resource(gvrPod).Namespace("app").Create(ctx, pod("sneaky", "data", "ReplicaSet", "x"), metav1.CreateOptions{}); err == nil {
		t.Error("pod started while frozen")
	}
	fs, _ := ActiveFreezes(ctx, k.dyn)
	if len(fs) != 1 || fs[0].Reason != "longhorn upgrade" || len(fs[0].Owners) != 1 {
		t.Fatalf("active: %+v", fs)
	}
	if err := Unfreeze(ctx, k.c, fs[0], fastF); err != nil {
		t.Fatal(err)
	}
	if fs, _ := ActiveFreezes(ctx, k.dyn); len(fs) != 0 {
		t.Error("still frozen")
	}
}

func TestSwitchBlockers(t *testing.T) {
	ctx := context.Background()
	k := world()
	if p, _ := Build(ctx, k.c, "app", "data", "pvc-old"); len(p.Blockers) == 0 {
		t.Error("switch to the current volume not blocked")
	}
	if _, err := Build(ctx, k.c, "app", "data", "pvc-nope"); err == nil {
		t.Error("missing target volume accepted")
	}
	// a CloudNativePG instance is allowed, with a clear warning
	k = world(ownedBy(obj("v1", "Pod", "app", "db-1", nil, map[string]any{
		"spec": map[string]any{"volumes": []any{map[string]any{"persistentVolumeClaim": map[string]any{"claimName": "data"}}}},
	}), "postgresql.cnpg.io/v1", "Cluster", "db"))
	p, err := Build(ctx, k.c, "app", "data", "pvc-new")
	if err != nil || len(p.Blockers) > 0 || !strings.Contains(strings.Join(p.Warnings, " "), "CloudNativePG") {
		t.Errorf("CNPG: %v %v %v", err, p.Blockers, p.Warnings)
	}
}

func TestChecks(t *testing.T) {
	k := world()
	ctx := context.Background()
	dir := t.TempDir()
	p := &Plan{Namespace: "app", PVC: "data", NewVolume: "pvc-new"}
	write := func(name string, j journal) {
		b, _ := json.Marshal(j)
		_ = os.WriteFile(filepath.Join(dir, name), b, 0o644)
	}
	write("a.json", journal{Context: "t", Plan: p, Steps: []string{"PVC deleted"}, At: time.Now()})
	write("b.json", journal{Context: "t", Plan: p, Error: "x", RolledBack: true, At: time.Now()})
	write("c.json", journal{Context: "other", Plan: p, At: time.Now()})
	fp, err := PlanFreeze(ctx, k.c, []string{"app/data"}, "test")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ApplyFreeze(ctx, k.c, fp, fastF); err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, f := range Checks(ctx, k.c, dir) {
		got = append(got, f.Check)
	}
	if strings.Join(got, ",") != "switch,freeze" {
		t.Errorf("%v", got)
	}
}

func TestStopOrder(t *testing.T) {
	pods := []string{"ns/kafka-0", "ns/kafka-2", "ns/kafka-10", "ns/kafka-1", "ns/web-abc12"}
	sortStopOrder(pods)
	if strings.Join(pods, ",") != "ns/kafka-10,ns/kafka-2,ns/kafka-1,ns/kafka-0,ns/web-abc12" {
		t.Error(pods)
	}
	h := parseHook("opensearch: curl -XPOST localhost:9200/_flush", "30s", "x")
	if h.Container != "opensearch" || h.Command != "curl -XPOST localhost:9200/_flush" || h.Timeout != 30*time.Second {
		t.Errorf("%+v", h)
	}
	if h := parseHook("echo a: b", "", "x"); h.Container != "" || h.Command != "echo a: b" {
		t.Errorf("no container: %+v", h)
	}
}

func TestFreezeHooks(t *testing.T) {
	// pre-freeze on the pod, post-unfreeze from the lhtx-hooks ConfigMap
	k := world(obj("v1", "ConfigMap", lhNS, HooksConfigMap, nil, map[string]any{"data": map[string]any{
		"hooks.yaml": "- namespace: app\n  selector: {app: web}\n  container: main\n  postUnfreeze: echo settled\n"}}))
	ctx := context.Background()
	p0 := get(t, k.dyn, gvrPod, "app", "app-123-abc")
	p0.SetAnnotations(map[string]string{AnnPreFreeze: "main: echo flush"})
	p0.SetLabels(map[string]string{"app": "web"})
	_, _ = k.dyn.Resource(gvrPod).Namespace("app").Update(ctx, p0, metav1.UpdateOptions{})
	var mu sync.Mutex
	var ran []string
	k.c.SetExec(func(_ context.Context, ns, pod, container, cmd string) (string, error) {
		mu.Lock()
		defer mu.Unlock()
		ran = append(ran, pod+"/"+container+": "+cmd)
		if cmd == "fail" {
			return "boom", errors.New("exit 1")
		}
		return "ok", nil
	})
	p, err := PlanFreeze(ctx, k.c, []string{"app/data"}, "t")
	if err != nil {
		t.Fatal(err)
	}
	g := p.Groups[0]
	if g.Pre == nil || g.Pre.Source != "pod annotation" || g.Post == nil || g.Post.Source != "ConfigMap lhtx-hooks" || g.Post.Container != "main" {
		t.Fatalf("hooks: %+v %+v", g.Pre, g.Post)
	}
	if _, err := ApplyFreeze(ctx, k.c, p, fastF); err != nil {
		t.Fatal(err)
	}
	fs, _ := ActiveFreezes(ctx, k.dyn)
	if len(fs) != 1 || len(fs[0].PostHooks) != 1 {
		t.Fatalf("post hooks not stored: %+v", fs)
	}
	if err := Unfreeze(ctx, k.c, fs[0], fastF); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	got := strings.Join(ran, " | ")
	mu.Unlock()
	if got != "app-123-abc/main: echo flush | app-123-new/main: echo settled" {
		t.Errorf("ran: %s", got)
	}

	// a failing pre-freeze hook stops before anything is deleted
	k = world()
	p0 = get(t, k.dyn, gvrPod, "app", "app-123-abc")
	p0.SetAnnotations(map[string]string{AnnPreFreeze: "fail"})
	_, _ = k.dyn.Resource(gvrPod).Namespace("app").Update(ctx, p0, metav1.UpdateOptions{})
	k.c.SetExec(func(_ context.Context, _, _, _, cmd string) (string, error) { return "boom", errors.New("exit 1") })
	p, _ = PlanFreeze(ctx, k.c, []string{"app/data"}, "t")
	if _, err := ApplyFreeze(ctx, k.c, p, fastF); err == nil || !strings.Contains(err.Error(), "nothing stopped") {
		t.Fatalf("want abort, got %v", err)
	}
	if fs, _ := ActiveFreezes(ctx, k.dyn); len(fs) != 0 {
		t.Error("policy left after a failed hook")
	}
	if _, err := k.dyn.Resource(gvrPod).Namespace("app").Get(ctx, "app-123-abc", metav1.GetOptions{}); err != nil {
		t.Error("pod deleted although the hook failed")
	}
}

func TestFreezeCNPGHibernation(t *testing.T) {
	gvrCluster := schema.GroupVersionResource{Group: "postgresql.cnpg.io", Version: "v1", Resource: "clusters"}
	dbPod := ownedBy(obj("v1", "Pod", "app", "db-1", nil, map[string]any{
		"spec": map[string]any{"volumes": []any{map[string]any{"persistentVolumeClaim": map[string]any{"claimName": "db"}}}},
	}), "postgresql.cnpg.io/v1", "Cluster", "db")
	k := world(dbPod, obj("postgresql.cnpg.io/v1", "Cluster", "app", "db", nil, map[string]any{"spec": map[string]any{"instances": int64(1)}}))
	ctx := context.Background()
	// the operator stops its instance when hibernation is requested
	k.dyn.PrependReactor("patch", "clusters", func(a k8stesting.Action) (bool, runtime.Object, error) {
		if strings.Contains(string(a.(k8stesting.PatchActionImpl).GetPatch()), `"on"`) {
			go func() { _ = k.dyn.Resource(gvrPod).Namespace("app").Delete(ctx, "db-1", metav1.DeleteOptions{}) }()
		}
		return false, nil, nil
	})
	p, err := PlanFreeze(ctx, k.c, []string{"app/db"}, "t")
	if err != nil || len(p.Groups) != 1 || !p.Groups[0].Hibernate {
		t.Fatalf("%v %+v", err, p)
	}
	if _, err := ApplyFreeze(ctx, k.c, p, fastF); err != nil {
		t.Fatal(err)
	}
	cl := get(t, k.dyn, gvrCluster, "app", "db")
	if cl.GetAnnotations()[annCNPG] != "on" {
		t.Error("not hibernated")
	}
	fs, _ := ActiveFreezes(ctx, k.dyn)
	if len(fs) != 1 || len(fs[0].Hibernated) != 1 {
		t.Fatalf("%+v", fs)
	}
	if err := Unfreeze(ctx, k.c, fs[0], FreezeOptions{Poll: 10 * time.Millisecond, Wait: 200 * time.Millisecond}); err != nil &&
		!strings.Contains(err.Error(), "not attached") {
		t.Fatal(err)
	}
	if get(t, k.dyn, gvrCluster, "app", "db").GetAnnotations()[annCNPG] != "off" {
		t.Error("not woken up")
	}
}

// The agent restarts in the middle of a freeze: the second run takes the
// existing policy over and keeps the owners of the pods already deleted.
func TestFreezeResume(t *testing.T) {
	k := world()
	ctx := context.Background()
	p, err := PlanFreeze(ctx, k.c, []string{"app/data"}, "upgrade")
	if err != nil {
		t.Fatal(err)
	}
	o := fastF
	o.Name, o.Request = freezePrefix+"upgrade", "upgrade"
	if _, err := ApplyFreeze(ctx, k.c, p, o); err != nil {
		t.Fatal(err)
	}
	p2, err := PlanFreeze(ctx, k.c, []string{"app/data"}, "upgrade")
	if err != nil {
		t.Fatal(err)
	}
	if len(p2.Pods) != 0 || len(p2.Owners) != 0 {
		t.Fatalf("pods still there: %+v", p2)
	}
	f, err := ApplyFreeze(ctx, k.c, p2, o)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Owners) != 1 || len(f.Attached) != 1 || f.Request != "upgrade" {
		t.Fatalf("resumed freeze lost state: %+v", f)
	}
	got, _ := GetFreeze(ctx, k.dyn, o.Name)
	if got == nil || len(got.Owners) != 1 || got.Request != "upgrade" {
		t.Fatalf("stored: %+v", got)
	}
	p3, err := PlanFreeze(ctx, k.c, []string{"app/other"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ApplyFreeze(ctx, k.c, p3, o); err == nil {
		t.Error("took over a policy of other PVCs")
	}
}

// A controller still in its back-off ignores the first nudge (seen with a
// ReplicaSet on k3s): unfreeze nudges again until the volume is attached.
func TestUnfreezeRenudge(t *testing.T) {
	k := world()
	ctx := context.Background()
	p, err := PlanFreeze(ctx, k.c, []string{"app/data"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ApplyFreeze(ctx, k.c, p, fastF); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	patches := 0
	k.dyn.PrependReactor("patch", "replicasets", func(a k8stesting.Action) (bool, runtime.Object, error) {
		mu.Lock()
		defer mu.Unlock()
		patches++
		if patches == 1 {
			return true, &unstructured.Unstructured{Object: map[string]any{}}, nil // ignored
		}
		return false, nil, nil
	})
	fs, _ := ActiveFreezes(ctx, k.dyn)
	o := fastF
	o.Renudge = 50 * time.Millisecond
	if err := Unfreeze(ctx, k.c, fs[0], o); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if patches < 2 {
		t.Errorf("nudged %d times", patches)
	}
}

// A pre-freeze hook runs only in a ready pod; an app that is not running
// has nothing to prepare, the freeze goes on.
func TestPreHookSkippedWithoutReadyPod(t *testing.T) {
	k := world()
	ctx := context.Background()
	p, err := PlanFreeze(ctx, k.c, []string{"app/data"}, "")
	if err != nil {
		t.Fatal(err)
	}
	p.Groups[0].Pre = &Hook{Command: "false", Timeout: time.Second, Source: "test"}
	for _, x := range p.Pods {
		ns, n, _ := strings.Cut(x, "/")
		u, _ := k.dyn.Resource(gvrPod).Namespace(ns).Get(ctx, n, metav1.GetOptions{})
		_ = unstructured.SetNestedSlice(u.Object, []any{map[string]any{"type": "Ready", "status": "False"}}, "status", "conditions")
		_, _ = k.dyn.Resource(gvrPod).Namespace(ns).Update(ctx, u, metav1.UpdateOptions{})
	}
	var logs []string
	o := fastF
	o.Log = func(f string, a ...any) { logs = append(logs, fmt.Sprintf(f, a...)) }
	k.c.SetExec(func(context.Context, string, string, string, string) (string, error) {
		t.Error("hook ran in a pod that is not ready")
		return "", nil
	})
	if _, err := ApplyFreeze(ctx, k.c, p, o); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(logs, "\n"), "hook skipped") {
		t.Errorf("no skip logged: %v", logs)
	}
}

// encrypted: app/data's volumes are encrypted with the key in Secret app/data
func encrypted(t *testing.T, k *cluster) {
	ctx := context.Background()
	pv := get(t, k.dyn, gvrPV, "", "pvc-old")
	_ = unstructured.SetNestedField(pv.Object, "true", "spec", "csi", "volumeAttributes", "encrypted")
	_ = unstructured.SetNestedMap(pv.Object, map[string]any{"name": "data", "namespace": "app"}, "spec", "csi", "nodeStageSecretRef")
	_, _ = k.dyn.Resource(gvrPV).Update(ctx, pv, metav1.UpdateOptions{})
	for _, v := range []string{"pvc-old", "pvc-new"} {
		u := get(t, k.dyn, longhorn.GVRVolume, lhNS, v)
		_ = unstructured.SetNestedField(u.Object, true, "spec", "encrypted")
		_, _ = k.dyn.Resource(longhorn.GVRVolume).Namespace(lhNS).Update(ctx, u, metav1.UpdateOptions{})
	}
	_, _ = k.dyn.Resource(gvrSecret).Namespace("app").Create(ctx, keySecret("old-key"), metav1.CreateOptions{})
}

func keySecret(key string) *unstructured.Unstructured {
	data := map[string]any{}
	for f, v := range map[string]string{"CRYPTO_KEY_VALUE": key, "CRYPTO_KEY_CIPHER": "aes-xts-plain64", "CRYPTO_KEY_HASH": "sha256",
		"CRYPTO_KEY_SIZE": "256", "CRYPTO_PBKDF": "argon2i"} {
		data[f] = base64.StdEncoding.EncodeToString([]byte(v))
	}
	return obj("v1", "Secret", "app", "data", nil, map[string]any{"type": "Opaque", "data": data})
}

// The switch of an encrypted volume keeps the key refs; a manager that
// replaces the key when the PVC is re-created is caught before any pod
// starts, and the old key is kept.
func TestSwitchKeyGuard(t *testing.T) {
	ctx := context.Background()
	o := Options{Poll: 10 * time.Millisecond, Wait: 3 * time.Second, KeySettle: 100 * time.Millisecond}

	k := world()
	encrypted(t, k)
	p, err := Build(ctx, k.c, "app", "data", "pvc-new")
	if err != nil || len(p.Blockers) > 0 || p.KeySecret == nil {
		t.Fatalf("plan: %v %v %+v", err, p.Blockers, p.KeySecret)
	}
	if err := Run(ctx, k.c, p, o); err != nil {
		t.Fatalf("unchanged key: %v", err)
	}
	if ref, _, _ := unstructured.NestedString(get(t, k.dyn, gvrPV, "", "pvc-new").Object, "spec", "csi", "nodeStageSecretRef", "name"); ref != "data" {
		t.Errorf("new PV lost the key ref: %q", ref)
	}

	// a "manager" generates a new key when the PVC is created again
	k = world()
	encrypted(t, k)
	k.dyn.PrependReactor("create", "persistentvolumeclaims", func(k8stesting.Action) (bool, runtime.Object, error) {
		go func() {
			time.Sleep(20 * time.Millisecond)
			_, _ = k.dyn.Resource(gvrSecret).Namespace("app").Update(ctx, keySecret("NEW-random-key"), metav1.UpdateOptions{})
		}()
		return false, nil, nil
	})
	p, _ = Build(ctx, k.c, "app", "data", "pvc-new")
	err = Run(ctx, k.c, p, o)
	if err == nil || !strings.Contains(err.Error(), "ANOTHER key") {
		t.Fatalf("replaced key not caught: %v", err)
	}
	if fs, _ := ActiveFreezes(ctx, k.dyn); len(fs) != 1 {
		t.Error("the PVC must stay frozen")
	}
	l, _ := k.dyn.Resource(gvrSecret).Namespace("app").List(ctx, metav1.ListOptions{LabelSelector: longhorn.LabelKeyBackup + "=true"})
	if len(l.Items) != 1 {
		t.Fatalf("no key backup: %d", len(l.Items))
	}
	old, _ := longhorn.Fingerprint(keySecret("old-key"))
	if fp, _ := longhorn.Fingerprint(&l.Items[0]); fp != old {
		t.Error("the backup does not hold the old key")
	}
	// unfreeze refuses while the key Secret still holds the new key …
	fs, _ := ActiveFreezes(ctx, k.dyn)
	if err := Unfreeze(ctx, k.c, fs[0], fastF); err == nil || !strings.Contains(err.Error(), "does not hold the key") {
		t.Fatalf("unfreeze with the wrong key: %v", err)
	}
	// … and goes on once the old key is back
	_, _ = k.dyn.Resource(gvrSecret).Namespace("app").Update(ctx, keySecret("old-key"), metav1.UpdateOptions{})
	if err := Unfreeze(ctx, k.c, fs[0], fastF); err != nil {
		t.Fatalf("unfreeze with the old key back: %v", err)
	}
}
