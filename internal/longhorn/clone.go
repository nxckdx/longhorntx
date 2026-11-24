package longhorn

import (
	"fmt"
	"strconv"
	"time"
)

// Clone a volume or one of its snapshots into a NEW volume
// (spec.dataSource "vol://<volume>" or "snap://<volume>/<snapshot>"), the
// safe alternative to reverting: the running volume is not touched and the
// old state can be inspected or copied from the clone.

// CloneName suggests "<pvc>-clone-<MMDD>".
func CloneName(v *Volume, now time.Time) string {
	n := RestoreName(v.PVC(), v.Name, now)
	return n[:len(n)-len("-restore-0102")] + "-clone-" + now.Format("0102")
}

// CloneVolume builds the actions; snapshot "" clones the current data.
func CloneVolume(src *Volume, snapshot string, s RestoreSpec, exists func(kind string) bool) ([]*Action, error) {
	if src.Spec.DataEngine == "v2" {
		return nil, fmt.Errorf("cloning v2 data engine volumes is not supported by lhtx")
	}
	if !volNameRE.MatchString(s.Name) || len(s.Name) > 63 {
		return nil, fmt.Errorf("volume name %q: lowercase letters, digits and '-', at most 63 characters", s.Name)
	}
	if exists("volume") {
		return nil, fmt.Errorf("volume %s exists – choose another name", s.Name)
	}
	if s.Replicas < 1 || s.Replicas > 20 {
		return nil, fmt.Errorf("replicas must be 1–20")
	}
	ds, what := "vol://"+src.Name, "current data"
	if snapshot != "" {
		ds, what = "snap://"+src.Name+"/"+snapshot, "snapshot "+snapshot
	}
	size := int64(src.Spec.Size)
	mode := src.Spec.AccessMode
	if mode == "" {
		mode = "rwo"
	}
	vol := &Action{
		Title: fmt.Sprintf("Clone %s of %s into new volume %s", what, volLabel(src), s.Name),
		Verb:  VerbCreate, GVR: GVRVolume, Name: s.Name,
		Object: lhObject("Volume", s.Name, nil, map[string]any{
			"dataSource":       ds,
			"size":             strconv.FormatInt(size, 10),
			"numberOfReplicas": int64(s.Replicas),
			"accessMode":       mode,
			"dataEngine":       "v1",
			"frontend":         "blockdev",
		}),
		summary: fmt.Sprintf("volume %s from %s (%s, %d replicas)", s.Name, ds, humanBytes(size), s.Replicas),
	}
	vol.Warnings = append(vol.Warnings,
		fmt.Sprintf("copies the %s of %s into a new volume; %s is not changed", what, volLabel(src), volLabel(src)),
		"Longhorn attaches the source while copying (a clone ticket); the new volume can be attached when the clone is done")
	if src.Status.Robustness == "faulted" {
		vol.Warnings = append(vol.Warnings, "the source is faulted: the clone will fail until a replica is usable")
	}
	acts := []*Action{vol}
	if s.PVCName == "" {
		return acts, nil
	}
	more, err := pvAndPVC(s, size, mode, exists)
	if err != nil {
		return nil, err
	}
	return append(acts, more...), nil
}
