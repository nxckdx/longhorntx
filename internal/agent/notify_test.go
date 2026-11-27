package agent

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/nxckdx/longhorntx/internal/longhorn"
)

func TestNotifiable(t *testing.T) {
	for _, c := range []struct {
		e    longhorn.HistoryEvent
		want bool
	}{
		{longhorn.HistoryEvent{Kind: longhorn.HistBackupFailed}, true},
		{longhorn.HistoryEvent{Kind: longhorn.HistRobustness, Text: "healthy → degraded"}, true},
		{longhorn.HistoryEvent{Kind: longhorn.HistRobustness, Text: "degraded → healthy"}, true},
		{longhorn.HistoryEvent{Kind: longhorn.HistRobustness, Text: "unknown → healthy"}, false}, // attached
		{longhorn.HistoryEvent{Kind: longhorn.HistRebuildDone}, false},
		{longhorn.HistoryEvent{Kind: longhorn.HistBackup}, false},
	} {
		if got := notifiable(c.e); got != c.want {
			t.Errorf("%+v: %v", c.e, got)
		}
	}
}

// A burst is sent as one message; ntfy gets text with a title header.
func TestNotifyBatchesNtfy(t *testing.T) {
	got := make(chan *http.Request, 5)
	bodies := make(chan string, 5)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got <- r
		bodies <- string(b)
	}))
	defer srv.Close()
	a := &Agent{Logf: t.Logf}
	n := newNotifier(a, Notify{URL: srv.URL, Format: "ntfy", Cluster: "prod", Every: time.Hour})
	n.gather = 50 * time.Millisecond
	go n.run(t.Context())
	for i := range 20 {
		n.add(notice{Kind: longhorn.HistReplicaFailed, PVC: "shop/db", Text: "replica on n" + string(rune('a'+i)) + " failed", Bad: true})
	}
	n.add(notice{Kind: longhorn.HistRobustness, PVC: "shop/web", Text: "degraded → healthy"})
	select {
	case r := <-got:
		body := <-bodies
		if !strings.Contains(r.Header.Get("Title"), "prod") || !strings.Contains(r.Header.Get("Title"), "20 problem(s), 1 recovered") || r.Header.Get("Priority") != "high" {
			t.Errorf("headers: %v", r.Header)
		}
		if !strings.Contains(body, "✖ shop/db: replica on na failed") || !strings.Contains(body, "and 6 more") {
			t.Errorf("body:\n%s", body)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("nothing sent")
	}
	select {
	case <-got:
		t.Error("a burst must be one message")
	case <-time.After(300 * time.Millisecond):
	}
}

func TestNotifyFormats(t *testing.T) {
	n := newNotifier(&Agent{}, Notify{URL: "x", Format: "slack"})
	batch := []notice{{PVC: "a/b", Text: "backup x failed: no target", Bad: true}}
	_, body, _ := n.render(batch)
	var m map[string]string
	if err := json.Unmarshal(body, &m); err != nil || !strings.Contains(m["text"], "a/b: backup x failed") {
		t.Errorf("slack: %s %v", body, err)
	}
	n.cfg.Format = "discord"
	_, body, _ = n.render(batch)
	if err := json.Unmarshal(body, &m); err != nil || m["content"] == "" {
		t.Errorf("discord: %s", body)
	}
	n.cfg.Format = "json"
	_, body, _ = n.render(batch)
	var j struct {
		Title  string
		Events []notice
	}
	if err := json.Unmarshal(body, &j); err != nil || len(j.Events) != 1 || !j.Events[0].Bad {
		t.Errorf("json: %s", body)
	}
}
