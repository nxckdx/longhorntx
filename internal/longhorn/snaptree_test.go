package longhorn

import (
	"fmt"
	"strings"
	"testing"
)

func snap(name, parent, created string, children ...string) *VolumeSnapshot {
	s := &VolumeSnapshot{}
	s.Name, s.Status.Parent, s.Status.CreationTime = name, parent, created
	s.Status.Children = map[string]bool{}
	for _, c := range children {
		s.Status.Children[c] = true
	}
	return s
}

func treeString(lines []SnapLine) string {
	var b []string
	for _, l := range lines {
		h := ""
		if l.Head {
			h = "*"
		}
		b = append(b, fmt.Sprintf("%d%s%s", l.Depth, l.Snap.Name, h))
	}
	return strings.Join(b, " ")
}

func TestSnapshotTree(t *testing.T) {
	// linear: a ← b ← c ← head
	lin := []*VolumeSnapshot{
		snap("a", "", "1", "b"), snap("c", "b", "3", "volume-head"), snap("b", "a", "2", "c"),
	}
	if got := treeString(SnapshotTree(lin)); got != "0c* 0b 0a" {
		t.Errorf("linear: %s", got)
	}
	// revert to a after c existed: a has children b (old line) and d (new line)
	// a ← b ← c ; a ← d ← head
	br := []*VolumeSnapshot{
		snap("a", "", "1", "b", "d"), snap("b", "a", "2", "c"), snap("c", "b", "3"),
		snap("d", "a", "4", "volume-head"),
	}
	if got := treeString(SnapshotTree(br)); got != "0d* 1c 1b 0a" {
		t.Errorf("branch: %s", got)
	}
	// parent missing (purged) and no head: still all shown once
	lost := []*VolumeSnapshot{snap("x", "gone", "5"), snap("y", "", "1")}
	if got := treeString(SnapshotTree(lost)); got != "0x 0y" {
		t.Errorf("lost: %s", got)
	}
}
