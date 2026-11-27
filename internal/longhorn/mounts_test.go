package longhorn

import (
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func mountVol(name, ns, pvc, node, state string) *Volume {
	v := &Volume{}
	v.Name = name
	v.Status.KubernetesStatus = KubernetesStatus{Namespace: ns, PVCName: pvc, PVName: name}
	v.Status.State = state
	v.Status.CurrentNodeID = node
	return v
}

func waitingPod(ns, name, node, pvc string, since time.Time) corev1.Pod {
	p := corev1.Pod{}
	p.Namespace, p.Name = ns, name
	p.CreationTimestamp = metav1.NewTime(since)
	p.Spec.NodeName = node
	p.Spec.Volumes = []corev1.Volume{{Name: "data", VolumeSource: corev1.VolumeSource{
		PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: pvc}}}}
	p.Status.Phase = corev1.PodPending
	p.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodScheduled, Status: corev1.ConditionTrue, LastTransitionTime: metav1.NewTime(since)}}
	p.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "app",
		State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ContainerCreating"}}}}
	return p
}

func TestAnalyzeMountsNodeHang(t *testing.T) {
	now := time.Now()
	vols := []*Volume{
		mountVol("pvc-a", "app", "data-a", "n1", "attached"),
		mountVol("pvc-b", "app", "data-b", "n1", "attached"),
		mountVol("pvc-c", "app", "data-c", "", "attaching"),
	}
	pullErr := waitingPod("app", "img", "n2", "data-a", now.Add(-time.Hour))
	pullErr.Status.ContainerStatuses[0].State.Waiting.Reason = "ImagePullBackOff"
	in := MountInput{
		Pods: []corev1.Pod{
			waitingPod("app", "a-0", "n1", "data-a", now.Add(-15*time.Minute)),
			waitingPod("app", "b-0", "n1", "data-b", now.Add(-5*time.Minute)),
			waitingPod("app", "c-0", "n2", "data-c", now.Add(-3*time.Minute)),
			waitingPod("app", "fresh", "n1", "data-b", now.Add(-30*time.Second)), // too new
			pullErr, // not a mount problem
		},
		Volumes: vols,
		Attachments: map[string]*storagev1.VolumeAttachment{
			AttachmentName("pvc-a", CSIDriver, "n1"): {Status: storagev1.VolumeAttachmentStatus{Attached: true}},
			AttachmentName("pvc-b", CSIDriver, "n1"): {Status: storagev1.VolumeAttachmentStatus{Attached: true}},
			AttachmentName("pvc-c", CSIDriver, "n2"): {Status: storagev1.VolumeAttachmentStatus{Attached: false}},
		},
		Events: map[string][]Event{"app/b-0": {{Reason: "FailedMount", Message: "MountVolume.MountDevice failed: rpc error: code = DeadlineExceeded"}}},
		Nodes: map[string]*corev1.Node{"n1": {
			ObjectMeta: metav1.ObjectMeta{Name: "n1"},
			Status: corev1.NodeStatus{
				NodeInfo:     corev1.NodeSystemInfo{KubeletVersion: "v1.33.5+rke2r1"},
				VolumesInUse: []corev1.UniqueVolumeName{"kubernetes.io/csi/driver.longhorn.io^pvc-b"},
				Conditions:   []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}},
			}}},
		CSINodes: map[string]bool{"n1": true, "n2": true, "n3": false},
		CSIPods: []corev1.Pod{
			{ObjectMeta: metav1.ObjectMeta{Name: "longhorn-csi-plugin-x"}, Spec: corev1.PodSpec{NodeName: "n1"},
				Status: corev1.PodStatus{Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}}},
		},
		CSIPodsRead: true,
		LHNodes:     []string{"n1", "n2", "n3"},
	}
	rep := AnalyzeMounts(in, now, 0)
	if len(rep.Stuck) != 3 {
		t.Fatalf("stuck = %+v", rep.Stuck)
	}
	if s := rep.Stuck[0]; s.Pod != "app/a-0" || s.Stage != StageMount || !s.NoEvents {
		t.Errorf("first = %+v", s)
	}
	if s := rep.Stuck[1]; s.Pod != "app/b-0" || s.NoEvents || !strings.Contains(s.Detail, "DeadlineExceeded") {
		t.Errorf("second = %+v", s)
	}
	if s := rep.Stuck[2]; s.Stage != StageAttach || !strings.Contains(s.Detail, "attaching") {
		t.Errorf("third = %+v", s)
	}
	var n1 NodeMounts
	for _, nm := range rep.Nodes {
		if nm.Node == "n1" {
			n1 = nm
		}
	}
	if n1.Stuck != 2 || n1.NotInUse != 1 || n1.Distro != "rke2" || n1.Oldest != 15*time.Minute {
		t.Errorf("n1 = %+v", n1)
	}

	f := MountFindings(rep)
	var got []string
	for _, x := range f {
		got = append(got, x.Severity.String()+" "+x.Object)
	}
	want := []string{"CRITICAL node/n1", "WARNING node/n2", "CRITICAL node/n3", "WARNING volume/pvc-c"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("findings = %v", got)
	}
	if !strings.Contains(f[0].Hint, "rke2-agent") || !strings.Contains(f[0].Hint, "restart csi-plugin n1") {
		t.Errorf("hint = %s", f[0].Hint)
	}
}

func TestKubeletRestart(t *testing.T) {
	for _, c := range []struct {
		nm   NodeMounts
		want string
	}{
		{NodeMounts{Distro: "rke2"}, "rke2-agent"},
		{NodeMounts{Distro: "rke2", ControlPlane: true}, "rke2-server"},
		{NodeMounts{Distro: "k3s"}, "k3s-agent"},
		{NodeMounts{Distro: "talos", Address: "10.0.0.5"}, "talosctl -n 10.0.0.5 service kubelet restart"},
		{NodeMounts{}, "systemctl restart kubelet"},
	} {
		if got := c.nm.KubeletRestart(); !strings.Contains(got, c.want) {
			t.Errorf("%+v: %s", c.nm, got)
		}
	}
}

func TestAttachmentName(t *testing.T) {
	// as kubelet / kube-controller-manager compute it
	got := AttachmentName("pvc-1", "driver.longhorn.io", "node-1")
	if !strings.HasPrefix(got, "csi-") || len(got) != 4+64 {
		t.Errorf("name = %s", got)
	}
}

func TestParseKubelet(t *testing.T) {
	m := `# HELP volume_manager_total_volumes x
volume_manager_total_volumes{plugin_name="kubernetes.io/csi:driver.longhorn.io",state="actual_state_of_world"} 2
volume_manager_total_volumes{plugin_name="kubernetes.io/csi:driver.longhorn.io",state="desired_state_of_world"} 4
volume_manager_total_volumes{plugin_name="kubernetes.io/configmap",state="desired_state_of_world"} 9
storage_operation_duration_seconds_count{migrated="false",operation_name="volume_mount",status="fail-unknown",volume_plugin="kubernetes.io/csi:driver.longhorn.io"} 3
storage_operation_duration_seconds_count{migrated="false",operation_name="volume_mount",status="success",volume_plugin="kubernetes.io/csi:driver.longhorn.io"} 7
csi_operations_seconds_count{driver_name="driver.longhorn.io",grpc_status_code="DeadlineExceeded",method_name="/csi.v1.Node/NodeStageVolume",migrated="false"} 2
csi_operations_seconds_count{driver_name="driver.longhorn.io",grpc_status_code="OK",method_name="/csi.v1.Node/NodeStageVolume",migrated="false"} 5
`
	k := &KubeletProbe{Healthz: parseHealthz([]byte("[+]ping ok\n[+]log ok\n[-]syncloop failed: reason withheld\nhealthz check failed"))}
	parseKubeletMetrics([]byte(m), k)
	if k.Desired != 4 || k.Actual != 2 || k.Failed["volume_mount"] != 3 || k.Failed["NodeStageVolume DeadlineExceeded"] != 2 || len(k.Failed) != 2 {
		t.Errorf("probe = %+v", k)
	}
	if k.Healthz != "failing: syncloop" {
		t.Errorf("healthz = %q", k.Healthz)
	}
	if s := k.Summary(); !strings.Contains(s, "2/4 Longhorn volumes mounted") {
		t.Errorf("summary = %s", s)
	}
	if h := parseHealthz([]byte("[+]ping ok\n[+]syncloop ok\nhealthz check passed")); h != "ok" {
		t.Errorf("healthy = %q", h)
	}
}

func TestShortMountMessage(t *testing.T) {
	got := shortMountMessage(`MountVolume.MountDevice failed for volume "pvc-939b" : rpc error: code = Unavailable desc = x`)
	if got != "MountDevice failed: rpc error: code = Unavailable desc = x" {
		t.Errorf("got %q", got)
	}
}

func TestAnalyzeMountsRWX(t *testing.T) {
	now := time.Now()
	v := mountVol("pvc-r", "app", "shared", "n2", "attached")
	v.Spec.AccessMode = "rwx"
	in := MountInput{
		Pods:        []corev1.Pod{waitingPod("app", "w-0", "n1", "shared", now.Add(-5*time.Minute))},
		Volumes:     []*Volume{v},
		Attachments: map[string]*storagev1.VolumeAttachment{AttachmentName("pvc-r", CSIDriver, "n1"): {Status: storagev1.VolumeAttachmentStatus{Attached: true}}},
	}
	rep := AnalyzeMounts(in, now, 0)
	for _, nm := range rep.Nodes {
		if nm.Stuck != 0 {
			t.Errorf("RWX counted on the node: %+v", nm)
		}
	}
	f := MountFindings(rep)
	if len(f) != 1 || !strings.Contains(f[0].Hint, "restart share-manager pvc-r") {
		t.Errorf("findings = %+v", f)
	}
}
