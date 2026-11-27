package shrink

import (
	"strings"
	"testing"
	"time"

	"github.com/nxckdx/longhorntx/internal/longhorn"
)

func TestShrinkName(t *testing.T) {
	now := time.Date(2026, 11, 27, 0, 0, 0, 0, time.UTC)
	if n := shrinkName("data", now); n != "data-shrink-1127" {
		t.Errorf("name = %s", n)
	}
	long := shrinkName(strings.Repeat("very-long-pvc-name-", 5), now)
	if len(long) > longhorn.MaxVolumeName || !strings.HasSuffix(long, "-shrink-1127") || strings.Contains(long, "--") {
		t.Errorf("long name = %s (%d)", long, len(long))
	}
}

func TestWithout(t *testing.T) {
	got := without([]string{"a encrypted b", "keep", "x older than an expansion"}, "encrypted")
	if len(got) != 2 || got[0] != "keep" {
		t.Errorf("got %v", got)
	}
}
