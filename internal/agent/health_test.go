package agent

import (
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
