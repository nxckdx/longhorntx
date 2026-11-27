package longhorn

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// History (kept by the lhtx agent): what happened to a volume – robustness
// changes, rebuilds and how long they took, failed replicas, backups,
// expansions, engine upgrades. Longhorn itself keeps none of this (events
// expire after an hour). One VolumeHistory per volume holds its last
// MaxVolumeHistory events; one cluster-wide VolumeHistory (ClusterHistory)
// holds the last MaxClusterHistory notable ones, so "what went wrong this
// week" is one small object. Stored like digests: gzipped JSON, base64.

// GVRVolumeHistory: lhtx.io/v1alpha1 volumehistories in the Longhorn namespace.
var GVRVolumeHistory = schema.GroupVersionResource{Group: "lhtx.io", Version: "v1alpha1", Resource: "volumehistories"}

const (
	// ClusterHistory is the name of the cluster-wide history object.
	ClusterHistory    = "lhtx-cluster"
	MaxVolumeHistory  = 50
	MaxClusterHistory = 300
)

// History event kinds.
const (
	HistRobustness    = "robustness"     // healthy → degraded, …
	HistRebuildStart  = "rebuild"        // a replica is being rebuilt
	HistRebuildDone   = "rebuilt"        // … done, with the duration
	HistReplicaFailed = "replica-failed" // a replica failed
	HistBackup        = "backup"         // a backup completed
	HistBackupFailed  = "backup-failed"
	HistExpanded      = "expanded"
	HistEngine        = "engine" // engine image changed
	// HistRestoreProbe: the lhtx agent restored the volume's latest backup
	// into a test volume (all data read back); …Failed: it could not.
	HistRestoreProbe       = "restore-probe"
	HistRestoreProbeFailed = "restore-probe-failed"
)

// HistoryEvent is one entry.
type HistoryEvent struct {
	At     time.Time     `json:"at"`
	Kind   string        `json:"kind"`
	Text   string        `json:"text"`
	Took   time.Duration `json:"took,omitempty"`
	Volume string        `json:"volume,omitempty"` // cluster history
	PVC    string        `json:"pvc,omitempty"`    // cluster history
}

// Notable: goes into the cluster history as well.
func (e HistoryEvent) Notable() bool {
	switch e.Kind {
	case HistRobustness, HistReplicaFailed, HistBackupFailed, HistRebuildDone, HistRestoreProbe, HistRestoreProbeFailed:
		return true
	}
	return false
}

// Bad: a failure or degradation (shown highlighted).
func (e HistoryEvent) Bad() bool {
	switch e.Kind {
	case HistReplicaFailed, HistBackupFailed, HistRestoreProbeFailed:
		return true
	case HistRobustness:
		return strings.HasSuffix(e.Text, "degraded") || strings.HasSuffix(e.Text, "faulted")
	}
	return false
}

// EncodeHistory stores events (newest last), trimmed to max.
func EncodeHistory(events []HistoryEvent, max int) (string, error) {
	if len(events) > max {
		events = events[len(events)-max:]
	}
	raw, err := json.Marshal(events)
	if err != nil {
		return "", err
	}
	var buf bytes.Buffer
	zw, _ := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	_, _ = zw.Write(raw)
	_ = zw.Close()
	return digestGzipPrefix + base64.StdEncoding.EncodeToString(buf.Bytes()), nil
}

// DecodeHistory reads the events of a VolumeHistory.
func DecodeHistory(u *unstructured.Unstructured) ([]HistoryEvent, error) {
	data, _, _ := unstructured.NestedString(u.Object, "events")
	raw := []byte(data)
	if b64, ok := strings.CutPrefix(data, digestGzipPrefix); ok {
		z, err := base64.StdEncoding.DecodeString(b64)
		if err != nil {
			return nil, err
		}
		zr, err := gzip.NewReader(bytes.NewReader(z))
		if err != nil {
			return nil, err
		}
		if raw, err = io.ReadAll(zr); err != nil {
			return nil, err
		}
	}
	var out []HistoryEvent
	if len(raw) == 0 {
		return nil, nil
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("VolumeHistory %s: %w", u.GetName(), err)
	}
	return out, nil
}

// HistoryObject builds a VolumeHistory; owner (the volume) may be nil for
// the cluster history.
func HistoryObject(name, namespace string, owner *Volume, events []HistoryEvent, max int) (*unstructured.Unstructured, error) {
	data, err := EncodeHistory(events, max)
	if err != nil {
		return nil, err
	}
	md := map[string]any{"name": name, "namespace": namespace,
		"labels": map[string]any{"app.kubernetes.io/managed-by": "lhtx-agent"}}
	if owner != nil {
		f := false
		md["ownerReferences"] = []any{map[string]any{"apiVersion": Group + "/" + Version, "kind": "Volume",
			"name": owner.Name, "uid": string(owner.UID), "blockOwnerDeletion": f}}
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "lhtx.io/v1alpha1", "kind": "VolumeHistory", "metadata": md, "events": data,
	}}, nil
}

// ErrNoHistory: no agent keeps a history here (or none for this volume yet).
var ErrNoHistory = fmt.Errorf("no history – the lhtx agent keeps it (lhtx agent --help)")

// History reads the history of a volume, or the cluster history for
// ClusterHistory; newest first.
func (c *Client) History(ctx context.Context, name string) ([]HistoryEvent, error) {
	u, err := c.dyn.Resource(GVRVolumeHistory).Namespace(c.Namespace).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, ErrNoHistory
	}
	if err != nil {
		return nil, err
	}
	ev, err := DecodeHistory(u)
	if err != nil {
		return nil, err
	}
	sort.SliceStable(ev, func(i, j int) bool { return ev[i].At.After(ev[j].At) })
	return ev, nil
}

// HistoryChecks reads the cluster history (one GET; nothing without the
// agent) and reports repeated trouble of the last 7 days: backups of a
// volume failing again and again, replicas of a volume failing again and
// again.
func (c *Client) HistoryChecks(ctx context.Context) []Finding {
	ev, err := c.History(ctx, ClusterHistory)
	if err != nil {
		return nil
	}
	type key struct{ vol, kind string }
	n := map[key]int{}
	label := map[string]string{}
	for _, e := range ev {
		if time.Since(e.At) > 7*24*time.Hour {
			continue
		}
		n[key{e.Volume, e.Kind}]++
		label[e.Volume] = e.PVC
	}
	var out []Finding
	for k, cnt := range n {
		obj := label[k.vol]
		if obj == "" {
			obj = k.vol
		}
		switch {
		case k.kind == HistBackupFailed && cnt >= 2:
			out = append(out, Finding{Severity: SevWarning, Check: "history", Object: obj,
				Message: fmt.Sprintf("backups failed %d times in the last 7 days", cnt), Hint: "lhtx history " + k.vol})
		case k.kind == HistReplicaFailed && cnt >= 3:
			out = append(out, Finding{Severity: SevWarning, Check: "history", Object: obj,
				Message: fmt.Sprintf("replicas failed %d times in the last 7 days", cnt), Hint: "lhtx history " + k.vol + " – a bad disk or node?"})
		}
	}
	return out
}
