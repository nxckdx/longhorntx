package agent

import (
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"

	"github.com/nxckdx/longhorntx/internal/longhorn"
)

func TestShareEvents(t *testing.T) {
	sm := func(s string) *longhorn.ShareManager {
		return &longhorn.ShareManager{Status: longhorn.ShareManagerStatus{State: s}}
	}
	for _, c := range []struct {
		o, n string
		rec  bool
		bad  bool
	}{
		{"stopped", "starting", false, false},
		{"starting", "running", false, false},
		{"running", "stopping", false, false},
		{"running", "error", true, true},
		{"running", "starting", true, true}, // NFS server restarted under its clients
		{"error", "running", true, false},   // recovered
		{"error", "starting", false, false}, // on the way back: only the end is recorded
	} {
		ev := shareEvents(sm(c.o), sm(c.n), time.Now())
		if (len(ev) == 1) != c.rec {
			t.Errorf("%s → %s: %v", c.o, c.n, ev)
			continue
		}
		if c.rec && (ev[0].Bad() != c.bad || notifiable(ev[0]) != true) {
			t.Errorf("%s → %s: bad=%v notifiable=%v", c.o, c.n, ev[0].Bad(), notifiable(ev[0]))
		}
	}
}

func TestSharePodEvents(t *testing.T) {
	pod := func(restarts int32) *corev1.Pod {
		p := &corev1.Pod{}
		p.Name, p.Spec.NodeName = "share-manager-pvc-1", "n1"
		cs := corev1.ContainerStatus{RestartCount: restarts}
		if restarts > 0 {
			cs.LastTerminationState.Terminated = &corev1.ContainerStateTerminated{Reason: "OOMKilled", ExitCode: 137}
		}
		p.Status.ContainerStatuses = []corev1.ContainerStatus{cs}
		return p
	}
	if ev := sharePodEvents(pod(0), pod(0), time.Now()); len(ev) != 0 {
		t.Errorf("no restart: %v", ev)
	}
	ev := sharePodEvents(pod(0), pod(1), time.Now())
	if len(ev) != 1 || !ev[0].Bad() || !strings.Contains(ev[0].Text, "OOMKilled") {
		t.Errorf("restart: %v", ev)
	}
}
