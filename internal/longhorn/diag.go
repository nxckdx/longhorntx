package longhorn

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/yaml"
)

// Diagnostics bundle for one volume: everything needed to understand (and
// report upstream) a stuck rebuild or snapshot removal. Read-only.

type DiagOptions struct {
	Since      time.Duration // log window
	LimitBytes int64         // per container
	NoLogs     bool
	FullLogs   bool // keep all log lines, not only those mentioning the volume
	Version    string
}

type DiagResult struct {
	Files []string
	Notes []string // non-fatal problems (e.g. a log that could not be read)
}

// CollectDiag writes a tar.gz to w.
func (c *Client) CollectDiag(ctx context.Context, vol *Volume, opt DiagOptions, w io.Writer) (*DiagResult, error) {
	gz := gzip.NewWriter(w)
	tw := tar.NewWriter(gz)
	res := &DiagResult{}
	now := time.Now()
	add := func(name string, data []byte) error {
		res.Files = append(res.Files, name)
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(data)), ModTime: now}); err != nil {
			return err
		}
		_, err := tw.Write(data)
		return err
	}
	note := func(format string, a ...any) { res.Notes = append(res.Notes, fmt.Sprintf(format, a...)) }

	sel := volumeSelector(vol.Name)
	objs := []struct {
		file, sel string
		gvr       schema.GroupVersionResource
	}{
		{"volume.yaml", "metadata.name=" + vol.Name, GVRVolume},
		{"engines.yaml", sel, GVREngine},
		{"replicas.yaml", sel, GVRReplica},
		{"snapshots.yaml", sel, GVRSnapshot},
	}
	var engines []*Engine
	var replicas []*Replica
	var snaps []*VolumeSnapshot
	for _, o := range objs {
		opts := metav1.ListOptions{LabelSelector: o.sel}
		if strings.HasPrefix(o.sel, "metadata.name=") {
			opts = metav1.ListOptions{FieldSelector: o.sel}
		}
		ul, err := c.res(o.gvr).List(ctx, opts)
		if err != nil {
			note("%s: %v", o.file, err)
			continue
		}
		for i := range ul.Items {
			u := &ul.Items[i]
			switch o.gvr {
			case GVREngine:
				if e, err := convert[Engine](u); err == nil {
					engines = append(engines, e)
				}
			case GVRReplica:
				if r, err := convert[Replica](u); err == nil {
					replicas = append(replicas, r)
				}
			case GVRSnapshot:
				if s, err := convert[VolumeSnapshot](u); err == nil {
					snaps = append(snaps, s)
				}
			}
		}
		if err := add(o.file, toYAML(ul.Items)); err != nil {
			return nil, err
		}
	}

	// nodes involved
	nodes := map[string]bool{}
	if vol.Status.CurrentNodeID != "" {
		nodes[vol.Status.CurrentNodeID] = true
	}
	for _, e := range engines {
		if e.Spec.NodeID != "" {
			nodes[e.Spec.NodeID] = true
		}
	}
	for _, r := range replicas {
		if r.Spec.NodeID != "" {
			nodes[r.Spec.NodeID] = true
		}
	}
	nodeNames := sortedKeys(nodes)
	var nodeItems []unstructured.Unstructured
	var nodeObjs []*Node
	for _, n := range nodeNames {
		u, err := c.res(GVRNode).Get(ctx, n, metav1.GetOptions{})
		if err != nil {
			note("node %s: %v", n, err)
			continue
		}
		nodeItems = append(nodeItems, *u)
		if no, err := convert[Node](u); err == nil {
			nodeObjs = append(nodeObjs, no)
		}
	}
	if err := add("nodes.yaml", toYAML(nodeItems)); err != nil {
		return nil, err
	}

	// events
	vv := BuildVolumeView(vol, replicas, IndexDisks(nodeObjs), ActiveEngines(engines)[vol.Name])
	evs, err := c.Events(ctx, VolumeEventRefs(c.Namespace, &vv, engines))
	if err != nil {
		note("events: %v", err)
	}
	var eb strings.Builder
	for _, e := range evs {
		fmt.Fprintf(&eb, "%s  %-7s  %-24s  %-40s  x%d  %s\n", e.Time.Format(time.RFC3339), e.Type, e.Reason, e.Object, max(e.Count, 1), e.Message)
	}
	if err := add("events.txt", []byte(eb.String())); err != nil {
		return nil, err
	}

	// summary + doctor findings for this volume
	in := DoctorInput{State: State{Volumes: []*Volume{vol}, Replicas: replicas, Nodes: nodeObjs, Engines: engines}, Snapshots: snaps, Now: now}
	if err := add("summary.txt", []byte(diagSummary(c, vol, vv, engines, snaps, Diagnose(in), opt, now))); err != nil {
		return nil, err
	}

	// logs
	if !opt.NoLogs {
		core, err := c.core()
		if err != nil {
			note("logs: %v", err)
		} else {
			for _, n := range nodeNames {
				for _, sel := range []string{"longhorn.io/component=instance-manager,longhorn.io/node=" + n, "app=longhorn-manager"} {
					pods, err := core.Pods(c.Namespace).List(ctx, metav1.ListOptions{LabelSelector: sel, FieldSelector: "spec.nodeName=" + n})
					if err != nil {
						note("pods %s on %s: %v", sel, n, err)
						continue
					}
					for _, p := range pods.Items {
						container := pickContainer(&p)
						data, err := c.podLog(ctx, p.Name, container, opt, vol.Name)
						if err != nil {
							note("log %s/%s: %v", p.Name, container, err)
							continue
						}
						if err := add("logs/"+p.Name+".log", data); err != nil {
							return nil, err
						}
					}
				}
			}
		}
	}

	if len(res.Notes) > 0 {
		if err := add("notes.txt", []byte(strings.Join(res.Notes, "\n")+"\n")); err != nil {
			return nil, err
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	return res, gz.Close()
}

func pickContainer(p *corev1.Pod) string {
	for _, c := range p.Spec.Containers {
		if c.Name == "longhorn-manager" || c.Name == "instance-manager" {
			return c.Name
		}
	}
	if len(p.Spec.Containers) > 0 {
		return p.Spec.Containers[0].Name
	}
	return ""
}

func (c *Client) podLog(ctx context.Context, pod, container string, opt DiagOptions, vol string) ([]byte, error) {
	core, err := c.core()
	if err != nil {
		return nil, err
	}
	since := int64(opt.Since.Seconds())
	lo := &corev1.PodLogOptions{Container: container, Timestamps: true}
	if since > 0 {
		lo.SinceSeconds = &since
	}
	if opt.LimitBytes > 0 {
		lo.LimitBytes = &opt.LimitBytes
	}
	rc, err := core.Pods(c.Namespace).GetLogs(pod, lo).Stream(ctx)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	if opt.FullLogs {
		return io.ReadAll(rc)
	}
	// keep lines that mention the volume (replica and engine names contain it)
	var out strings.Builder
	sc := bufio.NewScanner(rc)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		if l := sc.Text(); strings.Contains(l, vol) {
			out.WriteString(l + "\n")
		}
	}
	return []byte(out.String()), sc.Err()
}

func toYAML(items []unstructured.Unstructured) []byte {
	var b strings.Builder
	for i := range items {
		u := items[i].DeepCopy()
		unstructured.RemoveNestedField(u.Object, "metadata", "managedFields")
		y, err := yaml.Marshal(u.Object)
		if err != nil {
			continue
		}
		b.WriteString("---\n")
		b.Write(y)
	}
	return []byte(b.String())
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// diagSummary is a human-readable overview of the state that matters for
// stuck rebuilds and snapshot removal.
func diagSummary(c *Client, vol *Volume, vv VolumeView, engines []*Engine, snaps []*VolumeSnapshot, fs []Finding, opt DiagOptions, now time.Time) string {
	var b strings.Builder
	p := func(format string, a ...any) { fmt.Fprintf(&b, format+"\n", a...) }
	p("lhtx %s diagnostics · %s · context %s", opt.Version, now.UTC().Format(time.RFC3339), c.Cluster.Context)
	p("")
	p("volume     %s (%s)", vol.Name, dashIfEmpty(vol.PVC()))
	p("state      %s, %s, node %s, %d replicas wanted, data engine %s",
		vol.Status.State, vol.Status.Robustness, dashIfEmpty(vol.Status.CurrentNodeID), vol.Spec.NumberOfReplicas, dashIfEmpty(vol.Spec.DataEngine))
	p("size       %s (actual %s)", humanBytes(int64(vol.Spec.Size)), humanBytes(int64(vol.Status.ActualSize)))
	p("")
	p("replicas")
	for _, r := range vv.Replicas {
		reb := ""
		if r.Rebuild != nil {
			reb = fmt.Sprintf(" rebuild %d%% state=%s err=%s", r.Rebuild.Progress, dashIfEmpty(r.Rebuild.State), dashIfEmpty(r.Rebuild.Error))
		}
		p("  %-55s node=%-14s mode=%-3s health=%-10s healthyAt=%s failedAt=%s%s",
			r.Name, r.Node, dashIfEmpty(r.Mode), r.Health, dashIfEmpty(r.Replica.Spec.HealthyAt), dashIfEmpty(r.Replica.Spec.FailedAt), reb)
	}
	for _, e := range engines {
		p("")
		p("engine %s (active=%v, node %s, state %s)", e.Name, e.Spec.Active, e.Spec.NodeID, e.Status.CurrentState)
		for _, name := range sortedMapKeys(e.Status.ReplicaTransitionTimeMap) {
			t := e.Status.ReplicaTransitionTimeMap[name]
			p("  mode change %-55s %s (%s ago)", name, t, sinceText(t, now))
		}
		for _, addr := range sortedMapKeys(e.Status.RebuildStatus) {
			rs := e.Status.RebuildStatus[addr]
			p("  rebuildStatus %s: rebuilding=%v progress=%d state=%s from=%v err=%s", addr, rs.IsRebuilding, rs.Progress, rs.State, rs.FromReplicaAddressList, dashIfEmpty(rs.Error))
		}
		for _, addr := range sortedMapKeys(e.Status.PurgeStatus) {
			ps := e.Status.PurgeStatus[addr]
			p("  purgeStatus %s: purging=%v progress=%d state=%s err=%s", addr, ps.IsPurging, ps.Progress, dashIfEmpty(ps.State), dashIfEmpty(ps.Error))
		}
	}
	p("")
	p("snapshots (%d)", len(snaps))
	SortSnapshots(snaps)
	for _, s := range snaps {
		flags := []string{}
		if s.Status.MarkRemoved {
			flags = append(flags, "markRemoved")
		}
		if s.DeletionTimestamp != nil {
			flags = append(flags, "deleting since "+s.DeletionTimestamp.UTC().Format(time.RFC3339))
		}
		if !s.Status.ReadyToUse {
			flags = append(flags, "notReady")
		}
		p("  %-50s %s %10s %-8s parent=%s %s", s.Name, dashIfEmpty(s.Status.CreationTime), humanBytes(int64(s.Status.Size)), SnapshotKind(s), dashIfEmpty(s.Status.Parent), strings.Join(flags, ","))
	}
	p("")
	p("findings")
	if len(fs) == 0 {
		p("  none")
	}
	for _, f := range fs {
		p("  %-8s %-15s %s", f.Severity, f.Check, f.Message)
	}
	return b.String()
}

func sinceText(ts string, now time.Time) string {
	t, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		return "?"
	}
	return roundDur(now.Sub(t))
}

func sortedMapKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
