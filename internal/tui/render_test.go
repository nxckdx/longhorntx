package tui

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"

	"github.com/nxckdx/longhorntx/internal/longhorn"
)

func TestDetailRebuildBox(t *testing.T) {
	var v longhorn.Volume
	_ = json.Unmarshal([]byte(`{"metadata":{"name":"pvc-3f2a"},"spec":{"size":"21474836480","numberOfReplicas":2},
		"status":{"state":"attached","robustness":"degraded","currentNodeID":"node-2",
		"kubernetesStatus":{"namespace":"gitea","pvcName":"gitea-data"}}}`), &v)
	var ok, reb longhorn.Replica
	_ = json.Unmarshal([]byte(`{"metadata":{"name":"pvc-3f2a-r-aaaa"},"spec":{"volumeName":"pvc-3f2a","nodeID":"node-1",
		"diskID":"u1","diskPath":"/var/lib/longhorn","healthyAt":"2026-01-01T00:00:00Z"},"status":{"currentState":"running"}}`), &ok)
	_ = json.Unmarshal([]byte(`{"metadata":{"name":"pvc-3f2a-r-bbbb"},"spec":{"volumeName":"pvc-3f2a","nodeID":"node-3",
		"diskID":"u3","diskPath":"/var/lib/longhorn"},"status":{"currentState":"running","storageIP":"10.0.0.3","port":1}}`), &reb)
	var e longhorn.Engine
	_ = json.Unmarshal([]byte(`{"metadata":{"name":"pvc-3f2a-e-0"},"spec":{"volumeName":"pvc-3f2a","active":true},
		"status":{"replicaModeMap":{"pvc-3f2a-r-aaaa":"RW","pvc-3f2a-r-bbbb":"WO"},
		"rebuildStatus":{"tcp://10.0.0.3:1":{"isRebuilding":true,"progress":64}}}}`), &e)
	var n longhorn.Node
	_ = json.Unmarshal([]byte(`{"metadata":{"name":"node-1"},"status":{"diskStatus":{"nvme0":{"diskUUID":"u1","diskPath":"/var/lib/longhorn"}}}}`), &n)

	m := newModel(t.Context(), Options{})
	m.width, m.height = 100, 30
	m.views = longhorn.Build(longhorn.State{
		Volumes: []*longhorn.Volume{&v}, Replicas: []*longhorn.Replica{&ok, &reb},
		Nodes: []*longhorn.Node{&n}, Engines: []*longhorn.Engine{&e},
	})
	now := time.Now()
	m.rates.Observe("rebuild/pvc-3f2a-r-bbbb", 60, int64(v.Spec.Size), now.Add(-10*time.Second))
	m.rates.Observe("rebuild/pvc-3f2a-r-bbbb", 64, int64(v.Spec.Size), now)
	p := &page{kind: pDetail, vol: "pvc-3f2a"}
	m.stack = append(m.stack, p)

	out := ansi.Strip(m.viewDetail(p))
	if os.Getenv("SHOW") != "" {
		t.Log("\n" + out)
	}
	for _, want := range []string{"gitea/gitea-data", "⚠ degraded", "┌─ node-3", "◐ rebuilding", "64%", "MiB/s · ~", "/var/lib/longhorn"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

func TestActivityPage(t *testing.T) {
	m := newModel(t.Context(), Options{})
	m.width, m.height = 140, 20
	var b longhorn.Backup
	_ = json.Unmarshal([]byte(`{"metadata":{"name":"backup-abc","labels":{"backup-volume":"pvc-x"}},
		"status":{"state":"InProgress","progress":42,"volumeName":"pvc-x","volumeSize":"10737418240"}}`), &b)
	m.acts = []longhorn.Activity{
		{Kind: "rebuild", Volume: "pvc-3f2a", PVC: "gitea/gitea-data", Target: "pvc-3f2a-r-bbbb", Node: "node-3", Progress: 64, Total: 20 << 30},
		longhorn.BuildActivities(nil, nil, []*longhorn.Backup{&b})[0],
	}
	now := time.Now()
	m.rates.Observe("rebuild/pvc-3f2a-r-bbbb", 60, 20<<30, now.Add(-10*time.Second))
	m.rates.Observe("rebuild/pvc-3f2a-r-bbbb", 64, 20<<30, now)
	m.stack = []*page{{kind: pActivity}}
	out := ansi.Strip(m.viewPage(m.top()))
	if os.Getenv("SHOW") != "" {
		t.Log("\n" + out)
	}
	for _, want := range []string{"rebuild", "gitea/gitea-data", "replica r-bbbb", "64%", "~", "backup", "42%", "InProgress"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

func TestVolumeListRebuildAndProblemsFirst(t *testing.T) {
	m := newModel(t.Context(), Options{})
	m.width, m.height = 150, 12
	mk := func(name, pvc, health string) *longhorn.Volume {
		var v longhorn.Volume
		_ = json.Unmarshal([]byte(`{"metadata":{"name":"`+name+`"},"spec":{"size":"21474836480","numberOfReplicas":2},
			"status":{"state":"attached","robustness":"`+health+`","currentNodeID":"node-1",
			"kubernetesStatus":{"namespace":"`+strings.Split(pvc, "/")[0]+`","pvcName":"`+strings.Split(pvc, "/")[1]+`"}}}`), &v)
		return &v
	}
	vols := []*longhorn.Volume{mk("pvc-a", "app/aaa", "healthy"), mk("pvc-b", "app/bbb", "healthy"), mk("pvc-z", "zz/zzz", "degraded")}
	m.views = longhorn.Build(longhorn.State{Volumes: vols})
	m.acts = []longhorn.Activity{{Kind: "rebuild", Volume: "pvc-b", Target: "pvc-b-r-1", Progress: 64, Total: 20 << 30}}
	m.stack = []*page{{kind: pVolumes}}
	out := ansi.Strip(m.viewPage(m.top()))
	if !strings.Contains(out, "rebuild") || !strings.Contains(out, "64%") || !strings.Contains(out, "█") {
		t.Errorf("rebuild bar missing:\n%s", out)
	}
	m.problemsFirst = true
	out = ansi.Strip(m.viewPage(m.top()))
	if os.Getenv("SHOW") != "" {
		t.Log("\n" + out)
	}
	lines := strings.Split(out, "\n")
	if !strings.Contains(lines[1], "zz/zzz") || !strings.Contains(lines[2], "app/bbb") {
		t.Errorf("problems first order wrong:\n%s", out)
	}
}
