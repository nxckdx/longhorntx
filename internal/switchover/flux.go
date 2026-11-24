// Package switchover switches a PVC to another Longhorn volume (usually one
// restored from a backup): pause the GitOps objects that manage the
// workload, scale it down, re-create the PVC bound to the new volume, scale
// up, resume. The old volume is kept for a rollback (switch back to it).
package switchover

import (
	"context"
	"fmt"
	"sort"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
)

var (
	GVRKustomization = schema.GroupVersionResource{Group: "kustomize.toolkit.fluxcd.io", Version: "v1", Resource: "kustomizations"}
	GVRHelmRelease   = schema.GroupVersionResource{Group: "helm.toolkit.fluxcd.io", Version: "v2", Resource: "helmreleases"}
)

// FluxRef is a Flux object that reconciles a workload or PVC.
type FluxRef struct {
	Kind      string `json:"kind"` // Kustomization | HelmRelease
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	// WasSuspended: already suspended before the switch – stays suspended.
	WasSuspended bool `json:"wasSuspended"`
}

func (f FluxRef) String() string { return f.Kind + " " + f.Namespace + "/" + f.Name }

func (f FluxRef) gvr() schema.GroupVersionResource {
	if f.Kind == "HelmRelease" {
		return GVRHelmRelease
	}
	return GVRKustomization
}

// fluxOwners reads the labels Flux puts on everything it applies.
func fluxOwners(labels map[string]string) []FluxRef {
	var out []FluxRef
	if n := labels["kustomize.toolkit.fluxcd.io/name"]; n != "" {
		out = append(out, FluxRef{Kind: "Kustomization", Namespace: labels["kustomize.toolkit.fluxcd.io/namespace"], Name: n})
	}
	if n := labels["helm.toolkit.fluxcd.io/name"]; n != "" {
		out = append(out, FluxRef{Kind: "HelmRelease", Namespace: labels["helm.toolkit.fluxcd.io/namespace"], Name: n})
	}
	return out
}

// unsupportedOwner names GitOps tools lhtx cannot pause yet.
func unsupportedOwner(labels, ann map[string]string) string {
	switch {
	case ann["argocd.argoproj.io/tracking-id"] != "" || labels["argocd.argoproj.io/instance"] != "":
		return "Argo CD (not supported yet: disable auto-sync yourself first)"
	case labels["resourceset.fluxcd.controlplane.io/name"] != "":
		return "a Flux Operator ResourceSet (not supported yet)"
	}
	return ""
}

func addFlux(set map[string]FluxRef, refs ...FluxRef) {
	for _, r := range refs {
		set[r.String()] = r
	}
}

func sortedFlux(set map[string]FluxRef) []FluxRef {
	out := make([]FluxRef, 0, len(set))
	for _, r := range set {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].String() < out[j].String() })
	return out
}

// readSuspended reports spec.suspend of a Flux object.
func readSuspended(ctx context.Context, dyn dynamic.Interface, f FluxRef) (bool, error) {
	u, err := dyn.Resource(f.gvr()).Namespace(f.Namespace).Get(ctx, f.Name, metav1.GetOptions{})
	if err != nil {
		return false, fmt.Errorf("%s: %w", f, err)
	}
	s, _, _ := unstructured.NestedBool(u.Object, "spec", "suspend")
	return s, nil
}

// setSuspend pauses (true) or resumes (false) a Flux object, like
// `flux suspend|resume`. Resume removes the field.
func setSuspend(ctx context.Context, dyn dynamic.Interface, f FluxRef, suspend bool) error {
	data := []byte(`{"spec":{"suspend":true}}`)
	if !suspend {
		// like `flux resume`: drop the flag and ask for a reconcile now
		data = fmt.Appendf(nil, `{"metadata":{"annotations":{"reconcile.fluxcd.io/requestedAt":%q}},"spec":{"suspend":null}}`,
			time.Now().UTC().Format(time.RFC3339Nano))
	}
	_, err := dyn.Resource(f.gvr()).Namespace(f.Namespace).Patch(ctx, f.Name, types.MergePatchType, data, metav1.PatchOptions{FieldManager: "lhtx"})
	if err != nil {
		return fmt.Errorf("%s: %w", f, err)
	}
	return nil
}
