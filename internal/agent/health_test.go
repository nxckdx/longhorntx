package agent

import (
	"strings"
	"testing"
	"time"
)

func TestHealth(t *testing.T) {
	a := &Agent{Resync: time.Second}
	if a.ready() != nil || a.live() != nil {
		t.Error("a standby must be ready and live")
	}
	a.health.leading.Store(true)
	if a.ready() == nil {
		t.Error("leader not synced yet must not be ready")
	}
	a.health.synced.Store(true)
	a.health.beat()
	if a.ready() != nil || a.live() != nil {
		t.Error("synced leader with a fresh loop must be ready and live")
	}
	a.health.lastLoop.Store(time.Now().Add(-time.Hour).UnixNano())
	if a.live() == nil {
		t.Error("a stuck loop must fail liveness")
	}
}

func TestMetrics(t *testing.T) {
	a := &Agent{}
	a.SetVersion("1.2.3")
	a.op("freeze", "done")
	a.op("freeze", "done")
	a.op("switch", "failed")
	a.digestWrites.Add(3)
	a.metrics.volumeEvents.Add(40)
	var b strings.Builder
	a.writeMetrics(&b)
	out := b.String()
	for _, want := range []string{`lhtx_agent_build_info{version="1.2.3"} 1`, "lhtx_agent_leader 0",
		`lhtx_agent_operations_total{kind="freeze",result="done"} 2`, `lhtx_agent_operations_total{kind="switch",result="failed"} 1`,
		"lhtx_agent_digest_writes_total 3", "lhtx_agent_volume_events_total 40"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in\n%s", want, out)
		}
	}
}
