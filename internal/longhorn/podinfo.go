package longhorn

import (
	"context"
	"encoding/json"
	"fmt"
	"io"

	corev1 "k8s.io/api/core/v1"
)

// PodLogTail returns the last lines of a pod's log (any namespace).
func (c *Client) PodLogTail(ctx context.Context, namespace, pod string, lines int64) (string, error) {
	cl, err := c.core()
	if err != nil {
		return "", err
	}
	rc, err := cl.Pods(namespace).GetLogs(pod, &corev1.PodLogOptions{TailLines: &lines}).Stream(ctx)
	if err != nil {
		return "", err
	}
	defer rc.Close()
	b, err := io.ReadAll(io.LimitReader(rc, 1<<20))
	return string(b), err
}

// VolumeUsage asks kubelet on node how many bytes the filesystem of a
// mounted PVC uses (/stats/summary through the node proxy – needs get on
// nodes/proxy).
func (c *Client) VolumeUsage(ctx context.Context, node, namespace, pvc string) (int64, error) {
	cl, err := c.core()
	if err != nil {
		return 0, err
	}
	rc := cl.RESTClient()
	if rc == nil {
		return 0, fmt.Errorf("no API client")
	}
	ctx, cancel := context.WithTimeout(ctx, kubeletTimeout)
	defer cancel()
	b, err := rc.Get().AbsPath("/api/v1/nodes", node, "proxy", "stats", "summary").DoRaw(ctx)
	if err != nil {
		return 0, err
	}
	var sum struct {
		Pods []struct {
			Volume []struct {
				UsedBytes *int64 `json:"usedBytes"`
				PVCRef    *struct {
					Name      string `json:"name"`
					Namespace string `json:"namespace"`
				} `json:"pvcRef"`
			} `json:"volume"`
		} `json:"pods"`
	}
	if err := json.Unmarshal(b, &sum); err != nil {
		return 0, err
	}
	for _, p := range sum.Pods {
		for _, v := range p.Volume {
			if v.PVCRef != nil && v.PVCRef.Name == pvc && v.PVCRef.Namespace == namespace && v.UsedBytes != nil {
				return *v.UsedBytes, nil
			}
		}
	}
	return 0, fmt.Errorf("kubelet on %s reports no usage for %s/%s", node, namespace, pvc)
}
