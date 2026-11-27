package switchover

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"

	"github.com/nxckdx/longhorntx/internal/longhorn"
)

// Freezing PVCs: a ValidatingAdmissionPolicy (Kubernetes ≥ 1.30, checked by
// the API server itself – no webhook server) denies new pods that mount the
// PVCs and creating/deleting the PVCs by anyone but lhtx's user. Then the
// pods are deleted. Nothing in the desired state changes: Deployments,
// StatefulSets, operators, Flux and Argo CD keep their specs and simply
// cannot start pods until the freeze ends – so no operator "repairs" a
// scale-down by deleting its cluster.

var (
	gvrReplicaSet = schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "replicasets"}
	GVRPolicy     = schema.GroupVersionResource{Group: "admissionregistration.k8s.io", Version: "v1", Resource: "validatingadmissionpolicies"}
	GVRBinding    = schema.GroupVersionResource{Group: "admissionregistration.k8s.io", Version: "v1", Resource: "validatingadmissionpolicybindings"}
	gvrSSR        = schema.GroupVersionResource{Group: "authentication.k8s.io", Version: "v1", Resource: "selfsubjectreviews"}
)

const (
	freezePrefix   = "lhtx-freeze-"
	labelManagedBy = "app.kubernetes.io/managed-by"
	annPVCs        = "lhtx.io/pvcs"     // ns/name,…
	annOwners      = "lhtx.io/owners"   // JSON []Owner of the deleted pods
	annAttached    = "lhtx.io/attached" // ns/name of PVCs whose volume was attached
	annReason      = "lhtx.io/reason"
	annPostHooks   = "lhtx.io/post-hooks" // JSON []groupHook: run after unfreeze
	annHibernated  = "lhtx.io/hibernated" // JSON []Owner: CNPG clusters to wake up
	annCNPG        = "cnpg.io/hibernation"
	annUser        = "lhtx.io/user"
	annRequest     = "lhtx.io/request" // name of the Freeze resource the agent works on
	denyMarker     = "frozen by lhtx"
)

// Owner is the controller of a deleted pod; it is nudged on unfreeze.
type Owner struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Namespace  string `json:"namespace"`
	Name       string `json:"name"`
}

func (o Owner) String() string { return o.Kind + " " + o.Namespace + "/" + o.Name }

// Group is one app: the pods of one owner, stopped together in order.
type Group struct {
	Owner Owner    `json:"owner"`
	Pods  []string `json:"pods"` // ns/name in stop order (highest ordinal first)
	Pre   *Hook    `json:"pre,omitempty"`
	Post  *Hook    `json:"post,omitempty"`
	// Hibernate: a CloudNativePG cluster – the operator shuts it down
	// cleanly (cnpg.io/hibernation: on) instead of lhtx deleting pods.
	Hibernate bool `json:"hibernate,omitempty"`
	// Display names the app as people know it (the Deployment rather than
	// its ReplicaSet).
	Display string `json:"display,omitempty"`
}

func (g Group) name() string {
	switch {
	case g.Display != "":
		return g.Display
	case g.Owner.Kind == "":
		return "pod " + g.Pods[0]
	}
	return g.Owner.Kind + " " + g.Owner.Namespace + "/" + g.Owner.Name
}

type groupHook struct {
	Owner Owner `json:"owner"`
	Hook  *Hook `json:"hook"`
}

// Freeze is an active freeze (one policy + binding).
type Freeze struct {
	Name       string      `json:"name"`
	PVCs       []string    `json:"pvcs"`
	Owners     []Owner     `json:"owners"`
	PostHooks  []groupHook `json:"postHooks,omitempty"`
	Hibernated []Owner     `json:"hibernated,omitempty"`
	Attached   []string    `json:"attached"`
	Reason     string      `json:"reason"`
	User       string      `json:"user"`
	Created    time.Time   `json:"created"`
	// Request is set when the lhtx agent owns the freeze: the name of its
	// Freeze resource (unfreeze = delete that resource).
	Request string `json:"request,omitempty"`
}

// FreezePlan is what a freeze will do, for the preview.
type FreezePlan struct {
	PVCs     []string // ns/name
	Pods     []string // ns/name of the pods that will be stopped
	Owners   []Owner
	Groups   []Group  // the apps, with hooks and how they are stopped
	Attached []string // PVCs whose volume is attached now
	Manual   []string // volumes attached without a pod (tickets) – stay attached
	Warnings []string
	User     string
	Reason   string
	// AllowLabel: pods with this label created by User may mount the
	// frozen PVCs (the copy pod of lhtx shrink); "" = none.
	AllowLabel string `json:",omitempty"`
}

// Username asks the API server who we are (SelfSubjectReview).
func Username(ctx context.Context, dyn dynamic.Interface) (string, error) {
	u := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "authentication.k8s.io/v1", "kind": "SelfSubjectReview"}}
	out, err := dyn.Resource(gvrSSR).Create(ctx, u, metav1.CreateOptions{})
	if err != nil {
		return "", fmt.Errorf("who am I (SelfSubjectReview): %w", err)
	}
	name, _, _ := unstructured.NestedString(out.Object, "status", "userInfo", "username")
	if name == "" {
		return "", fmt.Errorf("the API server did not tell the user name")
	}
	return name, nil
}

// Who is the caller as the API server sees it.
type Who struct {
	Username string
	Groups   []string
}

// WhoAmI asks the API server for the caller's name and groups.
func WhoAmI(ctx context.Context, dyn dynamic.Interface) (*Who, error) {
	u := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "authentication.k8s.io/v1", "kind": "SelfSubjectReview"}}
	out, err := dyn.Resource(gvrSSR).Create(ctx, u, metav1.CreateOptions{})
	if err != nil {
		return nil, fmt.Errorf("who am I (SelfSubjectReview): %w", err)
	}
	w := &Who{}
	w.Username, _, _ = unstructured.NestedString(out.Object, "status", "userInfo", "username")
	w.Groups, _, _ = unstructured.NestedStringSlice(out.Object, "status", "userInfo", "groups")
	if w.Username == "" {
		return nil, fmt.Errorf("the API server did not tell the user name")
	}
	return w, nil
}

// PlanFreeze looks up the pods that mount the PVCs and the volumes behind
// them. One LIST of pods per namespace involved.
func PlanFreeze(ctx context.Context, c *longhorn.Client, pvcs []string, reason string) (*FreezePlan, error) {
	if len(pvcs) == 0 {
		return nil, fmt.Errorf("no PVCs to freeze")
	}
	dyn := c.Dynamic()
	p := &FreezePlan{Reason: reason}
	set := map[string]bool{}
	byNS := map[string]bool{}
	for _, x := range pvcs {
		ns, n, ok := strings.Cut(x, "/")
		if !ok || ns == "" || n == "" || !dnsName(ns) || !dnsName(n) {
			return nil, fmt.Errorf("PVC %q: namespace/name", x)
		}
		set[x], byNS[ns] = true, true
	}
	for x := range set {
		p.PVCs = append(p.PVCs, x)
	}
	sort.Strings(p.PVCs)

	vols, err := c.Volumes(ctx)
	if err != nil {
		return nil, err
	}
	for _, v := range vols {
		if set[v.BoundPVC()] && v.Status.State == "attached" {
			p.Attached = append(p.Attached, v.BoundPVC())
		}
	}
	sort.Strings(p.Attached)

	owners := map[string]Owner{}
	withPod := map[string]bool{}
	cnpg := map[string]bool{}
	groups := map[string]*Group{}
	var order []string
	firstPod := map[string]*unstructured.Unstructured{}
	rules, err := hookRules(ctx, c)
	if err != nil {
		return nil, err
	}
	for ns := range byNS {
		pods, err := dyn.Resource(gvrPod).Namespace(ns).List(ctx, metav1.ListOptions{})
		if err != nil {
			return nil, fmt.Errorf("pods in %s: %w", ns, err)
		}
		for i := range pods.Items {
			pod := &pods.Items[i]
			mounted := false
			for _, cl := range claimsOf(pod) {
				if set[ns+"/"+cl] {
					mounted, withPod[ns+"/"+cl] = true, true
				}
			}
			if !mounted {
				continue
			}
			p.Pods = append(p.Pods, ns+"/"+pod.GetName())
			o, hasOwner := controllerOf(pod)
			key := o.String()
			if !hasOwner {
				key = "pod " + ns + "/" + pod.GetName()
			}
			if groups[key] == nil {
				groups[key] = &Group{Owner: o, Hibernate: hasOwner && o.Kind == "Cluster" && strings.HasPrefix(o.APIVersion, "postgresql.cnpg.io/")}
				order = append(order, key)
				firstPod[key] = pod
			}
			groups[key].Pods = append(groups[key].Pods, ns+"/"+pod.GetName())
			if hasOwner {
				owners[o.String()] = o
			} else {
				p.Warnings = append(p.Warnings, fmt.Sprintf("pod %s/%s has no owner: it is deleted and will not come back by itself", ns, pod.GetName()))
			}
			if k, n := owner(pod); k == "Cluster" && !cnpg[ns+"/"+n] {
				cnpg[ns+"/"+n] = true
			}
		}
	}
	sort.Strings(p.Pods)
	sort.Strings(order)
	for _, k := range order {
		g := groups[k]
		sortStopOrder(g.Pods)
		g.Pre, g.Post = findHooks(ctx, c, firstPod[k], rules)
		if g.Owner.Kind == "ReplicaSet" {
			if rs, err := dyn.Resource(gvrReplicaSet).Namespace(g.Owner.Namespace).Get(ctx, g.Owner.Name, metav1.GetOptions{}); err == nil {
				if d, ok := controllerOf(rs); ok && d.Kind == "Deployment" {
					g.Display = "Deployment " + d.Namespace + "/" + d.Name
				}
			}
		}
		p.Groups = append(p.Groups, *g)
	}
	for _, o := range owners {
		p.Owners = append(p.Owners, o)
	}
	sort.Slice(p.Owners, func(i, j int) bool { return p.Owners[i].String() < p.Owners[j].String() })
	for _, a := range p.Attached {
		if !withPod[a] {
			p.Manual = append(p.Manual, a)
		}
	}
	if len(p.Manual) > 0 {
		p.Warnings = append(p.Warnings, fmt.Sprintf("%d volumes are attached without a pod (manual or backup tickets) and stay attached: %s – lhtx detach them if needed",
			len(p.Manual), strings.Join(firstN(p.Manual, 5), ", ")))
	}
	p.Warnings = append(p.Warnings, "Longhorn itself can still attach frozen volumes (recurring jobs with "+
		"allow-recurring-job-while-volume-detached, manual attach) – the policy only stops pods")
	p.User, err = Username(ctx, dyn)
	if err != nil {
		return nil, err
	}
	return p, nil
}

// sortStopOrder sorts pods like a StatefulSet scales down: highest ordinal
// first (name-2, name-1, name-0); other names after, alphabetically.
func sortStopOrder(pods []string) {
	ord := func(s string) int {
		i := strings.LastIndex(s, "-")
		if i < 0 {
			return -1
		}
		n, err := strconv.Atoi(s[i+1:])
		if err != nil {
			return -1
		}
		return n
	}
	sort.SliceStable(pods, func(i, j int) bool {
		a, b := ord(pods[i]), ord(pods[j])
		if a != b {
			return a > b
		}
		return pods[i] < pods[j]
	})
}

func claimsOf(pod *unstructured.Unstructured) []string {
	var out []string
	vols, _, _ := unstructured.NestedSlice(pod.Object, "spec", "volumes")
	for _, v := range vols {
		if m, ok := v.(map[string]any); ok {
			if c, _, _ := unstructured.NestedString(m, "persistentVolumeClaim", "claimName"); c != "" {
				out = append(out, c)
			}
		}
	}
	return out
}

func controllerOf(o *unstructured.Unstructured) (Owner, bool) {
	for _, r := range o.GetOwnerReferences() {
		if r.Controller != nil && *r.Controller {
			return Owner{APIVersion: r.APIVersion, Kind: r.Kind, Namespace: o.GetNamespace(), Name: r.Name}, true
		}
	}
	return Owner{}, false
}

func dnsName(s string) bool {
	if s == "" || len(s) > 253 {
		return false
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' || r == '.') {
			return false
		}
	}
	return true
}

func firstN(s []string, n int) []string {
	if len(s) <= n {
		return s
	}
	return append(append([]string{}, s[:n]...), fmt.Sprintf("… %d more", len(s)-n))
}

// policyObjects builds the ValidatingAdmissionPolicy and its binding.
func policyObjects(name string, p *FreezePlan, now time.Time, request string) (*unstructured.Unstructured, *unstructured.Unstructured) {
	keys := make([]string, len(p.PVCs))
	nss := map[string]bool{}
	for i, x := range p.PVCs {
		keys[i] = "'" + x + "': true" // names are DNS names: no quoting issues
		ns, _, _ := strings.Cut(x, "/")
		nss[ns] = true
	}
	var nsList []any
	for ns := range nss {
		nsList = append(nsList, ns)
	}
	sort.Slice(nsList, func(i, j int) bool { return nsList[i].(string) < nsList[j].(string) })
	owners, _ := json.Marshal(p.Owners)
	var posts []groupHook
	var hib []Owner
	for _, g := range p.Groups {
		if g.Post != nil {
			posts = append(posts, groupHook{Owner: g.Owner, Hook: g.Post})
		}
		if g.Hibernate {
			hib = append(hib, g.Owner)
		}
	}
	postJSON, _ := json.Marshal(posts)
	hibJSON, _ := json.Marshal(hib)
	meta := map[string]any{"name": name,
		"labels": map[string]any{labelManagedBy: "lhtx"},
		"annotations": map[string]any{annPVCs: strings.Join(p.PVCs, ","), annOwners: string(owners),
			annAttached: strings.Join(p.Attached, ","), annReason: p.Reason, annUser: p.User,
			annPostHooks: string(postJSON), annHibernated: string(hibJSON),
			"lhtx.io/created": now.UTC().Format(time.RFC3339)}}
	if request != "" {
		meta["annotations"].(map[string]any)[annRequest] = request
	}
	msg := denyMarker + " (" + name + "): the PVC is frozen for maintenance – lhtx unfreeze ends it"
	pol := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "admissionregistration.k8s.io/v1", "kind": "ValidatingAdmissionPolicy",
		"metadata": meta,
		"spec": map[string]any{
			"failurePolicy": "Fail",
			"matchConstraints": map[string]any{
				"namespaceSelector": map[string]any{"matchExpressions": []any{map[string]any{
					"key": "kubernetes.io/metadata.name", "operator": "In", "values": nsList}}},
				"resourceRules": []any{
					map[string]any{"apiGroups": []any{""}, "apiVersions": []any{"v1"}, "operations": []any{"CREATE"}, "resources": []any{"pods"}},
					map[string]any{"apiGroups": []any{""}, "apiVersions": []any{"v1"}, "operations": []any{"CREATE", "DELETE"}, "resources": []any{"persistentvolumeclaims"}},
				},
			},
			"variables": []any{map[string]any{"name": "frozen", "expression": "{" + strings.Join(keys, ", ") + "}"}},
			"validations": []any{
				map[string]any{
					"expression": "request.resource.resource != 'pods' || !has(object.spec.volumes) || " + allowPod(p) +
						"!object.spec.volumes.exists(v, has(v.persistentVolumeClaim) && " +
						"(request.namespace + '/' + v.persistentVolumeClaim.claimName) in variables.frozen)",
					"message": msg, "reason": "Forbidden",
				},
				map[string]any{
					"expression": "request.resource.resource != 'persistentvolumeclaims' || request.userInfo.username == '" + p.User + "' || " +
						"!((request.namespace + '/' + (request.operation == 'DELETE' ? request.name : object.metadata.name)) in variables.frozen)",
					"message": msg, "reason": "Forbidden",
				},
			},
		},
	}}
	bind := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "admissionregistration.k8s.io/v1", "kind": "ValidatingAdmissionPolicyBinding",
		"metadata": map[string]any{"name": name, "labels": map[string]any{labelManagedBy: "lhtx"}},
		"spec":     map[string]any{"policyName": name, "validationActions": []any{"Deny"}},
	}}
	return pol, bind
}

// allowPod: the exception for lhtx's own copy pod – only with the label
// and only created by the user who froze.
func allowPod(p *FreezePlan) string {
	if p.AllowLabel == "" {
		return ""
	}
	return "(has(object.metadata.labels) && '" + p.AllowLabel + "' in object.metadata.labels && request.userInfo.username == '" + p.User + "') || "
}

// FreezeOptions control Apply/Unfreeze.
type FreezeOptions struct {
	Log  func(format string, a ...any)
	Wait time.Duration // waits for detach / pods to come back (default 10 min)
	Poll time.Duration // default 3 s
	// Renudge: how often owners are nudged again while volumes are still
	// not attached after an unfreeze (default 30 s).
	Renudge time.Duration
	// Name of the policy; empty = lhtx-freeze-<UTC time>. With a name an
	// existing policy is taken over (the agent resuming after a restart).
	Name string
	// Request: the agent's Freeze resource, stored on the policy.
	Request string
	// IgnoreKey: unfreeze even if a key Secret still differs from the copy
	// a switch kept (see checkKeys).
	IgnoreKey bool
}

func (o *FreezeOptions) defaults() {
	if o.Log == nil {
		o.Log = func(string, ...any) {}
	}
	if o.Wait == 0 {
		o.Wait = 10 * time.Minute
	}
	if o.Poll == 0 {
		o.Poll = 3 * time.Second
	}
	if o.Renudge == 0 {
		o.Renudge = 30 * time.Second
	}
}

// ApplyFreeze creates the policy, checks that the API server enforces it,
// deletes the pods and waits until the volumes are detached. On failure
// before pods were deleted the policy is removed again.
func ApplyFreeze(ctx context.Context, c *longhorn.Client, p *FreezePlan, o FreezeOptions) (*Freeze, error) {
	o.defaults()
	dyn := c.Dynamic()
	now := time.Now()
	name := o.Name
	if name == "" {
		name = freezePrefix + now.UTC().Format("20060102-150405")
	}
	pol, bind := policyObjects(name, p, now, o.Request)
	var prev *Freeze
	if _, err := dyn.Resource(GVRPolicy).Create(ctx, pol, metav1.CreateOptions{FieldManager: "lhtx"}); apierrors.IsAlreadyExists(err) && o.Name != "" {
		if prev, err = takeOver(ctx, dyn, name, p); err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, fmt.Errorf("create ValidatingAdmissionPolicy (needs Kubernetes ≥ 1.30 and cluster-admin rights): %w", err)
	}
	if _, err := dyn.Resource(GVRBinding).Create(ctx, bind, metav1.CreateOptions{FieldManager: "lhtx"}); err != nil && !(prev != nil && apierrors.IsAlreadyExists(err)) {
		if prev == nil {
			_ = dyn.Resource(GVRPolicy).Delete(context.WithoutCancel(ctx), name, metav1.DeleteOptions{})
		}
		return nil, fmt.Errorf("create policy binding: %w", err)
	}
	f := &Freeze{Name: name, PVCs: p.PVCs, Owners: p.Owners, Attached: p.Attached, Reason: p.Reason, User: p.User, Created: now, Request: o.Request}
	if prev != nil {
		f.Owners, f.Attached, f.Created = p.Owners, p.Attached, prev.Created
		o.Log("✔ policy %s exists – resuming (%d PVCs)", name, len(p.PVCs))
	} else {
		o.Log("✔ policy %s created (%d PVCs)", name, len(p.PVCs))
	}
	if err := verifyEnforced(ctx, dyn, p, o); err != nil {
		if prev == nil {
			_ = removePolicy(context.WithoutCancel(ctx), dyn, name)
		}
		return nil, err
	}
	o.Log("✔ the API server enforces it (test pod rejected in a dry run)")
	for _, g := range p.Groups {
		if g.Post != nil {
			f.PostHooks = append(f.PostHooks, groupHook{Owner: g.Owner, Hook: g.Post})
		}
		if g.Hibernate {
			f.Hibernated = append(f.Hibernated, g.Owner)
		}
	}
	if prev != nil {
		f.PostHooks, f.Hibernated = mergeHooks(prev.PostHooks, f.PostHooks), mergeOwners(prev.Hibernated, f.Hibernated)
	}
	// pre-freeze hooks first: if one fails nothing is stopped
	for _, g := range p.Groups {
		if g.Pre == nil {
			continue
		}
		target := readyPod(ctx, dyn, g.Pods)
		if target == "" {
			o.Log("⚠ %s: no ready pod – the app is not running, pre-freeze hook skipped", g.name())
			continue
		}
		ns, pod, _ := strings.Cut(target, "/")
		if err := runHook(ctx, c, g.Pre, ns, pod, "pre-freeze", o.Log); err != nil {
			if prev != nil {
				return f, err
			}
			_ = removePolicy(context.WithoutCancel(ctx), dyn, name)
			return nil, fmt.Errorf("%w – policy removed, nothing stopped", err)
		}
	}
	if err := stopGroups(ctx, c, p.Groups, o); err != nil {
		return f, fmt.Errorf("%w (frozen anyway: lhtx unfreeze %s)", err, name)
	}
	return f, waitVolumes(ctx, c, p.Attached, "detached", o, func(left []string) error {
		return fmt.Errorf("still attached after %s: %s (frozen anyway: lhtx unfreeze %s)", o.Wait, strings.Join(firstN(left, 5), ", "), name)
	}, nil)
}

// readyPod picks the pod a pre-freeze hook runs in: a ready one, the
// lowest ordinal first (it is stopped last). Read live – the plan may be
// minutes old. "" if none is ready.
func readyPod(ctx context.Context, dyn dynamic.Interface, pods []string) string {
	for i := len(pods) - 1; i >= 0; i-- {
		ns, n, _ := strings.Cut(pods[i], "/")
		p, err := dyn.Resource(gvrPod).Namespace(ns).Get(ctx, n, metav1.GetOptions{})
		if err == nil && p.GetDeletionTimestamp() == nil && podReady(p) {
			return pods[i]
		}
	}
	return ""
}

// verifyEnforced creates a test pod that mounts a frozen PVC in a server
// dry run until the policy rejects it (policies take a moment to load).
func verifyEnforced(ctx context.Context, dyn dynamic.Interface, p *FreezePlan, o FreezeOptions) error {
	ns, pvc, _ := strings.Cut(p.PVCs[0], "/")
	deadline := time.Now().Add(30 * time.Second)
	var last error
	for time.Now().Before(deadline) {
		err := testPod(ctx, dyn, ns, pvc)
		if err != nil && strings.Contains(err.Error(), denyMarker) {
			return nil
		}
		last = err
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
	if last != nil {
		return fmt.Errorf("could not verify the policy – the test pod was rejected for another reason: %v (policy removed, nothing deleted)", last)
	}
	return fmt.Errorf("the API server does not enforce the policy after 30 s (policy removed, nothing deleted)")
}

// waitLifted: after the policy is deleted the API server still enforces it
// for a moment (its policy cache). Owners nudged in that moment fail to
// create their pods and fall into their back-off – the app would come back
// only with the next nudge. Wait (at most 20 s) until a dry-run pod on the
// PVC is no longer denied by the freeze.
func waitLifted(ctx context.Context, dyn dynamic.Interface, pvcs []string) {
	if len(pvcs) == 0 {
		return
	}
	ns, pvc, _ := strings.Cut(pvcs[0], "/")
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		err := testPod(ctx, dyn, ns, pvc)
		if err == nil || !strings.Contains(err.Error(), denyMarker) {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// testPod creates (dry run) a minimal pod mounting the PVC; the error says
// whether the freeze policy denies it.
func testPod(ctx context.Context, dyn dynamic.Interface, ns, pvc string) error {
	f := false
	t := true
	uid := int64(65534)
	pod := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": "Pod",
		"metadata": map[string]any{"generateName": "lhtx-freeze-check-", "namespace": ns},
		"spec": map[string]any{
			// restricted Pod Security, so only our policy can reject it
			"securityContext": map[string]any{"runAsNonRoot": t, "runAsUser": uid, "seccompProfile": map[string]any{"type": "RuntimeDefault"}},
			"containers": []any{map[string]any{"name": "check", "image": "registry.k8s.io/pause:3.10",
				"securityContext": map[string]any{"allowPrivilegeEscalation": f, "capabilities": map[string]any{"drop": []any{"ALL"}}},
				"resources":       map[string]any{"limits": map[string]any{"cpu": "10m", "memory": "16Mi"}, "requests": map[string]any{"cpu": "10m", "memory": "16Mi"}}}},
			"volumes": []any{map[string]any{"name": "v", "persistentVolumeClaim": map[string]any{"claimName": pvc}}},
		},
	}}
	_, err := dyn.Resource(gvrPod).Namespace(ns).Create(ctx, pod, metav1.CreateOptions{DryRun: []string{metav1.DryRunAll}})
	return err
}

// waitVolumes waits until the volumes of the PVCs reach state.
func waitVolumes(ctx context.Context, c *longhorn.Client, pvcs []string, state string, o FreezeOptions, timeout func([]string) error, tick func(left []string)) error {
	if len(pvcs) == 0 {
		return nil
	}
	want := map[string]bool{}
	for _, x := range pvcs {
		want[x] = true
	}
	deadline := time.Now().Add(o.Wait)
	lastLog := time.Time{}
	for {
		vols, err := c.Volumes(ctx)
		if err != nil {
			return err
		}
		// the volume each PVC uses now (an old volume keeps the PVC's name
		// in its status, with lastPVCRefAt set)
		cur := map[string]string{}
		for _, v := range vols {
			if p := v.BoundPVC(); want[p] {
				cur[p] = v.Status.State
			}
		}
		var left []string
		for p := range want {
			st, ok := cur[p]
			if (ok && st != state) || (!ok && state == "attached") {
				left = append(left, p)
			}
		}
		sort.Strings(left)
		if len(left) == 0 {
			o.Log("✔ all %d volumes %s", len(pvcs), state)
			return nil
		}
		if time.Now().After(deadline) {
			return timeout(left)
		}
		if time.Since(lastLog) > 10*time.Second {
			lastLog = time.Now()
			o.Log("… %d of %d volumes not %s yet: %s", len(left), len(pvcs), state, strings.Join(firstN(left, 3), ", "))
		}
		if tick != nil {
			tick(left)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(o.Poll):
		}
	}
}

// ActiveFreezes lists the freezes in the cluster (one small LIST).
func ActiveFreezes(ctx context.Context, dyn dynamic.Interface) ([]Freeze, error) {
	l, err := dyn.Resource(GVRPolicy).List(ctx, metav1.ListOptions{LabelSelector: labelManagedBy + "=lhtx"})
	if err != nil {
		return nil, err
	}
	var out []Freeze
	for i := range l.Items {
		if !strings.HasPrefix(l.Items[i].GetName(), freezePrefix) {
			continue
		}
		out = append(out, freezeOf(&l.Items[i]))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Created.Before(out[j].Created) })
	return out, nil
}

// freezeOf reads a freeze from its policy's annotations.
func freezeOf(it *unstructured.Unstructured) Freeze {
	a := it.GetAnnotations()
	f := Freeze{Name: it.GetName(), Reason: a[annReason], User: a[annUser], PVCs: splitList(a[annPVCs]),
		Attached: splitList(a[annAttached]), Request: a[annRequest]}
	_ = json.Unmarshal([]byte(a[annOwners]), &f.Owners)
	_ = json.Unmarshal([]byte(a[annPostHooks]), &f.PostHooks)
	_ = json.Unmarshal([]byte(a[annHibernated]), &f.Hibernated)
	f.Created, _ = time.Parse(time.RFC3339, a["lhtx.io/created"])
	return f
}

// GetFreeze reads one freeze by its policy name (nil if there is none).
func GetFreeze(ctx context.Context, dyn dynamic.Interface, name string) (*Freeze, error) {
	u, err := dyn.Resource(GVRPolicy).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	f := freezeOf(u)
	return &f, nil
}

// takeOver continues with an existing policy (the agent restarted during a
// freeze): pods already deleted are no longer in the new plan, so their
// owners, attached volumes and hooks are merged into the plan and stored.
func takeOver(ctx context.Context, dyn dynamic.Interface, name string, p *FreezePlan) (*Freeze, error) {
	prev, err := GetFreeze(ctx, dyn, name)
	if err != nil || prev == nil {
		return nil, fmt.Errorf("policy %s: %v", name, err)
	}
	if strings.Join(prev.PVCs, ",") != strings.Join(p.PVCs, ",") {
		return nil, fmt.Errorf("policy %s exists for other PVCs", name)
	}
	p.Owners = mergeOwners(prev.Owners, p.Owners)
	p.Attached = mergeStrings(prev.Attached, p.Attached)
	owners, _ := json.Marshal(p.Owners)
	patch, _ := json.Marshal(map[string]any{"metadata": map[string]any{"annotations": map[string]any{
		annOwners: string(owners), annAttached: strings.Join(p.Attached, ",")}}})
	if _, err := dyn.Resource(GVRPolicy).Patch(ctx, name, types.MergePatchType, patch, metav1.PatchOptions{FieldManager: "lhtx"}); err != nil {
		return nil, fmt.Errorf("update policy %s: %w", name, err)
	}
	return prev, nil
}

func mergeOwners(a, b []Owner) []Owner {
	seen := map[string]bool{}
	var out []Owner
	for _, o := range append(append([]Owner{}, a...), b...) {
		if !seen[o.String()] {
			seen[o.String()] = true
			out = append(out, o)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].String() < out[j].String() })
	return out
}

func mergeHooks(a, b []groupHook) []groupHook {
	seen := map[string]bool{}
	var out []groupHook
	for _, h := range append(append([]groupHook{}, a...), b...) {
		if !seen[h.Owner.String()] {
			seen[h.Owner.String()] = true
			out = append(out, h)
		}
	}
	return out
}

func mergeStrings(a, b []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, x := range append(append([]string{}, a...), b...) {
		if !seen[x] {
			seen[x] = true
			out = append(out, x)
		}
	}
	sort.Strings(out)
	return out
}

func splitList(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(s, ",")
}

func removePolicy(ctx context.Context, dyn dynamic.Interface, name string) error {
	if err := dyn.Resource(GVRBinding).Delete(ctx, name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	if err := dyn.Resource(GVRPolicy).Delete(ctx, name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return nil
}

// Unfreeze removes the policy, nudges the owners of the deleted pods so
// their controllers start pods now instead of after their back-off, and
// waits until the volumes that were attached are attached again.
func Unfreeze(ctx context.Context, c *longhorn.Client, f Freeze, o FreezeOptions) error {
	o.defaults()
	dyn := c.Dynamic()
	if !o.IgnoreKey {
		if err := checkKeys(ctx, c, f.PVCs); err != nil {
			return err
		}
	}
	if err := removePolicy(ctx, dyn, f.Name); err != nil {
		return fmt.Errorf("remove policy %s: %w", f.Name, err)
	}
	waitLifted(ctx, dyn, f.PVCs)
	o.Log("✔ policy %s removed: pods may start again", f.Name)
	woken := map[string]bool{}
	for _, ow := range f.Hibernated {
		if err := hibernate(ctx, c, ow, false); err != nil {
			o.Log("⚠ could not wake %s: %v – set %s: off yourself", ow, err, annCNPG)
			continue
		}
		woken[ow.String()] = true
		o.Log("✔ %s woken up (%s: off)", ow, annCNPG)
	}
	nudged := 0
	for _, ow := range f.Owners {
		if woken[ow.String()] {
			continue
		}
		if err := nudge(ctx, c, ow); err != nil {
			o.Log("⚠ could not nudge %s: %v (it starts its pods after its own back-off)", ow, err)
			continue
		}
		nudged++
	}
	if nudged > 0 {
		o.Log("✔ %d owners nudged (annotation lhtx.io/unfrozen-at) so they start their pods now", nudged)
	}
	// Controllers that failed to create pods during the freeze may still
	// be in their back-off (a ReplicaSet waits for its "expectations" up to
	// 5 minutes and ignores a nudge until then). Every Renudge, owners in
	// the namespaces of the volumes still missing that have no pod at all
	// are nudged again – one with a pod (starting, attaching) is left alone.
	lastNudge := time.Now()
	renudge := func(left []string) {
		if time.Since(lastNudge) < o.Renudge {
			return
		}
		lastNudge = time.Now()
		nss := map[string]bool{}
		for _, x := range left {
			ns, _, _ := strings.Cut(x, "/")
			nss[ns] = true
		}
		hasPod := map[string]bool{}
		for ns := range nss {
			pods, err := dyn.Resource(gvrPod).Namespace(ns).List(ctx, metav1.ListOptions{})
			if err != nil {
				return
			}
			for i := range pods.Items {
				if co, ok := controllerOf(&pods.Items[i]); ok && pods.Items[i].GetDeletionTimestamp() == nil {
					hasPod[co.Kind+" "+ns+"/"+co.Name] = true
				}
			}
		}
		n := 0
		for _, ow := range f.Owners {
			if nss[ow.Namespace] && !woken[ow.String()] && !hasPod[ow.String()] && nudge(ctx, c, ow) == nil {
				n++
			}
		}
		if n > 0 {
			o.Log("↻ %d owners without a pod nudged again (controllers may still be in their back-off)", n)
		}
	}
	if err := waitVolumes(ctx, c, f.Attached, "attached", o, func(left []string) error {
		return fmt.Errorf("not attached again after %s: %s – check their pods/operators", o.Wait, strings.Join(firstN(left, 8), ", "))
	}, renudge); err != nil {
		return err
	}
	return runPostHooks(ctx, c, f.PostHooks, o)
}

// checkKeys: a switch that saw a key Secret replaced kept the old key in a
// copy (label lhtx.io/key-backup) and left the PVC frozen. Before pods may
// start, the key Secret must hold that key again – else they would only
// fail to mount. Skipped if the Secrets cannot be read.
func checkKeys(ctx context.Context, c *longhorn.Client, pvcs []string) error {
	want := map[string]bool{}
	nss := map[string]bool{}
	for _, x := range pvcs {
		want[x] = true
		ns, _, _ := strings.Cut(x, "/")
		nss[ns] = true
	}
	for ns := range nss {
		l, err := c.Dynamic().Resource(gvrSecret).Namespace(ns).List(ctx, metav1.ListOptions{LabelSelector: longhorn.LabelKeyBackup + "=true"})
		if err != nil {
			continue
		}
		for i := range l.Items {
			b := &l.Items[i]
			a := b.GetAnnotations()
			if !want[a["lhtx.io/pvc"]] {
				continue
			}
			kns, kname, _ := strings.Cut(a["lhtx.io/key-of"], "/")
			old, err := longhorn.Fingerprint(b)
			if err != nil {
				continue
			}
			cur, err := c.KeyFingerprint(ctx, longhorn.SecretRef{Namespace: kns, Name: kname})
			if err != nil && !errors.Is(err, longhorn.ErrNoSecret) {
				continue
			}
			if cur != old {
				return fmt.Errorf("the key Secret %s does not hold the key kept in %s/%s (the volume was encrypted with that one) – "+
					"put it back first, or the pods only fail to mount (--ignore-key to unfreeze anyway)", a["lhtx.io/key-of"], ns, b.GetName())
			}
		}
	}
	return nil
}

// nudge touches the owner's metadata (not its spec): controllers and most
// operators reconcile on any update, so a new pod is created right away.
func nudge(ctx context.Context, c *longhorn.Client, ow Owner) error {
	gvr, err := resourceFor(c, ow.APIVersion, ow.Kind)
	if err != nil {
		return err
	}
	// nanoseconds: a repeated nudge must change the object, or no update
	// event reaches the controller
	body := fmt.Sprintf(`{"metadata":{"annotations":{"lhtx.io/unfrozen-at":%q}}}`, time.Now().UTC().Format(time.RFC3339Nano))
	_, err = c.Dynamic().Resource(gvr).Namespace(ow.Namespace).Patch(ctx, ow.Name, types.MergePatchType, []byte(body), metav1.PatchOptions{FieldManager: "lhtx"})
	return err
}

// resourceFor maps apiVersion+kind to the resource via discovery (plural
// names of operator CRDs vary), with the usual lowercase+s as fallback.
func resourceFor(c *longhorn.Client, apiVersion, kind string) (schema.GroupVersionResource, error) {
	gv, err := schema.ParseGroupVersion(apiVersion)
	if err != nil {
		return schema.GroupVersionResource{}, err
	}
	fallback := gv.WithResource(strings.ToLower(kind) + "s")
	dc, err := discovery.NewDiscoveryClientForConfig(c.Cluster.Config)
	if err != nil {
		return fallback, nil
	}
	l, err := dc.ServerResourcesForGroupVersion(apiVersion)
	if err != nil {
		return fallback, nil
	}
	for _, r := range l.APIResources {
		if r.Kind == kind && !strings.Contains(r.Name, "/") {
			return gv.WithResource(r.Name), nil
		}
	}
	return fallback, nil
}

// stopGroups stops the apps: each app's pods one after the other in stop
// order (like a StatefulSet scales down), waiting until a pod is gone
// before the next; up to 8 apps in parallel. CloudNativePG clusters are
// hibernated instead, so the operator shuts them down cleanly.
func stopGroups(ctx context.Context, c *longhorn.Client, groups []Group, o FreezeOptions) error {
	sem := make(chan struct{}, 8)
	errs := make(chan error, len(groups))
	for _, g := range groups {
		sem <- struct{}{}
		go func(g Group) {
			defer func() { <-sem }()
			errs <- stopGroup(ctx, c, g, o)
		}(g)
	}
	var msgs []string
	for range groups {
		if err := <-errs; err != nil {
			msgs = append(msgs, err.Error())
		}
	}
	if len(msgs) > 0 {
		return fmt.Errorf("%s", strings.Join(msgs, "; "))
	}
	return nil
}

func stopGroup(ctx context.Context, c *longhorn.Client, g Group, o FreezeOptions) error {
	dyn := c.Dynamic()
	if g.Hibernate {
		if err := hibernate(ctx, c, g.Owner, true); err != nil {
			return fmt.Errorf("hibernate %s: %w", g.name(), err)
		}
		o.Log("✔ %s: hibernation requested (%s: on) – the operator stops its instances", g.name(), annCNPG)
		for _, x := range g.Pods {
			if err := waitPodGone(ctx, dyn, x, o); err != nil {
				return fmt.Errorf("%s: %w", g.name(), err)
			}
		}
		o.Log("✔ %s: %d instances stopped", g.name(), len(g.Pods))
		return nil
	}
	for i, x := range g.Pods {
		ns, n, _ := strings.Cut(x, "/")
		err := dyn.Resource(gvrPod).Namespace(ns).Delete(ctx, n, metav1.DeleteOptions{})
		if err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("delete pod %s: %w", x, err)
		}
		if err := waitPodGone(ctx, dyn, x, o); err != nil {
			return err
		}
		if len(g.Pods) > 1 {
			o.Log("✔ %s: %s stopped (%d/%d)", g.name(), n, i+1, len(g.Pods))
		}
	}
	if len(g.Pods) == 1 {
		o.Log("✔ %s: %s stopped", g.name(), shortNames(g.Pods)[0])
	}
	return nil
}

// waitPodGone waits until the pod has terminated (its grace period).
func waitPodGone(ctx context.Context, dyn dynamic.Interface, x string, o FreezeOptions) error {
	ns, n, _ := strings.Cut(x, "/")
	deadline := time.Now().Add(o.Wait)
	for {
		_, err := dyn.Resource(gvrPod).Namespace(ns).Get(ctx, n, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("pod %s still terminating after %s", x, o.Wait)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(o.Poll):
		}
	}
}

// hibernate sets or clears CloudNativePG's declarative hibernation.
func hibernate(ctx context.Context, c *longhorn.Client, ow Owner, on bool) error {
	gvr, err := resourceFor(c, ow.APIVersion, ow.Kind)
	if err != nil {
		return err
	}
	v := "off"
	if on {
		v = "on"
	}
	body := fmt.Sprintf(`{"metadata":{"annotations":{%q:%q}}}`, annCNPG, v)
	_, err = c.Dynamic().Resource(gvr).Namespace(ow.Namespace).Patch(ctx, ow.Name, types.MergePatchType, []byte(body), metav1.PatchOptions{FieldManager: "lhtx"})
	return err
}

// runPostHooks waits for a ready pod of each app and runs its hook.
func runPostHooks(ctx context.Context, c *longhorn.Client, hooks []groupHook, o FreezeOptions) error {
	var failed []string
	for _, gh := range hooks {
		pod, err := waitReadyPod(ctx, c, gh.Owner, o)
		if err == nil {
			err = runHook(ctx, c, gh.Hook, gh.Owner.Namespace, pod, "post-unfreeze", o.Log)
		}
		if err != nil {
			failed = append(failed, gh.Owner.String()+": "+err.Error())
		}
	}
	if len(failed) > 0 {
		return fmt.Errorf("post-unfreeze hooks failed – run them by hand: %s", strings.Join(failed, "; "))
	}
	return nil
}

// waitReadyPod finds a running, ready pod of the owner (or of the CNPG
// cluster / StatefulSet whose pods name it).
func waitReadyPod(ctx context.Context, c *longhorn.Client, ow Owner, o FreezeOptions) (string, error) {
	deadline := time.Now().Add(o.Wait)
	for {
		pods, err := c.Dynamic().Resource(gvrPod).Namespace(ow.Namespace).List(ctx, metav1.ListOptions{})
		if err != nil {
			return "", err
		}
		for i := range pods.Items {
			p := &pods.Items[i]
			if co, ok := controllerOf(p); !ok || co.Kind != ow.Kind || co.Name != ow.Name {
				continue
			}
			if podReady(p) {
				return p.GetName(), nil
			}
		}
		if time.Now().After(deadline) {
			return "", fmt.Errorf("no ready pod of %s after %s", ow, o.Wait)
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(o.Poll):
		}
	}
}

func podReady(p *unstructured.Unstructured) bool {
	conds, _, _ := unstructured.NestedSlice(p.Object, "status", "conditions")
	for _, c := range conds {
		m, _ := c.(map[string]any)
		if m["type"] == "Ready" && m["status"] == "True" {
			return true
		}
	}
	return false
}

// GroupLines describes the apps for a preview: how each is stopped and
// which hooks run.
func (p *FreezePlan) GroupLines() []string {
	var out []string
	for _, g := range p.Groups {
		how := fmt.Sprintf("stop %d pod(s) one by one: %s", len(g.Pods), strings.Join(firstN(shortNames(g.Pods), 4), " → "))
		if len(g.Pods) == 1 {
			how = "stop " + shortNames(g.Pods)[0]
		}
		if g.Hibernate {
			how = fmt.Sprintf("CloudNativePG: hibernate (%s: on) – the operator stops its %d instance(s) cleanly", annCNPG, len(g.Pods))
		}
		out = append(out, g.name()+": "+how)
		if g.Pre != nil {
			out = append(out, "    before: "+g.Pre.String())
		}
		if g.Post != nil {
			out = append(out, "    after:  "+g.Post.String())
		}
	}
	return out
}

func shortNames(pods []string) []string {
	out := make([]string, len(pods))
	for i, x := range pods {
		_, n, _ := strings.Cut(x, "/")
		out[i] = n
	}
	return out
}

// HookCount counts the apps with hooks and without (for hints).
func (p *FreezePlan) HookCount() (with, without int) {
	for _, g := range p.Groups {
		if g.Pre != nil || g.Post != nil || g.Hibernate {
			with++
		} else {
			without++
		}
	}
	return
}
