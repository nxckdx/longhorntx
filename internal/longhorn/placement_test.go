package longhorn

import (
	"strings"
	"testing"
)

const gi = int64(1) << 30

func pNode(name, zone string, avail, scheduled int64, tags ...string) *Node {
	n := &Node{}
	n.Name = name
	n.Spec.AllowScheduling = true
	n.Spec.Tags = tags
	n.Spec.Disks = map[string]DiskSpec{"d": {Path: "/var/lib/longhorn", AllowScheduling: true}}
	n.Status.Zone = zone
	n.Status.Conditions = []Condition{{Type: "Ready", Status: "True"}, {Type: "Schedulable", Status: "True"}}
	n.Status.DiskStatus = map[string]DiskStatus{"d": {DiskUUID: name + "-uuid", StorageMaximum: Quantity(30 * gi),
		StorageAvailable: Quantity(avail), StorageScheduled: Quantity(scheduled),
		Conditions: []Condition{{Type: "Ready", Status: "True"}, {Type: "Schedulable", Status: "True"}}}}
	return n
}

func pVol(size int64, replicas int) *Volume {
	v := &Volume{}
	v.Name = "vol"
	v.Spec.Size, v.Spec.NumberOfReplicas = Quantity(size), replicas
	v.Status.ActualSize = Quantity(size / 2)
	return v
}

func pRep(node string) *Replica {
	r := &Replica{}
	r.Name = "r-" + node
	r.Spec.VolumeName, r.Spec.NodeID, r.Spec.DiskID = "vol", node, node+"-uuid"
	return r
}

func verdict(p *Placement, node string) NodeVerdict {
	for _, n := range p.Nodes {
		if n.Node == node {
			return n
		}
	}
	return NodeVerdict{}
}

func TestPlacement(t *testing.T) {
	s := PlacementSettingsFrom(nil)
	// lab case: two disks nearly fully scheduled, one replica placed
	nodes := []*Node{pNode("a", "", 20*gi, 29*gi), pNode("b", "", 20*gi, 10*gi), pNode("c", "", 20*gi, 10*gi)}
	v := pVol(2*gi, 2)
	p := AnalyzePlacement(v, []*Replica{pRep("b")}, nodes, s)
	if a := verdict(p, "a"); a.OK || !strings.Contains(a.Reason, "over-provisioned") {
		t.Errorf("a: %+v", a)
	}
	if b := verdict(p, "b"); b.OK || !strings.Contains(b.Reason, "already holds a replica (r-b") {
		t.Errorf("b: %+v", b)
	}
	if c := verdict(p, "c"); !c.OK || c.Soft {
		t.Errorf("c: %+v", c)
	}
	if p.Missing() != 1 || p.Problem() != "" {
		t.Errorf("missing %d, problem %q", p.Missing(), p.Problem())
	}
	// c fills up: nowhere to go
	nodes[2] = pNode("c", "", 8*gi, 10*gi)
	p = AnalyzePlacement(v, []*Replica{pRep("b")}, nodes, s)
	if c := verdict(p, "c"); c.OK || !strings.Contains(c.Reason, "too little space") {
		t.Errorf("c full: %+v", c)
	}
	if pr := p.Problem(); !strings.Contains(pr, "1 of 2 replicas missing, room for 0") || !strings.Contains(pr, "over-provisioned: a;") {
		t.Errorf("problem %q", pr)
	}
	// soft anti-affinity: b may take a second one
	s2 := s
	s2.NodeSoft = true
	p = AnalyzePlacement(v, []*Replica{pRep("b")}, nodes, s2)
	if b := verdict(p, "b"); !b.OK || !b.Soft {
		t.Errorf("soft b: %+v", b)
	}
	if p.Problem() != "" {
		t.Errorf("soft: problem %q", p.Problem())
	}
}

func TestPlacementNodeChecks(t *testing.T) {
	s := PlacementSettingsFrom(map[string]string{"allow-empty-node-selector-volume": "false", "replica-zone-soft-anti-affinity": "false"})
	cordoned := pNode("cordoned", "z2", 20*gi, 0)
	cordoned.Status.Conditions[1] = Condition{Type: "Schedulable", Status: "False", Reason: "KubernetesNodeCordoned", Message: "node is cordoned"}
	off := pNode("off", "z3", 20*gi, 0)
	off.Spec.AllowScheduling = false
	tagged := pNode("tagged", "z4", 20*gi, 0, "ssd")
	sameZone := pNode("samezone", "z1", 20*gi, 0)
	nodes := []*Node{pNode("a", "z1", 20*gi, 0), cordoned, off, tagged, sameZone}
	p := AnalyzePlacement(pVol(gi, 3), []*Replica{pRep("a")}, nodes, s)
	for node, want := range map[string]string{
		"cordoned": "KubernetesNodeCordoned",
		"off":      "scheduling disabled",
		"tagged":   "no node selector",
		"samezone": `zone "z1" already holds a replica`,
	} {
		if n := verdict(p, node); n.OK || !strings.Contains(n.Reason, want) {
			t.Errorf("%s: %+v, want %q", node, n, want)
		}
	}
	// with a matching selector the tagged node is fine
	v := pVol(gi, 3)
	v.Spec.NodeSelector = []string{"ssd"}
	p = AnalyzePlacement(v, nil, nodes, s)
	if n := verdict(p, "tagged"); !n.OK {
		t.Errorf("tagged with selector: %+v", n)
	}
	if n := verdict(p, "a"); n.OK || !strings.Contains(n.Reason, "lack the volume's node selector") {
		t.Errorf("a without the tag: %+v", n)
	}
	// strict-local
	v = pVol(gi, 1)
	v.Spec.DataLocality, v.Status.CurrentNodeID = "strict-local", "a"
	p = AnalyzePlacement(v, nil, nodes, PlacementSettingsFrom(nil))
	if n := verdict(p, "samezone"); n.OK || !strings.Contains(n.Reason, "strict-local") {
		t.Errorf("strict-local: %+v", n)
	}
}

func TestPlacementSettingsJSON(t *testing.T) {
	s := PlacementSettingsFrom(map[string]string{"storage-over-provisioning-percentage": `{"v1":"200","v2":"100"}`, "replica-soft-anti-affinity": "true"})
	if s.OverProvisioning != 200 || !s.NodeSoft || s.MinimalAvailable != 25 {
		t.Errorf("%+v", s)
	}
}

func TestPlacementFindings(t *testing.T) {
	v := pVol(2*gi, 2)
	v.Status.Conditions = []Condition{{Type: "Scheduled", Status: "False", Reason: "ReplicaSchedulingFailure", Message: "insufficient storage"}}
	ok := pVol(gi, 1)
	ok.Name = "fine"
	read := 0
	nodes := []*Node{pNode("a", "", 2*gi, 0)}
	fineRep := pRep("a")
	fineRep.Name, fineRep.Spec.VolumeName = "fine-r", "fine"
	f := PlacementFindings([]*Volume{v, ok}, []*Replica{pRep("a"), fineRep}, nodes, func() PlacementSettings { read++; return PlacementSettingsFrom(nil) })
	if len(f) != 1 || f[0].Object != "volume/vol" || !strings.Contains(f[0].Message, "room for 0") || read != 1 {
		t.Errorf("findings %+v (settings read %d)", f, read)
	}
	read = 0
	if f := PlacementFindings([]*Volume{ok}, []*Replica{fineRep}, nodes, func() PlacementSettings { read++; return PlacementSettings{} }); len(f) != 0 || read != 0 {
		t.Errorf("healthy: %+v, settings read %d", f, read)
	}
}

// Longhorn may say "scheduled" while a replica has no node (volume created
// with degraded availability) – doctor reports it, with hints.
func TestPlacementFindingsNodelessReplica(t *testing.T) {
	v := pVol(gi, 3)
	v.Status.Conditions = []Condition{{Type: "Scheduled", Status: "True"}}
	cordoned := pNode("c", "", 20*gi, 0)
	cordoned.Status.Conditions[1] = Condition{Type: "Schedulable", Status: "False", Reason: "KubernetesNodeCordoned", Message: "Node c is cordoned"}
	nodes := []*Node{pNode("a", "", 20*gi, 0), pNode("b", "", 20*gi, 0), cordoned}
	unplaced := pRep("x")
	unplaced.Spec.NodeID = ""
	reps := []*Replica{pRep("a"), pRep("b"), unplaced}
	f := PlacementFindings([]*Volume{v}, reps, nodes, func() PlacementSettings { return PlacementSettingsFrom(nil) })
	if len(f) != 1 || !strings.Contains(f[0].Message, "not schedulable: c") {
		t.Fatalf("findings %+v", f)
	}
	p := AnalyzePlacement(v, reps, nodes, PlacementSettingsFrom(nil))
	h := strings.Join(p.Hints(), "\n")
	if !strings.Contains(h, "kubectl uncordon") || !strings.Contains(h, "replica count") {
		t.Errorf("hints %q", h)
	}
	// with room (c uncordoned) a missing replica is just being placed
	nodes[2] = pNode("c", "", 20*gi, 0)
	if f := PlacementFindings([]*Volume{v}, reps, nodes, func() PlacementSettings { return PlacementSettingsFrom(nil) }); len(f) != 0 {
		t.Errorf("room: %+v", f)
	}
}
