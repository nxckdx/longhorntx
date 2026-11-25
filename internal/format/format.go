// Package format holds small helpers shared by CLI and TUI output.
package format

import (
	"fmt"
	"strings"
	"time"
)

// Bytes formats a byte count with binary units ("20 GiB", "512 MiB").
func Bytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	v := float64(n) / float64(div)
	s := fmt.Sprintf("%.1f", v)
	s = strings.TrimSuffix(s, ".0")
	return s + " " + string("KMGTPE"[exp]) + "iB"
}

// Rate formats bytes/s.
func Rate(bps float64) string { return Bytes(int64(bps)) + "/s" }

// Duration formats an ETA coarsely: "45 s", "3 min", "1 h 20 min".
func Duration(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%d s", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%d min", int(d.Minutes()+0.5))
	default:
		h := int(d.Hours())
		return fmt.Sprintf("%d h %d min", h, int(d.Minutes())-h*60)
	}
}

// Age formats an RFC3339 timestamp as age ("5m", "3d"); "" if unparsable.
func Age(ts string, now time.Time) string {
	t, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		return ""
	}
	d := now.Sub(t)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

// Short truncates s to n runes with an ellipsis.
func Short(s string, n int) string {
	r := []rune(s)
	if n <= 0 || len(r) <= n {
		return s
	}
	if n == 1 {
		return "…"
	}
	return string(r[:n-1]) + "…"
}

// Interval writes a Go duration string compactly ("5m0s" → "5m",
// "1h30m0s" → "1h30m"); anything else is returned as it is.
func Interval(s string) string {
	d, err := time.ParseDuration(s)
	if err != nil || d == 0 {
		return s
	}
	out := d.String()
	for _, suf := range []string{"m0s", "h0m"} {
		if strings.HasSuffix(out, suf) {
			out = strings.TrimSuffix(out, suf[1:])
		}
	}
	return out
}
