package longhorn

import (
	"context"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// Restarts of Longhorn's per-node / per-volume pods – the fixes the stuck
// mount and RWX checks point to. Both delete one pod (UID precondition);
// its controller (ShareManager / DaemonSet) starts a new one.

var GVRPod = schema.GroupVersionResource{Version: "v1", Resource: "pods"}

// ShareManagerPod is the share-manager pod of an RWX volume.
func (c *Client) ShareManagerPod(ctx context.Context, volume string) (*corev1.Pod, error) {
	return c.onePod(ctx, LabelShareManager+"="+volume, "share-manager pod of "+volume)
}

// CSIPluginPod is the longhorn-csi-plugin pod on a node.
func (c *Client) CSIPluginPod(ctx context.Context, node string) (*corev1.Pod, error) {
	cl, err := c.core()
	if err != nil {
		return nil, err
	}
	l, err := cl.Pods(c.Namespace).List(ctx, metav1.ListOptions{LabelSelector: "app=longhorn-csi-plugin", FieldSelector: "spec.nodeName=" + node})
	if err != nil {
		return nil, err
	}
	if len(l.Items) == 0 {
		return nil, fmt.Errorf("no longhorn-csi-plugin pod on node %s", node)
	}
	return &l.Items[0], nil
}

func (c *Client) onePod(ctx context.Context, selector, what string) (*corev1.Pod, error) {
	cl, err := c.core()
	if err != nil {
		return nil, err
	}
	l, err := cl.Pods(c.Namespace).List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		return nil, err
	}
	if len(l.Items) == 0 {
		return nil, fmt.Errorf("no %s", what)
	}
	return &l.Items[0], nil
}

// RestartShareManager deletes the share-manager pod of an RWX volume.
func RestartShareManager(v *Volume, pod *corev1.Pod) (*Action, error) {
	if v.Spec.AccessMode != "rwx" {
		return nil, fmt.Errorf("%s is not an RWX volume", volLabel(v))
	}
	a := &Action{
		Title: fmt.Sprintf("Restart the share manager of %s (pod %s on %s)", volLabel(v), pod.Name, pod.Spec.NodeName),
		Verb:  VerbDelete, GVR: GVRPod, Name: pod.Name, UID: string(pod.UID),
		Destructive: true,
		Warnings: []string{"the NFS server of the volume is gone until Longhorn has started a new share-manager pod (usually seconds to a minute): " +
			"client I/O fails (softerr) or blocks (hard) meanwhile; clients reconnect on their own, pods that still fail afterwards need a restart"},
	}
	var clients []string
	for _, w := range v.Status.KubernetesStatus.WorkloadsStatus {
		clients = append(clients, w.PodName)
	}
	if len(clients) > 0 {
		a.Warnings = append(a.Warnings, fmt.Sprintf("%d client pod(s): %s", len(clients), strings.Join(clients, ", ")))
	}
	return a, nil
}

// RestartCSIPlugin deletes the longhorn-csi-plugin pod on a node.
func RestartCSIPlugin(node string, pod *corev1.Pod) *Action {
	return &Action{
		Title: fmt.Sprintf("Restart the Longhorn CSI plugin on %s (pod %s)", node, pod.Name),
		Verb:  VerbDelete, GVR: GVRPod, Name: pod.Name, UID: string(pod.UID),
		Destructive: true,
		Warnings: []string{"mounts and unmounts of Longhorn volumes on this node wait until the DaemonSet has started the new pod; " +
			"volumes already mounted keep working"},
	}
}
