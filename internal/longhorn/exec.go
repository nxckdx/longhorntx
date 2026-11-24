package longhorn

import (
	"bytes"
	"context"
	"fmt"
	"net/url"
	"strings"

	"k8s.io/apimachinery/pkg/util/httpstream"
	"k8s.io/client-go/tools/remotecommand"
)

// ExecFunc runs `sh -c command` in a container and returns its output.
type ExecFunc func(ctx context.Context, namespace, pod, container, command string) (string, error)

// SetExec replaces how commands are run (demo mode, tests).
func (c *Client) SetExec(f ExecFunc) { c.exec = f }

// Exec runs `sh -c command` in a container through the pods/exec
// subresource (like kubectl exec: WebSocket, SPDY as fallback) and returns
// stdout and stderr.
func (c *Client) Exec(ctx context.Context, namespace, pod, container, command string) (string, error) {
	if c.exec != nil {
		return c.exec(ctx, namespace, pod, container, command)
	}
	q := url.Values{}
	if container != "" {
		q.Set("container", container)
	}
	for _, a := range []string{"sh", "-c", command} {
		q.Add("command", a)
	}
	q.Set("stdout", "true")
	q.Set("stderr", "true")
	u, err := url.Parse(strings.TrimRight(c.Cluster.Config.Host, "/") + c.Cluster.Config.APIPath +
		fmt.Sprintf("/api/v1/namespaces/%s/pods/%s/exec?%s", namespace, pod, q.Encode()))
	if err != nil {
		return "", err
	}
	ws, err := remotecommand.NewWebSocketExecutor(c.Cluster.Config, "GET", u.String())
	if err != nil {
		return "", err
	}
	spdy, err := remotecommand.NewSPDYExecutor(c.Cluster.Config, "POST", u)
	if err != nil {
		return "", err
	}
	ex, err := remotecommand.NewFallbackExecutor(ws, spdy, func(err error) bool {
		return httpstream.IsUpgradeFailure(err) || httpstream.IsHTTPSProxyError(err)
	})
	if err != nil {
		return "", err
	}
	var out bytes.Buffer
	err = ex.StreamWithContext(ctx, remotecommand.StreamOptions{Stdout: &out, Stderr: &out})
	return out.String(), err
}
