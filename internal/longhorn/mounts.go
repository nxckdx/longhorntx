package longhorn

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	storagev1client "k8s.io/client-go/kubernetes/typed/storage/v1"
	"k8s.io/client-go/rest"

	"github.com/nxckdx/longhorntx/internal/format"
)

// Mounts: pods that wait for a Longhorn volume although Longhorn looks
// fine. The usual cause is on the node: kubelet's volume manager or the
// Longhorn CSI plugin hangs, the node stays Ready, and nothing reports an
// error – the pod just sits in ContainerCreating. lhtx finds those pods
// (one field-selected LIST of pending pods), says how far each got
// (VolumeAttachment attached? Longhorn volume attached on that node?), and
// groups them per node: several pods stuck after the attach on one node is
// a node problem. For such nodes it can ask kubelet itself (/healthz and
// its volume manager metrics, through the API server's node proxy – needs
// the nodes/proxy right; skipped without).

// CSIDriver is Longhorn's CSI driver name.
const CSIDriver = "driver.longhorn.io"

// Mount stages: how far a waiting pod got.
const (
	StageAttach      = "attach"       // the volume is not attached to the node yet
	StageAttachError = "attach-error" // attaching failed
	StageMount       = "mount"        // attached, but kubelet / the CSI plugin did not mount it
)

// StuckMount is one pod waiting for one Longhorn volume.
type StuckMount struct {
	Pod     string        `json:"pod"` // namespace/name
	Node    string        `json:"node"`
	PVC     string        `json:"pvc"`
	Volume  string        `json:"volume"`
	Waiting time.Duration `json:"waiting"`
	Stage   string        `json:"stage"`
	Detail  string        `json:"detail,omitempty"`
	// NoEvents: kubelet reported nothing for the pod – typical for a hung
	// kubelet (a failing mount at least produces FailedMount events).
	NoEvents bool `json:"noEvents,omitempty"`
}

// NodeMounts is what lhtx knows about mounting on one node.
type NodeMounts struct {
	Node         string        `json:"node"`
	Distro       string        `json:"distro,omitempty"` // rke2 | k3s | talos | "" (plain kubelet)
	ControlPlane bool          `json:"controlPlane,omitempty"`
	Address      string        `json:"address,omitempty"`
	Ready        *bool         `json:"ready,omitempty"`
	CSI          string        `json:"csi"`                 // ok | not registered | unknown
	CSIPlugin    string        `json:"csiPlugin,omitempty"` // the longhorn-csi-plugin pod
	CSIPluginOK  bool          `json:"csiPluginOK"`
	CSIRestarts  int32         `json:"csiPluginRestarts,omitempty"`
	csiUnknown   bool          // the plugin pods could not be listed
	Stuck        int           `json:"stuck"`    // pods waiting after the attach
	Oldest       time.Duration `json:"oldest"`   // longest wait of those
	NotInUse     int           `json:"notInUse"` // … whose volume kubelet does not even list as in use
	Kubelet      *KubeletProbe `json:"kubelet,omitempty"`
}

// KubeletProbe is kubelet's own view, read through the node proxy.
type KubeletProbe struct {
	Healthz string             `json:"healthz"`          // "ok", the failing checks, or why it could not be read
	Desired int                `json:"desired"`          // Longhorn volumes kubelet's volume manager wants mounted
	Actual  int                `json:"actual"`           // … and has mounted
	Failed  map[string]float64 `json:"failed,omitempty"` // failed volume / CSI operations since kubelet started
	Err     string             `json:"error,omitempty"`
}

// MountReport is the result of Mounts.
type MountReport struct {
	Stuck []StuckMount `json:"stuck"`
	Nodes []NodeMounts `json:"nodes"`
}

// MountOptions: MinWait is how long a pod must wait to count (default 2
// minutes); Kubelet asks kubelet on nodes with pods stuck after the attach
// (and on ProbeNodes).
type MountOptions struct {
	MinWait    time.Duration
	Kubelet    bool
	ProbeNodes []string
}

// MountInput is everything AnalyzeMounts looks at (tests build it by hand).
type MountInput struct {
	Pods        []corev1.Pod // pending pods
	Volumes     []*Volume
	Attachments map[string]*storagev1.VolumeAttachment // by AttachmentName; nil entry = none
	Events      map[string][]Event                     // by pod namespace/name
	Nodes       map[string]*corev1.Node                // only nodes of interest
	CSINodes    map[string]bool                        // node → Longhorn's driver registered; nil = unknown
	CSIPods     []corev1.Pod                           // longhorn-csi-plugin pods
	CSIPodsRead bool                                   // CSIPods could be listed
	LHNodes     []string                               // Longhorn nodes (where the CSI plugin should run)
	Kubelet     map[string]*KubeletProbe
}

// AttachmentName is the VolumeAttachment name Kubernetes uses for a CSI
// volume on a node (pkg/volume/csi: sha256 of handle + driver + node).
func AttachmentName(handle, driver, node string) string {
	return fmt.Sprintf("csi-%x", sha256.Sum256([]byte(handle+driver+node)))
}

// waitingForVolumes: scheduled, not being deleted, and no container has
// started yet (volumes are mounted before the first init container runs).
func waitingForVolumes(p *corev1.Pod) bool {
	if p.Spec.NodeName == "" || p.DeletionTimestamp != nil || p.Status.Phase != corev1.PodPending {
		return false
	}
	for _, cs := range append(append([]corev1.ContainerStatus{}, p.Status.InitContainerStatuses...), p.Status.ContainerStatuses...) {
		if cs.State.Running != nil || cs.State.Terminated != nil || cs.LastTerminationState.Terminated != nil {
			return false
		}
		if w := cs.State.Waiting; w != nil && w.Reason != "" && w.Reason != "ContainerCreating" && w.Reason != "PodInitializing" {
			return false // image pull, config errors, …
		}
	}
	return true
}

func scheduledAt(p *corev1.Pod) time.Time {
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodScheduled && c.Status == corev1.ConditionTrue && !c.LastTransitionTime.IsZero() {
			return c.LastTransitionTime.Time
		}
	}
	return p.CreationTimestamp.Time
}

// longhornClaims: the pod's PVCs that are Longhorn volumes.
func longhornClaims(p *corev1.Pod, byPVC map[string]*Volume) []*Volume {
	var out []*Volume
	for _, v := range p.Spec.Volumes {
		if v.PersistentVolumeClaim == nil {
			continue
		}
		if vol := byPVC[p.Namespace+"/"+v.PersistentVolumeClaim.ClaimName]; vol != nil {
			out = append(out, vol)
		}
	}
	return out
}

func hasClaims(p *corev1.Pod) bool {
	for _, v := range p.Spec.Volumes {
		if v.PersistentVolumeClaim != nil {
			return true
		}
	}
	return false
}

func volumesByPVC(vols []*Volume) map[string]*Volume {
	m := map[string]*Volume{}
	for _, v := range vols {
		if pvc := v.BoundPVC(); pvc != "" {
			m[pvc] = v
		}
	}
	return m
}

// mountEventReasons: what kubelet / the attach-detach controller report.
var mountEventReasons = map[string]bool{"FailedMount": true, "FailedAttachVolume": true, "FailedMapVolume": true}

// AnalyzeMounts works out the stuck mounts and the per-node view.
func AnalyzeMounts(in MountInput, now time.Time, minWait time.Duration) MountReport {
	if minWait <= 0 {
		minWait = 2 * time.Minute
	}
	byPVC := volumesByPVC(in.Volumes)
	var rep MountReport
	for i := range in.Pods {
		p := &in.Pods[i]
		if !waitingForVolumes(p) {
			continue
		}
		wait := now.Sub(scheduledAt(p))
		if wait < minWait {
			continue
		}
		key := p.Namespace + "/" + p.Name
		var detail string
		noEvents := true
		for _, e := range in.Events[key] { // newest first
			if mountEventReasons[e.Reason] {
				noEvents = false
				if detail == "" {
					detail = e.Message
				}
			}
		}
		for _, vol := range longhornClaims(p, byPVC) {
			s := StuckMount{Pod: key, Node: p.Spec.NodeName, PVC: vol.PVC(), Volume: vol.Name,
				Waiting: wait.Round(time.Second), Detail: detail, NoEvents: noEvents}
			va, known := in.Attachments[AttachmentName(vol.Name, CSIDriver, p.Spec.NodeName)]
			switch {
			case !known:
				s.Stage = StageAttach // not looked up
			case va == nil:
				s.Stage = StageAttach
				if s.Detail == "" {
					s.Detail = "no VolumeAttachment yet (kube-controller-manager has not asked for the attach)"
				}
			case va.Status.AttachError != nil && !va.Status.Attached:
				s.Stage = StageAttachError
				s.Detail = strings.TrimSpace(va.Status.AttachError.Message)
			case !va.Status.Attached:
				s.Stage = StageAttach
				if s.Detail == "" {
					s.Detail = fmt.Sprintf("Longhorn is attaching (volume %s, on %s)", vol.Status.State, orDash(vol.Status.CurrentNodeID))
				}
			default:
				s.Stage = StageMount
				if vol.Status.State == "attached" && vol.Status.CurrentNodeID == p.Spec.NodeName && s.Detail == "" {
					s.Detail = "attached by Longhorn, but not mounted on the node"
				}
			}
			rep.Stuck = append(rep.Stuck, s)
		}
	}
	sort.Slice(rep.Stuck, func(i, j int) bool {
		if rep.Stuck[i].Node != rep.Stuck[j].Node {
			return rep.Stuck[i].Node < rep.Stuck[j].Node
		}
		return rep.Stuck[i].Waiting > rep.Stuck[j].Waiting
	})

	// per node: every Longhorn node (CSI registration, plugin pod) plus
	// the nodes of stuck pods
	nodes := map[string]*NodeMounts{}
	get := func(n string) *NodeMounts {
		if nm := nodes[n]; nm != nil {
			return nm
		}
		nm := &NodeMounts{Node: n, CSI: "unknown", csiUnknown: !in.CSIPodsRead || len(in.CSIPods) == 0}
		if in.CSINodes != nil {
			nm.CSI = "not registered"
			if in.CSINodes[n] {
				nm.CSI = "ok"
			}
		}
		if kn := in.Nodes[n]; kn != nil {
			nm.Distro, nm.ControlPlane, nm.Address = Distro(kn), isControlPlane(kn), nodeAddress(kn)
			for _, c := range kn.Status.Conditions {
				if c.Type == corev1.NodeReady {
					ok := c.Status == corev1.ConditionTrue
					nm.Ready = &ok
				}
			}
		}
		nm.Kubelet = in.Kubelet[n]
		nodes[n] = nm
		return nm
	}
	for _, n := range in.LHNodes {
		get(n)
	}
	for i := range in.CSIPods {
		p := &in.CSIPods[i]
		if p.Spec.NodeName == "" {
			continue
		}
		nm := get(p.Spec.NodeName)
		nm.CSIPlugin = p.Name
		nm.CSIPluginOK = podReady(p)
		for _, cs := range p.Status.ContainerStatuses {
			nm.CSIRestarts += cs.RestartCount
		}
	}
	for _, s := range rep.Stuck {
		nm := get(s.Node)
		if s.Stage != StageMount {
			continue
		}
		nm.Stuck++
		nm.Oldest = max(nm.Oldest, s.Waiting)
		if kn := in.Nodes[s.Node]; kn != nil && !volumeInUse(kn, s.Volume) {
			nm.NotInUse++
		}
	}
	for n, k := range in.Kubelet {
		get(n).Kubelet = k
	}
	for _, nm := range nodes {
		rep.Nodes = append(rep.Nodes, *nm)
	}
	sort.Slice(rep.Nodes, func(i, j int) bool { return rep.Nodes[i].Node < rep.Nodes[j].Node })
	return rep
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func podReady(p *corev1.Pod) bool {
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

// volumeInUse: kubelet's volume manager lists volumes it wants mounted in
// node.status.volumesInUse.
func volumeInUse(n *corev1.Node, vol string) bool {
	for _, v := range n.Status.VolumesInUse {
		if strings.HasSuffix(string(v), "^"+vol) {
			return true
		}
	}
	return false
}

// Distro guesses how kubelet runs on the node, for the restart hint.
func Distro(n *corev1.Node) string {
	kv, os := n.Status.NodeInfo.KubeletVersion, n.Status.NodeInfo.OSImage
	switch {
	case strings.Contains(kv, "+rke2"):
		return "rke2"
	case strings.Contains(kv, "+k3s"):
		return "k3s"
	case strings.HasPrefix(os, "Talos"):
		return "talos"
	}
	return ""
}

func isControlPlane(n *corev1.Node) bool {
	_, cp := n.Labels["node-role.kubernetes.io/control-plane"]
	_, master := n.Labels["node-role.kubernetes.io/master"]
	return cp || master
}

func nodeAddress(n *corev1.Node) string {
	for _, a := range n.Status.Addresses {
		if a.Type == corev1.NodeInternalIP {
			return a.Address
		}
	}
	return ""
}

// KubeletRestart is the command that restarts kubelet on the node.
func (nm NodeMounts) KubeletRestart() string {
	switch nm.Distro {
	case "rke2":
		if nm.ControlPlane {
			return "systemctl restart rke2-server (on the node; restarts its control plane too)"
		}
		return "systemctl restart rke2-agent (on the node)"
	case "k3s":
		if nm.ControlPlane {
			return "systemctl restart k3s (on the node; restarts its control plane too)"
		}
		return "systemctl restart k3s-agent (on the node)"
	case "talos":
		addr := nm.Address
		if addr == "" {
			addr = nm.Node
		}
		return "talosctl -n " + addr + " service kubelet restart"
	}
	return "systemctl restart kubelet (on the node)"
}

// MountFindings turns a report into doctor findings.
func MountFindings(rep MountReport) []Finding {
	var out []Finding
	for _, nm := range rep.Nodes {
		obj := "node/" + nm.Node
		if nm.CSI == "not registered" {
			out = append(out, Finding{Severity: SevCritical, Check: "mount", Object: obj,
				Message: "Longhorn's CSI driver is not registered with kubelet here – pods on this node cannot mount Longhorn volumes",
				Hint:    csiHint(nm)})
		} else if nm.CSIPlugin == "" && !nm.csiUnknown {
			out = append(out, Finding{Severity: SevWarning, Check: "mount", Object: obj,
				Message: "no longhorn-csi-plugin pod on this node", Hint: "kubectl -n longhorn-system get ds longhorn-csi-plugin"})
		} else if nm.CSIPlugin != "" && !nm.CSIPluginOK {
			out = append(out, Finding{Severity: SevWarning, Check: "mount", Object: obj,
				Message: fmt.Sprintf("longhorn-csi-plugin pod %s is not ready (%d restarts)", nm.CSIPlugin, nm.CSIRestarts),
				Hint:    "lhtx diag – or delete the pod, the DaemonSet recreates it"})
		}
		if nm.Stuck == 0 {
			continue
		}
		sev := SevWarning
		if nm.Stuck > 1 || nm.Oldest > 10*time.Minute {
			sev = SevCritical
		}
		msg := fmt.Sprintf("%d pod(s) wait for up to %s for Longhorn volumes that are attached here but not mounted – kubelet or the CSI plugin on this node hangs",
			nm.Stuck, format.Duration(nm.Oldest))
		if nm.NotInUse > 0 {
			msg += fmt.Sprintf("; kubelet does not even list %d of them as in use (its volume manager is not working on them)", nm.NotInUse)
		}
		if k := nm.Kubelet; k != nil && k.Err == "" {
			msg += "; kubelet: " + k.Summary()
		}
		out = append(out, Finding{Severity: sev, Check: "mount", Object: obj, Message: msg, Hint: csiHint(nm)})
	}
	for _, s := range rep.Stuck {
		if s.Stage == StageMount {
			continue // counted on the node
		}
		sev := SevWarning
		if s.Stage == StageAttachError || s.Waiting > 10*time.Minute {
			sev = SevCritical
		}
		msg := fmt.Sprintf("pod %s waits %s for the volume to attach to %s", s.Pod, format.Duration(s.Waiting), s.Node)
		if s.Detail != "" {
			msg += ": " + oneLine(s.Detail, 160)
		}
		out = append(out, Finding{Severity: sev, Check: "mount", Object: "volume/" + s.Volume, Message: msg, Hint: "lhtx mounts; lhtx diag " + s.Volume})
	}
	return out
}

// csiHint: the gentlest fix first (the CSI plugin pod), then kubelet.
func csiHint(nm NodeMounts) string {
	h := "restart kubelet: " + nm.KubeletRestart()
	if nm.CSIPlugin != "" {
		h = "first delete " + nm.CSIPlugin + " (kubectl -n longhorn-system delete pod " + nm.CSIPlugin + "); if that does not help, " + h
	}
	return h
}

func oneLine(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > n {
		s = s[:n-1] + "…"
	}
	return s
}

// Summary: kubelet's state in one line.
func (k *KubeletProbe) Summary() string {
	if k.Err != "" {
		return k.Err
	}
	parts := []string{"healthz " + k.Healthz}
	parts = append(parts, fmt.Sprintf("%d/%d Longhorn volumes mounted", k.Actual, k.Desired))
	if len(k.Failed) > 0 {
		var keys []string
		for op := range k.Failed {
			keys = append(keys, op)
		}
		sort.Strings(keys)
		var f []string
		for _, op := range keys {
			f = append(f, fmt.Sprintf("%s %g", op, k.Failed[op]))
		}
		parts = append(parts, "failed: "+strings.Join(f, ", "))
	}
	return strings.Join(parts, ", ")
}

// ---------------------------------------------------------------- reading

var errNoStorage = errors.New("storage.k8s.io client not available")

func (c *Client) storage() (storagev1client.StorageV1Interface, error) {
	c.storageOnce.Do(func() {
		if c.Cluster == nil || c.Cluster.Config == nil {
			c.storageErr = errNoStorage
			return
		}
		cfg := rest.CopyConfig(c.Cluster.Config)
		cfg.ContentType = "application/vnd.kubernetes.protobuf"
		cfg.AcceptContentTypes = "application/vnd.kubernetes.protobuf,application/json"
		c.storageClient, c.storageErr = storagev1client.NewForConfig(cfg)
	})
	return c.storageClient, c.storageErr
}

// Mounts reads what AnalyzeMounts needs: one LIST of pending pods; only if
// some wait for Longhorn volumes, their VolumeAttachments (GET by name),
// events and nodes. The CSI checks are two small LISTs (CSINodes, the
// longhorn-csi-plugin pods). vols: the Longhorn volumes (nil = read them).
func (c *Client) Mounts(ctx context.Context, vols []*Volume, lhNodes []string, opt MountOptions) (*MountReport, error) {
	cl, err := c.core()
	if err != nil {
		return nil, err
	}
	pl, err := cl.Pods("").List(ctx, metav1.ListOptions{FieldSelector: pendingPodsSelector})
	if err != nil {
		return nil, err
	}
	if opt.MinWait <= 0 {
		opt.MinWait = 2 * time.Minute
	}
	now := time.Now()
	if vols == nil { // read the volumes only if a pod waits for a PVC
		for _, p := range pl.Items {
			if waitingForVolumes(&p) && now.Sub(scheduledAt(&p)) >= opt.MinWait && hasClaims(&p) {
				if vols, err = c.Volumes(ctx); err != nil {
					return nil, err
				}
				break
			}
		}
	}
	in := MountInput{Volumes: vols, LHNodes: lhNodes, Attachments: map[string]*storagev1.VolumeAttachment{},
		Events: map[string][]Event{}, Nodes: map[string]*corev1.Node{}, Kubelet: map[string]*KubeletProbe{}}
	byPVC := volumesByPVC(vols)
	var refs []ObjectRef
	type want struct{ handle, node string }
	var vas []want
	interesting := map[string]bool{}
	for _, p := range pl.Items {
		if !waitingForVolumes(&p) || now.Sub(scheduledAt(&p)) < opt.MinWait {
			continue
		}
		claims := longhornClaims(&p, byPVC)
		if len(claims) == 0 {
			continue
		}
		in.Pods = append(in.Pods, p)
		refs = append(refs, ObjectRef{Namespace: p.Namespace, Kind: "Pod", Name: p.Name})
		interesting[p.Spec.NodeName] = true
		for _, v := range claims {
			vas = append(vas, want{v.Name, p.Spec.NodeName})
		}
	}
	for _, n := range opt.ProbeNodes {
		interesting[n] = true
	}

	var wg sync.WaitGroup
	var mu sync.Mutex
	if len(refs) > 0 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			evs, _ := c.Events(ctx, refs)
			mu.Lock()
			defer mu.Unlock()
			for _, e := range evs {
				// Object is Kind/name; the namespace is the pod's
				for _, r := range refs {
					if e.Object == "Pod/"+r.Name {
						k := r.Namespace + "/" + r.Name
						in.Events[k] = append(in.Events[k], e)
					}
				}
			}
		}()
	}
	if st, err := c.storage(); err == nil {
		for _, w := range vas {
			wg.Add(1)
			go func() {
				defer wg.Done()
				name := AttachmentName(w.handle, CSIDriver, w.node)
				va, err := st.VolumeAttachments().Get(ctx, name, metav1.GetOptions{})
				mu.Lock()
				defer mu.Unlock()
				switch {
				case err == nil:
					in.Attachments[name] = va
				case apierrors.IsNotFound(err):
					in.Attachments[name] = nil
				}
			}()
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			l, err := st.CSINodes().List(ctx, metav1.ListOptions{})
			if err != nil {
				return
			}
			m := map[string]bool{}
			for _, cn := range l.Items {
				for _, d := range cn.Spec.Drivers {
					if d.Name == CSIDriver {
						m[cn.Name] = true
					}
				}
			}
			mu.Lock()
			in.CSINodes = m
			mu.Unlock()
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		l, err := cl.Pods(c.Namespace).List(ctx, metav1.ListOptions{LabelSelector: "app=longhorn-csi-plugin"})
		if err != nil {
			return
		}
		mu.Lock()
		in.CSIPods, in.CSIPodsRead = l.Items, true
		mu.Unlock()
	}()
	for n := range interesting {
		wg.Add(1)
		go func() {
			defer wg.Done()
			kn, err := cl.Nodes().Get(ctx, n, metav1.GetOptions{})
			if err != nil {
				return
			}
			mu.Lock()
			in.Nodes[n] = kn
			mu.Unlock()
		}()
	}
	wg.Wait()
	if !in.CSIPodsRead && in.CSINodes == nil {
		in.LHNodes = nil // nothing known about the CSI side: no rows just for that
	}

	rep := AnalyzeMounts(in, now, opt.MinWait)
	if opt.Kubelet {
		probe := map[string]bool{}
		for _, n := range opt.ProbeNodes {
			probe[n] = true
		}
		for _, nm := range rep.Nodes {
			if nm.Stuck > 0 {
				probe[nm.Node] = true
			}
		}
		for n := range probe {
			wg.Add(1)
			go func() {
				defer wg.Done()
				k := c.ProbeKubelet(ctx, n)
				mu.Lock()
				in.Kubelet[n] = k
				mu.Unlock()
			}()
		}
		wg.Wait()
		if len(probe) > 0 {
			rep = AnalyzeMounts(in, now, opt.MinWait)
		}
	}
	return &rep, nil
}

// NodeNames: the names of Longhorn nodes.
func NodeNames(nodes []*Node) []string {
	out := make([]string, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, n.Name)
	}
	return out
}

// MountChecks (lhtx doctor, Health): stuck mounts and CSI problems.
// Asks kubelet only on nodes with pods stuck after the attach.
func (c *Client) MountChecks(ctx context.Context, vols []*Volume, lhNodes []string) []Finding {
	rep, err := c.Mounts(ctx, vols, lhNodes, MountOptions{Kubelet: true})
	if err != nil {
		return nil
	}
	return MountFindings(*rep)
}

// kubeletTimeout: a hung kubelet may not answer at all.
const kubeletTimeout = 10 * time.Second

// ProbeKubelet reads kubelet's /healthz and /metrics through the API
// server's node proxy (needs get on nodes/proxy).
func (c *Client) ProbeKubelet(ctx context.Context, node string) *KubeletProbe {
	cl, err := c.core()
	if err != nil {
		return &KubeletProbe{Err: err.Error()}
	}
	rc := cl.RESTClient()
	if rc == nil {
		return &KubeletProbe{Err: "no API client"}
	}
	ctx, cancel := context.WithTimeout(ctx, kubeletTimeout)
	defer cancel()
	k := &KubeletProbe{}
	body, err := rc.Get().AbsPath("/api/v1/nodes", node, "proxy", "healthz").Param("verbose", "1").DoRaw(ctx)
	switch {
	case apierrors.IsForbidden(err):
		k.Err = "not allowed to ask kubelet (needs get on nodes/proxy)"
		return k
	case ctx.Err() != nil:
		k.Err = fmt.Sprintf("kubelet did not answer within %s – it hangs", kubeletTimeout)
		return k
	case err != nil && len(body) == 0:
		k.Err = "kubelet /healthz: " + oneLine(err.Error(), 160)
		return k
	}
	k.Healthz = parseHealthz(body)
	body, err = rc.Get().AbsPath("/api/v1/nodes", node, "proxy", "metrics").DoRaw(ctx)
	if err != nil {
		if ctx.Err() != nil {
			k.Err = fmt.Sprintf("kubelet /metrics did not answer within %s – it hangs", kubeletTimeout)
		}
		return k
	}
	parseKubeletMetrics(body, k)
	return k
}

// parseHealthz: "ok", or the failing checks of the verbose output
// ("[-]syncloop failed: reason withheld").
func parseHealthz(b []byte) string {
	var bad []string
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		if name, ok := strings.CutPrefix(strings.TrimSpace(sc.Text()), "[-]"); ok {
			bad = append(bad, strings.Fields(name)[0])
		}
	}
	if len(bad) > 0 {
		return "failing: " + strings.Join(bad, ", ")
	}
	if strings.Contains(string(b), "ok") {
		return "ok"
	}
	return oneLine(string(b), 80)
}

// parseKubeletMetrics picks kubelet's volume manager and the failed
// operations for Longhorn volumes out of the Prometheus text.
func parseKubeletMetrics(b []byte, k *KubeletProbe) {
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if !strings.Contains(line, CSIDriver) || strings.HasPrefix(line, "#") {
			continue
		}
		name, labels, val, ok := parseMetricLine(line)
		if !ok {
			continue
		}
		switch name {
		case "volume_manager_total_volumes":
			switch labels["state"] {
			case "desired_state_of_world":
				k.Desired += int(val)
			case "actual_state_of_world":
				k.Actual += int(val)
			}
		case "storage_operation_duration_seconds_count":
			if st := labels["status"]; st != "" && st != "success" && val > 0 {
				addFailed(k, labels["operation_name"], val)
			}
		case "csi_operations_seconds_count":
			if code := labels["grpc_status_code"]; code != "" && code != "OK" && val > 0 {
				m := labels["method_name"]
				if i := strings.LastIndex(m, "/"); i >= 0 {
					m = m[i+1:]
				}
				addFailed(k, m+" "+code, val)
			}
		}
	}
}

func addFailed(k *KubeletProbe, op string, v float64) {
	if k.Failed == nil {
		k.Failed = map[string]float64{}
	}
	k.Failed[op] += v
}

// parseMetricLine: name{a="b",…} value
func parseMetricLine(line string) (string, map[string]string, float64, bool) {
	open := strings.IndexByte(line, '{')
	end := strings.LastIndexByte(line, '}')
	if open < 0 || end < open {
		return "", nil, 0, false
	}
	labels := map[string]string{}
	rest := line[open+1 : end]
	for rest != "" {
		eq := strings.IndexByte(rest, '=')
		if eq < 0 || eq+1 >= len(rest) || rest[eq+1] != '"' {
			break
		}
		key := strings.TrimSpace(rest[:eq])
		rest = rest[eq+2:]
		var val strings.Builder
		i := 0
		for ; i < len(rest); i++ {
			if rest[i] == '\\' && i+1 < len(rest) {
				i++
				val.WriteByte(rest[i])
				continue
			}
			if rest[i] == '"' {
				break
			}
			val.WriteByte(rest[i])
		}
		labels[key] = val.String()
		if i >= len(rest) {
			break
		}
		rest = strings.TrimPrefix(rest[i+1:], ",")
	}
	f := strings.Fields(line[end+1:])
	if len(f) == 0 {
		return "", nil, 0, false
	}
	v, err := strconv.ParseFloat(f[0], 64)
	if err != nil {
		return "", nil, 0, false
	}
	return line[:open], labels, v, true
}

// pendingPodsSelector is the field selector Mounts lists with.
var pendingPodsSelector = fields.OneTermEqualSelector("status.phase", string(corev1.PodPending)).String()
