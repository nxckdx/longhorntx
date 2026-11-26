package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"

	"github.com/nxckdx/longhorntx/internal/format"
	"github.com/nxckdx/longhorntx/internal/longhorn"
)

// Restore probe: "we have backups" is not "we can restore them". Every
// Interval the agent takes one volume – the one probed longest ago –
// restores its latest backup into a one-replica test volume, waits until
// Longhorn has read every block back from the backup target, and deletes
// the test volume. The result goes into the volume's history (failures
// are notable: lhtx history --bad, notifications, alerts). Needs --rw
// (creating and deleting Longhorn volumes).

// RestoreProbe configures it; Interval 0 = off.
type RestoreProbe struct {
	Interval time.Duration
	Timeout  time.Duration // per probe (default 2h)
	MaxSize  int64         // skip backups of larger volumes (0 = no limit)
}

func (a *Agent) runRestoreProbe(ctx context.Context) {
	p := a.Probe
	if p.Interval <= 0 {
		return
	}
	if !a.RW {
		a.Logf("restore probe: needs --rw (it creates and deletes test volumes) – off")
		return
	}
	if p.Timeout <= 0 {
		p.Timeout = 2 * time.Hour
	}
	a.Logf("restore probe every %s", p.Interval)
	a.cleanupProbes(ctx)
	for {
		wait := p.Interval
		if last := a.lastProbes(ctx); len(last) > 0 {
			newest := time.Time{}
			for _, t := range last {
				if t.After(newest) {
					newest = t
				}
			}
			wait = max(p.Interval-time.Since(newest), time.Minute)
		} else {
			wait = 5 * time.Minute // first one soon after the start
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		if err := a.probeOne(ctx, p); err != nil && ctx.Err() == nil {
			a.Logf("restore probe: %v", err)
		}
	}
}

// lastProbes: when each volume was probed last (from the cluster history).
func (a *Agent) lastProbes(ctx context.Context) map[string]time.Time {
	out := map[string]time.Time{}
	ev, err := a.c.History(ctx, longhorn.ClusterHistory)
	if err != nil {
		return out
	}
	for _, e := range ev {
		if e.Kind == longhorn.HistRestoreProbe || e.Kind == longhorn.HistRestoreProbeFailed {
			if e.At.After(out[e.Volume]) {
				out[e.Volume] = e.At
			}
		}
	}
	return out
}

// probeOne probes the volume probed longest ago (never = first).
func (a *Agent) probeOne(ctx context.Context, p RestoreProbe) error {
	vols, err := a.c.Volumes(ctx)
	if err != nil {
		return err
	}
	bvs, err := a.c.BackupVolumes(ctx)
	if err != nil {
		return err
	}
	byName := map[string]*longhorn.Volume{}
	for _, v := range vols {
		if v.Labels[longhorn.LabelRestoreProbe] == "" {
			byName[v.Name] = v
		}
	}
	last := a.lastProbes(ctx)
	type cand struct {
		vol  *longhorn.Volume
		last time.Time
	}
	var cs []cand
	seen := map[string]bool{}
	for _, bv := range bvs {
		v := byName[bv.Volume()]
		if v == nil || seen[v.Name] || bv.Status.LastBackupName == "" {
			continue // only volumes that still exist (the history belongs to them)
		}
		if p.MaxSize > 0 && int64(v.Spec.Size) > p.MaxSize {
			continue
		}
		seen[v.Name] = true
		cs = append(cs, cand{v, last[v.Name]})
	}
	if len(cs) == 0 {
		return nil
	}
	sort.Slice(cs, func(i, j int) bool {
		if !cs[i].last.Equal(cs[j].last) {
			return cs[i].last.Before(cs[j].last)
		}
		return cs[i].vol.Name < cs[j].vol.Name
	})
	vol := cs[0].vol
	bks, err := a.c.Backups(ctx, vol.Name)
	if err != nil {
		return err
	}
	var done []*longhorn.Backup
	for _, b := range bks {
		if b.Status.State == "Completed" && b.Status.URL != "" {
			done = append(done, b)
		}
	}
	b := longhorn.LatestBackup(done)
	if b == nil {
		return nil
	}
	return a.probe(ctx, p, vol, b)
}

func probeName(vol string) string {
	h := sha256.Sum256([]byte(vol + time.Now().String()))
	return "lhtx-probe-" + hex.EncodeToString(h[:])[:12]
}

// probeStartTimeout: a restore that has not started reading after this
// is failing (Longhorn reports some restore errors only in its log).
const probeStartTimeout = 15 * time.Minute

func (a *Agent) probe(ctx context.Context, p RestoreProbe, vol *longhorn.Volume, b *longhorn.Backup) error {
	start := time.Now()
	name := probeName(vol.Name)
	label := vol.Name
	if vol.PVC() != "" {
		label = vol.PVC()
	}
	a.Logf("restore probe: backup %s of %s into %s", b.Name, label, name)
	result := func(err error) error {
		e := longhorn.HistoryEvent{At: time.Now().UTC(), Took: time.Since(start).Round(time.Second)}
		if err == nil {
			e.Kind = longhorn.HistRestoreProbe
			e.Text = fmt.Sprintf("backup %s (%s) restored into a test volume – all data read back", b.Name, format.Bytes(int64(b.Status.Size)))
			a.op("restore-probe", "ok")
			a.metrics.probeOK.Store(time.Now().Unix())
		} else {
			e.Kind = longhorn.HistRestoreProbeFailed
			e.Text = fmt.Sprintf("backup %s could not be restored: %v", b.Name, err)
			a.op("restore-probe", "failed")
		}
		if r := a.history.Load(); r != nil {
			r.record(vol, []longhorn.HistoryEvent{e})
		} else if notifiable(e) {
			a.notifier.add(notice{At: e.At, Kind: e.Kind, Volume: vol.Name, PVC: vol.PVC(), Text: e.Text, Bad: true})
		}
		a.Logf("restore probe %s: %s", label, e.Text)
		return nil
	}
	obj, err := longhorn.RestoreProbeVolume(b, name)
	if err != nil {
		return result(err)
	}
	res := a.c.Dynamic().Resource(longhorn.GVRVolume).Namespace(a.c.Namespace)
	created, err := res.Create(ctx, &unstructured.Unstructured{Object: obj}, metav1.CreateOptions{FieldManager: "lhtx-agent"})
	if err != nil {
		return result(fmt.Errorf("create test volume: %w", err))
	}
	defer a.deleteProbe(context.WithoutCancel(ctx), name, string(created.GetUID()))
	pctx, cancel := context.WithTimeout(ctx, p.Timeout)
	defer cancel()
	for {
		select {
		case <-pctx.Done():
			if ctx.Err() != nil {
				return ctx.Err() // agent stopping: no verdict
			}
			return result(fmt.Errorf("not done within %s", p.Timeout))
		case <-time.After(10 * time.Second):
		}
		v, err := a.c.Volume(pctx, name)
		if err != nil {
			continue
		}
		msg, started := a.restoreState(pctx, name)
		if msg != "" {
			return result(fmt.Errorf("%s", msg))
		}
		if !started && time.Since(start) > probeStartTimeout {
			return result(fmt.Errorf("the restore did not start within %s (longhorn-manager's log says why: lhtx diag %s)", probeStartTimeout, name))
		}
		if v.Status.Robustness == "faulted" {
			return result(fmt.Errorf("test volume faulted"))
		}
		if v.Status.State == "detached" && !v.Status.RestoreRequired && v.Status.RestoreInitiated {
			return result(nil)
		}
	}
}

// restoreState: an error the engine reports for the restore, and whether
// it has started reading (progress, or a backup restored).
func (a *Agent) restoreState(ctx context.Context, vol string) (msg string, started bool) {
	engs, err := a.c.Engines(ctx, vol)
	if err != nil {
		return "", true // unknown: do not judge
	}
	for _, e := range engs {
		if e.Status.LastRestoredBackup != "" {
			started = true
		}
		for _, rs := range e.Status.RestoreStatus {
			if rs.Error != "" {
				return strings.TrimSpace(rs.Error), true
			}
			if rs.IsRestoring || rs.Progress > 0 || rs.LastRestored != "" {
				started = true
			}
		}
	}
	return "", started
}

func (a *Agent) deleteProbe(ctx context.Context, name, uid string) {
	res := a.c.Dynamic().Resource(longhorn.GVRVolume).Namespace(a.c.Namespace)
	opt := metav1.DeleteOptions{}
	if uid != "" {
		u := types.UID(uid)
		opt.Preconditions = &metav1.Preconditions{UID: &u}
	}
	if err := res.Delete(ctx, name, opt); err != nil && !apierrors.IsNotFound(err) {
		a.Logf("restore probe: delete %s: %v", name, err)
	}
}

// cleanupProbes deletes test volumes left behind by an agent that stopped
// during a probe.
func (a *Agent) cleanupProbes(ctx context.Context) {
	res := a.c.Dynamic().Resource(longhorn.GVRVolume).Namespace(a.c.Namespace)
	l, err := res.List(ctx, metav1.ListOptions{LabelSelector: longhorn.LabelRestoreProbe})
	if err != nil {
		return
	}
	for _, u := range l.Items {
		if strings.HasPrefix(u.GetName(), "lhtx-probe-") {
			a.Logf("restore probe: removing leftover test volume %s", u.GetName())
			a.deleteProbe(ctx, u.GetName(), string(u.GetUID()))
		}
	}
}
