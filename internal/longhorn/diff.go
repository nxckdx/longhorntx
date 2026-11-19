package longhorn

import (
	"fmt"
	"reflect"
	"sort"
)

// ChangedPaths returns the leaf paths that differ between two versions of an
// object (as decoded JSON). List indexes are folded to [*] so that changes
// aggregate across objects (e.g. metadata.managedFields[*].time).
// metadata.resourceVersion and metadata.generation are ignored: they change
// on every update by definition.
func ChangedPaths(a, b map[string]any) []string {
	set := map[string]bool{}
	diffValue("", a, b, set)
	delete(set, "metadata.resourceVersion")
	delete(set, "metadata.generation")
	out := make([]string, 0, len(set))
	for p := range set {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

func diffValue(path string, a, b any, set map[string]bool) {
	switch av := a.(type) {
	case map[string]any:
		bv, ok := b.(map[string]any)
		if !ok {
			set[path] = true
			return
		}
		for k, x := range av {
			diffValue(join(path, k), x, bv[k], set)
		}
		for k, y := range bv {
			if _, ok := av[k]; !ok {
				diffValue(join(path, k), nil, y, set)
			}
		}
	case []any:
		bv, ok := b.([]any)
		if !ok || len(av) != len(bv) {
			if !reflect.DeepEqual(a, b) {
				set[path+"[*]"] = true
			}
			return
		}
		for i := range av {
			diffValue(path+"[*]", av[i], bv[i], set)
		}
	default:
		if !reflect.DeepEqual(a, b) {
			// a new/removed subtree counts as one change at its root
			set[path] = true
		}
	}
}

func join(path, k string) string {
	if path == "" {
		return k
	}
	return path + "." + k
}

// FieldStats aggregates ChangedPaths over many events.
type FieldStats struct {
	Updates int            // modified events compared
	Count   map[string]int // path -> number of updates that changed it
}

func (f *FieldStats) Add(paths []string) {
	if f.Count == nil {
		f.Count = map[string]int{}
	}
	f.Updates++
	for _, p := range paths {
		f.Count[p]++
	}
}

// Top returns the n most frequently changed paths as "path (xx%)".
func (f *FieldStats) Top(n int) []string {
	type kv struct {
		k string
		v int
	}
	var all []kv
	for k, v := range f.Count {
		all = append(all, kv{k, v})
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].v != all[j].v {
			return all[i].v > all[j].v
		}
		return all[i].k < all[j].k
	})
	var out []string
	for i := 0; i < len(all) && i < n; i++ {
		out = append(out, fmt.Sprintf("%-60s %5d  %3d%%", all[i].k, all[i].v, all[i].v*100/max(f.Updates, 1)))
	}
	return out
}
