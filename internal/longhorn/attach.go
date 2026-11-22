package longhorn

import (
	"context"
	"fmt"
	"sort"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// Manual attach/detach (Longhorn ≥ 1.5): a volume is attached as long as
// its VolumeAttachment CR (same name as the volume) holds an attachment
// ticket. The web UI adds the ticket "longhorn-ui" of type "longhorn-api"
// and detach removes exactly that ticket (longhorn-ui v1.12.1,
// longhorn-manager VolumeManager.Attach/Detach). lhtx uses the same ticket,
// so attach in lhtx / detach in the UI (and vice versa) work together.

var GVRVolumeAttachment = gvr("volumeattachments")

const (
	UITicketID   = "longhorn-ui"
	UITicketType = "longhorn-api"
)

type VolumeAttachment struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata"`
	Spec              struct {
		Volume            string                       `json:"volume"`
		AttachmentTickets map[string]*AttachmentTicket `json:"attachmentTickets"`
	} `json:"spec"`
	Status struct {
		AttachmentTicketStatuses map[string]struct {
			Satisfied  bool        `json:"satisfied"`
			Conditions []Condition `json:"conditions"`
		} `json:"attachmentTicketStatuses"`
	} `json:"status"`
}

type AttachmentTicket struct {
	ID         string            `json:"id"`
	Type       string            `json:"type"`
	NodeID     string            `json:"nodeID"`
	Parameters map[string]string `json:"parameters"`
}

func (o *VolumeAttachment) DeepCopyObject() runtime.Object { c := *o; return &c }

func (c *Client) VolumeAttachment(ctx context.Context, volume string) (*VolumeAttachment, error) {
	u, err := c.res(GVRVolumeAttachment).Get(ctx, volume, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	return convert[VolumeAttachment](u)
}

// Tickets describes the attachment tickets, e.g. "csi-attacher@node-1".
func (va *VolumeAttachment) Tickets() []string {
	var out []string
	for _, t := range va.Spec.AttachmentTickets {
		s := t.Type + "@" + t.NodeID
		if t.ID == UITicketID {
			s = "manual (" + UITicketID + ")@" + t.NodeID
		}
		if t.Parameters["disableFrontend"] == "true" {
			s += " maintenance"
		}
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// AttachVolume attaches a volume to a node like the web UI's "Attach".
// maintenance attaches without frontend (no block device on the node), as
// the UI's "Maintenance" checkbox does – e.g. to revert a snapshot.
func AttachVolume(v *Volume, va *VolumeAttachment, node *Node, maintenance bool) (*Action, error) {
	if err := checkVolumeWritable(v); err != nil {
		return nil, err
	}
	if node == nil || !node.Ready() {
		return nil, fmt.Errorf("node is not ready")
	}
	if t, ok := va.Spec.AttachmentTickets[UITicketID]; ok {
		return nil, fmt.Errorf("volume is already attached manually to %s (detach first)", t.NodeID)
	}
	if v.Status.State == "attached" && v.Status.CurrentNodeID != "" && v.Status.CurrentNodeID != node.Name {
		return nil, fmt.Errorf("volume is attached to %s; Longhorn attaches a volume to one node only (attach to %s or detach first)", v.Status.CurrentNodeID, v.Status.CurrentNodeID)
	}
	for _, c := range v.Status.Conditions {
		if c.Type == "Restore" && c.Status == "True" {
			return nil, fmt.Errorf("volume is restoring data")
		}
	}
	a := &Action{
		Title: fmt.Sprintf("Attach %s to %s", volLabel(v), node.Name),
		Verb:  VerbPatch, GVR: GVRVolumeAttachment, Name: va.Name,
		Patch: patchWithRV(va.ResourceVersion, map[string]any{"attachmentTickets": map[string]any{
			UITicketID: map[string]any{
				"id": UITicketID, "type": UITicketType, "nodeID": node.Name,
				"parameters": map[string]any{"disableFrontend": fmt.Sprint(maintenance), "lastAttachedBy": ""},
			},
		}}),
		Changes: []Change{{"spec.attachmentTickets." + UITicketID, "", UITicketType + "@" + node.Name}},
	}
	if maintenance {
		a.Title += " (maintenance mode, no frontend)"
	}
	if v.Status.State == "attached" {
		a.Warnings = append(a.Warnings, "volume is already attached ("+strings.Join(va.Tickets(), ", ")+"): the manual ticket keeps it attached after the pod is gone")
	}
	a.Warnings = append(a.Warnings, "it stays attached until you detach it (lhtx or the Longhorn UI)")
	return a, nil
}

// DetachVolume removes the manual ticket. Tickets of other attachers (CSI
// for a running pod, backups, …) are left alone: the volume then stays
// attached for them.
func DetachVolume(v *Volume, va *VolumeAttachment) (*Action, error) {
	if v.Status.IsStandby {
		return nil, fmt.Errorf("cannot detach standby volume %s", v.Name)
	}
	t, ok := va.Spec.AttachmentTickets[UITicketID]
	if !ok {
		others := va.Tickets()
		if len(others) == 0 {
			return nil, fmt.Errorf("volume has no attachment tickets")
		}
		return nil, fmt.Errorf("volume was not attached manually; attached by %s – detach the workload instead (scale down the pod)", strings.Join(others, ", "))
	}
	a := &Action{
		Title: fmt.Sprintf("Detach %s from %s", volLabel(v), t.NodeID),
		Verb:  VerbPatch, GVR: GVRVolumeAttachment, Name: va.Name,
		Patch:   patchWithRV(va.ResourceVersion, map[string]any{"attachmentTickets": map[string]any{UITicketID: nil}}),
		Changes: []Change{{"spec.attachmentTickets." + UITicketID, UITicketType + "@" + t.NodeID, ""}},
	}
	var others []string
	for id, o := range va.Spec.AttachmentTickets {
		if id != UITicketID {
			others = append(others, o.Type+"@"+o.NodeID)
		}
	}
	if len(others) > 0 {
		sort.Strings(others)
		a.Warnings = append(a.Warnings, "stays attached for: "+strings.Join(others, ", "))
	} else if v.Status.State == "attached" {
		a.Warnings = append(a.Warnings, "the volume will be detached: anything using the block device on "+t.NodeID+" loses it")
		a.Destructive = true
	}
	return a, nil
}
