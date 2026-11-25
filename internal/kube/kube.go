// Package kube loads kubeconfig contexts and builds rest.Configs whose network
// traffic is measured on the TCP level.
package kube

import (
	"context"
	"fmt"
	"net"
	"os"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

// Options mirror the usual kubectl flags.
type Options struct {
	Kubeconfig string // explicit path; empty = KUBECONFIG / ~/.kube/config
	Context    string // empty = current-context
}

func (o Options) loader() clientcmd.ClientConfig {
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	if o.Kubeconfig != "" {
		rules.ExplicitPath = o.Kubeconfig
	}
	return clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules,
		&clientcmd.ConfigOverrides{CurrentContext: o.Context})
}

// RawConfig returns the merged kubeconfig.
func (o Options) RawConfig() (clientcmdapi.Config, error) {
	return o.loader().RawConfig()
}

// Contexts returns all context names, sorted, plus the current one.
func (o Options) Contexts() (names []string, current string, err error) {
	raw, err := o.RawConfig()
	if err != nil {
		return nil, "", err
	}
	for n := range raw.Contexts {
		names = append(names, n)
	}
	sort.Strings(names)
	current = raw.CurrentContext
	if o.Context != "" {
		current = o.Context
	}
	return names, current, nil
}

// Cluster bundles a rest.Config with its context name and a traffic counter.
type Cluster struct {
	Context string
	Config  *rest.Config
	Traffic *Counter
}

// Connect builds a rest.Config for the selected context. All TCP connections
// opened by clients built from it are counted in Cluster.Traffic.
func Connect(o Options) (*Cluster, error) {
	cc := o.loader()
	raw, err := cc.RawConfig()
	if err != nil {
		return nil, err
	}
	name := o.Context
	if name == "" {
		name = raw.CurrentContext
	}
	if _, ok := raw.Contexts[name]; !ok {
		return nil, fmt.Errorf("kubeconfig context %q not found", name)
	}
	cfg, err := cc.ClientConfig()
	if err != nil {
		return nil, err
	}
	return wrap(name, cfg), nil
}

// InCluster builds the Cluster from the pod's service account (lhtx agent).
func InCluster() (*Cluster, error) {
	cfg, err := rest.InClusterConfig()
	if err != nil {
		return nil, err
	}
	return wrap("in-cluster", cfg), nil
}

func wrap(name string, cfg *rest.Config) *Cluster {
	cfg.UserAgent = "lhtx"
	// API server warnings (e.g. about Longhorn's own finalizer names) went
	// to stderr via klog – noise for the user, and they break the TUI.
	cfg.WarningHandler = rest.NoWarnings{}
	// Defaults are tuned for controllers; we issue few requests.
	cfg.QPS, cfg.Burst = 20, 40

	ctr := &Counter{}
	// LHTX_THROTTLE_KBPS limits the download rate to simulate slow links
	// (e.g. LTE) when testing on a fast network.
	if kbps, _ := strconv.Atoi(os.Getenv("LHTX_THROTTLE_KBPS")); kbps > 0 {
		ctr.limit = float64(kbps) * 1024
	}
	d := &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}
	cfg.Dial = func(ctx context.Context, network, addr string) (net.Conn, error) {
		c, err := d.DialContext(ctx, network, addr)
		if err != nil {
			return nil, err
		}
		return &countingConn{Conn: c, ctr: ctr}, nil
	}
	return &Cluster{Context: name, Config: cfg, Traffic: ctr}
}

// Counter tracks bytes on the wire (after TLS, i.e. what your LTE stick sees,
// minus TCP/IP headers).
type Counter struct {
	rx, tx atomic.Int64

	limit   float64 // bytes/s, 0 = unlimited (shared by all connections)
	mu      sync.Mutex
	allowed time.Time // earliest time the next read may complete
}

func (c *Counter) RX() int64 { return c.rx.Load() }
func (c *Counter) TX() int64 { return c.tx.Load() }

type countingConn struct {
	net.Conn
	ctr *Counter
}

func (c *countingConn) Read(b []byte) (int, error) {
	if c.ctr.limit > 0 && len(b) > 4096 {
		b = b[:4096] // small reads keep the throttle smooth
	}
	n, err := c.Conn.Read(b)
	c.ctr.rx.Add(int64(n))
	if c.ctr.limit > 0 && n > 0 {
		c.ctr.throttle(n)
	}
	return n, err
}

func (c *Counter) throttle(n int) {
	c.mu.Lock()
	now := time.Now()
	if c.allowed.Before(now) {
		c.allowed = now
	}
	c.allowed = c.allowed.Add(time.Duration(float64(n) / c.limit * float64(time.Second)))
	wait := c.allowed.Sub(now)
	c.mu.Unlock()
	time.Sleep(wait)
}

func (c *countingConn) Write(b []byte) (int, error) {
	n, err := c.Conn.Write(b)
	c.ctr.tx.Add(int64(n))
	return n, err
}
