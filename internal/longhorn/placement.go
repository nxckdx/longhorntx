package longhorn

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
)

// Placement answers "why does Longhorn not place this volume's replicas?"
// – it goes through the replica scheduler's checks (longhorn-manager
// scheduler/replica_scheduler.go) for every node and disk: node ready and
// schedulable, scheduling allowed, tags vs the volume's selectors, disk
// type, disk condition, free space (minimal available percentage, on the
// volume's actual size) and over-provisioning (on its size), and replica
// anti-affinity per node, zone and disk. Read-only, from the node and
// replica CRs and a few settings.

// PlacementSettings: the global settings the scheduler reads.
type PlacementSettings struct {
	OverProvisioning  int  `json:"overProvisioningPercentage"` // storage-over-provisioning-percentage
	MinimalAvailable  int  `json:"minimalAvailablePercentage"` // storage-minimal-available-percentage
	NodeSoft          bool `json:"replicaSoftAntiAffinity"`
	ZoneSoft          bool `json:"replicaZoneSoftAntiAffinity"`
	DiskSoft          bool `json:"replicaDiskSoftAntiAffinity"`
	AllowEmptyNodeSel bool `json:"allowEmptyNodeSelectorVolume"`
	AllowEmptyDiskSel bool `json:"allowEmptyDiskSelectorVolume"`
}

// placementSettingNames are the settings PlacementSettingsFrom reads.
var placementSettingNames = []string{
	"storage-over-provisioning-percentage", "storage-minimal-available-percentage",
	"replica-soft-anti-affinity", "replica-zone-soft-anti-affinity", "replica-disk-soft-anti-affinity",
	"allow-empty-node-selector-volume", "allow-empty-disk-selector-volume",
}

// settingValue: data-engine specific settings hold {"v1": "…", "v2": "…"}.
func settingValue(v string) string {
	if strings.HasPrefix(strings.TrimSpace(v), "{") {
		var m map[string]string
		if json.Unmarshal([]byte(v), &m) == nil {
			return m["v1"]
		}
	}
	return v
}

// PlacementSettingsFrom reads the settings (name → value); missing ones
// keep Longhorn's defaults.
func PlacementSettingsFrom(vals map[string]string) PlacementSettings {
	s := PlacementSettings{OverProvisioning: 100, MinimalAvailable: 25, ZoneSoft: true, DiskSoft: true,
		AllowEmptyNodeSel: true, AllowEmptyDiskSel: true}
	num := func(name string, dst *int) {
		if n, err := strconv.Atoi(settingValue(vals[name])); err == nil {
			*dst = n
		}
	}
	flag := func(name string, dst *bool) {
		if b, err := strconv.ParseBool(settingValue(vals[name])); err == nil {
			*dst = b
		}
	}
	num("storage-over-provisioning-percentage", &s.OverProvisioning)
	num("storage-minimal-available-percentage", &s.MinimalAvailable)
	flag("replica-soft-anti-affinity", &s.NodeSoft)
	flag("replica-zone-soft-anti-affinity", &s.ZoneSoft)
	flag("replica-disk-soft-anti-affinity", &s.DiskSoft)
	flag("allow-empty-node-selector-volume", &s.AllowEmptyNodeSel)
	flag("allow-empty-disk-selector-volume", &s.AllowEmptyDiskSel)
	return s
}

// PlacementSettings reads the settings with one GET each.
func (c *Client) PlacementSettings(ctx context.Context) PlacementSettings {
	vals := map[string]string{}
	for _, n := range placementSettingNames {
		if v, err := c.Setting(ctx, n); err == nil {
			vals[n] = v
		}
	}
	return PlacementSettingsFrom(vals)
}

// DiskVerdict: can this disk take a new replica of the volume?
type DiskVerdict struct {
	Disk      string `json:"disk"`
	Path      string `json:"path"`
	OK        bool   `json:"ok"`
	Soft      bool   `json:"soft,omitempty"` // only by soft disk anti-affinity: it already holds a replica
	Reason    string `json:"reason,omitempty"`
	Available int64  `json:"available"`
	Scheduled int64  `json:"scheduled"`
	Limit     int64  `json:"limit"` // over-provisioning limit for scheduled sizes
}

// NodeVerdict: can this node take a new replica? OK with Soft: only
// because anti-affinity is soft (it, or its zone, already holds one).
type NodeVerdict struct {
	Node    string        `json:"node"`
	Zone    string        `json:"zone,omitempty"`
	OK      bool          `json:"ok"`
	Soft    bool          `json:"soft,omitempty"`
	Replica string        `json:"replica,omitempty"` // a replica of the volume is here
	Reason  string        `json:"reason,omitempty"`
	Disks   []DiskVerdict `json:"disks,omitempty"`
}

// Placement is the analysis for one volume.
type Placement struct {
	Volume   string            `json:"volume"`
	Size     int64             `json:"size"`
	Actual   int64             `json:"actualSize"`
	Want     int               `json:"want"` // numberOfReplicas
	Have     int               `json:"have"` // replicas placed on a node, not failed
	Fresh    int               `json:"fresh"`
	Soft     int               `json:"soft"`
	Nodes    []NodeVerdict     `json:"nodes"`
	Settings PlacementSettings `json:"settings"`
	// Scheduled: the volume's own condition (False = Longhorn could not
	// place a replica), with Longhorn's reason
	Scheduled       bool   `json:"scheduled"`
	ScheduledReason string `json:"scheduledReason,omitempty"`
}

// Missing: replicas Longhorn still has to place.
func (p *Placement) Missing() int { return max(0, p.Want-p.Have) }

// Placeable: how many new replicas could go somewhere now (fresh nodes
// first, then soft ones – a node takes one replica per round, like the
// scheduler).
func (p *Placement) Placeable() int { return p.Fresh + p.Soft }

// Problem: why the missing replicas cannot all be placed ("" = they can).
func (p *Placement) Problem() string {
	if p.Missing() == 0 || p.Placeable() >= p.Missing() {
		return ""
	}
	byReason := map[string][]string{}
	var order []string
	for _, n := range p.Nodes {
		if n.OK {
			continue
		}
		r := shortReason(n)
		if byReason[r] == nil {
			order = append(order, r)
		}
		byReason[r] = append(byReason[r], n.Node)
	}
	var rs []string
	for _, r := range order {
		rs = append(rs, r+": "+strings.Join(byReason[r], ", "))
	}
	return fmt.Sprintf("%d of %d replicas missing, room for %d – %s", p.Missing(), p.Want, p.Placeable(), strings.Join(rs, "; "))
}

// shortReason: the reason without its details (numbers, names).
func shortReason(n NodeVerdict) string {
	r := n.Reason
	for _, d := range n.Disks {
		if !d.OK && d.Reason != "" && strings.HasPrefix(r, d.Path+": ") {
			r = d.Reason // the first disk's (one disk per node is usual)
			break
		}
	}
	if i := strings.IndexAny(r, ":("); i > 0 {
		r = r[:i]
	}
	return strings.TrimSpace(r)
}

// selectorsInTags: like Longhorn's types.IsSelectorsInTags – every
// selector must be a tag; no selectors fit an untagged node/disk, or any
// one if allowEmpty.
func selectorsInTags(tags, selectors []string, allowEmpty bool) bool {
	if len(selectors) == 0 {
		return allowEmpty || len(tags) == 0
	}
	for _, s := range selectors {
		if !slices.Contains(tags, s) {
			return false
		}
	}
	return true
}

func override(v string, global bool) bool {
	switch v {
	case "enabled":
		return true
	case "disabled":
		return false
	}
	return global
}

func condition(cs []Condition, t string) (Condition, bool) {
	for _, c := range cs {
		if c.Type == t {
			return c, true
		}
	}
	return Condition{}, false
}

func condReason(c Condition) string {
	switch {
	case c.Reason == "" && c.Message == "":
		return "no reason given"
	case c.Message == "":
		return c.Reason
	case c.Reason == "":
		return c.Message
	}
	return c.Reason + ": " + c.Message
}

// AnalyzePlacement checks every node and disk for a new replica of v.
func AnalyzePlacement(v *Volume, replicas []*Replica, nodes []*Node, s PlacementSettings) *Placement {
	p := &Placement{Volume: v.Name, Size: int64(v.Spec.Size), Actual: int64(v.Status.ActualSize), Want: v.Spec.NumberOfReplicas, Settings: s, Scheduled: true}
	if c, ok := condition(v.Status.Conditions, "Scheduled"); ok && c.Status == "False" {
		p.Scheduled, p.ScheduledReason = false, condReason(c)
	}
	nodeSoft := override(v.Spec.ReplicaSoftAntiAffinity, s.NodeSoft)
	zoneSoft := override(v.Spec.ReplicaZoneSoftAntiAffinity, s.ZoneSoft)
	diskSoft := override(v.Spec.ReplicaDiskSoftAntiAffinity, s.DiskSoft)

	// where the volume's replicas are (failed or evicting ones do not hold
	// their node, zone or disk for long)
	zoneOf := map[string]string{}
	for _, n := range nodes {
		zoneOf[n.Name] = n.Status.Zone
	}
	onNode, onDisk, usedZone := map[string]string{}, map[string]bool{}, map[string]bool{}
	for _, r := range replicas {
		if r.Spec.NodeID == "" || r.DeletionTimestamp != nil || r.Spec.FailedAt != "" {
			continue
		}
		p.Have++
		if r.Spec.EvictionRequested {
			continue
		}
		onNode[r.Spec.NodeID] = r.Name
		onDisk[r.Spec.DiskID] = true
		usedZone[zoneOf[r.Spec.NodeID]] = true
	}
	strictNode := ""
	if v.Spec.DataLocality == "strict-local" {
		strictNode = v.Status.CurrentNodeID
		if strictNode == "" {
			strictNode = v.Spec.NodeID
		}
	}
	engine := v.Spec.DataEngine
	if engine == "" {
		engine = "v1"
	}

	for _, n := range nodes {
		nv := NodeVerdict{Node: n.Name, Zone: n.Status.Zone, Replica: onNode[n.Name]}
		nv.Reason = nodeBlocked(n, v, s, strictNode)
		if nv.Reason == "" {
			for name, ds := range n.Status.DiskStatus {
				dv := diskVerdict(name, n.Spec.Disks[name], ds, v, s, engine, onDisk[ds.DiskUUID], diskSoft)
				nv.Disks = append(nv.Disks, dv)
			}
			sort.Slice(nv.Disks, func(i, j int) bool { return nv.Disks[i].Path < nv.Disks[j].Path })
			var fresh, soft bool
			var why []string
			for _, d := range nv.Disks {
				switch {
				case d.OK && !d.Soft:
					fresh = true
				case d.OK:
					soft = true
				default:
					why = append(why, d.Path+": "+d.Reason)
				}
			}
			switch {
			case !fresh && !soft && len(why) == 0:
				nv.Reason = "no disks"
			case !fresh && !soft:
				nv.Reason = strings.Join(why, "; ")
			default:
				nv.OK, nv.Soft = true, !fresh
			}
		}
		// anti-affinity: one replica per node and zone unless soft
		if nv.OK {
			switch {
			case nv.Replica != "" && !(nodeSoft && zoneSoft):
				nv.OK, nv.Reason = false, "already holds a replica ("+nv.Replica+"; replica-soft-anti-affinity off)"
			case nv.Replica != "":
				nv.Soft, nv.Reason = true, "a second replica on this node (soft anti-affinity)"
			case usedZone[n.Status.Zone] && !zoneSoft:
				nv.OK, nv.Reason = false, fmt.Sprintf("zone %q already holds a replica (replica-zone-soft-anti-affinity off)", n.Status.Zone)
			case usedZone[n.Status.Zone] && n.Status.Zone != "":
				nv.Soft, nv.Reason = true, fmt.Sprintf("zone %q already holds a replica (soft zone anti-affinity)", n.Status.Zone)
			}
		}
		switch {
		case nv.OK && !nv.Soft:
			p.Fresh++
		case nv.OK:
			p.Soft++
		}
		p.Nodes = append(p.Nodes, nv)
	}
	sort.Slice(p.Nodes, func(i, j int) bool { return p.Nodes[i].Node < p.Nodes[j].Node })
	return p
}

// nodeBlocked: why the node takes no replica at all ("" = it may).
func nodeBlocked(n *Node, v *Volume, s PlacementSettings, strictNode string) string {
	if n.DeletionTimestamp != nil {
		return "being deleted"
	}
	if c, ok := condition(n.Status.Conditions, "Ready"); !ok || c.Status != "True" {
		return "not ready (" + condReason(c) + ")"
	}
	if c, ok := condition(n.Status.Conditions, "Schedulable"); ok && c.Status != "True" {
		return "not schedulable (" + condReason(c) + ")"
	}
	if !n.Spec.AllowScheduling {
		return "scheduling disabled on the node"
	}
	if strictNode != "" && n.Name != strictNode {
		return "strict-local: only " + strictNode
	}
	if !selectorsInTags(n.Spec.Tags, v.Spec.NodeSelector, s.AllowEmptyNodeSel) {
		if len(v.Spec.NodeSelector) == 0 {
			return fmt.Sprintf("tagged %v, the volume has no node selector (allow-empty-node-selector-volume off)", n.Spec.Tags)
		}
		return fmt.Sprintf("tags %v lack the volume's node selector %v", n.Spec.Tags, v.Spec.NodeSelector)
	}
	return ""
}

func diskVerdict(name string, spec DiskSpec, st DiskStatus, v *Volume, s PlacementSettings, engine string, holds, diskSoft bool) DiskVerdict {
	d := DiskVerdict{Disk: name, Path: spec.Path, Available: int64(st.StorageAvailable), Scheduled: int64(st.StorageScheduled)}
	if d.Path == "" {
		d.Path = st.DiskPath
	}
	max := int64(st.StorageMaximum)
	d.Limit = int64(float64(max-int64(spec.StorageReserved)) * float64(s.OverProvisioning) / 100)
	typ := spec.DiskType
	if typ == "" {
		typ = "filesystem"
	}
	switch {
	case !spec.AllowScheduling:
		d.Reason = "scheduling disabled on the disk"
	case spec.EvictionRequested:
		d.Reason = "eviction requested"
	case engine == "v1" && typ != "filesystem", engine == "v2" && typ != "block":
		d.Reason = fmt.Sprintf("a %s disk, the volume is %s", typ, engine)
	}
	if d.Reason == "" {
		if c, ok := condition(st.Conditions, "Schedulable"); !ok || c.Status != "True" {
			d.Reason = "not schedulable (" + condReason(c) + ")"
		}
	}
	if d.Reason == "" && !selectorsInTags(spec.Tags, v.Spec.DiskSelector, s.AllowEmptyDiskSel) {
		if len(v.Spec.DiskSelector) == 0 {
			d.Reason = fmt.Sprintf("tagged %v, the volume has no disk selector (allow-empty-disk-selector-volume off)", spec.Tags)
		} else {
			d.Reason = fmt.Sprintf("tags %v lack the volume's disk selector %v", spec.Tags, v.Spec.DiskSelector)
		}
	}
	if d.Reason == "" && holds {
		if !diskSoft {
			d.Reason = "already holds a replica (replica-disk-soft-anti-affinity off)"
		} else {
			d.OK, d.Soft = true, true
		}
		return d
	}
	if d.Reason == "" {
		size, actual := int64(v.Spec.Size), int64(v.Status.ActualSize)
		minFree := int64(float64(max) * float64(s.MinimalAvailable) / 100)
		switch {
		case max <= 0 || d.Available <= 0:
			d.Reason = "no space reported"
		case d.Available-actual <= minFree:
			d.Reason = fmt.Sprintf("too little space: %s available, the replica needs %s now and %s (%d %%) must stay free",
				human(d.Available), human(actual), human(minFree), s.MinimalAvailable)
		case size+d.Scheduled > d.Limit:
			d.Reason = fmt.Sprintf("over-provisioned: %s scheduled + %s > limit %s (%d %% of %s)",
				human(d.Scheduled), human(size), human(d.Limit), s.OverProvisioning, human(max-int64(spec.StorageReserved)))
		default:
			d.OK = true
		}
	}
	return d
}

func human(n int64) string { return humanBytes(n) }

// PlacementFindings (doctor): volumes Longhorn could not place a replica
// for, with the reason per node. settings is read only when one is found.
func PlacementFindings(vols []*Volume, replicas []*Replica, nodes []*Node, settings func() PlacementSettings) []Finding {
	byVol := map[string][]*Replica{}
	for _, r := range replicas {
		byVol[r.Spec.VolumeName] = append(byVol[r.Spec.VolumeName], r)
	}
	var out []Finding
	var s *PlacementSettings
	for _, v := range vols {
		c, ok := condition(v.Status.Conditions, "Scheduled")
		if !ok || c.Status != "False" {
			continue
		}
		if s == nil {
			x := settings()
			s = &x
		}
		p := AnalyzePlacement(v, byVol[v.Name], nodes, *s)
		msg := p.Problem()
		if msg == "" {
			msg = "Longhorn could not place a replica (" + condReason(c) + "); by lhtx's reading there is room now – it retries"
		}
		out = append(out, Finding{Severity: SevWarning, Check: "placement", Object: "volume/" + v.Name, Message: msg,
			Hint: "lhtx placement " + v.Name})
	}
	return out
}
