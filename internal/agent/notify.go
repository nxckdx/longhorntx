package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/nxckdx/longhorntx/internal/longhorn"
)

// Notifications without Prometheus: the agent sends what goes wrong (and
// what recovers) to a webhook – ntfy, Slack/Mattermost/Rocket.Chat,
// Discord, or plain JSON for anything else. Events are collected and sent
// together (a node restart degrades every volume at once: one message, not
// sixty), at most once per Every.

// Notify configures the notifications; URL "" = off.
type Notify struct {
	URL     string
	Format  string        // json | ntfy | slack | discord
	Cluster string        // shown in the message
	Every   time.Duration // at most one message per (default 1 minute)
}

// NotifyFormats lists the formats.
var NotifyFormats = []string{"json", "ntfy", "slack", "discord"}

// notice is one thing worth telling.
type notice struct {
	At     time.Time `json:"at"`
	Kind   string    `json:"kind"`
	Volume string    `json:"volume,omitempty"`
	PVC    string    `json:"pvc,omitempty"`
	Text   string    `json:"text"`
	Bad    bool      `json:"bad"`
}

type notifier struct {
	cfg    Notify
	a      *Agent
	client *http.Client
	mu     sync.Mutex
	queue  []notice
	wake   chan struct{}
	gather time.Duration // let a burst gather before sending
}

func newNotifier(a *Agent, cfg Notify) *notifier {
	if cfg.Every <= 0 {
		cfg.Every = time.Minute
	}
	if cfg.Format == "" {
		cfg.Format = "json"
	}
	return &notifier{cfg: cfg, a: a, client: &http.Client{Timeout: 15 * time.Second}, wake: make(chan struct{}, 1), gather: 10 * time.Second}
}

// notifiable: failures and degradations, and volumes healthy again.
func notifiable(e longhorn.HistoryEvent) bool {
	return e.Bad() || e.Kind == longhorn.HistMounted || e.Kind == longhorn.HistShareManager || (e.Kind == longhorn.HistRobustness && strings.HasSuffix(e.Text, "→ healthy") &&
		(strings.HasPrefix(e.Text, "degraded") || strings.HasPrefix(e.Text, "faulted")))
}

func (n *notifier) add(x notice) {
	if n == nil {
		return
	}
	n.mu.Lock()
	n.queue = append(n.queue, x)
	n.mu.Unlock()
	select {
	case n.wake <- struct{}{}:
	default:
	}
}

func (n *notifier) run(ctx context.Context) {
	n.a.Logf("notifications to a %s webhook", n.cfg.Format)
	for {
		select {
		case <-ctx.Done():
			return
		case <-n.wake:
		}
		// let a burst gather
		select {
		case <-ctx.Done():
			return
		case <-time.After(n.gather):
		}
		n.mu.Lock()
		batch := n.queue
		n.queue = nil
		n.mu.Unlock()
		if len(batch) > 0 {
			n.send(ctx, batch)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(n.cfg.Every):
		}
	}
}

func (n *notifier) send(ctx context.Context, batch []notice) {
	ctype, body, headers := n.render(batch)
	var err error
	for attempt := range 3 {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Duration(attempt*attempt) * 5 * time.Second):
			}
		}
		if err = n.post(ctx, ctype, body, headers); err == nil {
			n.a.op("notify", "sent")
			return
		}
	}
	n.a.op("notify", "failed")
	n.a.Logf("notification (%d events) not sent: %v", len(batch), err)
}

func (n *notifier) post(ctx context.Context, ctype string, body []byte, headers map[string]string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, n.cfg.URL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", ctype)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := n.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("HTTP %s", resp.Status)
	}
	return nil
}

// maxLines: a message lists at most this many events (and counts the rest).
const maxLines = 15

func (n *notifier) render(batch []notice) (ctype string, body []byte, headers map[string]string) {
	bad := 0
	for _, x := range batch {
		if x.Bad {
			bad++
		}
	}
	where := "Longhorn"
	if n.cfg.Cluster != "" {
		where += " (" + n.cfg.Cluster + ")"
	}
	title := fmt.Sprintf("%s: %d problem(s)", where, bad)
	if bad == 0 {
		title = where + ": recovered"
	} else if bad < len(batch) {
		title += fmt.Sprintf(", %d recovered", len(batch)-bad)
	}
	var lines []string
	for i, x := range batch {
		if i == maxLines {
			lines = append(lines, fmt.Sprintf("… and %d more (lhtx history --since 1h)", len(batch)-maxLines))
			break
		}
		mark := "✔"
		if x.Bad {
			mark = "✖"
		}
		who := x.PVC
		if who == "" {
			who = x.Volume
		}
		if who != "" {
			who += ": "
		}
		lines = append(lines, fmt.Sprintf("%s %s%s", mark, who, x.Text))
	}
	text := strings.Join(lines, "\n")
	switch n.cfg.Format {
	case "ntfy":
		prio := "default"
		tags := "white_check_mark"
		if bad > 0 {
			prio, tags = "high", "warning"
		}
		return "text/plain; charset=utf-8", []byte(text), map[string]string{"Title": title, "Priority": prio, "Tags": tags}
	case "slack":
		b, _ := json.Marshal(map[string]string{"text": "*" + title + "*\n" + text})
		return "application/json", b, nil
	case "discord":
		msg := "**" + title + "**\n" + text
		if len(msg) > 1900 {
			msg = msg[:1900] + "…"
		}
		b, _ := json.Marshal(map[string]string{"content": msg})
		return "application/json", b, nil
	}
	b, _ := json.Marshal(map[string]any{"title": title, "cluster": n.cfg.Cluster, "events": batch})
	return "application/json", b, nil
}
