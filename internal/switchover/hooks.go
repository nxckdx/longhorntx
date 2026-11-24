package switchover

import (
	"context"
	"fmt"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/yaml"

	"github.com/nxckdx/longhorntx/internal/longhorn"
)

// Hooks let apps shut down cleanly before a freeze and settle after it –
// app knowledge lhtx cannot have (OpenSearch: disable shard allocation and
// flush; Kafka; …). Found, first match wins:
//
//  1. annotations on the pod (most charts have podAnnotations)
//  2. annotations on its owners, up the chain (ReplicaSet → Deployment,
//     StatefulSet, the operator's resource)
//  3. rules in the ConfigMap "lhtx-hooks" in the Longhorn namespace (for
//     charts that offer neither), key hooks.yaml
//
// Annotation values are "[container:] command", run with sh -c in one pod
// of the app via pods/exec.
const (
	AnnPreFreeze    = "lhtx.io/pre-freeze"
	AnnPostUnfreeze = "lhtx.io/post-unfreeze"
	AnnHookTimeout  = "lhtx.io/hook-timeout"
	HooksConfigMap  = "lhtx-hooks"
	defaultHookTime = 2 * time.Minute
)

var gvrConfigMap = schema.GroupVersionResource{Version: "v1", Resource: "configmaps"}

// Hook is one command for one app.
type Hook struct {
	Container string        `json:"container,omitempty"`
	Command   string        `json:"command"`
	Timeout   time.Duration `json:"timeout"`
	Source    string        `json:"source"` // where it was found, for the plan
}

func (h *Hook) String() string {
	c := h.Command
	if len(c) > 70 {
		c = c[:67] + "…"
	}
	if h.Container != "" {
		c = h.Container + ": " + c
	}
	return c + " (from " + h.Source + ")"
}

// HookRule is an entry of the lhtx-hooks ConfigMap.
type HookRule struct {
	Namespace    string            `json:"namespace"`
	Selector     map[string]string `json:"selector"` // pod labels
	Container    string            `json:"container,omitempty"`
	PreFreeze    string            `json:"preFreeze,omitempty"`
	PostUnfreeze string            `json:"postUnfreeze,omitempty"`
	Timeout      string            `json:"timeout,omitempty"`
}

// parseHook reads "[container:] command"; a container name is a DNS label
// followed by ": ".
func parseHook(v, timeout, source string) *Hook {
	v = strings.TrimSpace(v)
	if v == "" {
		return nil
	}
	h := &Hook{Command: v, Timeout: defaultHookTime, Source: source}
	if c, rest, ok := strings.Cut(v, ": "); ok && dnsLabel(c) {
		h.Container, h.Command = c, strings.TrimSpace(rest)
	}
	if d, err := time.ParseDuration(timeout); err == nil && d > 0 {
		h.Timeout = d
	}
	return h
}

func dnsLabel(s string) bool {
	if s == "" || len(s) > 63 {
		return false
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-') {
			return false
		}
	}
	return true
}

// hookRules reads the ConfigMap (missing = no rules).
func hookRules(ctx context.Context, c *longhorn.Client) ([]HookRule, error) {
	cm, err := c.Dynamic().Resource(gvrConfigMap).Namespace(c.Namespace).Get(ctx, HooksConfigMap, metav1.GetOptions{})
	if apierrors.IsNotFound(err) || apierrors.IsForbidden(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	data, _, _ := unstructured.NestedString(cm.Object, "data", "hooks.yaml")
	var rules []HookRule
	if err := yaml.Unmarshal([]byte(data), &rules); err != nil {
		return nil, fmt.Errorf("ConfigMap %s/%s hooks.yaml: %w", c.Namespace, HooksConfigMap, err)
	}
	return rules, nil
}

// findHooks resolves the hooks of an app from one of its pods.
func findHooks(ctx context.Context, c *longhorn.Client, pod *unstructured.Unstructured, rules []HookRule) (pre, post *Hook) {
	take := func(o *unstructured.Unstructured, what string) {
		a := o.GetAnnotations()
		if pre == nil {
			pre = parseHook(a[AnnPreFreeze], a[AnnHookTimeout], what)
		}
		if post == nil {
			post = parseHook(a[AnnPostUnfreeze], a[AnnHookTimeout], what)
		}
	}
	take(pod, "pod annotation")
	// up the owner chain: ReplicaSet → Deployment, StatefulSet, operator CR
	cur := pod
	for range 4 {
		ow, ok := controllerOf(cur)
		if !ok || (pre != nil && post != nil) {
			break
		}
		gvr, err := resourceFor(c, ow.APIVersion, ow.Kind)
		if err != nil {
			break
		}
		o, err := c.Dynamic().Resource(gvr).Namespace(ow.Namespace).Get(ctx, ow.Name, metav1.GetOptions{})
		if err != nil {
			break
		}
		take(o, ow.Kind+" "+ow.Name+" annotation")
		cur = o
	}
	for _, r := range rules {
		if (pre != nil && post != nil) || r.Namespace != pod.GetNamespace() ||
			!labels.SelectorFromSet(r.Selector).Matches(labels.Set(pod.GetLabels())) {
			continue
		}
		src := "ConfigMap " + HooksConfigMap
		prefix := ""
		if r.Container != "" {
			prefix = r.Container + ": "
		}
		if pre == nil && r.PreFreeze != "" {
			pre = parseHook(prefix+r.PreFreeze, r.Timeout, src)
		}
		if post == nil && r.PostUnfreeze != "" {
			post = parseHook(prefix+r.PostUnfreeze, r.Timeout, src)
		}
	}
	return pre, post
}

// runHook executes a hook in a pod and logs the last lines of its output.
func runHook(ctx context.Context, c *longhorn.Client, h *Hook, ns, pod, what string, log func(string, ...any)) error {
	ctx, cancel := context.WithTimeout(ctx, h.Timeout)
	defer cancel()
	out, err := c.Exec(ctx, ns, pod, h.Container, h.Command)
	tail := strings.TrimSpace(out)
	if lines := strings.Split(tail, "\n"); len(lines) > 3 {
		tail = strings.Join(lines[len(lines)-3:], " ⏎ ")
	} else {
		tail = strings.Join(lines, " ⏎ ")
	}
	if len(tail) > 160 {
		tail = tail[:157] + "…"
	}
	if err != nil {
		return fmt.Errorf("%s hook in %s/%s failed: %v %s", what, ns, pod, err, tail)
	}
	log("✔ %s hook in %s/%s: %s", what, ns, pod, firstNonEmpty(tail, "ok"))
	return nil
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
