package deploy

import (
	"os"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

func TestManifests(t *testing.T) {
	ro := Manifests("1.2.3", "storage", false)
	if strings.Contains(ro, "args: [agent, --rw") || strings.Contains(ro, "name: lhtx-agent-rw") || strings.Contains(ro, "pods/exec") || strings.Contains(ro, "secrets") {
		t.Error("the read-only manifests carry write rights")
	}
	out := Manifests("1.2.3", "storage", true)
	if !strings.Contains(out, "args: [agent, --rw, --namespace=storage]") {
		t.Error("--rw missing")
	}
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
	for k, n := range map[string]int{"CustomResourceDefinition": 5, "ServiceAccount": 1, "ClusterRole": 6, "ValidatingAdmissionPolicy": 1, "ValidatingAdmissionPolicyBinding": 1,
		"ClusterRoleBinding": 2, "Role": 2, "RoleBinding": 2, "Deployment": 1, "Service": 1} {
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

func TestChartSwitchCRD(t *testing.T) {
	b, err := os.ReadFile("../charts/lhtx-agent/templates/crd-switch.yaml")
	if err != nil {
		t.Fatal(err)
	}
	chart := strings.Replace(string(b), "  annotations:\n    helm.sh/resource-policy: keep\n", "", 1)
	chart = strings.TrimPrefix(chart, "{{- if .Values.crds.install }}\n")
	chart = strings.TrimSuffix(chart, "{{- end }}\n")
	crdDoc := strings.SplitN(switchRes, "\n---\n", 2)[0]
	want := crdDoc[strings.Index(crdDoc, "apiVersion:"):] + "\n"
	if chart != want {
		t.Error("charts/lhtx-agent/templates/crd-switch.yaml differs from the CRD in deploy/switch.yaml")
	}
}

func TestChartDigestCRD(t *testing.T) {
	b, err := os.ReadFile("../charts/lhtx-agent/templates/crd-digest.yaml")
	if err != nil {
		t.Fatal(err)
	}
	chart := strings.Replace(string(b), "  annotations:\n    helm.sh/resource-policy: keep\n", "", 1)
	chart = strings.TrimPrefix(chart, "{{- if .Values.crds.install }}\n")
	chart = strings.TrimSuffix(chart, "{{- end }}\n")
	crdDoc := strings.SplitN(digestRes, "\n---\n", 2)[0]
	want := crdDoc[strings.Index(crdDoc, "apiVersion:"):] + "\n"
	if chart != want {
		t.Error("charts/lhtx-agent/templates/crd-digest.yaml differs from the CRD in deploy/digest.yaml")
	}
}

func TestChartHistoryCRD(t *testing.T) {
	b, err := os.ReadFile("../charts/lhtx-agent/templates/crd-history.yaml")
	if err != nil {
		t.Fatal(err)
	}
	chart := strings.Replace(string(b), "  annotations:\n    helm.sh/resource-policy: keep\n", "", 1)
	chart = strings.TrimPrefix(chart, "{{- if .Values.crds.install }}\n")
	chart = strings.TrimSuffix(chart, "{{- end }}\n")
	var crdDoc string
	for _, d := range strings.Split(digestRes, "\n---\n") {
		if strings.Contains(d, "volumehistories.lhtx.io") {
			crdDoc = d
		}
	}
	want := crdDoc[strings.Index(crdDoc, "apiVersion:"):] + "\n"
	if chart != want {
		t.Error("charts/lhtx-agent/templates/crd-history.yaml differs from the CRD in deploy/digest.yaml")
	}
}

func TestChartLogQueryCRD(t *testing.T) {
	b, err := os.ReadFile("../charts/lhtx-agent/templates/crd-logquery.yaml")
	if err != nil {
		t.Fatal(err)
	}
	chart := strings.Replace(string(b), "  annotations:\n    helm.sh/resource-policy: keep\n", "", 1)
	chart = strings.TrimPrefix(chart, "{{- if .Values.crds.install }}\n")
	chart = strings.TrimSuffix(chart, "{{- end }}\n")
	crdDoc := strings.SplitN(logQueryRes, "\n---\n", 2)[0]
	want := crdDoc[strings.Index(crdDoc, "apiVersion:"):] + "\n"
	if chart != want {
		t.Error("charts/lhtx-agent/templates/crd-logquery.yaml differs from the CRD in deploy/logquery.yaml")
	}
}
