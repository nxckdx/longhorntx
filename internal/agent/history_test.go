package agent

import (
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/nxckdx/longhorntx/internal/longhorn"
)

func TestHistoryDetection(t *testing.T) {
	now := time.Now().Truncate(time.Second) // Kubernetes timestamps have seconds
	v := &longhorn.Volume{}
	v.Name, v.CreationTimestamp = "vol", metav1.NewTime(now.Add(-24*time.Hour))
	v.Status.Robustness, v.Spec.Size, v.Status.CurrentImage = "healthy", longhorn.Quantity(10<<30), "longhornio/longhorn-engine:v1.11.0"

	w := *v
	w.Status.Robustness = "unknown" // detached: not history
	if ev := volumeEvents(v, &w, now); len(ev) != 0 {
		t.Errorf("healthy → unknown recorded: %+v", ev)
	}
	w.Status.Robustness = "degraded"
	w.Spec.Size = longhorn.Quantity(20 << 30)
	w.Status.CurrentImage = "longhornio/longhorn-engine:v1.12.1"
	ev := volumeEvents(v, &w, now)
	if len(ev) != 3 || !ev[0].Bad() || ev[1].Kind != longhorn.HistExpanded || !strings.Contains(ev[2].Text, "v1.11.0 → v1.12.1") {
		t.Errorf("volume events: %+v", ev)
	}

	// a replica created with the volume is no rebuild; a later one is
	r := &longhorn.Replica{}
	r.Spec.NodeID, r.Spec.DiskPath, r.CreationTimestamp = "n1", "/var/lib/longhorn/", metav1.NewTime(v.CreationTimestamp.Add(time.Second))
	if ev := replicaEvents(nil, r, v, now); len(ev) != 0 {
		t.Errorf("initial replica recorded as rebuild: %+v", ev)
	}
	r.CreationTimestamp = metav1.NewTime(now.Add(-10 * time.Minute))
	unscheduled := *r
	unscheduled.Spec.NodeID, unscheduled.Spec.DiskPath = "", ""
	if ev := replicaEvents(nil, &unscheduled, v, now); len(ev) != 0 {
		t.Errorf("rebuild start before scheduling: %+v", ev)
	}
	if ev := replicaEvents(&unscheduled, r, v, now); len(ev) != 1 || ev[0].Kind != longhorn.HistRebuildStart || !strings.Contains(ev[0].Text, "n1:") {
		t.Errorf("rebuild start: %+v", ev)
	}
	done := *r
	done.Spec.HealthyAt = now.Add(-time.Minute).UTC().Format(time.RFC3339)
	ev = replicaEvents(r, &done, v, now)
	if len(ev) != 1 || ev[0].Kind != longhorn.HistRebuildDone || ev[0].Took != 9*time.Minute || !ev[0].Notable() {
		t.Errorf("rebuild done: %+v", ev)
	}
	failed := done
	failed.Spec.FailedAt = now.UTC().Format(time.RFC3339)
	if ev := replicaEvents(&done, &failed, v, now); len(ev) != 1 || ev[0].Kind != longhorn.HistReplicaFailed {
		t.Errorf("replica failed: %+v", ev)
	}

	b := &longhorn.Backup{}
	b.Name, b.CreationTimestamp, b.Status.State = "backup-1", metav1.NewTime(now.Add(-2*time.Minute)), "InProgress"
	nb := *b
	nb.Status.State, nb.Status.Error = "Error", "target unavailable"
	if ev := backupEvents(b, &nb, now); len(ev) != 1 || !ev[0].Bad() || ev[0].Took != 2*time.Minute {
		t.Errorf("backup failed: %+v", ev)
	}
	nb.Status.State = "Completed"
	if ev := backupEvents(b, &nb, now); len(ev) != 1 || ev[0].Kind != longhorn.HistBackup || ev[0].Notable() {
		t.Errorf("backup completed: %+v", ev)
	}
}

func TestHistoryEncoding(t *testing.T) {
	var ev []longhorn.HistoryEvent
	for i := range 60 {
		ev = append(ev, longhorn.HistoryEvent{At: time.Unix(int64(i), 0), Kind: longhorn.HistBackup, Text: "b"})
	}
	u, err := longhorn.HistoryObject("vol", "ns", nil, ev, longhorn.MaxVolumeHistory)
	if err != nil {
		t.Fatal(err)
	}
	back, err := longhorn.DecodeHistory(u)
	if err != nil || len(back) != longhorn.MaxVolumeHistory || back[0].At.Unix() != 10 {
		t.Fatalf("trim to the newest %d: %d, %v", longhorn.MaxVolumeHistory, len(back), err)
	}
}

func TestTrimHistory(t *testing.T) {
	now := time.Now()
	ev := []longhorn.HistoryEvent{{At: now.Add(-100 * 24 * time.Hour), Kind: "a"}, {At: now.Add(-10 * 24 * time.Hour), Kind: "b"}, {At: now, Kind: "c"}}
	a := &Agent{}
	if got := a.trimHistory(ev, 2); len(got) != 2 || got[0].Kind != "b" {
		t.Errorf("count: %v", got)
	}
	a.HistoryMaxAge = 30 * 24 * time.Hour
	if got := a.trimHistory(ev, 50); len(got) != 2 || got[0].Kind != "b" {
		t.Errorf("age: %v", got)
	}
	if a.historyVolumeMax() != 50 || (&Agent{HistoryVolumeMax: 99999}).historyVolumeMax() != 1000 || (&Agent{HistoryClusterMax: 20}).historyClusterMax() != 20 {
		t.Error("clamp")
	}
}
