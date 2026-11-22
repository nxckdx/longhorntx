package longhorn

import (
	"strings"
	"testing"
)

func TestAttachDetach(t *testing.T) {
	node := &Node{}
	node.Name = "n1"
	node.Status.Conditions = []Condition{{Type: "Ready", Status: "True"}}
	v := &Volume{}
	v.Name = "pvc-1"
	v.Status.State = "detached"
	va := &VolumeAttachment{}
	va.Name, va.ResourceVersion = "pvc-1", "5"
	va.Spec.AttachmentTickets = map[string]*AttachmentTicket{}

	a, err := AttachVolume(v, va, node, true)
	if err != nil {
		t.Fatal(err)
	}
	tk := a.Patch["spec"].(map[string]any)["attachmentTickets"].(map[string]any)[UITicketID].(map[string]any)
	if tk["type"] != UITicketType || tk["nodeID"] != "n1" || tk["parameters"].(map[string]any)["disableFrontend"] != "true" {
		t.Errorf("unexpected ticket: %v", tk)
	}

	// attached by CSI elsewhere → refuse other nodes, detach refuses CSI tickets
	v.Status.State, v.Status.CurrentNodeID = "attached", "n2"
	va.Spec.AttachmentTickets["csi-x"] = &AttachmentTicket{ID: "csi-x", Type: "csi-attacher", NodeID: "n2"}
	if _, err := AttachVolume(v, va, node, false); err == nil {
		t.Error("attaching to a second node must be refused")
	}
	if _, err := DetachVolume(v, va); err == nil || !strings.Contains(err.Error(), "csi-attacher@n2") {
		t.Errorf("detach without manual ticket must name the attachers, got %v", err)
	}

	// manual + CSI ticket: detach removes only ours, not destructive
	va.Spec.AttachmentTickets[UITicketID] = &AttachmentTicket{ID: UITicketID, Type: UITicketType, NodeID: "n2"}
	a, err = DetachVolume(v, va)
	if err != nil {
		t.Fatal(err)
	}
	if a.Destructive || a.Patch["spec"].(map[string]any)["attachmentTickets"].(map[string]any)[UITicketID] != nil {
		t.Errorf("unexpected detach: destructive=%v patch=%v", a.Destructive, a.Patch)
	}
	// only our ticket on an attached volume: destructive
	delete(va.Spec.AttachmentTickets, "csi-x")
	if a, _ = DetachVolume(v, va); !a.Destructive {
		t.Error("detaching the last ticket of an attached volume must be destructive")
	}
}
