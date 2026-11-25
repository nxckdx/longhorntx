package longhorn

import (
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestDigestChanged(t *testing.T) {
	v := &Volume{}
	v.Name, v.Namespace, v.ResourceVersion = "vol", DefaultNamespace, "1"
	v.Status.State, v.Status.Robustness, v.Status.ActualSize = "attached", "healthy", Quantity(10<<30)
	v.ManagedFields = nil
	w := *v
	w.ResourceVersion = "2"
	w.Status.ActualSize += 10 << 20 // 10 MiB more: noise
	w.Annotations = map[string]string{"x": "y"}
	if DigestChanged(v, &w) {
		t.Error("10 MiB on 10 GiB and a new resourceVersion must not rewrite the digest")
	}
	w.Status.ActualSize = v.Status.ActualSize + 200<<20 // 200 MiB: notable
	if !DigestChanged(v, &w) {
		t.Error("200 MiB more must rewrite")
	}
	w.Status.ActualSize = v.Status.ActualSize
	w.Status.Robustness = "degraded"
	if !DigestChanged(v, &w) {
		t.Error("robustness change must rewrite")
	}
	small := *v
	small.Status.ActualSize = Quantity(100 << 20)
	s2 := small
	s2.Status.ActualSize = Quantity(102 << 20) // 2 % of a small volume
	if !DigestChanged(&small, &s2) {
		t.Error("2 % must rewrite")
	}
	if !DigestChanged(nil, v) {
		t.Error("no digest yet")
	}

	u, err := DigestObject(&w)
	if err != nil {
		t.Fatal(err)
	}
	back, err := VolumeFromDigest(u)
	if err != nil || back.Status.Robustness != "degraded" || back.Annotations != nil || back.ResourceVersion != "2" {
		t.Errorf("round trip: %+v %v", back, err)
	}
	if refs, _, _ := unstructured.NestedSlice(u.Object, "metadata", "ownerReferences"); len(refs) != 1 {
		t.Error("no owner reference")
	}
}
