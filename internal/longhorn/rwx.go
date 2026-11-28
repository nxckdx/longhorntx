package longhorn

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	discoveryv1client "k8s.io/client-go/kubernetes/typed/discovery/v1"
	"k8s.io/client-go/rest"

	"github.com/nxckdx/longhorntx/internal/format"
)

// RWX volumes: Longhorn serves a ReadWriteMany volume over NFS from a
// share-manager pod (one per volume, on the node the volume is attached
// to), reached through a Service named like the volume. When that pod hangs
// or restarts, every client pod hangs or gets I/O errors – and Longhorn's
// volume still looks healthy. lhtx checks the chain: ShareManager state →
// share-manager pod (ready, restarts, node) → the Service's endpoints → the
// NFS mount options the clients use.

var GVRShareManager = gvr("sharemanagers")

// LabelShareManager: on the share-manager pod and the Service/EndpointSlice.
const LabelShareManager = "longhorn.io/share-manager"

// DefaultNFSOptions: what Longhorn's CSI plugin mounts with when the
// StorageClass sets no nfsOptions (verified on v1.12: it falls back to
// "soft" where the kernel has no softerr).
const DefaultNFSOptions = "vers=4.1,noresvport,softerr,timeo=600,retrans=5"

type ShareManager struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata"`
	Status            ShareManagerStatus `json:"status"`
}

type ShareManagerStatus struct {
	State    string `json:"state"` // starting | running | stopping | stopped | error
	Endpoint string `json:"endpoint"`
	OwnerID  string `json:"ownerID"`
}

func (c *Client) ShareManagers(ctx context.Context) ([]*ShareManager, error) {
	return list[ShareManager](ctx, c, GVRShareManager, "")
}

// RWXVolume is the share chain of one RWX volume.
type RWXVolume struct {
	Volume      string        `json:"volume"`
	PVC         string        `json:"pvc,omitempty"`
	State       string        `json:"state"`      // the volume's
	Share       string        `json:"shareState"` // the ShareManager's ("" = none)
	Endpoint    string        `json:"endpoint,omitempty"`
	AttachedTo  string        `json:"attachedTo,omitempty"`
	Pod         string        `json:"pod,omitempty"` // share-manager pod
	PodNode     string        `json:"podNode,omitempty"`
	PodReady    bool          `json:"podReady"`
	PodAge      time.Duration `json:"podAge,omitempty"`
	Restarts    int32         `json:"restarts,omitempty"`
	LastRestart string        `json:"lastRestart,omitempty"` // reason, when
	restartedAt time.Time
	Endpoints   int      `json:"readyEndpoints"`
	EPKnown     bool     `json:"-"`
	NFSOptions  string   `json:"nfsOptions,omitempty"` // "" = unknown
	Clients     []string `json:"clients,omitempty"`    // pods using it (name: status)
	// NFSDownSince: the lhtx agent's NFS probe gets no answer since then
	// (from the cluster history; zero = answering or no agent).
	NFSDownSince time.Time `json:"nfsDownSince,omitzero"`
}

// RWXInput is what AnalyzeRWX looks at.
type RWXInput struct {
	Volumes    []*Volume
	Shares     map[string]*ShareManager
	Pods       []corev1.Pod      // share-manager pods
	Endpoints  map[string]int    // service (= volume) → ready endpoints; nil = unknown
	NFSOptions map[string]string // volume → the PV's nfsOptions ("" = Longhorn's default); nil = not read
	History    []HistoryEvent    // the cluster history (agent): NFS probe results
}

// RWXVolumes: the RWX volumes of a list.
func RWXVolumes(vols []*Volume) []*Volume {
	var out []*Volume
	for _, v := range vols {
		if v.Spec.AccessMode == "rwx" {
			out = append(out, v)
		}
	}
	return out
}

func AnalyzeRWX(in RWXInput, now time.Time) []RWXVolume {
	pods := map[string]*corev1.Pod{}
	for i := range in.Pods {
		p := &in.Pods[i]
		if v := p.Labels[LabelShareManager]; v != "" {
			pods[v] = p
		}
	}
	nfsDown := map[string]time.Time{} // the latest share-manager event per volume decides
	nfsSeen := map[string]time.Time{}
	for _, e := range in.History {
		if e.Kind != HistShareManager || !e.At.After(nfsSeen[e.Volume]) {
			continue
		}
		nfsSeen[e.Volume] = e.At
		if strings.HasPrefix(e.Text, "NFS server does not answer") {
			nfsDown[e.Volume] = e.At
		} else {
			delete(nfsDown, e.Volume)
		}
	}
	var out []RWXVolume
	for _, v := range RWXVolumes(in.Volumes) {
		r := RWXVolume{Volume: v.Name, PVC: v.BoundPVC(), State: v.Status.State, AttachedTo: v.Status.CurrentNodeID}
		if sm := in.Shares[v.Name]; sm != nil {
			r.Share, r.Endpoint = sm.Status.State, sm.Status.Endpoint
		}
		if p := pods[v.Name]; p != nil {
			r.Pod, r.PodNode, r.PodReady = p.Name, p.Spec.NodeName, podReady(p)
			if p.Status.StartTime != nil {
				r.PodAge = now.Sub(p.Status.StartTime.Time).Round(time.Second)
			}
			for _, cs := range p.Status.ContainerStatuses {
				r.Restarts += cs.RestartCount
				if t := cs.LastTerminationState.Terminated; t != nil && t.FinishedAt.After(r.restartedAt) {
					r.restartedAt = t.FinishedAt.Time
					r.LastRestart = fmt.Sprintf("%s (exit %d) %s ago", orDash(t.Reason), t.ExitCode, format.Duration(now.Sub(t.FinishedAt.Time)))
				}
			}
		}
		if in.Endpoints != nil {
			r.EPKnown = true
			r.Endpoints = in.Endpoints[v.Name]
		}
		if in.NFSOptions != nil {
			if o, ok := in.NFSOptions[v.Name]; ok {
				r.NFSOptions = o
				if o == "" {
					r.NFSOptions = DefaultNFSOptions + " (default)"
				}
			}
		}
		for _, w := range v.Status.KubernetesStatus.WorkloadsStatus {
			if v.Status.KubernetesStatus.LastPodRefAt == "" && w.PodName != "" {
				r.Clients = append(r.Clients, w.PodName+": "+w.PodStatus)
			}
		}
		if t, ok := nfsDown[v.Name]; ok && r.Share == "running" {
			r.NFSDownSince = t
		}
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Volume < out[j].Volume })
	return out
}

// inUse: something mounts the volume (or should).
func (r RWXVolume) inUse() bool {
	return r.State == "attached" || r.State == "attaching" || len(r.Clients) > 0
}

func (r RWXVolume) label() string {
	if r.PVC != "" {
		return r.PVC
	}
	return r.Volume
}

// RWXFindings: what is wrong with the share chains.
func RWXFindings(rs []RWXVolume, now time.Time) []Finding {
	var out []Finding
	add := func(sev Severity, r RWXVolume, msg, hint string) {
		out = append(out, Finding{Severity: sev, Check: "rwx", Object: r.label(), Message: msg, Hint: hint})
	}
	restart := func(r RWXVolume) string {
		return "lhtx --rw restart share-manager " + r.Volume + " (Longhorn starts a new one; clients reconnect)"
	}
	for _, r := range rs {
		switch {
		case r.Share == "error":
			add(SevCritical, r, "the share manager (NFS server of this RWX volume) is in error – client pods hang or get I/O errors", restart(r))
			continue
		case r.inUse() && r.Share == "running" && r.Pod == "":
			add(SevCritical, r, "the share manager is running, but its pod is gone – clients cannot reach the NFS server", restart(r))
			continue
		case r.inUse() && r.Share == "running" && !r.PodReady:
			add(SevCritical, r, fmt.Sprintf("share-manager pod %s is not ready – clients cannot reach the NFS server", r.Pod), restart(r))
			continue
		case r.inUse() && r.Share != "running" && (r.Pod == "" || r.PodAge > 5*time.Minute):
			add(SevWarning, r, fmt.Sprintf("in use, but the share manager is %q – clients wait for the NFS server", orDash(r.Share)), "lhtx diag "+r.Volume)
			continue
		}
		if r.Share != "running" {
			continue
		}
		if !r.NFSDownSince.IsZero() {
			add(SevCritical, r, fmt.Sprintf("the NFS server does not answer (lhtx agent, for %s) although its pod is Ready – client pods hang",
				format.Duration(now.Sub(r.NFSDownSince))), restart(r))
		}
		if r.Endpoint == "" {
			add(SevWarning, r, "the share manager runs but has no NFS endpoint yet", "lhtx diag "+r.Volume)
		}
		if r.EPKnown && r.Endpoints == 0 {
			add(SevCritical, r, "the NFS Service has no ready endpoint – clients cannot connect", restart(r))
		}
		if r.Restarts > 0 && now.Sub(r.restartedAt) < 24*time.Hour {
			add(SevWarning, r, fmt.Sprintf("the share-manager pod restarted %d time(s), last: %s – clients may have seen I/O errors or stale file handles", r.Restarts, r.LastRestart),
				"lhtx diag "+r.Volume+"; client pods that still fail: restart them")
		}
		if r.AttachedTo != "" && r.PodNode != "" && r.AttachedTo != r.PodNode {
			add(SevWarning, r, fmt.Sprintf("the volume is attached to %s but the share-manager pod runs on %s", r.AttachedTo, r.PodNode), "lhtx diag "+r.Volume)
		}
		if hasNFSOption(r.NFSOptions, "hard") {
			add(SevInfo, r, "mounted with \"hard\": if the share manager goes away, client I/O blocks until it is back and pods cannot be killed (Longhorn's default is softerr)",
				"nfsOptions in the StorageClass")
		}
	}
	return out
}

func hasNFSOption(opts, name string) bool {
	opts, _, _ = strings.Cut(opts, " (")
	for _, o := range strings.Split(opts, ",") {
		if strings.TrimSpace(o) == name {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------- reading

func (c *Client) discovery() (discoveryv1client.DiscoveryV1Interface, error) {
	c.discoveryOnce.Do(func() {
		if c.Cluster == nil || c.Cluster.Config == nil {
			c.discoveryErr = errNoStorage
			return
		}
		cfg := rest.CopyConfig(c.Cluster.Config)
		cfg.ContentType = "application/vnd.kubernetes.protobuf"
		cfg.AcceptContentTypes = "application/vnd.kubernetes.protobuf,application/json"
		c.discoveryClient, c.discoveryErr = discoveryv1client.NewForConfig(cfg)
	})
	return c.discoveryClient, c.discoveryErr
}

// RWX reads the share chains of the RWX volumes in vols (nil = read the
// volumes). Nothing is read when there is no RWX volume. nfsOptions needs
// one PV GET per RWX volume (lhtx rwx, not doctor).
func (c *Client) RWX(ctx context.Context, vols []*Volume, nfsOptions bool) ([]RWXVolume, error) {
	if vols == nil {
		var err error
		if vols, err = c.Volumes(ctx); err != nil {
			return nil, err
		}
	}
	rwx := RWXVolumes(vols)
	if len(rwx) == 0 {
		return nil, nil
	}
	in := RWXInput{Volumes: rwx, Shares: map[string]*ShareManager{}}
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		firstErr error
	)
	wg.Add(1)
	go func() {
		defer wg.Done()
		sms, err := c.ShareManagers(ctx)
		mu.Lock()
		defer mu.Unlock()
		if err != nil {
			firstErr = err
			return
		}
		for _, sm := range sms {
			in.Shares[sm.Name] = sm
		}
	}()
	cl, err := c.core()
	if err != nil {
		return nil, err
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		l, err := cl.Pods(c.Namespace).List(ctx, metav1.ListOptions{LabelSelector: LabelShareManager})
		if err == nil {
			mu.Lock()
			in.Pods = l.Items
			mu.Unlock()
		}
	}()
	if dc, err := c.discovery(); err == nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			l, err := dc.EndpointSlices(c.Namespace).List(ctx, metav1.ListOptions{LabelSelector: LabelShareManager})
			if err != nil {
				return
			}
			m := map[string]int{}
			for _, s := range l.Items {
				m[s.Labels[discoveryv1.LabelServiceName]] += readyEndpoints(&s)
			}
			mu.Lock()
			in.Endpoints = m
			mu.Unlock()
		}()
	}
	if nfsOptions {
		in.NFSOptions = map[string]string{}
		for _, v := range rwx {
			pv := v.Status.KubernetesStatus.PVName
			if pv == "" {
				continue
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				p, err := cl.PersistentVolumes().Get(ctx, pv, metav1.GetOptions{})
				if err != nil || p.Spec.CSI == nil {
					return
				}
				opts := p.Spec.CSI.VolumeAttributes["nfsOptions"]
				if opts == "" && len(p.Spec.MountOptions) > 0 {
					opts = strings.Join(p.Spec.MountOptions, ",")
				}
				mu.Lock()
				in.NFSOptions[v.Name] = opts
				mu.Unlock()
			}()
		}
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		if ev, err := c.History(ctx, ClusterHistory); err == nil {
			mu.Lock()
			in.History = ev
			mu.Unlock()
		}
	}()
	wg.Wait()
	if firstErr != nil {
		return nil, firstErr
	}
	return AnalyzeRWX(in, time.Now()), nil
}

func readyEndpoints(s *discoveryv1.EndpointSlice) int {
	n := 0
	for _, e := range s.Endpoints {
		if e.Conditions.Ready == nil || *e.Conditions.Ready {
			n++
		}
	}
	return n
}

// RWXChecks (lhtx doctor, Health): broken share chains. Nothing is read
// without RWX volumes.
func (c *Client) RWXChecks(ctx context.Context, vols []*Volume) []Finding {
	rs, err := c.RWX(ctx, vols, false)
	if err != nil {
		return nil
	}
	return RWXFindings(rs, time.Now())
}
