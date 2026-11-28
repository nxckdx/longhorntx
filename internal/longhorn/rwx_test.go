package longhorn

import (
	"sort"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func rwxVol(name, state, node string, clients ...string) *Volume {
	v := mountVol(name, "app", name+"-pvc", node, state)
	v.Spec.AccessMode = "rwx"
	for _, c := range clients {
		v.Status.KubernetesStatus.WorkloadsStatus = append(v.Status.KubernetesStatus.WorkloadsStatus, WorkloadStatus{PodName: c, PodStatus: "Running"})
	}
	return v
}

func smPod(vol, node string, ready bool, restarts int32, finished time.Time) corev1.Pod {
	p := corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "share-manager-" + vol, Labels: map[string]string{LabelShareManager: vol}}}
	p.Spec.NodeName = node
	st := corev1.ConditionFalse
	if ready {
		st = corev1.ConditionTrue
	}
	p.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: st}}
	start := metav1.NewTime(finished.Add(-time.Hour))
	p.Status.StartTime = &start
	cs := corev1.ContainerStatus{RestartCount: restarts}
	if restarts > 0 {
		cs.LastTerminationState.Terminated = &corev1.ContainerStateTerminated{Reason: "Error", ExitCode: 1, FinishedAt: metav1.NewTime(finished)}
	}
	p.Status.ContainerStatuses = []corev1.ContainerStatus{cs}
	return p
}

func TestRWXFindings(t *testing.T) {
	now := time.Now()
	sm := func(state string) *ShareManager {
		return &ShareManager{Status: ShareManagerStatus{State: state, Endpoint: "nfs://10.0.0.1/x"}}
	}
	in := RWXInput{
		Volumes: []*Volume{
			rwxVol("ok", "attached", "n1", "a"),
			rwxVol("err", "attached", "n1", "b"),
			rwxVol("notready", "attached", "n1", "c"),
			rwxVol("noep", "attached", "n2", "d"),
			rwxVol("restarted", "attached", "n3", "e"),
			rwxVol("idle", "detached", ""),
			mountVol("rwo", "app", "rwo", "n1", "attached"), // not RWX
		},
		Shares: map[string]*ShareManager{"ok": sm("running"), "err": sm("error"), "notready": sm("running"),
			"noep": sm("running"), "restarted": sm("running"), "idle": sm("stopped")},
		Pods: []corev1.Pod{
			smPod("ok", "n1", true, 0, now), smPod("notready", "n1", false, 0, now),
			smPod("noep", "n1", true, 0, now), smPod("restarted", "n3", true, 2, now.Add(-time.Hour)),
		},
		Endpoints:  map[string]int{"ok": 1, "notready": 0, "restarted": 1},
		NFSOptions: map[string]string{"ok": "", "noep": "vers=4.0,hard"},
		History: []HistoryEvent{
			{At: now.Add(-20 * time.Minute), Kind: HistShareManager, Volume: "ok", Text: "NFS server does not answer (x)"},
			{At: now.Add(-10 * time.Minute), Kind: HistShareManager, Volume: "ok", Text: "NFS server not answering → running"},
			{At: now.Add(-5 * time.Minute), Kind: HistShareManager, Volume: "restarted", Text: "NFS server does not answer (x)"},
		},
	}
	rs := AnalyzeRWX(in, now)
	if len(rs) != 6 {
		t.Fatalf("rwx = %d", len(rs))
	}
	var got []string
	for _, f := range RWXFindings(rs, now) {
		got = append(got, f.Severity.String()+" "+f.Object+" "+strings.Fields(f.Message)[1])
	}
	want := []string{
		"CRITICAL app/err-pvc share",
		"WARNING app/noep-pvc volume",   // attached to n2, pod on n1
		"CRITICAL app/noep-pvc NFS",     // no endpoint
		"INFO app/noep-pvc with",        // hard
		"CRITICAL app/notready-pvc pod", // not ready
		"WARNING app/restarted-pvc share-manager",
		"CRITICAL app/restarted-pvc NFS", // agent: not answering
	}
	sort.Strings(got)
	sort.Strings(want)
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("findings:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	for _, r := range rs {
		if r.Volume == "ok" && !strings.HasPrefix(r.NFSOptions, "vers=4.1") {
			t.Errorf("default options: %q", r.NFSOptions)
		}
	}
}
