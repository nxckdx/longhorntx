package longhorn

import (
	"testing"
	"time"

	"k8s.io/client-go/tools/cache"
)

func TestParseRefresh(t *testing.T) {
	cases := map[string]time.Duration{"": RefreshAuto, "auto": RefreshAuto, "watch": RefreshWatch, "30s": 30 * time.Second, "2m": 2 * time.Minute}
	for in, want := range cases {
		if got, err := ParseRefresh(in); err != nil || got != want {
			t.Errorf("%q: %v %v", in, got, err)
		}
	}
	for _, bad := range []string{"1s", "soon", "-5s"} {
		if _, err := ParseRefresh(bad); err == nil {
			t.Errorf("%q should fail", bad)
		}
	}
}

func TestMergeVolumeListKeepsNewer(t *testing.T) {
	s := &Store{volStore: cache.NewStore(cache.MetaNamespaceKeyFunc)}
	mk := func(name, rv string, size int64) *Volume {
		v := &Volume{}
		v.Name, v.Namespace, v.ResourceVersion = name, "longhorn-system", rv
		v.Status.ActualSize = Quantity(size)
		return v
	}
	// a GET delivered a newer version than the (stale) cached list
	_ = s.volStore.Add(mk("a", "1000", 2))
	s.mergeVolumeList([]any{mk("a", "999", 1), mk("b", "998", 5)}, "1001")
	got, _, _ := s.volStore.GetByKey("longhorn-system/a")
	if got.(*Volume).Status.ActualSize != 2 {
		t.Error("newer cached version was overwritten by an older list")
	}
	if len(s.volStore.List()) != 2 {
		t.Error("list items missing")
	}
	// older cached version is replaced
	s.mergeVolumeList([]any{mk("a", "1002", 3)}, "1003")
	got, _, _ = s.volStore.GetByKey("longhorn-system/a")
	if got.(*Volume).Status.ActualSize != 3 || len(s.volStore.List()) != 1 {
		t.Error("list should win when newer; deleted volumes must disappear")
	}
}
