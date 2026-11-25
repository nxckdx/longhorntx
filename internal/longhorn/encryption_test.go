package longhorn

import (
	"context"
	"encoding/base64"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynfake "k8s.io/client-go/dynamic/fake"
	metafake "k8s.io/client-go/metadata/fake"
	"k8s.io/client-go/rest"

	"github.com/nxckdx/longhorntx/internal/kube"
)

func encBackup() *Backup {
	b := &Backup{}
	b.Name = "backup-1"
	b.Status.State, b.Status.URL, b.Status.VolumeSize = "Completed", "nfs://x?backup=1", Quantity(1<<30)
	b.Status.Labels = map[string]string{"volume-encrypted": "enabled"}
	return b
}

func keySecret(ns, name, key string) *unstructured.Unstructured {
	data := map[string]any{}
	for k, v := range map[string]string{"CRYPTO_KEY_VALUE": key, "CRYPTO_KEY_PROVIDER": "secret", "CRYPTO_KEY_CIPHER": "aes-xts-plain64",
		"CRYPTO_KEY_HASH": "sha256", "CRYPTO_KEY_SIZE": "256", "CRYPTO_PBKDF": "argon2i"} {
		data[k] = base64.StdEncoding.EncodeToString([]byte(v))
	}
	return &unstructured.Unstructured{Object: map[string]any{"apiVersion": "v1", "kind": "Secret",
		"metadata": map[string]any{"name": name, "namespace": ns}, "type": "Opaque", "data": data}}
}

// keyClient: a cluster with key Secrets (dynamic for data, metadata for
// names/managers).
func keyClient(ctxName string, secrets ...*unstructured.Unstructured) *Client {
	var objs []runtime.Object
	for _, s := range secrets {
		objs = append(objs, s)
	}
	dyn := dynfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{gvrSecret: "SecretList"}, objs...)
	c := NewClientFrom(&kube.Cluster{Context: ctxName, Config: &rest.Config{}, Traffic: &kube.Counter{}}, DefaultNamespace, dyn, nil)
	sch := metafake.NewTestScheme()
	_ = metav1.AddMetaToScheme(sch)
	var metas []runtime.Object
	for _, s := range secrets {
		metas = append(metas, &metav1.PartialObjectMetadata{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"},
			ObjectMeta: metav1.ObjectMeta{Name: s.GetName(), Namespace: s.GetNamespace(), OwnerReferences: s.GetOwnerReferences()}})
	}
	sch.AddKnownTypeWithName(schema.GroupVersionKind{Version: "v1", Kind: "Secret"}, &metav1.PartialObjectMetadata{})
	sch.AddKnownTypeWithName(schema.GroupVersionKind{Version: "v1", Kind: "SecretList"}, &metav1.PartialObjectMetadataList{})
	c.SetMetadataClient(metafake.NewSimpleMetadataClient(sch, metas...))
	return c
}

func spec(pvcNS, pvcName string, e *Encryption) RestoreSpec {
	return RestoreSpec{Name: "vol-new", Replicas: 2, StorageClass: "longhorn-static", PVCNamespace: pvcNS, PVCName: pvcName, Encryption: e}
}

func TestResolveKey(t *testing.T) {
	ctx := context.Background()
	src := SecretRef{"apps", "vault"}
	c := keyClient("a", keySecret("apps", "vault", "k1"), keySecret("other", "same", "k1"), keySecret("other", "wrong", "k2"))

	// same namespace: refer to the source's Secret, nothing copied
	k, err := c.ResolveKey(ctx, spec("apps", "vault-restore", &Encryption{Secret: src, PerPVC: true}))
	if err != nil || k.Target != src || k.CopyFrom != nil {
		t.Fatalf("same namespace: %+v %v", k, err)
	}
	// another namespace without a choice: an error that names the options
	if _, err := c.ResolveKey(ctx, spec("other", "x", &Encryption{Secret: src})); err == nil ||
		!strings.Contains(err.Error(), "--secret") || !strings.Contains(err.Error(), "--copy-secret") {
		t.Errorf("other namespace accepted without a choice: %v", err)
	}
	// --secret: compared with the source's key
	if k, err := c.ResolveKey(ctx, spec("other", "x", &Encryption{Secret: src, Mode: KeyExisting, Target: SecretRef{"other", "same"}})); err != nil || k.Target.Name != "same" {
		t.Errorf("existing with the same key: %+v %v", k, err)
	}
	if _, err := c.ResolveKey(ctx, spec("other", "x", &Encryption{Secret: src, Mode: KeyExisting, Target: SecretRef{"other", "wrong"}})); err == nil {
		t.Error("a Secret with another key was accepted")
	}
	if _, err := c.ResolveKey(ctx, spec("other", "x", &Encryption{Secret: src, Mode: KeyExisting, Target: SecretRef{"other", "missing"}})); err == nil {
		t.Error("a missing Secret was accepted")
	}
	// --copy-secret: per-PVC Secrets are named after the new PVC
	k, err = c.ResolveKey(ctx, spec("other", "vault-copy", &Encryption{Secret: src, PerPVC: true, Mode: KeyCopy}))
	if err != nil || k.CopyFrom == nil || k.Target != (SecretRef{"other", "vault-copy"}) {
		t.Fatalf("copy: %+v %v", k, err)
	}
	// … an existing target with the same key is reused, another key refused
	if k, err := c.ResolveKey(ctx, spec("other", "same", &Encryption{Secret: src, PerPVC: true, Mode: KeyCopy})); err != nil || k.CopyFrom != nil {
		t.Errorf("copy onto the same key: %+v %v", k, err)
	}
	if _, err := c.ResolveKey(ctx, spec("other", "wrong", &Encryption{Secret: src, PerPVC: true, Mode: KeyCopy})); err == nil {
		t.Error("copy over a different key accepted")
	}
	// the source's key is unknown (volume deleted, only the backup left)
	if _, err := c.ResolveKey(ctx, spec("apps", "x", &Encryption{})); err == nil || !strings.Contains(err.Error(), "--secret") {
		t.Errorf("unknown key: %v", err)
	}
	if k, err := c.ResolveKey(ctx, spec("other", "x", &Encryption{Mode: KeyExisting, Target: SecretRef{"other", "same"}})); err != nil ||
		!strings.Contains(strings.Join(k.Notes, " "), "could not compare") {
		t.Errorf("disaster recovery with --secret: %+v %v", k, err)
	}

	// migration: the key comes from another cluster
	b := keyClient("b")
	k, err = b.ResolveKey(ctx, spec("apps", "vault", &Encryption{Secret: src, From: c, Mode: KeyCopy}))
	if err != nil || k.CopyFrom == nil || k.CopyClient != c {
		t.Fatalf("copy from another cluster: %+v %v", k, err)
	}
	if _, err := b.ResolveKey(ctx, spec("apps", "vault", &Encryption{Secret: src, From: c})); err == nil || !strings.Contains(err.Error(), "another cluster") {
		t.Errorf("migration without a choice: %v", err)
	}
	acts, err := RestoreBackup(encBackup(), nil, RestoreSpec{Name: "vault", Replicas: 1, StorageClass: "longhorn-static",
		PVCNamespace: "apps", PVCName: "vault", Encryption: &Encryption{Secret: src}, Key: k}, func(string) bool { return false })
	if err != nil || len(acts) != 4 {
		t.Fatalf("restore with copy: %v %d", err, len(acts))
	}
	if err := b.Apply(ctx, acts[1], DryRunNone); err != nil {
		t.Fatal(err)
	}
	if fa, _ := c.KeyFingerprint(ctx, src); fa == "" {
		t.Fatal("no fingerprint")
	} else if fb, _ := b.KeyFingerprint(ctx, SecretRef{"apps", "vault"}); fa != fb {
		t.Error("the copied key differs")
	}
}

func TestRestoreEncryptedActions(t *testing.T) {
	none := func(string) bool { return false }
	s := RestoreSpec{Name: "vault-restore", Replicas: 2, StorageClass: "longhorn-static", PVCNamespace: "apps", PVCName: "vault-restore",
		Encryption: &Encryption{Secret: SecretRef{"apps", "vault"}},
		Key:        &KeyPlan{Target: SecretRef{"apps", "vault-restore"}, CopyFrom: &SecretRef{"apps", "vault"}}}
	acts, err := RestoreBackup(encBackup(), nil, s, none)
	if err != nil {
		t.Fatal(err)
	}
	if len(acts) != 4 || acts[1].CopySecretFrom == nil {
		t.Fatalf("want volume, secret copy, PV, PVC: %+v", acts)
	}
	if acts[0].Object["spec"].(map[string]any)["encrypted"] != true {
		t.Error("restored volume not encrypted")
	}
	csi := acts[2].Object["spec"].(map[string]any)["csi"].(map[string]any)
	if r := csi["nodeStageSecretRef"].(map[string]any); r["name"] != "vault-restore" || csi["volumeAttributes"].(map[string]any)["encrypted"] != "true" {
		t.Errorf("PV csi %v", csi)
	}
	if p := acts[1].Preview("x"); strings.Contains(p, "CRYPTO") {
		t.Errorf("preview shows secret data: %s", p)
	}
	// a key that was not resolved is refused
	s.Key = nil
	if _, err := RestoreBackup(encBackup(), nil, s, none); err == nil {
		t.Error("unresolved key accepted")
	}
}
