package longhorn

import (
	"context"
	"sort"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/runtime/schema"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"
)

var (
	GVRSnapshot     = gvr("snapshots")
	GVRBackup       = gvr("backups")
	GVRBackupTarget = gvr("backuptargets")
	GVRRecurringJob = gvr("recurringjobs")
)

// Snapshots lists the snapshot CRs of a volume.
func (c *Client) Snapshots(ctx context.Context, volume string) ([]*VolumeSnapshot, error) {
	return list[VolumeSnapshot](ctx, c, GVRSnapshot, volumeSelector(volume))
}

// Backups lists backups of a volume. volume == "" lists all backups, which
// can be several MB on clusters with long retention – avoid that.
func (c *Client) Backups(ctx context.Context, volume string) ([]*Backup, error) {
	sel := ""
	if volume != "" {
		sel = LabelBackupVolume + "=" + volume
	}
	return list[Backup](ctx, c, GVRBackup, sel)
}

func (c *Client) BackupTargets(ctx context.Context) ([]*BackupTarget, error) {
	return list[BackupTarget](ctx, c, GVRBackupTarget, "")
}

func (c *Client) RecurringJobs(ctx context.Context) ([]*RecurringJob, error) {
	return list[RecurringJob](ctx, c, GVRRecurringJob, "")
}

func (c *Client) Settings(ctx context.Context) ([]*Setting, error) {
	return list[Setting](ctx, c, GVRSetting, "")
}

// ---------------------------------------------------------------- events

// Event is a trimmed core/v1 Event.
type Event struct {
	Time    time.Time `json:"time"`
	Type    string    `json:"type"` // Normal | Warning
	Reason  string    `json:"reason"`
	Object  string    `json:"object"` // Kind/name
	Message string    `json:"message"`
	Count   int32     `json:"count"`
}

// ObjectRef names an object whose events should be fetched.
type ObjectRef struct {
	Namespace, Kind, Name string
}

func (c *Client) core() (corev1client.CoreV1Interface, error) {
	c.coreOnce.Do(func() {
		cfg := rest.CopyConfig(c.Cluster.Config)
		// core types support protobuf: noticeably smaller than JSON
		cfg.ContentType = "application/vnd.kubernetes.protobuf"
		cfg.AcceptContentTypes = "application/vnd.kubernetes.protobuf,application/json"
		c.coreClient, c.coreErr = corev1client.NewForConfig(cfg)
	})
	return c.coreClient, c.coreErr
}

// Events fetches events for the given objects, using one small field-selected
// LIST per object (in parallel) instead of listing the whole namespace.
func (c *Client) Events(ctx context.Context, refs []ObjectRef) ([]Event, error) {
	cl, err := c.core()
	if err != nil {
		return nil, err
	}
	var (
		mu       sync.Mutex
		wg       sync.WaitGroup
		out      []Event
		firstErr error
	)
	for _, ref := range refs {
		wg.Add(1)
		go func(ref ObjectRef) {
			defer wg.Done()
			sel := fields.Set{"involvedObject.name": ref.Name}
			if ref.Kind != "" {
				sel["involvedObject.kind"] = ref.Kind
			}
			el, err := cl.Events(ref.Namespace).List(ctx, metav1.ListOptions{FieldSelector: sel.String()})
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				if firstErr == nil {
					firstErr = err
				}
				return
			}
			for i := range el.Items {
				out = append(out, trimEvent(&el.Items[i]))
			}
		}(ref)
	}
	wg.Wait()
	sort.Slice(out, func(i, j int) bool { return out[i].Time.After(out[j].Time) })
	if len(out) == 0 && firstErr != nil {
		return nil, firstErr
	}
	return out, nil
}

func trimEvent(e *corev1.Event) Event {
	t := e.LastTimestamp.Time
	if t.IsZero() {
		t = e.EventTime.Time
	}
	if t.IsZero() {
		t = e.FirstTimestamp.Time
	}
	if t.IsZero() {
		t = e.CreationTimestamp.Time
	}
	cnt := e.Count
	if e.Series != nil && e.Series.Count > cnt {
		cnt = e.Series.Count
	}
	return Event{
		Time:    t,
		Type:    e.Type,
		Reason:  e.Reason,
		Object:  e.InvolvedObject.Kind + "/" + e.InvolvedObject.Name,
		Message: strings.TrimSpace(e.Message),
		Count:   cnt,
	}
}

// VolumeEventRefs returns the objects relevant for a volume's events: the
// volume, its engines and replicas, and its PVC.
func VolumeEventRefs(namespace string, vv *VolumeView, engines []*Engine) []ObjectRef {
	refs := []ObjectRef{{Namespace: namespace, Kind: "Volume", Name: vv.Name}}
	for _, e := range engines {
		if e.Spec.VolumeName == vv.Name {
			refs = append(refs, ObjectRef{Namespace: namespace, Kind: "Engine", Name: e.Name})
		}
	}
	for _, r := range vv.Replicas {
		refs = append(refs, ObjectRef{Namespace: namespace, Kind: "Replica", Name: r.Name})
	}
	if ks := vv.Volume.Status.KubernetesStatus; ks.PVCName != "" {
		refs = append(refs, ObjectRef{Namespace: ks.Namespace, Kind: "PersistentVolumeClaim", Name: ks.PVCName})
	}
	return refs
}

// ---------------------------------------------------------------- recurring jobs per volume

const (
	labelRecurringJob      = "recurring-job.longhorn.io/"
	labelRecurringJobGroup = "recurring-job-group.longhorn.io/"
)

// VolumeRecurringJobs returns the jobs that apply to a volume, either
// directly (label recurring-job.longhorn.io/<job>) or via a group
// (recurring-job-group.longhorn.io/<group>).
func VolumeRecurringJobs(v *Volume, jobs []*RecurringJob) []*RecurringJob {
	direct, groups := map[string]bool{}, map[string]bool{}
	for k, val := range v.Labels {
		if val != "enabled" {
			continue
		}
		if n, ok := strings.CutPrefix(k, labelRecurringJob); ok {
			direct[n] = true
		}
		if n, ok := strings.CutPrefix(k, labelRecurringJobGroup); ok {
			groups[n] = true
		}
	}
	var out []*RecurringJob
	for _, j := range jobs {
		match := direct[j.Name]
		for _, g := range j.Spec.Groups {
			match = match || groups[g]
		}
		if match {
			out = append(out, j)
		}
	}
	sort.Slice(out, func(i, k int) bool { return out[i].Name < out[k].Name })
	return out
}

func get[T any](ctx context.Context, c *Client, g schema.GroupVersionResource, name string) (*T, error) {
	u, err := c.res(g).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	return convert[T](u)
}

func (c *Client) Replica(ctx context.Context, name string) (*Replica, error) {
	return get[Replica](ctx, c, GVRReplica, name)
}

func (c *Client) Node(ctx context.Context, name string) (*Node, error) {
	return get[Node](ctx, c, GVRNode, name)
}

func (c *Client) SnapshotCR(ctx context.Context, name string) (*VolumeSnapshot, error) {
	return get[VolumeSnapshot](ctx, c, GVRSnapshot, name)
}

func (c *Client) SettingCR(ctx context.Context, name string) (*Setting, error) {
	return get[Setting](ctx, c, GVRSetting, name)
}

func (c *Client) Orphans(ctx context.Context) ([]*Orphan, error) {
	return list[Orphan](ctx, c, GVROrphan, "")
}

func (c *Client) OrphanCR(ctx context.Context, name string) (*Orphan, error) {
	return get[Orphan](ctx, c, GVROrphan, name)
}
