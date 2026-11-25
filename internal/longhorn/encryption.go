package longhorn

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/metadata"
)

// Encrypted volumes (Longhorn: dm-crypt, key from a Kubernetes Secret named
// in the StorageClass): the Longhorn volume only says "encrypted"; which
// Secret holds the key is recorded on the PV (spec.csi.nodeStageSecretRef
// etc.). A StorageClass can use one Secret for all volumes or one per PVC
// ("${pvc.name}" / "${pvc.namespace}"). Restores, clones and switches must
// carry this over, or the new volume cannot be attached.

// SecretRef names a Secret.
type SecretRef struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
}

func (r SecretRef) String() string { return r.Namespace + "/" + r.Name }

// Encryption of a volume as lhtx found it.
type Encryption struct {
	// Secret the PV refers to; empty if unknown (no PV any more).
	Secret SecretRef
	// PerPVC: the Secret is named after the PVC (StorageClass with
	// ${pvc.name}) – a new PVC name needs a copy of the Secret.
	PerPVC bool
}

// pvSecretKeys are the CSI secret references a Longhorn PV can have.
var pvSecretKeys = []string{"nodeStageSecretRef", "nodePublishSecretRef", "nodeExpandSecretRef", "controllerExpandSecretRef"}

// PVSecretRefs returns the CSI secret references of a PV (for copying to a
// new PV); nil if it has none.
func PVSecretRefs(pv *unstructured.Unstructured) map[string]any {
	out := map[string]any{}
	for _, k := range pvSecretKeys {
		if m, ok, _ := unstructured.NestedMap(pv.Object, "spec", "csi", k); ok && m["name"] != nil {
			out[k] = m
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// VolumeEncryption reads how a volume is encrypted: nil if it is not.
func (c *Client) VolumeEncryption(ctx context.Context, v *Volume) (*Encryption, error) {
	if !v.Spec.Encrypted {
		return nil, nil
	}
	e := &Encryption{}
	ks := v.Status.KubernetesStatus
	if ks.PVName == "" {
		return e, nil
	}
	pv, err := c.dyn.Resource(GVRPV).Get(ctx, ks.PVName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return e, nil
	}
	if err != nil {
		return nil, fmt.Errorf("PV %s of encrypted volume %s: %w", ks.PVName, v.Name, err)
	}
	if m, ok, _ := unstructured.NestedStringMap(pv.Object, "spec", "csi", "nodeStageSecretRef"); ok {
		e.Secret = SecretRef{Namespace: m["namespace"], Name: m["name"]}
	}
	e.PerPVC = e.Secret.Name != "" && e.Secret.Name == ks.PVCName && e.Secret.Namespace == ks.Namespace
	return e, nil
}

// SecretExists checks a Secret by name with the metadata API (no data).
func (c *Client) SecretExists(ctx context.Context, r SecretRef) (bool, error) {
	if c.meta == nil {
		m, err := metadata.NewForConfig(c.Cluster.Config)
		if err != nil {
			return false, err
		}
		c.meta = m
	}
	_, err := c.meta.Resource(gvrSecret).Namespace(r.Namespace).Get(ctx, r.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	return err == nil, err
}

// keySecretFor decides which Secret the PV of a new PVC refers to: the
// same one, or – one Secret per PVC – one named after the new PVC, copied
// from the source's if needed.
func keySecretFor(e *Encryption, pvcNamespace, pvcName string) (target SecretRef, copyFrom *SecretRef) {
	if !e.PerPVC {
		return e.Secret, nil
	}
	target = SecretRef{Namespace: pvcNamespace, Name: pvcName}
	if target != e.Secret {
		src := e.Secret
		copyFrom = &src
	}
	return target, copyFrom
}

func secretRefsFor(r SecretRef) map[string]any {
	out := map[string]any{}
	for _, k := range []string{"nodeStageSecretRef", "nodePublishSecretRef", "nodeExpandSecretRef"} {
		out[k] = map[string]any{"name": r.Name, "namespace": r.Namespace}
	}
	return out
}
