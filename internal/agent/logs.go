package agent

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"

	"github.com/nxckdx/longhorntx/internal/longhorn"
	"github.com/nxckdx/longhorntx/internal/switchover"
)

// LogQuery (lhtx.io/v1alpha1, Longhorn namespace): lhtx diag asks the agent
// for the instance-manager and longhorn-manager logs of a volume's nodes.
// The agent reads them inside the cluster, keeps only the lines that
// mention the volume and puts them – gzipped – into the status, so only
// the matching lines travel over a slow link instead of megabytes of logs.
// The requester is vouched for by the admission policy (like Switches) and
// must be allowed to read pod logs in the Longhorn namespace themselves.
var GVRLogQuery = schema.GroupVersionResource{Group: "lhtx.io", Version: "v1alpha1", Resource: "logqueries"}

const (
	// logQueryMaxBytes bounds the encoded logs in a status (etcd objects
	// must stay below ~1.5 MB).
	logQueryMaxBytes = 900 << 10
	// logQueryTTL: finished queries are removed after this (lhtx removes
	// them itself after reading).
	logQueryTTL = 15 * time.Minute
)

// LogQuerySpec asks for the logs of a volume.
type LogQuerySpec struct {
	Volume          string   `json:"volume"`
	Nodes           []string `json:"nodes"`
	Since           string   `json:"since,omitempty"`
	LimitBytes      int64    `json:"limitBytes,omitempty"` // per container, read side
	RequestedBy     string   `json:"requestedBy"`
	RequestedGroups []string `json:"requestedGroups,omitempty"`
}

// LogQueryStatus carries the result.
type LogQueryStatus struct {
	Phase   string            `json:"phase,omitempty"` // Done | Failed
	Message string            `json:"message,omitempty"`
	Logs    map[string]string `json:"logs,omitempty"` // pod → gz:base64 of the matching lines
	Notes   []string          `json:"notes,omitempty"`
}

func (a *Agent) reconcileLogQuery(ctx context.Context, name string) {
	res := a.c.Dynamic().Resource(GVRLogQuery).Namespace(a.c.Namespace)
	u, err := res.Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return
	}
	var q struct {
		Spec   LogQuerySpec   `json:"spec"`
		Status LogQueryStatus `json:"status"`
	}
	b, _ := json.Marshal(u.Object)
	_ = json.Unmarshal(b, &q)
	if q.Status.Phase != "" {
		if time.Since(u.GetCreationTimestamp().Time) > logQueryTTL {
			_ = res.Delete(ctx, name, metav1.DeleteOptions{})
		}
		return
	}
	st := a.answerLogQuery(ctx, q.Spec)
	if st.Phase == "Done" {
		a.op("logs", "done")
	} else {
		a.op("logs", "failed")
	}
	patch, _ := json.Marshal(map[string]any{"status": st})
	if _, err := res.Patch(ctx, name, types.MergePatchType, patch, metav1.PatchOptions{FieldManager: "lhtx-agent"}, "status"); err != nil && !apierrors.IsNotFound(err) {
		a.Logf("logquery %s: %v", name, err)
	}
}

func (a *Agent) answerLogQuery(ctx context.Context, sp LogQuerySpec) LogQueryStatus {
	fail := func(f string, x ...any) LogQueryStatus {
		return LogQueryStatus{Phase: "Failed", Message: fmt.Sprintf(f, x...)}
	}
	if sp.RequestedBy == "" || sp.Volume == "" {
		return fail("spec.volume and spec.requestedBy are required")
	}
	if _, err := a.c.Dynamic().Resource(gvrVAP).Get(ctx, RequesterPolicy, metav1.GetOptions{}); err != nil {
		return fail("the admission policy %s is missing (it vouches for requestedBy)", RequesterPolicy)
	}
	ok, err := a.sar(ctx, sp.RequestedBy, sp.RequestedGroups, "get", "", "pods", "log", a.c.Namespace)
	if err != nil {
		return fail("%v", err)
	}
	if !ok {
		return fail("%s may not read pod logs in %s", sp.RequestedBy, a.c.Namespace)
	}
	opt := longhorn.DiagOptions{LimitBytes: sp.LimitBytes}
	if d, err := time.ParseDuration(sp.Since); err == nil {
		opt.Since = d
	}
	logs, notes, err := a.c.VolumeLogs(ctx, sp.Volume, sp.Nodes, opt)
	if err != nil {
		return fail("%v", err)
	}
	enc, more := encodeLogs(logs, logQueryMaxBytes)
	return LogQueryStatus{Phase: "Done", Logs: enc, Notes: append(notes, more...)}
}

// encodeLogs gzips each log; if all together exceed max, the largest logs
// keep only their newest lines until they fit.
func encodeLogs(logs map[string][]byte, max int) (map[string]string, []string) {
	var notes []string
	raw := map[string][]byte{}
	for k, v := range logs {
		raw[k] = v
	}
	for range 20 {
		out, total := map[string]string{}, 0
		for k, v := range raw {
			e := gz(v)
			out[k] = e
			total += len(e)
		}
		if total <= max {
			return out, notes
		}
		// halve the largest log, keeping its end (the newest lines)
		names := make([]string, 0, len(raw))
		for k := range raw {
			names = append(names, k)
		}
		sort.Slice(names, func(i, j int) bool { return len(raw[names[i]]) > len(raw[names[j]]) })
		big := raw[names[0]]
		cut := big[len(big)/2:]
		if i := bytes.IndexByte(cut, '\n'); i >= 0 {
			cut = cut[i+1:]
		}
		raw[names[0]] = cut
		notes = append(notes, "log "+names[0]+" shortened to its newest lines (the query result has a size limit)")
	}
	return map[string]string{}, append(notes, "logs too large even shortened – use lhtx diag --full-logs")
}

func gz(b []byte) string {
	var buf bytes.Buffer
	zw, _ := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	_, _ = zw.Write(b)
	_ = zw.Close()
	return "gz:" + base64.StdEncoding.EncodeToString(buf.Bytes())
}

func ungz(s string) ([]byte, error) {
	b64, ok := strings.CutPrefix(s, "gz:")
	if !ok {
		return []byte(s), nil
	}
	z, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return nil, err
	}
	zr, err := gzip.NewReader(bytes.NewReader(z))
	if err != nil {
		return nil, err
	}
	return io.ReadAll(zr)
}

// LogQueriesAvailable: the agent answers LogQueries (CRD installed, the
// caller may create them – a dry run).
func LogQueriesAvailable(ctx context.Context, c *longhorn.Client) bool {
	u := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "lhtx.io/v1alpha1", "kind": "LogQuery",
		"metadata": map[string]any{"generateName": "probe-", "namespace": c.Namespace},
		"spec":     map[string]any{"volume": "probe", "requestedBy": "probe"}}}
	_, err := c.Dynamic().Resource(GVRLogQuery).Namespace(c.Namespace).Create(ctx, u, metav1.CreateOptions{DryRun: []string{metav1.DryRunAll}})
	return err == nil || (apierrors.IsInvalid(err) || apierrors.IsForbidden(err)) && strings.Contains(err.Error(), RequesterPolicy)
}

// RemoteLogs asks the agent for a volume's filtered logs (for
// longhorn.DiagOptions.RemoteLogs): creates a LogQuery, waits for the
// answer, removes the query.
func RemoteLogs(c *longhorn.Client) func(ctx context.Context, vol string, nodes []string, opt longhorn.DiagOptions) (map[string][]byte, []string, error) {
	return func(ctx context.Context, vol string, nodes []string, opt longhorn.DiagOptions) (map[string][]byte, []string, error) {
		who, err := switchover.WhoAmI(ctx, c.Dynamic())
		if err != nil {
			return nil, nil, err
		}
		nodesAny := make([]any, len(nodes))
		for i, n := range nodes {
			nodesAny[i] = n
		}
		groups := make([]any, len(who.Groups))
		for i, g := range who.Groups {
			groups[i] = g
		}
		spec := map[string]any{"volume": vol, "nodes": nodesAny, "requestedBy": who.Username, "requestedGroups": groups}
		if opt.Since > 0 {
			spec["since"] = opt.Since.String()
		}
		if opt.LimitBytes > 0 {
			spec["limitBytes"] = opt.LimitBytes
		}
		u := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "lhtx.io/v1alpha1", "kind": "LogQuery",
			"metadata": map[string]any{"generateName": "lhtx-diag-", "namespace": c.Namespace}, "spec": spec}}
		res := c.Dynamic().Resource(GVRLogQuery).Namespace(c.Namespace)
		created, err := res.Create(ctx, u, metav1.CreateOptions{FieldManager: "lhtx"})
		if err != nil {
			return nil, nil, err
		}
		name := created.GetName()
		defer func() { _ = res.Delete(context.WithoutCancel(ctx), name, metav1.DeleteOptions{}) }()
		deadline := time.Now().Add(3 * time.Minute)
		for {
			cur, err := res.Get(ctx, name, metav1.GetOptions{})
			if err != nil {
				return nil, nil, err
			}
			var st struct {
				Status LogQueryStatus `json:"status"`
			}
			b, _ := json.Marshal(cur.Object)
			_ = json.Unmarshal(b, &st)
			switch st.Status.Phase {
			case "Failed":
				return nil, nil, fmt.Errorf("%s", st.Status.Message)
			case "Done":
				out := map[string][]byte{}
				for k, v := range st.Status.Logs {
					d, err := ungz(v)
					if err != nil {
						return nil, nil, err
					}
					out[k] = d
				}
				return out, st.Status.Notes, nil
			}
			if time.Now().After(deadline) {
				return nil, nil, fmt.Errorf("no answer from the agent within 3 minutes")
			}
			select {
			case <-ctx.Done():
				return nil, nil, ctx.Err()
			case <-time.After(time.Second):
			}
		}
	}
}
