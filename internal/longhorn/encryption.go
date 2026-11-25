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

// Encryption of a source volume, and how the new volume gets its key.
type Encryption struct {
	// Secret the source PV refers to; empty if unknown (no PV any more).
	Secret SecretRef
	// PerPVC: the Secret is named after the PVC (StorageClass with
	// ${pvc.name}).
	PerPVC bool
	// From: the cluster the source key lives in (nil: this one) – a
	// migration restores in another cluster.
	From *Client `json:"-"`

	// how the new PV gets its key (see ResolveKey)
	Mode   KeyMode
	Target SecretRef // KeyExisting: the Secret to use; KeyCopy: optional name
}

// KeyMode: how a restored or cloned encrypted volume gets its key.
type KeyMode string

const (
	// KeyAuto: refer to the source's Secret in the same namespace;
	// anything else needs an explicit choice.
	KeyAuto KeyMode = ""
	// KeyReference: the new PV refers to the source's Secret.
	KeyReference KeyMode = "reference"
	// KeyExisting: a Secret that is already there (created by ESO, Sealed
	// Secrets, by hand …); lhtx checks it holds the same key.
	KeyExisting KeyMode = "existing"
	// KeyCopy: lhtx copies the source's Secret (also across clusters).
	KeyCopy KeyMode = "copy"
)

// KeyPlan is the resolved key handling for the new PV.
type KeyPlan struct {
	Target     SecretRef  // the Secret the new PV refers to
	CopyFrom   *SecretRef // copy this one to Target first
	CopyClient *Client    // the cluster CopyFrom lives in
	Notes      []string   // shown as warnings
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

func secretRefsFor(r SecretRef) map[string]any {
	out := map[string]any{}
	for _, k := range []string{"nodeStageSecretRef", "nodePublishSecretRef", "nodeExpandSecretRef"} {
		out[k] = map[string]any{"name": r.Name, "namespace": r.Namespace}
	}
	return out
}

// EncryptionChecks finds encrypted volumes that could not be attached: their
// PV names no key Secret, or the Secret is gone. Costs nothing without
// encrypted volumes; else one LIST of PVs and one metadata LIST of Secrets
// (names only) per namespace involved.
func (c *Client) EncryptionChecks(ctx context.Context, vols []*Volume) []Finding {
	byPV := map[string]*Volume{}
	for _, v := range vols {
		if v.Spec.Encrypted && v.Status.KubernetesStatus.PVName != "" {
			byPV[v.Status.KubernetesStatus.PVName] = v
		}
	}
	if len(byPV) == 0 {
		return nil
	}
	pvs, err := c.dyn.Resource(GVRPV).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil
	}
	var out []Finding
	need := map[SecretRef][]*Volume{}
	for i := range pvs.Items {
		v := byPV[pvs.Items[i].GetName()]
		if v == nil {
			continue
		}
		m, ok, _ := unstructured.NestedStringMap(pvs.Items[i].Object, "spec", "csi", "nodeStageSecretRef")
		if !ok || m["name"] == "" {
			out = append(out, Finding{Severity: SevWarning, Check: "encryption", Object: volLabel(v),
				Message: "encrypted, but its PV " + pvs.Items[i].GetName() + " names no key Secret",
				Hint:    "the volume cannot be attached; add nodeStageSecretRef/nodePublishSecretRef to the PV"})
			continue
		}
		r := SecretRef{Namespace: m["namespace"], Name: m["name"]}
		need[r] = append(need[r], v)
	}
	have := map[SecretRef]bool{}
	listed := map[string]bool{}
	for r := range need {
		if listed[r.Namespace] {
			continue
		}
		listed[r.Namespace] = true
		if c.meta == nil {
			if m, err := metadata.NewForConfig(c.Cluster.Config); err == nil {
				c.meta = m
			}
		}
		if c.meta == nil {
			return out
		}
		l, err := c.meta.Resource(gvrSecret).Namespace(r.Namespace).List(ctx, metav1.ListOptions{})
		if err != nil {
			return out // not allowed to list: no finding rather than a wrong one
		}
		for _, it := range l.Items {
			have[SecretRef{Namespace: r.Namespace, Name: it.Name}] = true
		}
	}
	for r, vs := range need {
		if have[r] {
			continue
		}
		for _, v := range vs {
			out = append(out, Finding{Severity: SevCritical, Check: "encryption", Object: volLabel(v),
				Message: "its key Secret " + r.String() + " is missing – the volume cannot be attached",
				Hint:    "restore the Secret from your secret backup; without the key the data (and its backups) cannot be read"})
		}
	}
	return out
}
