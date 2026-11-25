package deploy

import (
	"os"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

func TestManifests(t *testing.T) {
	out := Manifests("1.2.3", "storage")
	kinds := map[string]int{}
	for _, doc := range strings.Split(out, "\n---\n") {
		var o struct {
			Kind     string `json:"kind"`
			Metadata struct {
				Name      string `json:"name"`
				Namespace string `json:"namespace"`
			} `json:"metadata"`
		}
		if err := yaml.Unmarshal([]byte(doc), &o); err != nil {
			t.Fatalf("%v in\n%s", err, doc)
		}
		if o.Kind == "" || o.Metadata.Name == "" {
			t.Fatalf("no kind/name:\n%s", doc)
		}
		if o.Metadata.Namespace != "" && o.Metadata.Namespace != "storage" {
			t.Errorf("%s %s in %s", o.Kind, o.Metadata.Name, o.Metadata.Namespace)
		}
		kinds[o.Kind]++
	}
	for k, n := range map[string]int{"CustomResourceDefinition": 1, "ServiceAccount": 1, "ClusterRole": 2,
		"ClusterRoleBinding": 1, "Role": 2, "RoleBinding": 2, "Deployment": 1} {
		if kinds[k] != n {
			t.Errorf("%d %s, want %d", kinds[k], k, n)
		}
	}
	if !strings.Contains(out, "ghcr.io/nxckdx/lhtx:1.2.3") || strings.Contains(out, "longhorn-system") {
		t.Error("tag or namespace not filled in")
	}
}

// The Helm chart carries the same CRD (wrapped in an if, with a keep
// annotation).
func TestChartCRD(t *testing.T) {
	b, err := os.ReadFile("../charts/lhtx-agent/templates/crd.yaml")
	if err != nil {
		t.Fatal(err)
	}
	chart := strings.Replace(string(b), "  annotations:\n    helm.sh/resource-policy: keep\n", "", 1)
	chart = strings.TrimPrefix(chart, "{{- if .Values.crds.install }}\n")
	chart = strings.TrimSuffix(chart, "{{- end }}\n")
	want := crd[strings.Index(crd, "apiVersion:"):]
	if chart != want {
		t.Error("charts/lhtx-agent/templates/crd.yaml differs from deploy/crd.yaml – copy it over")
	}
}
