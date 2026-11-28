package demo

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8stesting "k8s.io/client-go/testing"

	"github.com/nxckdx/longhorntx/internal/longhorn"
)

// Workloads behind the demo PVCs, so restore + switch can run in the demo:
// most are Deployments of the Flux Kustomization "apps", jellyfin comes
// from a HelmRelease, loki and postgres are StatefulSets, and prometheus
// comes from an operator.

var (
	gvrPod         = schema.GroupVersionResource{Version: "v1", Resource: "pods"}
	gvrPV          = schema.GroupVersionResource{Version: "v1", Resource: "persistentvolumes"}
	gvrReplicaSet  = schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "replicasets"}
	gvrDeployment  = schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}
	gvrStatefulSet = schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "statefulsets"}
	gvrHPA         = schema.GroupVersionResource{Group: "autoscaling", Version: "v2", Resource: "horizontalpodautoscalers"}
	gvrKS          = schema.GroupVersionResource{Group: "kustomize.toolkit.fluxcd.io", Version: "v1", Resource: "kustomizations"}
	gvrHR          = schema.GroupVersionResource{Group: "helm.toolkit.fluxcd.io", Version: "v2", Resource: "helmreleases"}
)

type demoWorkload struct {
	kind, name string // Deployment | StatefulSet | operator-StatefulSet
	operator   string // owner kind of an operator-managed StatefulSet
	helm       bool   // from the HelmRelease "jellyfin"
}

// workloadOf maps a demo PVC to its workload.
func workloadOf(pvc string) demoWorkload {
	switch {
	case strings.HasPrefix(pvc, "data-postgres-"):
		return demoWorkload{kind: "StatefulSet", name: "postgres"}
	case strings.HasPrefix(pvc, "prometheus-db-"):
		return demoWorkload{kind: "StatefulSet", name: "prometheus", operator: "Prometheus"}
	case pvc == "storage-loki-0":
		return demoWorkload{kind: "StatefulSet", name: "loki"}
	case pvc == "jellyfin-config":
		return demoWorkload{kind: "Deployment", name: "jellyfin", helm: true}
	case strings.HasPrefix(pvc, "nextcloud-"):
		return demoWorkload{kind: "Deployment", name: "nextcloud"}
	}
	name := strings.TrimSuffix(strings.TrimSuffix(strings.TrimSuffix(pvc, "-data"), "-shared-storage"), "-cache")
	name = strings.TrimSuffix(name, "-config")
	return demoWorkload{kind: "Deployment", name: name}
}

func fluxLabels(w demoWorkload) map[string]any {
	if w.helm {
		return map[string]any{"helm.toolkit.fluxcd.io/name": "jellyfin", "helm.toolkit.fluxcd.io/namespace": "flux-system",
			"app.kubernetes.io/managed-by": "Helm"}
	}
	if w.operator != "" {
		return map[string]any{}
	}
	return map[string]any{"kustomize.toolkit.fluxcd.io/name": "apps", "kustomize.toolkit.fluxcd.io/namespace": "flux-system"}
}

func owner(apiVersion, kind, name string) []any {
	return []any{map[string]any{"apiVersion": apiVersion, "kind": kind, "name": name, "uid": "demo-" + hex8(kind+name), "controller": true}}
}

func u(obj map[string]any) *unstructured.Unstructured { return &unstructured.Unstructured{Object: obj} }

// workloadObjects: PVs, workloads, ReplicaSets, pods and the Flux objects.
func (d *data) workloadObjects() []runtime.Object {
	out := []runtime.Object{
		u(map[string]any{"apiVersion": "kustomize.toolkit.fluxcd.io/v1", "kind": "Kustomization",
			"metadata": map[string]any{"name": "apps", "namespace": "flux-system"},
			"spec":     map[string]any{"interval": "10m", "path": "./apps", "prune": true}}),
		u(map[string]any{"apiVersion": "helm.toolkit.fluxcd.io/v2", "kind": "HelmRelease",
			"metadata": map[string]any{"name": "jellyfin", "namespace": "flux-system",
				"labels": map[string]any{"kustomize.toolkit.fluxcd.io/name": "apps", "kustomize.toolkit.fluxcd.io/namespace": "flux-system"}},
			"spec": map[string]any{"interval": "1h", "chart": map[string]any{"spec": map[string]any{"chart": "jellyfin"}}}}),
	}
	pvcsOf := map[string][]string{} // "ns/kind/name" → pvcs
	nodeOf := map[string]string{}
	for i, v := range d.volumes {
		def := volDefs[i]
		out = append(out, u(map[string]any{"apiVersion": "v1", "kind": "PersistentVolume",
			"metadata": map[string]any{"name": v.Name, "annotations": map[string]any{"pv.kubernetes.io/provisioned-by": "driver.longhorn.io"}},
			"spec": map[string]any{"persistentVolumeReclaimPolicy": "Delete", "storageClassName": "longhorn",
				"capacity":    map[string]any{"storage": fmt.Sprintf("%dGi", def.sizeGi)},
				"accessModes": []any{accessMode(def)},
				"claimRef":    map[string]any{"namespace": def.ns, "name": def.pvc},
				"csi": map[string]any{"driver": "driver.longhorn.io", "volumeHandle": v.Name, "fsType": "ext4",
					"volumeAttributes": map[string]any{"numberOfReplicas": fmt.Sprint(def.replicas), "staleReplicaTimeout": "30"}}},
			"status": map[string]any{"phase": "Bound"}}))
		if def.node == "" {
			continue
		}
		w := workloadOf(def.pvc)
		key := def.ns + "/" + w.kind + "/" + w.name
		pvcsOf[key] = append(pvcsOf[key], def.pvc)
		nodeOf[key] = def.node
	}
	for key, pvcs := range pvcsOf {
		parts := strings.SplitN(key, "/", 3)
		nsName, w := parts[0], workloadOf(pvcs[0])
		labels := fluxLabels(w)
		switch {
		case w.kind == "StatefulSet":
			meta := map[string]any{"name": w.name, "namespace": nsName, "labels": labels}
			if w.name == "loki" { // hooks as annotations on the StatefulSet
				meta["annotations"] = map[string]any{
					"lhtx.io/pre-freeze":    "loki: wget -qO- --post-data= localhost:3100/flush",
					"lhtx.io/post-unfreeze": "loki: wget -qO- localhost:3100/ready"}
			}
			sts := map[string]any{"apiVersion": "apps/v1", "kind": "StatefulSet",
				"metadata": meta,
				"spec":     map[string]any{"replicas": int64(len(pvcs))}, "status": map[string]any{"readyReplicas": int64(len(pvcs))}}
			if w.operator != "" {
				sts["metadata"].(map[string]any)["ownerReferences"] = owner("monitoring.coreos.com/v1", w.operator, w.name)
			}
			out = append(out, u(sts))
			sort.Strings(pvcs)
			for k, pvc := range pvcs { // one pod per ordinal
				out = append(out, d.pod(nsName, fmt.Sprintf("%s-%d", w.name, k), nodeOf[key], []string{pvc}, owner("apps/v1", "StatefulSet", w.name)))
			}
		default:
			rs := w.name + "-" + hex8(w.name)[:6]
			out = append(out,
				u(map[string]any{"apiVersion": "apps/v1", "kind": "Deployment",
					"metadata": map[string]any{"name": w.name, "namespace": nsName, "labels": labels},
					"spec":     map[string]any{"replicas": int64(1)}, "status": map[string]any{"readyReplicas": int64(1)}}),
				u(map[string]any{"apiVersion": "apps/v1", "kind": "ReplicaSet",
					"metadata": map[string]any{"name": rs, "namespace": nsName, "ownerReferences": owner("apps/v1", "Deployment", w.name)}}),
				d.pod(nsName, rs+"-"+hex8(rs)[:5], nodeOf[key], pvcs, owner("apps/v1", "ReplicaSet", rs)))
		}
	}
	return out
}

func accessMode(def volDef) string {
	if def.rwx {
		return "ReadWriteMany"
	}
	return "ReadWriteOnce"
}

func (d *data) pod(nsName, name, node string, pvcs []string, owners []any) *unstructured.Unstructured {
	var vols []any
	for i, p := range pvcs {
		vols = append(vols, map[string]any{"name": fmt.Sprintf("data%d", i), "persistentVolumeClaim": map[string]any{"claimName": p}})
	}
	app := name
	if len(owners) > 0 {
		o := owners[0].(map[string]any)
		app = o["name"].(string)
		if o["kind"] == "ReplicaSet" {
			app = app[:strings.LastIndex(app, "-")]
		}
	}
	return u(map[string]any{"apiVersion": "v1", "kind": "Pod",
		"metadata": map[string]any{"name": name, "namespace": nsName, "ownerReferences": owners, "labels": map[string]any{"app": app}},
		"spec":     map[string]any{"nodeName": node, "volumes": vols},
		"status": map[string]any{"phase": "Running",
			"conditions": []any{map[string]any{"type": "Ready", "status": "True"}}}})
}

// ---------------------------------------------------------------- simulation

// workloadReactors: scaling stops/starts pods and detaches/attaches their
// volumes; a new PVC binds at once.
func (s *sim) workloadReactors(ctx context.Context) {
	for _, res := range []string{"deployments", "statefulsets"} {
		gvr := gvrDeployment
		if res == "statefulsets" {
			gvr = gvrStatefulSet
		}
		s.dyn.PrependReactor("patch", res, func(a k8stesting.Action) (bool, runtime.Object, error) {
			pa := a.(k8stesting.PatchActionImpl)
			var p struct {
				Spec struct {
					Replicas *int64 `json:"replicas"`
				} `json:"spec"`
			}
			if isDryRun(a) || json.Unmarshal(pa.GetPatch(), &p) != nil || p.Spec.Replicas == nil {
				return false, nil, nil
			}
			go s.scaled(ctx, gvr, pa.GetNamespace(), pa.GetName(), *p.Spec.Replicas)
			return false, nil, nil
		})
	}
	// deleting a PVC releases its PV (like the PV controller)
	s.dyn.PrependReactor("delete", "persistentvolumeclaims", func(a k8stesting.Action) (bool, runtime.Object, error) {
		if !isDryRun(a) {
			da := a.(k8stesting.DeleteActionImpl)
			go s.releasePV(ctx, da.GetNamespace(), da.GetName())
		}
		return false, nil, nil
	})
	s.dyn.PrependReactor("create", "persistentvolumeclaims", func(a k8stesting.Action) (bool, runtime.Object, error) {
		if obj, ok := a.(k8stesting.CreateActionImpl).GetObject().(*unstructured.Unstructured); ok && !isDryRun(a) {
			_ = unstructured.SetNestedField(obj.Object, "Bound", "status", "phase")
		}
		return false, nil, nil
	})
}

// scaled follows a replica change of a workload.
func (s *sim) scaled(ctx context.Context, gvr schema.GroupVersionResource, nsName, name string, n int64) {
	time.Sleep(800 * time.Millisecond)
	pods, err := s.dyn.Resource(gvrPod).Namespace(nsName).List(ctx, metav1.ListOptions{})
	if err != nil {
		return
	}
	prefix := name + "-"
	var claims []string
	var node string
	for _, p := range pods.Items {
		if !strings.HasPrefix(p.GetName(), prefix) {
			continue
		}
		node, _, _ = unstructured.NestedString(p.Object, "spec", "nodeName")
		vols, _, _ := unstructured.NestedSlice(p.Object, "spec", "volumes")
		for _, v := range vols {
			if c, _, _ := unstructured.NestedString(v.(map[string]any), "persistentVolumeClaim", "claimName"); c != "" {
				claims = append(claims, c)
			}
		}
		if n == 0 {
			_ = s.dyn.Resource(gvrPod).Namespace(nsName).Delete(ctx, p.GetName(), metav1.DeleteOptions{})
		}
	}
	if n == 0 {
		s.claimsOf[nsName+"/"+name], s.nodeOf[nsName+"/"+name] = claims, node
		for _, c := range claims {
			s.setVolumeState(ctx, nsName, c, "detached", "")
		}
		_ = s.patchUnstructured(ctx, gvr, nsName, name, func(o *unstructured.Unstructured) {
			_ = unstructured.SetNestedField(o.Object, int64(0), "status", "readyReplicas")
		})
		return
	}
	claims, node = s.claimsOf[nsName+"/"+name], s.nodeOf[nsName+"/"+name]
	if len(claims) == 0 {
		return
	}
	owners := owner("apps/v1", "StatefulSet", name)
	podName := name + "-0"
	if gvr == gvrDeployment {
		rs := name + "-" + hex8(name)[:6]
		owners, podName = owner("apps/v1", "ReplicaSet", rs), rs+"-"+hex8(fmt.Sprint(time.Now().UnixNano()))[:5]
	}
	_, _ = s.dyn.Resource(gvrPod).Namespace(nsName).Create(ctx, s.d.pod(nsName, podName, node, claims, owners), metav1.CreateOptions{})
	for _, c := range claims {
		s.setVolumeState(ctx, nsName, c, "attached", node)
	}
	time.Sleep(time.Second)
	_ = s.patchUnstructured(ctx, gvr, nsName, name, func(o *unstructured.Unstructured) {
		_ = unstructured.SetNestedField(o.Object, n, "status", "readyReplicas")
	})
}

func (s *sim) markUnbound(ctx context.Context, vol string) {
	now := time.Now().UTC().Format(time.RFC3339)
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, v := range s.d.volumes {
		if v.Name == vol {
			v.Status.KubernetesStatus.LastPVCRefAt = now
			s.update(ctx, longhorn.GVRVolume, v)
			return
		}
	}
	s.patchStatus(ctx, longhorn.GVRVolume, vol, func(o *unstructured.Unstructured) {
		_ = unstructured.SetNestedField(o.Object, now, "status", "kubernetesStatus", "lastPVCRefAt")
	})
}

func (s *sim) releasePV(ctx context.Context, nsName, claim string) {
	time.Sleep(300 * time.Millisecond)
	pvs, err := s.dyn.Resource(gvrPV).List(ctx, metav1.ListOptions{})
	if err != nil {
		return
	}
	for _, pv := range pvs.Items {
		n, _, _ := unstructured.NestedString(pv.Object, "spec", "claimRef", "namespace")
		c, _, _ := unstructured.NestedString(pv.Object, "spec", "claimRef", "name")
		if n == nsName && c == claim {
			_ = unstructured.SetNestedField(pv.Object, "Released", "status", "phase")
			_, _ = s.dyn.Resource(gvrPV).Update(ctx, &pv, metav1.UpdateOptions{})
			// Longhorn notes that the PVC no longer uses the volume
			vol, _, _ := unstructured.NestedString(pv.Object, "spec", "csi", "volumeHandle")
			s.markUnbound(ctx, vol)
		}
	}
}

// setVolumeState attaches/detaches the Longhorn volume behind a PVC.
func (s *sim) setVolumeState(ctx context.Context, nsName, claim, state, node string) {
	pvc, err := s.dyn.Resource(gvrPVCs).Namespace(nsName).Get(ctx, claim, metav1.GetOptions{})
	if err != nil {
		return
	}
	pvName, _, _ := unstructured.NestedString(pvc.Object, "spec", "volumeName")
	pv, err := s.dyn.Resource(gvrPV).Get(ctx, pvName, metav1.GetOptions{})
	if err != nil {
		return
	}
	vol, _, _ := unstructured.NestedString(pv.Object, "spec", "csi", "volumeHandle")
	rob := "unknown"
	if state == "attached" {
		rob = "healthy"
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, v := range s.d.volumes {
		if v.Name == vol {
			v.Status.State, v.Status.CurrentNodeID, v.Status.Robustness = state, node, rob
			s.update(ctx, longhorn.GVRVolume, v)
			return
		}
	}
	s.patchStatus(ctx, longhorn.GVRVolume, vol, func(o *unstructured.Unstructured) {
		_ = unstructured.SetNestedField(o.Object, state, "status", "state")
		_ = unstructured.SetNestedField(o.Object, node, "status", "currentNodeID")
		_ = unstructured.SetNestedField(o.Object, rob, "status", "robustness")
	})
}

func (s *sim) patchUnstructured(ctx context.Context, g schema.GroupVersionResource, nsName, name string, set func(*unstructured.Unstructured)) error {
	res := s.dyn.Resource(g).Namespace(nsName)
	o, err := res.Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return err
	}
	set(o)
	_, err = res.Update(ctx, o, metav1.UpdateOptions{})
	return err
}

var gvrPVCs = longhorn.GVRPVC
