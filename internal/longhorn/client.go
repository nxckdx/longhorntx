package longhorn

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/dynamic"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/metadata"

	"github.com/nxckdx/longhorntx/internal/fuzzy"
	"github.com/nxckdx/longhorntx/internal/kube"
)

// MinVersion is the oldest Longhorn release lhtx supports (v1beta2 CRDs,
// rebuildStatus/purgeStatus layout as used here).
const MinVersion = "1.6.0"

var (
	GVRVolume  = gvr("volumes")
	GVRReplica = gvr("replicas")
	GVREngine  = gvr("engines")
	GVRNode    = gvr("nodes")
	GVRSetting = gvr("settings")
)

func gvr(r string) schema.GroupVersionResource {
	return schema.GroupVersionResource{Group: Group, Version: Version, Resource: r}
}

// Client does one-shot reads. It is used by the CLI and as the basis for the
// informer-backed Store.
type Client struct {
	Cluster   *kube.Cluster
	Namespace string
	dyn       dynamic.Interface

	coreOnce   sync.Once
	coreClient corev1client.CoreV1Interface
	coreErr    error

	meta metadata.Interface // secret names (secrets.go), created on first use
}

func NewClient(c *kube.Cluster, namespace string) (*Client, error) {
	if namespace == "" {
		namespace = DefaultNamespace
	}
	dyn, err := dynamic.NewForConfig(c.Config)
	if err != nil {
		return nil, err
	}
	return &Client{Cluster: c, Namespace: namespace, dyn: dyn}, nil
}

// NewClientFrom builds a client on given API clients (demo mode, tests).
func NewClientFrom(c *kube.Cluster, namespace string, dyn dynamic.Interface, core corev1client.CoreV1Interface) *Client {
	cl := &Client{Cluster: c, Namespace: namespace, dyn: dyn, coreClient: core}
	cl.coreOnce.Do(func() {})
	return cl
}

func (c *Client) res(g schema.GroupVersionResource) dynamic.ResourceInterface {
	return c.dyn.Resource(g).Namespace(c.Namespace)
}

// convert turns an unstructured object into one of our slim structs. Going
// through JSON keeps custom unmarshalers (Quantity) working and silently drops
// every field we do not model.
func convert[T any](u *unstructured.Unstructured) (*T, error) {
	b, err := json.Marshal(u.Object)
	if err != nil {
		return nil, err
	}
	out := new(T)
	if err := json.Unmarshal(b, out); err != nil {
		return nil, fmt.Errorf("decode %s %s: %w", u.GetKind(), u.GetName(), err)
	}
	return out, nil
}

func list[T any](ctx context.Context, c *Client, g schema.GroupVersionResource, selector string) ([]*T, error) {
	ul, err := c.res(g).List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		return nil, err
	}
	out := make([]*T, 0, len(ul.Items))
	for i := range ul.Items {
		o, err := convert[T](&ul.Items[i])
		if err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, nil
}

func (c *Client) Volumes(ctx context.Context) ([]*Volume, error) {
	return list[Volume](ctx, c, GVRVolume, "")
}

func (c *Client) Volume(ctx context.Context, name string) (*Volume, error) {
	u, err := c.res(GVRVolume).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	return convert[Volume](u)
}

// Replicas lists replicas; volume == "" lists all of them.
func (c *Client) Replicas(ctx context.Context, volume string) ([]*Replica, error) {
	return list[Replica](ctx, c, GVRReplica, volumeSelector(volume))
}

// Engines lists engines; volume == "" lists all of them.
func (c *Client) Engines(ctx context.Context, volume string) ([]*Engine, error) {
	return list[Engine](ctx, c, GVREngine, volumeSelector(volume))
}

func (c *Client) Nodes(ctx context.Context) ([]*Node, error) {
	return list[Node](ctx, c, GVRNode, "")
}

func (c *Client) Setting(ctx context.Context, name string) (string, error) {
	u, err := c.res(GVRSetting).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return "", err
	}
	s, err := convert[Setting](u)
	if err != nil {
		return "", err
	}
	return s.Value, nil
}

// LonghornVersion returns the installed Longhorn version ("v1.12.1").
func (c *Client) LonghornVersion(ctx context.Context) (string, error) {
	return c.Setting(ctx, "current-longhorn-version")
}

// ResolveVolume accepts a Longhorn volume name, a PV name, "namespace/pvc" or
// a bare PVC name (if unambiguous) and returns the matching volume.
func ResolveVolume(vols []*Volume, ref string) (*Volume, error) {
	var matches []*Volume
	for _, v := range vols {
		ks := v.Status.KubernetesStatus
		switch {
		case v.Name == ref, ks.PVName == ref, v.PVC() == ref:
			return v, nil
		case !strings.Contains(ref, "/") && ks.PVCName == ref:
			matches = append(matches, v)
		}
	}
	switch len(matches) {
	case 0:
		return resolveFuzzy(vols, ref)
	case 1:
		return matches[0], nil
	}
	var cands []string
	for _, m := range matches {
		cands = append(cands, m.PVC())
	}
	return nil, fmt.Errorf("PVC name %q is ambiguous, use namespace/pvc: %s", ref, strings.Join(cands, ", "))
}

func volumeSelector(volume string) string {
	if volume == "" {
		return ""
	}
	return LabelVolume + "=" + volume
}

// VersionAtLeast compares "v1.12.1"-style versions, ignoring suffixes.
func VersionAtLeast(have, want string) bool {
	h, w := parseVer(have), parseVer(want)
	for i := range 3 {
		if h[i] != w[i] {
			return h[i] > w[i]
		}
	}
	return true
}

func parseVer(s string) [3]int {
	s = strings.TrimPrefix(s, "v")
	if i := strings.IndexAny(s, "-+"); i >= 0 {
		s = s[:i]
	}
	var v [3]int
	for i, p := range strings.SplitN(s, ".", 3) {
		v[i], _ = strconv.Atoi(p)
	}
	return v
}

// ListCount lists a Longhorn resource like an informer's initial sync does
// (resourceVersion=0, served from the apiserver cache) and returns the
// number of items and the collection's resourceVersion. Used by measure.
func (c *Client) ListCount(ctx context.Context, resource string) (int, string, error) {
	ul, err := c.res(gvr(resource)).List(ctx, metav1.ListOptions{ResourceVersion: "0"})
	if err != nil {
		return 0, "", err
	}
	return len(ul.Items), ul.GetResourceVersion(), nil
}

// WatchFrom watches a Longhorn resource starting at rv (no initial events).
func (c *Client) WatchFrom(ctx context.Context, resource, rv string) (watch.Interface, error) {
	return c.res(gvr(resource)).Watch(ctx, metav1.ListOptions{ResourceVersion: rv, AllowWatchBookmarks: true})
}

// LatestResourceVersion returns the current resourceVersion of a collection
// using a LIST with limit=1 (cheap even for large collections).
func (c *Client) LatestResourceVersion(ctx context.Context, resource string) (string, error) {
	ul, err := c.res(gvr(resource)).List(ctx, metav1.ListOptions{Limit: 1})
	if err != nil {
		return "", err
	}
	return ul.GetResourceVersion(), nil
}

// resolveFuzzy is the fallback when nothing matches exactly: fzf-style
// matching on namespace/pvc and volume name ("valk data" → valkey-data).
// Only a unique hit is accepted.
func resolveFuzzy(vols []*Volume, ref string) (*Volume, error) {
	pat := fuzzy.Parse(ref)
	type hit struct {
		v     *Volume
		score int
	}
	var hits []hit
	for _, v := range vols {
		if s, ok := pat.Match([]string{v.PVC(), v.Name}); ok {
			hits = append(hits, hit{v, s})
		}
	}
	switch len(hits) {
	case 0:
		return nil, fmt.Errorf("volume %q not found", ref)
	case 1:
		return hits[0].v, nil
	}
	sort.SliceStable(hits, func(i, j int) bool { return hits[i].score > hits[j].score })
	var cands []string
	for i, h := range hits {
		if i == 8 {
			cands = append(cands, fmt.Sprintf("… (%d more)", len(hits)-8))
			break
		}
		name := h.v.PVC()
		if name == "" {
			name = h.v.Name
		}
		cands = append(cands, name)
	}
	return nil, fmt.Errorf("%q matches %d volumes, be more specific: %s", ref, len(hits), strings.Join(cands, ", "))
}

// Dynamic exposes the dynamic client for non-Longhorn objects (switchover:
// workloads, pods, Flux objects).
func (c *Client) Dynamic() dynamic.Interface { return c.dyn }
