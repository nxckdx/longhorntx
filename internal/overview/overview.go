// Package overview runs a short doctor against several kubeconfig contexts
// (lhtx overview, TUI context picker).
package overview

import (
	"context"
	"sort"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"

	"github.com/nxckdx/longhorntx/internal/format"
	"github.com/nxckdx/longhorntx/internal/kube"
	"github.com/nxckdx/longhorntx/internal/longhorn"
)

// Summary is one cluster.
type Summary struct {
	Context string `json:"context"`
	Error   string `json:"error,omitempty"`
	// NotInstalled: the cluster answered but has no longhorn.io CRDs. Not
	// an error (many kubeconfigs mix clusters with and without Longhorn).
	NotInstalled bool              `json:"notInstalled,omitempty"`
	Version      string            `json:"longhornVersion,omitempty"`
	Volumes      int               `json:"volumes"`
	Attached     int               `json:"attached"`
	Degraded     int               `json:"degraded"`
	Faulted      int               `json:"faulted"`
	Rebuilding   int               `json:"rebuilding"`
	Nodes        int               `json:"nodes"`
	NodesReady   int               `json:"nodesReady"`
	Critical     int               `json:"critical"`
	Warnings     int               `json:"warnings"`
	Worst        longhorn.Severity `json:"worst"`
	Top          string            `json:"top,omitempty"` // most severe finding
	Traffic      int64             `json:"trafficBytes"`
	Took         time.Duration     `json:"took"`
}

// Check connects to a context and summarizes it. Traffic: one LIST of
// volumes, replicas and nodes (plus small collections), like doctor.
func Check(ctx context.Context, ko kube.Options, kctx, namespace string) Summary {
	start := time.Now()
	ko.Context = kctx
	cl, err := kube.Connect(ko)
	if err != nil {
		return Summary{Context: kctx, Error: ShortError(err)}
	}
	c, err := longhorn.NewClient(cl, namespace)
	if err != nil {
		return Summary{Context: kctx, Error: ShortError(err)}
	}
	s := Summarize(ctx, c)
	s.Context = kctx
	s.Took = time.Since(start).Round(100 * time.Millisecond)
	return s
}

// Summarize runs the checks with an existing client.
func Summarize(ctx context.Context, c *longhorn.Client) (s Summary) {
	start := time.Now()
	s.Context = c.Cluster.Context
	defer func() {
		s.Traffic = c.Cluster.Traffic.RX() + c.Cluster.Traffic.TX()
		s.Took = time.Since(start).Round(100 * time.Millisecond)
	}()
	in, err := longhorn.LoadDoctorInput(ctx, c)
	if err != nil {
		if NotInstalled(err) {
			s.NotInstalled = true
			return s
		}
		s.Error = ShortError(err)
		return s
	}
	s.Version, _ = c.LonghornVersion(ctx)
	s.Volumes, s.Nodes = len(in.Volumes), len(in.Nodes)
	for _, v := range in.Volumes {
		if v.Status.State == "attached" {
			s.Attached++
		}
		switch v.Status.Robustness {
		case "degraded":
			s.Degraded++
		case "faulted":
			s.Faulted++
		}
	}
	s.Rebuilding = len(longhorn.RebuildingVolumes(in.Replicas))
	for _, n := range in.Nodes {
		if n.Ready() {
			s.NodesReady++
		}
	}
	fs := longhorn.Diagnose(in)
	s.Worst = longhorn.Worst(fs)
	sort.SliceStable(fs, func(i, j int) bool { return fs[i].Severity > fs[j].Severity })
	for _, f := range fs {
		switch f.Severity {
		case longhorn.SevCritical:
			s.Critical++
		case longhorn.SevWarning:
			s.Warnings++
		}
	}
	if len(fs) > 0 && fs[0].Severity > longhorn.SevInfo {
		s.Top = format.Short(fs[0].Object+": "+fs[0].Message, 80)
	}
	return s
}

// Status is a one-word state: OK, WARNING, CRITICAL, ERROR, "no Longhorn".
func (s Summary) Status() string {
	switch {
	case s.Error != "":
		return "ERROR"
	case s.NotInstalled:
		return "no Longhorn"
	case s.Worst > longhorn.SevInfo:
		return s.Worst.String()
	}
	return "OK"
}

// NotInstalled: listing longhorn.io volumes gives 404 when the CRDs are
// missing (a missing namespace would give an empty list instead).
func NotInstalled(err error) bool {
	return apierrors.IsNotFound(err) ||
		strings.Contains(err.Error(), "the server could not find the requested resource")
}

// ShortError trims client-go's verbose errors for a one-line table.
func ShortError(err error) string {
	msg := err.Error()
	if strings.Contains(msg, "context deadline exceeded") {
		return "timeout"
	}
	if len(msg) > 120 {
		msg = msg[:117] + "…"
	}
	return msg
}
