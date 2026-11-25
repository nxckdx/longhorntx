package agent

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync/atomic"
	"time"

	"k8s.io/client-go/tools/leaderelection"
)

// Health for the kubelet's probes – read-only, no actions (requests to the
// agent still go only through the Kubernetes API):
//
//	/healthz  liveness: the leader's loop runs and its lease is renewed
//	/readyz   readiness/startup: a standby is ready at once, the leader
//	          once its watches are synced
type health struct {
	leading  atomic.Bool
	synced   atomic.Bool
	lastLoop atomic.Int64 // unix nanos of the last loop pass
	watchdog *leaderelection.HealthzAdaptor
}

func (h *health) beat() { h.lastLoop.Store(time.Now().UnixNano()) }

func (a *Agent) live() error {
	h := &a.health
	if h.watchdog != nil {
		if err := h.watchdog.Check(nil); err != nil {
			return err // leader whose lease is overdue
		}
	}
	if !h.leading.Load() || !h.synced.Load() {
		return nil
	}
	if age := time.Since(time.Unix(0, h.lastLoop.Load())); age > 3*a.Resync+time.Minute {
		return fmt.Errorf("the loop has not run for %s", age.Round(time.Second))
	}
	return nil
}

func (a *Agent) ready() error {
	if a.health.leading.Load() && !a.health.synced.Load() {
		return errors.New("watches not synced yet")
	}
	return nil
}

// ServeHealth answers the probes on addr until ctx ends.
func (a *Agent) ServeHealth(ctx context.Context, addr string) error {
	mux := http.NewServeMux()
	probe := func(check func() error) http.HandlerFunc {
		return func(w http.ResponseWriter, _ *http.Request) {
			if err := check(); err != nil {
				http.Error(w, err.Error(), http.StatusServiceUnavailable)
				return
			}
			_, _ = w.Write([]byte("ok\n"))
		}
	}
	mux.Handle("/healthz", probe(a.live))
	mux.Handle("/readyz", probe(a.ready))
	l, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		_ = srv.Close()
	}()
	if err := srv.Serve(l); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
