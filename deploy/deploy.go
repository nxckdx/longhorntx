// Package deploy holds the lhtx agent's Kubernetes manifests; lhtx agent
// manifests prints them.
package deploy

import (
	_ "embed"
	"strings"
)

var (
	//go:embed crd.yaml
	crd string
	//go:embed switch.yaml
	switchRes string
	//go:embed digest.yaml
	digestRes string
	//go:embed logquery.yaml
	logQueryRes string
	//go:embed rbac.yaml
	rbac string
	//go:embed agent.yaml
	agent string
)

// Manifests returns all objects as one YAML stream, with the image tag and
// the namespace (where the agent runs: the Longhorn namespace) filled in.
func Manifests(tag, namespace string) string {
	out := strings.Join([]string{crd, switchRes, digestRes, logQueryRes, rbac, agent}, "---\n")
	if tag != "" {
		out = strings.ReplaceAll(out, "ghcr.io/nxckdx/lhtx:latest", "ghcr.io/nxckdx/lhtx:"+tag)
	}
	if namespace != "" && namespace != "longhorn-system" {
		out = strings.ReplaceAll(out, "namespace: longhorn-system", "namespace: "+namespace)
		out = strings.ReplaceAll(out, "--namespace=longhorn-system", "--namespace="+namespace)
	}
	return out
}
