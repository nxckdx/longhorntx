package longhorn

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/metadata"
)

// Keys of encrypted volumes, handled the same way whoever manages the
// Secret (by hand, External Secrets, Sealed Secrets, a policy engine, …):
// lhtx does not need to know the manager. It compares fingerprints – a
// hash of the key and its cipher parameters, kept in memory and never
// shown or stored – to make sure a new volume gets the same key, however
// the key got there.

// keyFields make up a Longhorn key (CRYPTO_KEY_PROVIDER is always "secret").
var keyFields = []string{"CRYPTO_KEY_VALUE", "CRYPTO_KEY_CIPHER", "CRYPTO_KEY_HASH", "CRYPTO_KEY_SIZE", "CRYPTO_PBKDF"}

// LabelKeyBackup marks copies of keys lhtx kept because a switch saw the
// key Secret replaced (lhtx doctor reports them).
const LabelKeyBackup = "lhtx.io/key-backup"

// ErrNoSecret: the Secret does not exist.
var ErrNoSecret = errors.New("secret not found")

// KeyFingerprint reads a key Secret and returns a hash of the key and its
// parameters. Needs the right to read the Secret.
func (c *Client) KeyFingerprint(ctx context.Context, r SecretRef) (string, error) {
	u, err := c.dyn.Resource(gvrSecret).Namespace(r.Namespace).Get(ctx, r.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return "", ErrNoSecret
	}
	if err != nil {
		return "", err
	}
	return Fingerprint(u)
}

// Fingerprint hashes the key and its parameters of a key Secret object.
func Fingerprint(u *unstructured.Unstructured) (string, error) {
	data, _, _ := unstructured.NestedStringMap(u.Object, "data")
	if data["CRYPTO_KEY_VALUE"] == "" {
		return "", fmt.Errorf("Secret %s/%s has no CRYPTO_KEY_VALUE – not a Longhorn key", u.GetNamespace(), u.GetName())
	}
	h := sha256.New()
	for _, k := range keyFields {
		v, err := base64.StdEncoding.DecodeString(data[k])
		if err != nil {
			return "", fmt.Errorf("Secret %s/%s: %s: %w", u.GetNamespace(), u.GetName(), k, err)
		}
		h.Write([]byte(k + "\x00"))
		h.Write(v)
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func (c *Client) metaClient() (metadata.Interface, error) {
	if c.meta == nil {
		m, err := metadata.NewForConfig(c.Cluster.Config)
		if err != nil {
			return nil, err
		}
		c.meta = m
	}
	return c.meta, nil
}

// SecretManager tells who manages a Secret, from its metadata only: its
// controller (ownerReference), else the field manager that wrote it last
// if that is not a person's tool. "" = nobody in particular.
func (c *Client) SecretManager(ctx context.Context, r SecretRef) string {
	m, err := c.metaClient()
	if err != nil {
		return ""
	}
	o, err := m.Resource(gvrSecret).Namespace(r.Namespace).Get(ctx, r.Name, metav1.GetOptions{})
	if err != nil {
		return ""
	}
	return managerOf(o.OwnerReferences, o.ManagedFields)
}

// managerOf: the controller owning an object, else the last field manager
// that is not a person's tool.
func managerOf(owners []metav1.OwnerReference, fields []metav1.ManagedFieldsEntry) string {
	for _, ref := range owners {
		if ref.Controller != nil && *ref.Controller {
			return ref.Kind + " " + ref.Name
		}
	}
	mgr := ""
	for _, f := range fields {
		if !personTool(f.Manager) {
			mgr = f.Manager
		}
	}
	if mgr != "" {
		mgr = "(last written by) " + mgr
	}
	return mgr
}

func personTool(m string) bool {
	for _, p := range []string{"kubectl", "lhtx", "helm", "k9s", "kustomize", "Mozilla", "rancher", "lens"} {
		if strings.HasPrefix(m, p) {
			return true
		}
	}
	return false
}

// ResolveKey decides which Secret the PV of a restored/cloned encrypted
// volume refers to and checks that it holds the source's key:
//
//   - same namespace, same cluster: refer to the source's Secret
//   - --secret (KeyExisting): a Secret already there – compared with the
//     source's key when both can be read
//   - --copy-secret (KeyCopy): copy the source's Secret (also from another
//     cluster); an existing target is reused only if it holds the same key
//
// Anything else (another namespace or cluster without a choice) is an error
// that names the options.
func (c *Client) ResolveKey(ctx context.Context, s RestoreSpec) (*KeyPlan, error) {
	e := s.Encryption
	if e == nil || s.PVCName == "" {
		return nil, nil
	}
	src := e.Secret
	srcC := c
	if e.From != nil {
		srcC = e.From
	}
	same := e.From == nil
	kp := &KeyPlan{}
	mode := e.Mode
	if mode == KeyAuto {
		switch {
		case e.Target.Name != "":
			mode = KeyExisting
		case same && src.Name != "" && src.Namespace == s.PVCNamespace:
			mode = KeyReference
		case src.Name == "":
			return nil, fmt.Errorf("the source is encrypted and lhtx cannot tell which Secret holds its key – the source volume " +
				"is in another cluster (--key-from-context CONTEXT, with --copy-secret or --secret) or its PV is gone " +
				"(--secret NAMESPACE/NAME with the key from your secret backup)")
		default:
			where := "another namespace"
			if !same {
				where = "another cluster"
			}
			return nil, fmt.Errorf("the source is encrypted and the PVC goes to %s: either --secret NAMESPACE/NAME "+
				"(a Secret there that holds the same key – from your secret manager; lhtx compares it) or --copy-secret "+
				"(lhtx copies %s)", where, src)
		}
	}
	switch mode {
	case KeyReference:
		if !same || src.Name == "" {
			return nil, fmt.Errorf("cannot refer to the source's key Secret: it is unknown or in another cluster")
		}
		kp.Target = src
		if src.Namespace != s.PVCNamespace {
			kp.Notes = append(kp.Notes, fmt.Sprintf("the new PV uses the key Secret %s in another namespace – deleting that namespace or Secret makes the volume unusable", src))
		} else {
			kp.Notes = append(kp.Notes, fmt.Sprintf("encrypted like the source: the new PV uses the same key Secret %s", src))
		}
	case KeyExisting:
		kp.Target = e.Target
		fpT, err := c.KeyFingerprint(ctx, kp.Target)
		if errors.Is(err, ErrNoSecret) {
			return nil, fmt.Errorf("key Secret %s does not exist", kp.Target)
		}
		if err != nil {
			return nil, err
		}
		switch fpS, err := srcKey(ctx, srcC, src); {
		case err != nil:
			kp.Notes = append(kp.Notes, fmt.Sprintf("uses key Secret %s – could not compare it with the source's key (%v): a wrong key does not harm the data, the volume just cannot be attached", kp.Target, err))
		case fpS != fpT:
			return nil, fmt.Errorf("key Secret %s holds a different key than the source's %s – the restored volume could not be opened", kp.Target, src)
		default:
			kp.Notes = append(kp.Notes, fmt.Sprintf("uses key Secret %s – same key as the source's %s (compared)", kp.Target, src))
		}
	case KeyCopy:
		if src.Name == "" {
			return nil, fmt.Errorf("--copy-secret: the source's key Secret is unknown – name it with --key-source NAMESPACE/NAME")
		}
		kp.Target = e.Target
		if kp.Target.Name == "" {
			kp.Target = SecretRef{Namespace: s.PVCNamespace, Name: src.Name}
			if e.PerPVC {
				kp.Target.Name = s.PVCName
			}
		}
		fpS, err := srcKey(ctx, srcC, src)
		if err != nil {
			return nil, fmt.Errorf("read the source's key: %w", err)
		}
		switch fpT, err := c.KeyFingerprint(ctx, kp.Target); {
		case errors.Is(err, ErrNoSecret):
			if same && kp.Target == src {
				return nil, fmt.Errorf("cannot copy %s onto itself", src)
			}
			kp.CopyFrom, kp.CopyClient = &src, srcC
			kp.Notes = append(kp.Notes, fmt.Sprintf("copies the key Secret %s to %s", src, kp.Target))
			if m := srcC.SecretManager(ctx, src); m != "" {
				kp.Notes = append(kp.Notes, fmt.Sprintf("%s is managed by %s – the copy is not: no rotation, not in your secret source; better create it with the same manager and use --secret", src, m))
			}
		case err != nil:
			return nil, err
		case fpT != fpS:
			return nil, fmt.Errorf("Secret %s exists with a different key – choose another name or remove it", kp.Target)
		default:
			kp.Notes = append(kp.Notes, fmt.Sprintf("key Secret %s already holds the source's key – used as is", kp.Target))
		}
	default:
		return nil, fmt.Errorf("unknown key mode %q", mode)
	}
	return kp, nil
}

func srcKey(ctx context.Context, c *Client, r SecretRef) (string, error) {
	if r.Name == "" {
		return "", fmt.Errorf("the source's key Secret is unknown")
	}
	return c.KeyFingerprint(ctx, r)
}
