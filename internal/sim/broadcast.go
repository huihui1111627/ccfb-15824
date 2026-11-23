package sim

import (
	"sort"

	"circusnet/internal/model"
)

// BroadcastOutcome describes the simulated flooding of one broadcast frame
// (ARP request or unknown unicast) on a VN, including which group ports were
// suppressed by the split-horizon rule.
type BroadcastOutcome struct {
	// Frames is the number of overlay copies the ingress switch sends.
	Frames int `json:"frames"`
	// Delivered lists switches that received a copy.
	Delivered []model.SwitchID `json:"delivered"`
	// Suppressed lists redundancy-group access ports that were skipped.
	Suppressed []Suppression `json:"suppressed"`
	// Total is the running fabric-wide broadcast counter after this frame.
	Total int64 `json:"total"`
}

// Suppression records one split-horizon decision.
type Suppression struct {
	Switch model.SwitchID `json:"switch"`
	Group  model.GroupID  `json:"group"`
	Reason string         `json:"reason"`
}

// FloodBroadcast simulates an ingress switch flooding a broadcast on a VN.
// Replication is per-tunnel (head-end replication). A receiving switch that is
// a member of a redundancy group does not copy the frame out of that group's
// access ports: the group tag in the member route identifies the ports, which
// is what stops a dual-homed device from receiving its own broadcast.
func (w *World) FloodBroadcast(from model.SwitchID, vni model.VNI) BroadcastOutcome {
	out := BroadcastOutcome{}
	fib := w.FIB[from][vni]
	if fib == nil || !w.operational(from) {
		return out
	}

	// The ingress switch's own access ports carry the frame to its local
	// segment. If the source came from a group segment, the peer member does
	// not get a copy through the tunnel either.
	receivers := make([]model.SwitchID, 0, len(fib.Tunnels))
	for remote, tun := range fib.Tunnels {
		if !tun.Up {
			continue
		}
		receivers = append(receivers, remote)
	}
	sort.Slice(receivers, func(i, j int) bool { return receivers[i] < receivers[j] })

	for _, remote := range receivers {
		// Split horizon at the receiver, driven by the group tags carried in
		// the member route it advertised.
		// The frame still traverses the tunnel to every member switch; the
		// split-horizon rule only prevents it from leaving that switch's
		// redundancy-group access port.
		out.Frames++
		out.Delivered = append(out.Delivered, remote)
		for _, gid := range w.idx.groupTags[remote][vni] {
			if !w.AccessUp[gid][remote] || !w.TopoSegmentReached(gid, from) {
				continue
			}
			out.Suppressed = append(out.Suppressed, Suppression{
				Switch: remote, Group: gid,
				Reason: "egress split horizon: group tag matches ingress segment",
			})
		}
	}
	w.Broadcasts += int64(out.Frames)
	w.BroadcastSuppressed += int64(len(out.Suppressed))
	out.Total = w.Broadcasts
	return out
}

// GroupTagsFor exposes the redundancy-group tags learned for a switch/VN, for
// diagnostics and the dashboard.
func (w *World) GroupTagsFor(sw model.SwitchID) map[model.VNI][]model.GroupID {
	out := map[model.VNI][]model.GroupID{}
	for vni, gids := range w.idx.groupTags[sw] {
		out[vni] = append([]model.GroupID(nil), gids...)
	}
	return out
}

// egressSuppressed reports whether a switch must keep a flooded frame away
// from one of its redundancy-group access ports. The group tag travels in the
// member route, so the receiver knows the ingress switch sits on the same
// shared segment and the frame must not go back out of that port. This is the
// "a dual-homed device never receives its own broadcast" rule.
func (w *World) egressSuppressed(sw model.SwitchID, vni model.VNI, ingress model.SwitchID) bool {
	for _, gid := range w.idx.groupTags[sw][vni] {
		if w.egressSuppressedFor(sw, vni, gid, ingress) {
			return true
		}
	}
	return false
}

// egressSuppressedFor is the per-group decision behind egressSuppressed.
func (w *World) egressSuppressedFor(sw model.SwitchID, vni model.VNI, gid model.GroupID, ingress model.SwitchID) bool {
	g := w.Topo.Groups[gid]
	if g == nil || g.VN != vni || !g.HasMember(sw) || !w.AccessUp[gid][sw] {
		return false
	}
	return w.TopoSegmentReached(gid, ingress)
}

// peerMember returns the other member of a redundancy group.
func (w *World) peerMember(gid model.GroupID, sw model.SwitchID) model.SwitchID {
	g := w.Topo.Groups[gid]
	if g == nil {
		return ""
	}
	for _, m := range g.Members {
		if m != sw {
			return m
		}
	}
	return ""
}

// TopoSegmentReached reports whether the ingress switch is on the group's
// shared segment (i.e. one of its members with an up access link).
func (w *World) TopoSegmentReached(gid model.GroupID, sw model.SwitchID) bool {
	g := w.Topo.Groups[gid]
	if g == nil {
		return false
	}
	return g.HasMember(sw) && w.AccessUp[gid][sw]
}
