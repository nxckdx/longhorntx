package agent

import (
	"strings"
	"testing"
	"time"

	"github.com/nxckdx/longhorntx/internal/longhorn"
)

// A stuck pod is reported once (history/notification, gauge); when it no
// longer waits, "mounted" with the total wait.
func TestMountCheckReportsOnceAndRecovery(t *testing.T) {
	a := &Agent{Logf: t.Logf}
	a.notifier = newNotifier(a, Notify{URL: "http://unused"})
	s := longhorn.StuckMount{Pod: "shop/db-0", Node: "n1", Volume: "pvc-1", Waiting: 6 * time.Minute, Stage: longhorn.StageMount}
	waits := map[string]*mountWait{}
	now := time.Now()
	a.mountCheck(t.Context(), &longhorn.MountReport{Stuck: []longhorn.StuckMount{s}}, waits, now)
	s.Waiting += time.Minute
	a.mountCheck(t.Context(), &longhorn.MountReport{Stuck: []longhorn.StuckMount{s}}, waits, now.Add(time.Minute))
	if g := *a.metrics.mountsWaiting.Load(); g[[2]string{"n1", "mount"}] != 1 {
		t.Errorf("gauge = %v", g)
	}
	a.mountCheck(t.Context(), &longhorn.MountReport{}, waits, now.Add(2*time.Minute))
	q := a.notifier.queue
	if len(q) != 2 || q[0].Kind != longhorn.HistMountStuck || !q[0].Bad || q[1].Kind != longhorn.HistMounted || q[1].Bad {
		t.Fatalf("notices = %+v", q)
	}
	if !strings.Contains(q[0].Text, "kubelet") {
		t.Errorf("text = %s", q[0].Text)
	}
	if len(*a.metrics.mountsWaiting.Load()) != 0 || len(waits) != 0 {
		t.Error("not cleared")
	}
}
