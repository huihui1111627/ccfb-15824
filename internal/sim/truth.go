package sim

import (
	"sort"

	"circusnet/internal/model"
)

// truthGraph is an independent view of the physical network used to derive
// the expected forwarding path. It deliberately does not reuse the control
// plane's FIB or tunnel code: it only knows which switches are operational,
// which links are up and where devices really are.
type truthGraph struct {
	adj  map[model.SwitchID][]model.SwitchID
	dist map[model.SwitchID]map[model.SwitchID]int
	prev map[model.SwitchID]map[model.SwitchID]model.SwitchID
}

func (w *World) operational(sw model.SwitchID) bool {
	return w.SwUp[sw] && w.PortsOpen[sw]
}

func (w *World) newTruthGraph() *truthGraph {
	tg := &truthGraph{
		adj:  map[model.SwitchID][]model.SwitchID{},
		dist: map[model.SwitchID]map[model.SwitchID]int{},
		prev: map[model.SwitchID]map[model.SwitchID]model.SwitchID{},
	}
	for k, up := range w.linkUp {
		if !up {
			continue
		}
		a, b := model.SwitchID(k.A), model.SwitchID(k.B)
		if !w.operational(a) || !w.operational(b) {
			continue
		}
		tg.adj[a] = append(tg.adj[a], b)
		tg.adj[b] = append(tg.adj[b], a)
	}
	for n := range tg.adj {
		sort.Slice(tg.adj[n], func(i, j int) bool { return tg.adj[n][i] < tg.adj[n][j] })
	}
	for _, src := range w.Topo.SwitchOrder {
		if !w.operational(src) {
			continue
		}
		d := map[model.SwitchID]int{src: 0}
		parent := map[model.SwitchID]model.SwitchID{}
		queue := []model.SwitchID{src}
		for len(queue) > 0 {
			cur := queue[0]
			queue = queue[1:]
			for _, nxt := range tg.adj[cur] {
				if _, ok := d[nxt]; ok {
					continue
				}
				d[nxt] = d[cur] + 1
				parent[nxt] = cur
				queue = append(queue, nxt)
			}
		}
		tg.dist[src] = d
		tg.prev[src] = parent
	}
	return tg
}

func (tg *truthGraph) path(src, dst model.SwitchID) []model.SwitchID {
	if src == dst {
		if _, ok := tg.dist[src]; ok {
			return []model.SwitchID{src}
		}
		return nil
	}
	if _, ok := tg.dist[src][dst]; !ok {
		return nil
	}
	var rev []model.SwitchID
	for at := dst; ; at = tg.prev[src][at] {
		rev = append(rev, at)
		if at == src {
			break
		}
	}
	path := make([]model.SwitchID, 0, len(rev))
	for i := len(rev) - 1; i >= 0; i-- {
		path = append(path, rev[i])
	}
	return path
}

func (tg *truthGraph) reachable(src, dst model.SwitchID) bool {
	if src == dst {
		_, ok := tg.dist[src]
		return ok
	}
	_, ok := tg.dist[src][dst]
	return ok
}

// truthAttachments is the set of switches that can physically deliver to a
// device right now. A dual-homed device is on a segment reachable from both
// members whose access link is up; an external host is on a segment reachable
// from every border switch.
func (w *World) truthAttachments(d *model.Device) []model.SwitchID {
	var out []model.SwitchID
	switch {
	case d.External:
		seen := map[model.SwitchID]bool{}
		for _, vn := range w.Topo.VNs {
			if vn.VNI != d.VN {
				continue
			}
			for _, o := range vn.ExternalOwners {
				if seen[o] || !w.operational(o) || !w.Topo.Switch[o].InVirtualNetwork(d.VN) {
					continue
				}
				seen[o] = true
				out = append(out, o)
			}
		}
	case d.Kind == model.AttachGroup:
		for _, m := range d.GroupMembers {
			if w.operational(m) && w.AccessUp[d.Group][m] && w.Topo.Switch[m].InVirtualNetwork(d.VN) {
				out = append(out, m)
			}
		}
	default:
		if w.operational(d.Switch) && w.Topo.Switch[d.Switch].InVirtualNetwork(d.VN) {
			out = append(out, d.Switch)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// expectedEgress applies the documented placement policy to pick the switch
// that should be the last tunnel endpoint for a destination:
//   - a resolved external host (/32) prefers the border that owns its host
//     route, falling back to the covering prefix's lowest reachable border;
//   - a dual-homed device goes to the lowest-id reachable member;
//   - a single-homed device goes to its own switch.
//
// The FIB must agree with this, including the "lowest origin wins" tie-break.
func (w *World) expectedEgress(from model.SwitchID, d *model.Device, tg *truthGraph) (model.SwitchID, bool) {
	att := w.truthAttachments(d)
	switch {
	case d.External && d.ExternalKnown:
		// A resolved external host is owned by the border that advertised its
		// /32. The more-specific host route beats the covering prefix route,
		// including on the other border: that is the point of keeping host
		// and prefix routes separate. If that border no longer carries the
		// VN it cannot advertise the /32 any more, so the prefix applies.
		if d.Switch != "" && w.operational(d.Switch) &&
			w.Topo.Switch[d.Switch].InVirtualNetwork(d.VN) && tg.reachable(from, d.Switch) {
			return d.Switch, true
		}
		for _, a := range w.truthAttachments(d) {
			if a == from || tg.reachable(from, a) {
				if a == from {
					return from, true
				}
				return a, true
			}
		}
		return "", false
	case d.External:
		// A border delivers its own segment locally; any other switch uses
		// the lowest-id reachable border.
		for _, a := range att {
			if a == from {
				return from, true
			}
		}
		for _, a := range att {
			if tg.reachable(from, a) {
				return a, true
			}
		}
		return "", false
	case d.Kind == model.AttachGroup:
		// The group's devices sit on a shared segment. A member switch that is
		// attached delivers locally (this is also the split-horizon case);
		// everyone else tunnels to the lowest-id reachable member, matching
		// the deterministic origin tie-break in the FIB.
		for _, a := range att {
			if a == from {
				return from, true
			}
		}
	default:
		if len(att) == 1 && att[0] == from {
			return from, true
		}
	}
	for _, a := range att {
		if tg.reachable(from, a) {
			return a, true
		}
	}
	return "", false
}

// sourceInjection is the switch a device's frames enter the fabric on. For a
// dual-homed device the model uses the lowest-id member with an up access
// link, which is also what the FIB's origin tie-break produces.
// SourceInjectionFor is the exported form used by tools.
func (w *World) SourceInjectionFor(d *model.Device) (model.SwitchID, bool) {
	return w.sourceInjection(d)
}

func (w *World) sourceInjection(d *model.Device) (model.SwitchID, bool) {
	att := w.truthAttachments(d)
	if len(att) == 0 {
		return "", false
	}
	return att[0], true
}
