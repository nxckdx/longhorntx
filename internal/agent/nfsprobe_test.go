package agent

import (
	"encoding/binary"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/nxckdx/longhorntx/internal/longhorn"
)

// fakeNFS answers NULL calls like an NFS server (answer=true) or reads and
// never answers (a hung server).
func fakeNFS(t *testing.T, answer bool) string {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				req := make([]byte, 44)
				if _, err := io.ReadFull(c, req); err != nil {
					return
				}
				if !answer {
					time.Sleep(5 * time.Second)
					return
				}
				rep := make([]byte, 28)
				binary.BigEndian.PutUint32(rep[0:], 0x80000000|24)
				copy(rep[4:8], req[4:8])               // xid
				binary.BigEndian.PutUint32(rep[8:], 1) // REPLY; accepted, AUTH_NONE verifier, SUCCESS = zeros
				_, _ = c.Write(rep)
			}()
		}
	}()
	return ln.Addr().String()
}

func TestNFSNull(t *testing.T) {
	if err := nfsNull(t.Context(), fakeNFS(t, true), time.Second); err != nil {
		t.Errorf("answering server: %v", err)
	}
	err := nfsNull(t.Context(), fakeNFS(t, false), 300*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "no answer") {
		t.Errorf("hung server: %v", err)
	}
	if a, ok := nfsAddr("nfs://10.43.235.0/pvc-1"); !ok || a != "10.43.235.0:2049" {
		t.Errorf("addr = %s", a)
	}
}

func TestNFSResultTwoFailures(t *testing.T) {
	a := &Agent{Logf: t.Logf}
	a.notifier = newNotifier(a, Notify{URL: "http://unused"})
	st := map[string]*nfsState{}
	now := time.Now()
	fail := io.ErrUnexpectedEOF
	a.nfsResult(t.Context(), st, "pvc-1", fail, now)
	if len(a.notifier.queue) != 0 {
		t.Fatal("one failure must not report")
	}
	a.nfsResult(t.Context(), st, "pvc-1", fail, now.Add(time.Minute))
	a.nfsResult(t.Context(), st, "pvc-1", fail, now.Add(2*time.Minute))
	a.nfsResult(t.Context(), st, "pvc-1", nil, now.Add(3*time.Minute))
	q := a.notifier.queue
	if len(q) != 2 || !q[0].Bad || q[1].Bad || q[0].Kind != longhorn.HistShareManager {
		t.Fatalf("notices = %+v", q)
	}
}
