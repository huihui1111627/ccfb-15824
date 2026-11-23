package sim

import (
	"fmt"
	"math/rand"
	"sort"

	"circusnet/internal/fabric"
	"circusnet/internal/model"
)

// ChangeKind is one kind of network mutation.
type ChangeKind string

const (
	ChangeMove       ChangeKind = "move"
	ChangeLinkDown   ChangeKind = "link-down"
	ChangeLinkUp     ChangeKind = "link-up"
	ChangeAccessDown ChangeKind = "access-down"
	ChangeAccessUp   ChangeKind = "access-up"
	ChangeSwitchDown ChangeKind = "switch-down"
	ChangeSwitchUp   ChangeKind = "switch-up"
	ChangeVNLeave    ChangeKind = "vn-leave"
	ChangeVNJoin     ChangeKind = "vn-join"
)

// Result is the outcome of one convergence after a change.
type Result struct {
	Kind        ChangeKind `json:"kind"`
	Detail      string     `json:"detail"`
	Rounds      int        `json:"rounds"`
	Withdrawals int64      `json:"withdrawals"`
	Violations  int        `json:"violations"`
	Checked     int        `json:"checked"`
	Broadcasts  int        `json:"broadcasts"`
}

// Mutation applies one change and lets the caller converge and verify.
type Mutation struct {
	Kind   ChangeKind
	Device *model.Device
	Link   fabric.LinkKey
	Switch model.SwitchID
	VN     model.VNI
	Group  model.GroupID
	// To is printed into the timeline.
	To string
}

// MoveDevice relocates a single-homed device to a new switch in its VN. The
// MAC mobility sequence is bumped so remote switches prefer the new binding.
func (w *World) MoveDevice(d *model.Device, rng *rand.Rand) bool {
	if d.Kind != model.AttachSingle || d.External {
		return false
	}
	cands := w.Topo.SwitchesForVN(d.VN)
	if len(cands) < 2 {
		return false
	}
	var target model.SwitchID
	for i := 0; i < 16; i++ {
		target = cands[rng.Intn(len(cands))]
		if target != d.Switch {
			break
		}
	}
	if target == d.Switch || !w.SwUp[target] {
		return false
	}
	from := d.Switch
	d.Switch = target
	d.Port = fmt.Sprintf("p%d", rng.Intn(48))
	w.DeviceSeq[d.ID]++
	w.Migrations++
	w.reindex()
	w.log(EventMove, Event{Device: d.ID, MAC: d.MAC, From: string(from), To: string(target), VN: d.VN})
	return true
}

// FlipLink toggles an underlay link.
func (w *World) FlipLink(k fabric.LinkKey, up bool) bool {
	if !w.configuredLinks[k] || w.linkUp[k] == up {
		return false
	}
	w.linkUp[k] = up
	w.Underlay.SetLink(model.SwitchID(k.A), model.SwitchID(k.B), up)
	w.graphDirty = true
	kind := EventLinkDown
	if up {
		kind = EventLinkUp
	}
	w.log(kind, Event{From: k.A, To: k.B})
	return true
}

// SetAccess changes a member switch's access link into a redundancy group.
// When one link fails only that member's group route is withdrawn; the peer
// keeps advertising the whole group.
func (w *World) SetAccess(gid model.GroupID, sw model.SwitchID, up bool) bool {
	if w.AccessUp[gid][sw] == up {
		return false
	}
	w.AccessUp[gid][sw] = up
	if !up {
		w.GroupFailovers++
	}
	w.reindex()
	kind := EventAccessDown
	if up {
		kind = EventAccessUp
	}
	w.log(kind, Event{Group: gid, Switch: sw})
	return true
}

// SwitchDown crashes a switch: it stops advertising and its RIB/tunnels go
// away, but its configuration is retained for the restart.
func (w *World) SwitchDown(sw model.SwitchID) bool {
	if !w.SwUp[sw] {
		return false
	}
	w.SwUp[sw] = false
	w.PortsOpen[sw] = false
	w.graphDirty = true
	w.log(EventSwitchDown, Event{Switch: sw})
	return true
}

// SwitchUp restarts a switch with a new epoch. Its ports stay closed until
// the control plane reconverges and every pre-restart route has expired.
func (w *World) SwitchUp(sw model.SwitchID) bool {
	if w.SwUp[sw] {
		return false
	}
	w.SwUp[sw] = true
	w.PortsOpen[sw] = false
	w.graphDirty = true
	w.Epoch[sw]++
	w.bootRound[sw] = w.Round
	// The returning switch must not trust its own previous state.
	old := w.RIB[sw]
	rib := fabric.NewRIB(w.Topo.Switch[sw].VNs)
	rib.KnownEpoch = map[model.SwitchID]uint64{sw: w.Epoch[sw]}
	// Carry over the epochs already learned from peers; they are not stale.
	for origin, ep := range old.KnownEpoch {
		if origin != sw && ep > rib.KnownEpoch[origin] {
			rib.KnownEpoch[origin] = ep
		}
	}
	w.RIB[sw] = rib
	w.localVer[sw] = 0
	w.log(EventReboot, Event{Switch: sw, Detail: fmt.Sprintf("epoch %d; ports closed until converged", w.Epoch[sw])})
	return true
}

// SetVNMembership adds or removes a VN from a switch's configuration. Leaving
// withdraws the member route and the tunnel disappears with it.
func (w *World) SetVNMembership(sw model.SwitchID, vni model.VNI, join bool) bool {
	info := w.Topo.Switch[sw]
	if info.InVirtualNetwork(vni) == join {
		return false
	}
	if join {
		info.VNs = append(info.VNs, vni)
		sort.Slice(info.VNs, func(i, j int) bool { return info.VNs[i] < info.VNs[j] })
		w.RIB[sw].SetPolicy(info.VNs)
		// Ports re-learn what is attached: without a fresh sequence the
		// re-advertised routes would look older than the withdrawal that
		// preceded them.
		for _, d := range w.idx.vnDevices[vni] {
			if d.Switch == sw || (d.Kind == model.AttachGroup && w.groupHasMember(d.Group, sw)) {
				w.DeviceSeq[d.ID]++
			}
		}
		for gid, g := range w.Topo.Groups {
			if g.VN == vni && g.HasMember(sw) {
				w.GroupSeq[gid]++
			}
		}
		w.localVer[sw]++
		w.log(EventVNJoin, Event{Switch: sw, VN: vni})
	} else {
		var keep []model.VNI
		for _, v := range info.VNs {
			if v != vni {
				keep = append(keep, v)
			}
		}
		info.VNs = keep
		w.RIB[sw].SetPolicy(info.VNs)
		w.log(EventVNLeave, Event{Switch: sw, VN: vni})
	}
	return true
}

func (w *World) groupHasMember(gid model.GroupID, sw model.SwitchID) bool {
	g := w.Topo.Groups[gid]
	return g != nil && g.HasMember(sw)
}

// ConfiguredLinksSnapshot returns a copy of the configured link state.
func (w *World) ConfiguredLinksSnapshot() map[fabric.LinkKey]bool {
	out := make(map[fabric.LinkKey]bool, len(w.linkUp))
	for k, v := range w.linkUp {
		out[k] = v
	}
	return out
}

// TestableDevices returns devices that can currently send and receive.
func (w *World) TestableDevices() []*model.Device {
	var out []*model.Device
	for _, d := range w.Topo.Devices {
		if len(w.truthAttachments(d)) > 0 {
			out = append(out, d)
		}
	}
	return out
}

// LinkIsUp reports the current state of an underlay link.
func (w *World) LinkIsUp(a, b model.SwitchID) bool {
	return w.linkUp[fabric.NewLinkKey(a, b)]
}

// AccessIsUp reports whether a member switch's access link into a group is up.
func (w *World) AccessIsUp(gid model.GroupID, sw model.SwitchID) bool {
	return w.AccessUp[gid][sw]
}

// IsUp reports whether a switch is powered on.
func (w *World) IsUp(sw model.SwitchID) bool { return w.SwUp[sw] }

// PortsAreOpen reports whether a switch has passed route convergence.
func (w *World) PortsAreOpen(sw model.SwitchID) bool { return w.PortsOpen[sw] }
