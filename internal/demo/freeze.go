package demo

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8stesting "k8s.io/client-go/testing"
)

// What a real cluster does around a freeze: the API server enforces lhtx's
// ValidatingAdmissionPolicy (pods mounting frozen PVCs are rejected, also
// in dry runs), Longhorn detaches a volume when its pods are gone, and
// controllers start pods again when nudged after the freeze.

var (
	gvrVAP  = schema.GroupVersionResource{Group: "admissionregistration.k8s.io", Version: "v1", Resource: "validatingadmissionpolicies"}
	gvrVAPB = schema.GroupVersionResource{Group: "admissionregistration.k8s.io", Version: "v1", Resource: "validatingadmissionpolicybindings"}
	gvrProm = schema.GroupVersionResource{Group: "monitoring.coreos.com", Version: "v1", Resource: "prometheuses"}
)

// podInfo remembers what is needed to start a pod again.
type podInfo struct {
	ns, name, node string
	claims         []string
	owners         []any
	ownerKey       string // ns/Kind/name of the controller
}

type admission struct {
	mu      sync.Mutex
	frozen  map[string]map[string]bool // policy → ns/pvc
	pods    map[string]*podInfo        // ns/name → running pod
	stopped map[string][]*podInfo      // ownerKey → pods deleted while frozen
}

func ownerKey(ns string, owners []any) string {
	if len(owners) == 0 {
		return ""
	}
	o := owners[0].(map[string]any)
	return ns + "/" + o["kind"].(string) + "/" + o["name"].(string)
}

func (a *admission) frozenClaim(ns string, claims []string) string {
	for name, set := range a.frozen {
		for _, c := range claims {
			if set[ns+"/"+c] {
				return name
			}
		}
	}
	return ""
}

// registerPods records the demo's pods (called with the initial objects).
func (s *sim) registerPods(objs []runtime.Object) {
	for _, o := range objs {
		u, ok := o.(*unstructured.Unstructured)
		if !ok || u.GetKind() != "Pod" {
			continue
		}
		s.adm.pods[u.GetNamespace()+"/"+u.GetName()] = podInfoOf(u)
	}
}

func podInfoOf(u *unstructured.Unstructured) *podInfo {
	var claims []string
	vols, _, _ := unstructured.NestedSlice(u.Object, "spec", "volumes")
	for _, v := range vols {
		if c, _, _ := unstructured.NestedString(v.(map[string]any), "persistentVolumeClaim", "claimName"); c != "" {
			claims = append(claims, c)
		}
	}
	node, _, _ := unstructured.NestedString(u.Object, "spec", "nodeName")
	owners, _, _ := unstructured.NestedSlice(u.Object, "metadata", "ownerReferences")
	return &podInfo{ns: u.GetNamespace(), name: u.GetName(), node: node, claims: claims, owners: owners, ownerKey: ownerKey(u.GetNamespace(), owners)}
}

func (s *sim) admissionReactors(ctx context.Context) {
	a := s.adm
	s.dyn.PrependReactor("create", "selfsubjectreviews", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, u(map[string]any{"apiVersion": "authentication.k8s.io/v1", "kind": "SelfSubjectReview",
			"status": map[string]any{"userInfo": map[string]any{"username": "demo-admin"}}}), nil
	})
	// the demo user may do everything
	s.dyn.PrependReactor("create", "subjectaccessreviews", func(act k8stesting.Action) (bool, runtime.Object, error) {
		o := act.(k8stesting.CreateActionImpl).GetObject().(*unstructured.Unstructured).DeepCopy()
		_ = unstructured.SetNestedField(o.Object, true, "status", "allowed")
		return true, o, nil
	})
	s.dyn.PrependReactor("create", "validatingadmissionpolicies", func(act k8stesting.Action) (bool, runtime.Object, error) {
		if isDryRun(act) {
			return false, nil, nil
		}
		obj := act.(k8stesting.CreateActionImpl).GetObject().(*unstructured.Unstructured)
		set := map[string]bool{}
		for _, x := range strings.Split(obj.GetAnnotations()["lhtx.io/pvcs"], ",") {
			set[x] = true
		}
		a.mu.Lock()
		a.frozen[obj.GetName()] = set
		a.mu.Unlock()
		return false, nil, nil
	})
	s.dyn.PrependReactor("delete", "validatingadmissionpolicies", func(act k8stesting.Action) (bool, runtime.Object, error) {
		a.mu.Lock()
		delete(a.frozen, act.(k8stesting.DeleteActionImpl).GetName())
		a.mu.Unlock()
		return false, nil, nil
	})
	s.dyn.PrependReactor("create", "pods", func(act k8stesting.Action) (bool, runtime.Object, error) {
		ca := act.(k8stesting.CreateActionImpl)
		obj := ca.GetObject().(*unstructured.Unstructured)
		pi := podInfoOf(obj)
		a.mu.Lock()
		defer a.mu.Unlock()
		if name := a.frozenClaim(act.GetNamespace(), pi.claims); name != "" {
			return true, nil, apierrors.NewForbidden(gvrPod.GroupResource(), obj.GetName(),
				errors.New("ValidatingAdmissionPolicy '"+name+"' denied request: frozen by lhtx ("+name+")"))
		}
		if len(ca.CreateOptions.DryRun) > 0 {
			return true, obj, nil
		}
		a.pods[pi.ns+"/"+pi.name] = pi
		return false, nil, nil
	})
	// pods gone → their volumes detach; remembered for a restart
	s.dyn.PrependReactor("delete", "pods", func(act k8stesting.Action) (bool, runtime.Object, error) {
		if isDryRun(act) {
			return false, nil, nil
		}
		key := act.GetNamespace() + "/" + act.(k8stesting.DeleteActionImpl).GetName()
		a.mu.Lock()
		pi := a.pods[key]
		delete(a.pods, key)
		if pi != nil && pi.ownerKey != "" {
			a.stopped[pi.ownerKey] = append(a.stopped[pi.ownerKey], pi)
		}
		a.mu.Unlock()
		if pi != nil {
			go func() {
				time.Sleep(1200 * time.Millisecond)
				for _, c := range pi.claims {
					s.setVolumeState(ctx, pi.ns, c, "detached", "")
				}
			}()
		}
		return false, nil, nil
	})
	// a nudged owner starts its pods again (the policy may still deny it)
	s.dyn.PrependReactor("patch", "*", func(act k8stesting.Action) (bool, runtime.Object, error) {
		pa, ok := act.(k8stesting.PatchActionImpl)
		if !ok {
			return false, nil, nil
		}
		if !strings.Contains(string(pa.GetPatch()), "lhtx.io/unfrozen-at") {
			return false, nil, nil
		}
		kind := map[string]string{"replicasets": "ReplicaSet", "statefulsets": "StatefulSet",
			"prometheuses": "Prometheus", "deployments": "Deployment"}[pa.GetResource().Resource]
		go s.restart(ctx, pa.GetNamespace()+"/"+kind+"/"+pa.GetName())
		return false, nil, nil
	})
}

func (s *sim) restart(ctx context.Context, key string) {
	time.Sleep(900 * time.Millisecond)
	s.adm.mu.Lock()
	pods := s.adm.stopped[key]
	delete(s.adm.stopped, key)
	s.adm.mu.Unlock()
	for _, pi := range pods {
		name := pi.name
		if strings.Contains(pi.ownerKey, "/ReplicaSet/") { // a new pod name, like a ReplicaSet
			name = name[:strings.LastIndex(name, "-")+1] + hex8(fmt.Sprint(time.Now().UnixNano()))[:5]
		}
		_, err := s.dyn.Resource(gvrPod).Namespace(pi.ns).Create(ctx, s.d.pod(pi.ns, name, pi.node, pi.claims, pi.owners), metav1.CreateOptions{})
		if err != nil {
			s.adm.mu.Lock()
			s.adm.stopped[key] = append(s.adm.stopped[key], pi) // still frozen: try again on the next nudge
			s.adm.mu.Unlock()
			continue
		}
		for _, c := range pi.claims {
			s.setVolumeState(ctx, pi.ns, c, "attached", pi.node)
		}
	}
}

// operatorObjects are the owners the demo's operator pods point to, and a
// lhtx-hooks ConfigMap with a rule for nextcloud (maintenance mode).
func operatorObjects() []runtime.Object {
	return []runtime.Object{
		u(map[string]any{"apiVersion": "v1", "kind": "ConfigMap",
			"metadata": map[string]any{"name": "lhtx-hooks", "namespace": ns},
			"data": map[string]any{"hooks.yaml": "- namespace: cloud\n  selector: {app: nextcloud}\n" +
				"  preFreeze: php occ maintenance:mode --on\n  postUnfreeze: php occ maintenance:mode --off\n  timeout: 1m\n"}}),
		u(map[string]any{"apiVersion": "monitoring.coreos.com/v1", "kind": "Prometheus",
			"metadata": map[string]any{"name": "prometheus", "namespace": "monitoring"}, "spec": map[string]any{"replicas": int64(1)}}),
	}
}
