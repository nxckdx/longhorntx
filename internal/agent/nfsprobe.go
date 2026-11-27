package agent

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"sync"
	"time"

	"github.com/nxckdx/longhorntx/internal/longhorn"
)

// NFS probe: a hung NFS server (the share manager of an RWX volume) leaves
// everything Kubernetes and Longhorn report green – the pod is Ready, the
// ShareManager "running" – while every client hangs. Only asking the server
// tells. Every Interval the agent sends each running share manager an NFS
// NULL call (ONC RPC "ping", 44 bytes) and waits Timeout for the answer.
// After two failures in a row the volume's history gets a share-manager
// event (notification, alert); when it answers again, a recovery.

// NFSProbe configures it; Interval 0 = off.
type NFSProbe struct {
	Interval time.Duration
	Timeout  time.Duration // default 10s
}

const (
	nfsProgram = 100003
	nfsVersion = 4
	nfsPort    = "2049"
)

// nfsNull sends an NFSv4 NULL call to addr (host:port) and checks the reply.
func nfsNull(ctx context.Context, addr string, timeout time.Duration) error {
	d := net.Dialer{Timeout: timeout}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(timeout))
	xid := uint32(time.Now().UnixNano())
	msg := make([]byte, 44)
	binary.BigEndian.PutUint32(msg[0:], 0x80000000|40) // record mark: last fragment, 40 bytes
	binary.BigEndian.PutUint32(msg[4:], xid)
	binary.BigEndian.PutUint32(msg[8:], 0)  // CALL
	binary.BigEndian.PutUint32(msg[12:], 2) // RPC version
	binary.BigEndian.PutUint32(msg[16:], nfsProgram)
	binary.BigEndian.PutUint32(msg[20:], nfsVersion)
	binary.BigEndian.PutUint32(msg[24:], 0) // procedure NULL
	// credential and verifier: AUTH_NONE, length 0 (bytes 28..43 stay zero)
	if _, err := conn.Write(msg); err != nil {
		return fmt.Errorf("send: %w", err)
	}
	head := make([]byte, 4)
	if _, err := io.ReadFull(conn, head); err != nil {
		var ne net.Error
		if errors.As(err, &ne) && ne.Timeout() {
			return fmt.Errorf("no answer within %s", timeout)
		}
		return fmt.Errorf("read: %w", err)
	}
	n := binary.BigEndian.Uint32(head) & 0x7fffffff
	if n < 24 || n > 1024 {
		return fmt.Errorf("bad reply length %d", n)
	}
	body := make([]byte, n)
	if _, err := io.ReadFull(conn, body); err != nil {
		return fmt.Errorf("read: %w", err)
	}
	if binary.BigEndian.Uint32(body[0:]) != xid || binary.BigEndian.Uint32(body[4:]) != 1 {
		return fmt.Errorf("not an RPC reply")
	}
	if st := binary.BigEndian.Uint32(body[8:]); st != 0 {
		return fmt.Errorf("RPC call denied (%d)", st)
	}
	// verifier: flavor + length + body
	vlen := binary.BigEndian.Uint32(body[16:])
	off := 20 + int(vlen+3)/4*4
	if off+4 > len(body) {
		return fmt.Errorf("short reply")
	}
	if st := binary.BigEndian.Uint32(body[off:]); st != 0 {
		return fmt.Errorf("RPC call not accepted (%d)", st)
	}
	return nil
}

// nfsAddr: host:2049 from a ShareManager endpoint "nfs://10.43.0.5/pvc-…".
func nfsAddr(endpoint string) (string, bool) {
	u, err := url.Parse(endpoint)
	if err != nil || u.Hostname() == "" {
		return "", false
	}
	port := u.Port()
	if port == "" {
		port = nfsPort
	}
	return net.JoinHostPort(u.Hostname(), port), true
}

type nfsState struct {
	fails int
	down  bool
	since time.Time
}

func (a *Agent) runNFSProbe(ctx context.Context) {
	p := a.NFS
	if p.Interval <= 0 {
		return
	}
	if p.Timeout <= 0 {
		p.Timeout = 10 * time.Second
	}
	a.Logf("asking the NFS servers of RWX volumes every %s", p.Interval)
	states := map[string]*nfsState{}
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(p.Interval):
		}
		sms, err := a.c.ShareManagers(ctx)
		if err != nil {
			continue
		}
		type result struct {
			vol string
			err error
		}
		var wg sync.WaitGroup
		res := make(chan result, len(sms))
		for _, sm := range sms {
			addr, ok := nfsAddr(sm.Status.Endpoint)
			if sm.Status.State != "running" || !ok {
				continue
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				res <- result{sm.Name, nfsNull(ctx, addr, p.Timeout)}
			}()
		}
		wg.Wait()
		close(res)
		seen := map[string]bool{}
		for r := range res {
			seen[r.vol] = true
			a.nfsResult(ctx, states, r.vol, r.err, time.Now())
		}
		for vol := range states {
			if !seen[vol] {
				delete(states, vol) // stopped or gone: no verdict
			}
		}
		down := 0
		for _, s := range states {
			if s.down {
				down++
			}
		}
		a.metrics.nfsDown.Store(int64(down))
		a.metrics.nfsProbed.Store(true)
	}
}

// nfsResult: two failures in a row = down; one answer = up again.
func (a *Agent) nfsResult(ctx context.Context, states map[string]*nfsState, vol string, err error, now time.Time) {
	s := states[vol]
	if s == nil {
		s = &nfsState{}
		states[vol] = s
	}
	if err == nil {
		if s.down {
			a.recordMount(ctx, vol, longhorn.HistoryEvent{At: now.UTC(), Kind: longhorn.HistShareManager, Took: now.Sub(s.since).Round(time.Second),
				Text: "NFS server not answering → running"})
		}
		*s = nfsState{}
		return
	}
	s.fails++
	if s.fails == 1 {
		s.since = now
	}
	if s.fails == 2 && !s.down {
		s.down = true
		a.recordMount(ctx, vol, longhorn.HistoryEvent{At: now.UTC(), Kind: longhorn.HistShareManager,
			Text: fmt.Sprintf("NFS server does not answer (%v) although Kubernetes shows it running – client pods hang; lhtx --rw restart share-manager %s", err, vol)})
	}
}
