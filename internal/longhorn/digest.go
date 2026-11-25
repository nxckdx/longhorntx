package longhorn

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// Volume digests (written by the lhtx agent): on a busy cluster every write
// to a volume changes status.actualSize, and each change sends the whole
// Volume (~6 KiB plus managedFields) to every watcher – the bulk of lhtx's
// traffic on large clusters. The agent watches the volumes inside the
// cluster and keeps one small VolumeDigest per volume: the fields lhtx
// models, as one JSON string (so managedFields stay tiny), rewritten only
// when something lhtx shows changes – state, robustness, replicas, node, … –
// or the actual size moved by more than 1 % / 64 MiB. lhtx then watches the
// digests instead of the volumes. Access is plain RBAC on the digests; there
// is no endpoint of the agent involved.

// GVRVolumeDigest: lhtx.io/v1alpha1 volumedigests, in the Longhorn namespace,
// named like the volume.
var GVRVolumeDigest = schema.GroupVersionResource{Group: "lhtx.io", Version: "v1alpha1", Resource: "volumedigests"}

// DigestSizeStep: actual size changes below this (and below 1 %) are not
// written to the digest.
const DigestSizeStep = 64 << 20

const digestGzipPrefix = "gz:"

// Slim keeps only what a digest carries: the modeled fields, metadata
// without annotations, managedFields, owners and finalizers.
func Slim(v *Volume) *Volume {
	c := *v
	c.ObjectMeta = metav1.ObjectMeta{Name: v.Name, Namespace: v.Namespace, UID: v.UID,
		ResourceVersion: v.ResourceVersion, Labels: v.Labels, CreationTimestamp: v.CreationTimestamp,
		DeletionTimestamp: v.DeletionTimestamp}
	return &c
}

// DigestChanged reports whether the digest of old must be rewritten for
// new: anything modeled changed, or the actual size moved notably.
func DigestChanged(old, new *Volume) bool {
	if old == nil {
		return true
	}
	a, b := *Slim(old), *Slim(new)
	a.ResourceVersion, b.ResourceVersion = "", ""
	sa, sb := a.Status.ActualSize, b.Status.ActualSize
	a.Status.ActualSize, b.Status.ActualSize = 0, 0
	ja, _ := json.Marshal(a)
	jb, _ := json.Marshal(b)
	if string(ja) != string(jb) {
		return true
	}
	d := math.Abs(float64(sb) - float64(sa))
	return d >= DigestSizeStep || (sa > 0 && d/float64(sa) >= 0.01)
}

// DigestObject builds the VolumeDigest for a volume (owned by it, so it
// goes when the volume goes).
func DigestObject(v *Volume) (*unstructured.Unstructured, error) {
	raw, err := json.Marshal(Slim(v))
	if err != nil {
		return nil, err
	}
	// gzip + base64: watch events are not compressed by the API server,
	// and the JSON in a string would be escaped (~45 % smaller this way)
	var buf bytes.Buffer
	zw, _ := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	_, _ = zw.Write(raw)
	_ = zw.Close()
	data := digestGzipPrefix + base64.StdEncoding.EncodeToString(buf.Bytes())
	f := false
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "lhtx.io/v1alpha1", "kind": "VolumeDigest",
		"metadata": map[string]any{"name": v.Name, "namespace": v.Namespace,
			"labels": map[string]any{"app.kubernetes.io/managed-by": "lhtx-agent"},
			"ownerReferences": []any{map[string]any{"apiVersion": Group + "/" + Version, "kind": "Volume",
				"name": v.Name, "uid": string(v.UID), "blockOwnerDeletion": f}}},
		"data": data,
	}}, nil
}

// ConvertVolume converts a Volume object into the slim struct.
func ConvertVolume(u *unstructured.Unstructured) (*Volume, error) { return convert[Volume](u) }

// VolumeFromDigest reads the volume out of a VolumeDigest.
func VolumeFromDigest(u *unstructured.Unstructured) (*Volume, error) {
	data, _, _ := unstructured.NestedString(u.Object, "data")
	raw := []byte(data)
	if b64, ok := strings.CutPrefix(data, digestGzipPrefix); ok {
		z, err := base64.StdEncoding.DecodeString(b64)
		if err != nil {
			return nil, fmt.Errorf("VolumeDigest %s: %w", u.GetName(), err)
		}
		zr, err := gzip.NewReader(bytes.NewReader(z))
		if err != nil {
			return nil, fmt.Errorf("VolumeDigest %s: %w", u.GetName(), err)
		}
		if raw, err = io.ReadAll(zr); err != nil {
			return nil, fmt.Errorf("VolumeDigest %s: %w", u.GetName(), err)
		}
	}
	v := &Volume{}
	if err := json.Unmarshal(raw, v); err != nil {
		return nil, fmt.Errorf("VolumeDigest %s: %w", u.GetName(), err)
	}
	return v, nil
}
