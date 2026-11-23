package longhorn

import "sort"

// SnapLine is one snapshot in tree order (newest first).
type SnapLine struct {
	Snap  *VolumeSnapshot
	Depth int  // 0 = the chain leading to volume-head, >0 = branch (after a revert)
	Head  bool // parent of volume-head: the data the volume currently builds on
}

// SnapshotTree orders snapshots like the chain on disk: the line that leads
// to volume-head comes first, newest at the top. Children that are not on
// that line (a revert started a new chain) are shown as indented branches
// above the snapshot they fork from. Snapshots whose parent is missing come
// last at depth 0.
func SnapshotTree(snaps []*VolumeSnapshot) []SnapLine {
	by := map[string]*VolumeSnapshot{}
	for _, s := range snaps {
		by[s.Name] = s
	}
	children := func(s *VolumeSnapshot) []*VolumeSnapshot {
		var out []*VolumeSnapshot
		for c := range s.Status.Children {
			if cs := by[c]; cs != nil {
				out = append(out, cs)
			}
		}
		sort.Slice(out, func(i, j int) bool { return newer(out[i], out[j]) })
		return out
	}
	// tip: follow the newest child down to the end of a chain
	tip := func(s *VolumeSnapshot) *VolumeSnapshot {
		for seen := map[string]bool{}; !seen[s.Name]; {
			seen[s.Name] = true
			cs := children(s)
			if len(cs) == 0 {
				break
			}
			s = cs[0]
		}
		return s
	}

	var out []SnapLine
	done := map[string]bool{}
	var emit func(t, stop *VolumeSnapshot, depth int)
	emit = func(t, stop *VolumeSnapshot, depth int) {
		var prev *VolumeSnapshot
		for n := t; n != nil && n != stop && !done[n.Name]; n = by[n.Status.Parent] {
			done[n.Name] = true
			for _, c := range children(n) {
				if c != prev && !done[c.Name] {
					emit(tip(c), n, depth+1)
				}
			}
			out = append(out, SnapLine{Snap: n, Depth: depth, Head: n.Status.Children["volume-head"]})
			prev = n
		}
	}

	sorted := append([]*VolumeSnapshot(nil), snaps...)
	sort.Slice(sorted, func(i, j int) bool { return newer(sorted[i], sorted[j]) })
	var head *VolumeSnapshot
	for _, s := range sorted {
		if s.Status.Children["volume-head"] {
			head = s
			break
		}
	}
	if head != nil {
		emit(head, nil, 0)
	}
	// everything not reachable from the head line (or no head known)
	for _, s := range sorted {
		if !done[s.Name] {
			emit(tip(s), nil, 0)
		}
	}
	return out
}

func newer(a, b *VolumeSnapshot) bool {
	if a.Status.CreationTime != b.Status.CreationTime {
		return a.Status.CreationTime > b.Status.CreationTime
	}
	return a.Name > b.Name
}

// Prefix draws the branch depth ("│ " per level).
func (l SnapLine) Prefix() string {
	s := ""
	for i := 0; i < l.Depth; i++ {
		s += "│ "
	}
	return s
}

// Branched reports whether the tree has branches (reverts).
func Branched(lines []SnapLine) bool {
	for _, l := range lines {
		if l.Depth > 0 {
			return true
		}
	}
	return false
}
