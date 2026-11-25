// Package agent is the optional in-cluster part of lhtx. "lhtx agent" runs
// as a Deployment and carries out freezes requested as Freeze resources
// (lhtx.io/v1alpha1), so a freeze does not depend on a laptop staying
// online: the agent stops the apps, keeps watch, can end a freeze on its
// own (autoUnfreezeAfter) and resumes after a restart.
//
// Requests go only through the Kubernetes API: whoever may create a Freeze
// (RBAC) may freeze; the agent has no HTTP endpoint. lhtx works without the
// agent exactly as before – it uses the agent only when it finds it alive.
package agent

import (
	"encoding/json"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/nxckdx/longhorntx/internal/switchover"
)

// GVRFreeze is the cluster-scoped Freeze resource.
var GVRFreeze = schema.GroupVersionResource{Group: "lhtx.io", Version: "v1alpha1", Resource: "freezes"}

var gvrLease = schema.GroupVersionResource{Group: "coordination.k8s.io", Version: "v1", Resource: "leases"}

const (
	// LeaseName is the agent's leader election lease (in the Longhorn
	// namespace); lhtx reads it to see whether an agent is alive.
	LeaseName = "lhtx-agent"
	// LeaseLabel marks the lease, so lhtx also finds it outside the
	// Longhorn namespace (a LIST over all namespaces, needs the right).
	LeaseLabel = "app.kubernetes.io/name=lhtx-agent"
	Finalizer  = "lhtx.io/unfreeze"
	// AnnRequestedBy: the user who created the Freeze, as lhtx saw it
	// (informational – set by the client).
	AnnRequestedBy = "lhtx.io/requested-by"

	PhasePending    = "Pending"
	PhaseFreezing   = "Freezing"
	PhaseFrozen     = "Frozen"
	PhaseUnfreezing = "Unfreezing"
	PhaseFailed     = "Failed"

	maxLog = 60
)

// Spec of a Freeze.
type Spec struct {
	PVCs   []string `json:"pvcs"` // namespace/name
	Reason string   `json:"reason,omitempty"`
	// AutoUnfreezeAfter ends the freeze on its own (Go duration, e.g. "4h"),
	// counted from when it is frozen: a watchdog for forgotten freezes.
	AutoUnfreezeAfter string `json:"autoUnfreezeAfter,omitempty"`
	// Wait for detach / attach (default 10m).
	Wait string `json:"wait,omitempty"`
}

// Status of a Freeze, written by the agent.
type Status struct {
	Phase    string             `json:"phase,omitempty"`
	Message  string             `json:"message,omitempty"`
	Policy   string             `json:"policy,omitempty"`
	FrozenAt *metav1.Time       `json:"frozenAt,omitempty"`
	Agent    string             `json:"agent,omitempty"` // pod that works on it
	Freeze   *switchover.Freeze `json:"freeze,omitempty"`
	// Log holds the last lines; LogCount counts all lines ever written, so
	// a follower knows which ones it has not seen.
	Log      []string `json:"log,omitempty"`
	LogCount int      `json:"logCount,omitempty"`
}

// Freeze is the resource as lhtx reads it.
type Freeze struct {
	Name        string
	Spec        Spec
	Status      Status
	Deleting    bool
	Finalizers  []string
	Created     time.Time
	RequestedBy string
}

// PolicyName of the ValidatingAdmissionPolicy that belongs to a Freeze.
func PolicyName(freeze string) string { return "lhtx-freeze-" + freeze }

func fromU(u *unstructured.Unstructured) (*Freeze, error) {
	var raw struct {
		Spec   Spec   `json:"spec"`
		Status Status `json:"status"`
	}
	b, err := json.Marshal(u.Object)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return nil, err
	}
	return &Freeze{Name: u.GetName(), Spec: raw.Spec, Status: raw.Status, Deleting: u.GetDeletionTimestamp() != nil,
		Finalizers: u.GetFinalizers(), Created: u.GetCreationTimestamp().Time, RequestedBy: u.GetAnnotations()[AnnRequestedBy]}, nil
}

func durationOr(s string, d time.Duration) time.Duration {
	if v, err := time.ParseDuration(s); err == nil && v > 0 {
		return v
	}
	return d
}
