package longhorn

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ManagedBy names the GitOps/Helm tool that owns an object, "" if none is
// recognisable. A change through lhtx (or the web UI) to such an object is
// reverted on the next reconcile – the preview says so.
func ManagedBy(o metav1.Object) string {
	l, a := o.GetLabels(), o.GetAnnotations()
	switch {
	case l["kustomize.toolkit.fluxcd.io/name"] != "":
		return "Flux Kustomization " + nsName(l["kustomize.toolkit.fluxcd.io/namespace"], l["kustomize.toolkit.fluxcd.io/name"])
	case l["helm.toolkit.fluxcd.io/name"] != "":
		return "Flux HelmRelease " + nsName(l["helm.toolkit.fluxcd.io/namespace"], l["helm.toolkit.fluxcd.io/name"])
	case a["argocd.argoproj.io/tracking-id"] != "":
		app := a["argocd.argoproj.io/tracking-id"]
		for i := 0; i < len(app); i++ {
			if app[i] == ':' {
				app = app[:i]
				break
			}
		}
		return "Argo CD application " + app
	case l["argocd.argoproj.io/instance"] != "":
		return "Argo CD application " + l["argocd.argoproj.io/instance"]
	case l["app.kubernetes.io/managed-by"] == "Helm":
		return "Helm release " + nsName(a["meta.helm.sh/release-namespace"], a["meta.helm.sh/release-name"])
	}
	return ""
}

func nsName(ns, name string) string {
	if ns == "" {
		return name
	}
	return ns + "/" + name
}

// gitopsWarning is added to actions that change or delete such objects.
func gitopsWarning(o metav1.Object) []string {
	by := ManagedBy(o)
	if by == "" {
		return nil
	}
	if len(by) > 4 && by[:4] == "Helm" {
		return []string{"managed by " + by + ": the next helm upgrade overwrites this change – change the chart values instead"}
	}
	return []string{"managed by " + by + ": the next reconcile reverts this change – change it in Git instead (or suspend the sync first)"}
}
