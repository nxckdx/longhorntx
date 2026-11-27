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

	"github.com/nxckdx/longhorntx/internal/format"
)

// Capacity (kept by the lhtx agent): how full every Longhorn disk was over
// time, so lhtx can say when a disk will be full – Longhorn only knows the
// current state. The agent samples the nodes every few hours and keeps the
// samples in the VolumeHistory named CapacityHistory (gzipped JSON, like
// the history; ~60 days).

const (
	CapacityHistory = "lhtx-capacity"
	// MaxCapacitySamples: 60 days at the default interval of 6 hours.
	MaxCapacitySamples = 240
)

// DiskUse is one disk in a sample: used bytes and the usable size
// (maximum minus what is reserved for other uses).
type DiskUse struct {
	Used   int64 `json:"u"`
	Usable int64 `json:"m"`
}

// CapacitySample: all disks at one time, keyed "node:path".
type CapacitySample struct {
	At    time.Time          `json:"at"`
	Disks map[string]DiskUse `json:"d"`
}

// SampleCapacity takes a sample of the nodes' disks.
func SampleCapacity(nodes []*Node, now time.Time) CapacitySample {
	s := CapacitySample{At: now.UTC().Truncate(time.Second), Disks: map[string]DiskUse{}}
	for _, n := range nodes {
		for name, st := range n.Status.DiskStatus {
			path := st.DiskPath
			reserved := int64(0)
			if sp, ok := n.Spec.Disks[name]; ok {
				reserved = int64(sp.StorageReserved)
				if path == "" {
					path = sp.Path
				}
			}
			max := int64(st.StorageMaximum)
			if max <= 0 {
				continue
			}
			s.Disks[n.Name+":"+path] = DiskUse{Used: max - int64(st.StorageAvailable), Usable: max - reserved}
		}
	}
	return s
}

// EncodeCapacity / DecodeCapacity: like the history.
func EncodeCapacity(samples []CapacitySample, max int) (string, error) {
	if len(samples) > max {
		samples = samples[len(samples)-max:]
	}
	raw, err := json.Marshal(samples)
	if err != nil {
		return "", err
	}
	var buf bytes.Buffer
	zw, _ := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	_, _ = zw.Write(raw)
	_ = zw.Close()
	return digestGzipPrefix + base64.StdEncoding.EncodeToString(buf.Bytes()), nil
}

func DecodeCapacity(u *unstructured.Unstructured) ([]CapacitySample, error) {
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
	if len(raw) == 0 {
		return nil, nil
	}
	var out []CapacitySample
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("VolumeHistory %s: %w", u.GetName(), err)
	}
	return out, nil
}

// CapacityObject builds the VolumeHistory that holds the samples.
func CapacityObject(namespace string, samples []CapacitySample, max int) (*unstructured.Unstructured, error) {
	data, err := EncodeCapacity(samples, max)
	if err != nil {
		return nil, err
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "lhtx.io/v1alpha1", "kind": "VolumeHistory",
		"metadata": map[string]any{"name": CapacityHistory, "namespace": namespace,
			"labels": map[string]any{"app.kubernetes.io/managed-by": "lhtx-agent"}},
		"events": data,
	}}, nil
}

// ErrNoCapacity: no agent samples the disks here (yet).
var ErrNoCapacity = fmt.Errorf("no capacity samples – the lhtx agent takes them (lhtx agent --help)")

// Capacity reads the samples (oldest first).
func (c *Client) Capacity(ctx context.Context) ([]CapacitySample, error) {
	u, err := c.dyn.Resource(GVRVolumeHistory).Namespace(c.Namespace).Get(ctx, CapacityHistory, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, ErrNoCapacity
	}
	if err != nil {
		return nil, err
	}
	s, err := DecodeCapacity(u)
	if err != nil {
		return nil, err
	}
	sort.SliceStable(s, func(i, j int) bool { return s[i].At.Before(s[j].At) })
	return s, nil
}

// DiskForecast: one disk's trend.
type DiskForecast struct {
	Disk      string        `json:"disk"` // node:path
	Used      int64         `json:"used"`
	Usable    int64         `json:"usable"`
	PerDay    int64         `json:"perDay"`              // growth in bytes per day (negative = shrinking)
	FullIn    time.Duration `json:"fullIn,omitempty"`    // 0 = not filling up (or too little data)
	Span      time.Duration `json:"span"`                // the samples looked at
	TooLittle bool          `json:"tooLittle,omitempty"` // under 2 days or 4 samples
}

// ForecastWindow: the samples a forecast looks at.
const ForecastWindow = 14 * 24 * time.Hour

// Forecast fits a line through each disk's used bytes of the last
// ForecastWindow and says when it reaches the usable size.
func Forecast(samples []CapacitySample, now time.Time) []DiskForecast {
	type pt struct {
		t float64 // days
		y float64
	}
	series := map[string][]pt{}
	last := map[string]DiskUse{}
	first := map[string]time.Time{}
	for _, s := range samples {
		if now.Sub(s.At) > ForecastWindow {
			continue
		}
		for k, d := range s.Disks {
			series[k] = append(series[k], pt{t: s.At.Sub(now).Hours() / 24, y: float64(d.Used)})
			last[k] = d
			if _, ok := first[k]; !ok {
				first[k] = s.At
			}
		}
	}
	var out []DiskForecast
	for k, pts := range series {
		f := DiskForecast{Disk: k, Used: last[k].Used, Usable: last[k].Usable, Span: now.Sub(first[k]).Round(time.Minute)}
		if len(pts) < 4 || f.Span < 48*time.Hour {
			f.TooLittle = true
			out = append(out, f)
			continue
		}
		var sx, sy, sxx, sxy float64
		for _, p := range pts {
			sx += p.t
			sy += p.y
			sxx += p.t * p.t
			sxy += p.t * p.y
		}
		n := float64(len(pts))
		den := n*sxx - sx*sx
		if den != 0 {
			slope := (n*sxy - sx*sy) / den // bytes per day
			f.PerDay = int64(slope)
			if free := f.Usable - f.Used; slope > 0 && free > 0 {
				f.FullIn = time.Duration(float64(free) / slope * 24 * float64(time.Hour)).Round(time.Hour)
			}
		}
		out = append(out, f)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Disk < out[j].Disk })
	return out
}

// CapacityChecks (lhtx doctor, Health): disks that will be full soon by
// their trend. One GET; nothing without the agent.
func (c *Client) CapacityChecks(ctx context.Context) []Finding {
	s, err := c.Capacity(ctx)
	if err != nil {
		return nil
	}
	var out []Finding
	for _, f := range Forecast(s, time.Now()) {
		if f.FullIn == 0 {
			continue
		}
		days := int(f.FullIn.Hours() / 24)
		msg := fmt.Sprintf("full in ~%d days at the current growth (%s/day, %s of %s used)", days,
			format.Bytes(f.PerDay), format.Bytes(f.Used), format.Bytes(f.Usable))
		switch {
		case f.FullIn < 14*24*time.Hour:
			out = append(out, Finding{Severity: SevWarning, Check: "capacity", Object: f.Disk, Message: msg, Hint: "lhtx capacity"})
		case f.FullIn < 30*24*time.Hour:
			out = append(out, Finding{Severity: SevInfo, Check: "capacity", Object: f.Disk, Message: msg, Hint: "lhtx capacity"})
		}
	}
	return out
}
