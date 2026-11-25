package agent

import (
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/tools/cache"
)

// Prometheus metrics on the health port (/metrics, text format, no client
// library): what the agent does and saves. Counts only – no names of
// volumes, PVCs or people.
//
// How many lhtx TUIs use the agent's digests is not something the agent can
// see (lhtx only talks to the API server); the API server's own metric
// apiserver_longrunning_requests{resource="volumedigests",verb="WATCH"}
// counts their watches (plus the agent's own one) – the Grafana dashboard
// in the chart shows it.
type metrics struct {
	mu  sync.Mutex
	ops map[[2]string]int64 // kind, result → count

	volumeEvents  atomic.Int64 // volume updates the agent saw (what lhtx would have received)
	digestVolumes atomic.Int64 // volumes with a digest
	historyEvents atomic.Int64 // history events recorded
	historyKinds  sync.Map     // kind → *atomic.Int64
	rebuildSecs   atomic.Int64 // summed durations of finished rebuilds
	rebuilds      atomic.Int64

	freezes, switches cache.Store // set by Run (leader)
	version           string
}

// op counts a finished operation: kind freeze|unfreeze|auto-unfreeze|switch,
// result done|failed|stuck|key-changed.
func (a *Agent) op(kind, result string) {
	a.metrics.mu.Lock()
	defer a.metrics.mu.Unlock()
	if a.metrics.ops == nil {
		a.metrics.ops = map[[2]string]int64{}
	}
	a.metrics.ops[[2]string{kind, result}]++
}

// SetVersion labels lhtx_agent_build_info.
func (a *Agent) SetVersion(v string) { a.metrics.version = v }

func (a *Agent) writeMetrics(w io.Writer) {
	m := &a.metrics
	p := func(name, typ, help string) {
		fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s %s\n", name, help, name, typ)
	}
	b2i := func(b bool) int {
		if b {
			return 1
		}
		return 0
	}
	p("lhtx_agent_build_info", "gauge", "Version of the lhtx agent.")
	fmt.Fprintf(w, "lhtx_agent_build_info{version=%q} 1\n", m.version)
	p("lhtx_agent_leader", "gauge", "1 if this agent holds the lease and works, 0 on a standby.")
	fmt.Fprintf(w, "lhtx_agent_leader %d\n", b2i(a.health.leading.Load()))

	if m.freezes != nil {
		phases, pvcs := map[string]int{}, 0
		for _, o := range m.freezes.List() {
			if f, err := fromU(o.(*unstructured.Unstructured)); err == nil {
				ph := f.Status.Phase
				if f.Deleting {
					ph = PhaseUnfreezing
				}
				phases[orDash(ph)]++
				if ph == PhaseFrozen || ph == PhaseFreezing {
					pvcs += len(f.Spec.PVCs)
				}
			}
		}
		p("lhtx_agent_freezes", "gauge", "Freeze resources by phase.")
		writeByLabel(w, "lhtx_agent_freezes", "phase", phases)
		p("lhtx_agent_frozen_pvcs", "gauge", "PVCs frozen or being frozen by the agent.")
		fmt.Fprintf(w, "lhtx_agent_frozen_pvcs %d\n", pvcs)
	}
	if m.switches != nil {
		phases := map[string]int{}
		for _, o := range m.switches.List() {
			if s, err := switchFromU(o.(*unstructured.Unstructured)); err == nil {
				phases[orDash(s.Status.Phase)]++
			}
		}
		p("lhtx_agent_switches", "gauge", "Switch resources by phase.")
		writeByLabel(w, "lhtx_agent_switches", "phase", phases)
	}

	p("lhtx_agent_operations_total", "counter", "Finished operations by kind and result.")
	m.mu.Lock()
	keys := make([][2]string, 0, len(m.ops))
	for k := range m.ops {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i][0]+keys[i][1] < keys[j][0]+keys[j][1] })
	for _, k := range keys {
		fmt.Fprintf(w, "lhtx_agent_operations_total{kind=%q,result=%q} %d\n", k[0], k[1], m.ops[k])
	}
	m.mu.Unlock()

	p("lhtx_agent_volume_events_total", "counter", "Volume updates the agent received (what every lhtx watching volumes would receive).")
	fmt.Fprintf(w, "lhtx_agent_volume_events_total %d\n", m.volumeEvents.Load())
	p("lhtx_agent_digest_writes_total", "counter", "VolumeDigests written (what lhtx in agent mode receives instead).")
	fmt.Fprintf(w, "lhtx_agent_digest_writes_total %d\n", a.digestWrites.Load())
	p("lhtx_agent_digest_volumes", "gauge", "Volumes with a VolumeDigest.")
	fmt.Fprintf(w, "lhtx_agent_digest_volumes %d\n", m.digestVolumes.Load())
	p("lhtx_agent_history_events_total", "counter", "History events recorded by kind (rebuilt, replica-failed, backup-failed, …).")
	var kinds []string
	m.historyKinds.Range(func(k, _ any) bool { kinds = append(kinds, k.(string)); return true })
	sort.Strings(kinds)
	for _, k := range kinds {
		v, _ := m.historyKinds.Load(k)
		fmt.Fprintf(w, "lhtx_agent_history_events_total{kind=%q} %d\n", k, v.(*atomic.Int64).Load())
	}
	p("lhtx_agent_rebuild_seconds", "summary", "Durations of finished rebuilds.")
	fmt.Fprintf(w, "lhtx_agent_rebuild_seconds_sum %d\nlhtx_agent_rebuild_seconds_count %d\n", m.rebuildSecs.Load(), m.rebuilds.Load())
}

func writeByLabel(w io.Writer, name, label string, by map[string]int) {
	keys := make([]string, 0, len(by))
	for k := range by {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(w, "%s{%s=%q} %d\n", name, label, k, by[k])
	}
}

func orDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "-"
	}
	return s
}

func (a *Agent) metricsHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		a.writeMetrics(w)
	}
}
