package longhorn

import (
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestManagedBy(t *testing.T) {
	for _, tc := range []struct {
		labels, ann map[string]string
		want        string
	}{
		{map[string]string{"kustomize.toolkit.fluxcd.io/name": "infra", "kustomize.toolkit.fluxcd.io/namespace": "flux-system"}, nil, "Flux Kustomization flux-system/infra"},
		{map[string]string{"helm.toolkit.fluxcd.io/name": "longhorn", "helm.toolkit.fluxcd.io/namespace": "longhorn-system"}, nil, "Flux HelmRelease longhorn-system/longhorn"},
		{nil, map[string]string{"argocd.argoproj.io/tracking-id": "storage:longhorn.io/RecurringJob:longhorn-system/x"}, "Argo CD application storage"},
		{map[string]string{"argocd.argoproj.io/instance": "storage"}, nil, "Argo CD application storage"},
		{map[string]string{"app.kubernetes.io/managed-by": "Helm"}, map[string]string{"meta.helm.sh/release-name": "lh", "meta.helm.sh/release-namespace": "longhorn-system"}, "Helm release longhorn-system/lh"},
		{map[string]string{"app": "x"}, nil, ""},
	} {
		o := &metav1.ObjectMeta{Labels: tc.labels, Annotations: tc.ann}
		if got := ManagedBy(o); got != tc.want {
			t.Errorf("got %q want %q", got, tc.want)
		}
	}
	j := &RecurringJob{}
	j.Name, j.Labels = "trim", map[string]string{"kustomize.toolkit.fluxcd.io/name": "infra"}
	j.Spec = RecurringJobSpec{Task: "filesystem-trim", Cron: "0 0 * * *", Concurrency: 1}
	d, _ := DeleteJob(j, nil)
	if !strings.Contains(strings.Join(d.Warnings, "\n"), "managed by Flux Kustomization infra") {
		t.Errorf("%v", d.Warnings)
	}
}
