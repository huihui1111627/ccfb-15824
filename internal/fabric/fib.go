package fabric

import (
	"net/netip"
	"sort"
	"strings"

	"circusnet/internal/model"
)

// Egress is where a switch sends a frame for a destination.
type Egress struct {
	Switch  model.SwitchID `json:"switch"`
	NextHop model.SwitchID `json:"nextHop"`
	Local   bool           `json:"local"`
}

// HostFIB is one MAC/IP binding installed in a forwarding table.
type HostFIB struct {
	MAC       string           `json:"mac"`
	IP        netip.Addr       `json:"ip"`
	Type      RouteType        `json:"type"`
	Source    RouteSource      `json:"source"`
	Origin    model.SwitchID   `json:"origin"`
	Group     model.GroupID    `json:"group,omitempty"`
	Seq       uint64           `json:"seq"`
	Epoch     uint64           `json:"epoch"`
	Egress    Egress           `json:"egress"`
	Local     bool             `json:"local"`
	External  bool             `json:"external,omitempty"`
	GroupOrgs []model.SwitchID `json:"groupOrigins,omitempty"`
}

// PrefixCandidate is one origin that advertised an external prefix. The
// tunnel for the prefix is chosen from the candidates after every route has
// been installed.
type PrefixCandidate struct {
	Prefix netip.Prefix   `json:"prefix"`
	Origin model.SwitchID `json:"origin"`
}

// PrefixFIB is an external prefix installed in a forwarding table.
type PrefixFIB struct {
	Prefix netip.Prefix   `json:"prefix"`
	Origin model.SwitchID `json:"origin"`
	Egress Egress         `json:"egress"`
}

// Tunnel is a VXLAN-like overlay tunnel from Local to Remote for one VN.
type Tunnel struct {
	Local   model.SwitchID   `json:"local"`
	Remote  model.SwitchID   `json:"remote"`
	VNI     model.VNI        `json:"vni"`
	NextHop model.SwitchID   `json:"nextHop"`
	Path    []model.SwitchID `json:"path"`
	Up      bool             `json:"up"`
	Ready   bool             `json:"ready"`
}

// VNFIB is one switch's forwarding table for one virtual network. It is
// rebuilt from the RIB on every convergence round; nothing is merged in from
// a previous round.
type VNFIB struct {
	VNI      model.VNI                  `json:"vni"`
	Hosts    map[string]*HostFIB        `json:"-"`
	ByIP     map[netip.Addr]*HostFIB    `json:"-"`
	Tunnels  map[model.SwitchID]*Tunnel `json:"-"`
	Prefixes []PrefixFIB                `json:"prefixes"`
	// PrefixCandidates holds every advertised copy of each external prefix
	// until tunnels are known.
	PrefixCandidates []PrefixCandidate `json:"-"`
	// prefixTargets maps a covering prefix to its chosen endpoint.
	prefixTargets map[string]model.SwitchID
	// groupOrigins records which switches advertised which redundancy group,
	// so a failover can move the whole group to a surviving member.
	groupOrigins map[model.GroupID][]model.SwitchID
	// groupLocal marks groups that have at least one local access port.
	groupLocal map[model.GroupID]bool
	// localSwitch is the switch this table belongs to.
	localSwitch model.SwitchID
}

// SetLocalSwitch records which switch the table belongs to.
func (f *VNFIB) SetLocalSwitch(sw model.SwitchID) { f.localSwitch = sw }

func NewVNFIB(vni model.VNI) *VNFIB {
	return &VNFIB{
		VNI:           vni,
		Hosts:         map[string]*HostFIB{},
		ByIP:          map[netip.Addr]*HostFIB{},
		Tunnels:       map[model.SwitchID]*Tunnel{},
		groupOrigins:  map[model.GroupID][]model.SwitchID{},
		groupLocal:    map[model.GroupID]bool{},
		prefixTargets: map[string]model.SwitchID{},
	}
}

// SetPrefixTarget records the resolved endpoint for one external prefix. Any
// /32 inside it is still resolved by Lookup's host-binding pass first, so a
// resolved external host beats the covering prefix even when the two point at
// different border switches.
func (f *VNFIB) SetPrefixTarget(p netip.Prefix, target model.SwitchID) {
	f.prefixTargets[p.Masked().String()] = target
}

// InstallHost adds or replaces a host binding, keeping the preferred one under
// a deterministic ordering.
func (f *VNFIB) InstallHost(h *HostFIB) {
	mac := LowerMAC(h.MAC)
	if old, ok := f.Hosts[mac]; ok {
		if !PreferHost(h, old) {
			return
		}
		delete(f.ByIP, old.IP)
	}
	h.MAC = mac
	f.Hosts[mac] = h
	if h.IP.IsValid() {
		f.ByIP[h.IP] = h
	}
}

// PreferHost reports whether binding a should replace binding b. MAC mobility
// sequence is global and dominates between different origins. Within one
// origin the epoch dominates, so a restarted switch's fresh routes beat the
// copies it advertised before the restart even though its local sequence
// restarted at 1.
func PreferHost(a, b *HostFIB) bool {
	if a.Origin != b.Origin && a.Seq != b.Seq {
		return a.Seq > b.Seq
	}
	if a.Origin == b.Origin && a.Epoch != b.Epoch {
		return a.Epoch > b.Epoch
	}
	if a.Type != b.Type {
		return a.Type == RouteHost
	}
	if a.Type == RouteGroup {
		return a.Origin < b.Origin
	}
	if a.Seq != b.Seq {
		return a.Seq > b.Seq
	}
	if a.Epoch != b.Epoch {
		return a.Epoch > b.Epoch
	}
	return a.Origin < b.Origin
}

// SetPrefix installs a resolved external prefix.
func (f *VNFIB) SetPrefix(p PrefixFIB) {
	p.Prefix = p.Prefix.Masked()
	for i := range f.Prefixes {
		if f.Prefixes[i].Prefix == p.Prefix {
			f.Prefixes[i] = p
			return
		}
	}
	f.Prefixes = append(f.Prefixes, p)
	sort.Slice(f.Prefixes, func(i, j int) bool {
		if f.Prefixes[i].Prefix.Bits() != f.Prefixes[j].Prefix.Bits() {
			return f.Prefixes[i].Prefix.Bits() > f.Prefixes[j].Prefix.Bits()
		}
		return f.Prefixes[i].Prefix.String() < f.Prefixes[j].Prefix.String()
	})
}

// GroupOriginsInit records the first advertiser of a group entry.
func (f *VNFIB) GroupOriginsInit(mac string, gid model.GroupID, origin model.SwitchID) {
	_ = mac
	f.groupOrigins[gid] = appendUniqueSwitch(f.groupOrigins[gid], origin)
}

// AddGroupOrigin records an additional advertiser of a redundancy group.
func (f *VNFIB) AddGroupOrigin(gid model.GroupID, origin model.SwitchID) {
	f.groupOrigins[gid] = appendUniqueSwitch(f.groupOrigins[gid], origin)
}

// GroupOrigins returns the switches that advertised a redundancy group.
func (f *VNFIB) GroupOrigins(gid model.GroupID) []model.SwitchID {
	return f.groupOrigins[gid]
}

// MarkGroupLocal notes that the switch has an access port into the group.
func (f *VNFIB) MarkGroupLocal(gid model.GroupID) { f.groupLocal[gid] = true }

// GroupIsLocal reports whether the switch has an access port into the group.
func (f *VNFIB) GroupIsLocal(gid model.GroupID) bool { return f.groupLocal[gid] }

// Lookup resolves a destination IP: host bindings first (a /32 is more
// specific than any covering prefix), then the external prefix routes.
func (f *VNFIB) Lookup(ip netip.Addr) (HostFIB, bool) {
	if ip.IsValid() {
		if h, ok := f.ByIP[ip]; ok && h.Egress.Switch != "" {
			return *h, true
		}
	}
	for _, p := range f.Prefixes {
		if !p.Prefix.Contains(ip) {
			continue
		}
		eg := p.Egress
		origin := p.Origin
		if target, ok := f.prefixTargets[p.Prefix.String()]; ok && target != "" {
			if target == f.localSwitch {
				eg = Egress{Switch: target, Local: true}
			} else {
				eg = Egress{Switch: target, NextHop: target}
			}
			origin = target
		}
		return HostFIB{
			IP:     ip,
			Type:   RoutePrefix,
			Source: SourceExternal,
			Origin: origin,
			Egress: eg,
		}, true
	}
	return HostFIB{}, false
}

// LookupMAC resolves a hardware address.
func (f *VNFIB) LookupMAC(mac string) (HostFIB, bool) {
	h, ok := f.Hosts[strings.ToLower(mac)]
	if !ok {
		return HostFIB{}, false
	}
	return *h, true
}

// SortedHosts returns the host bindings ordered by MAC.
func (f *VNFIB) SortedHosts() []*HostFIB {
	out := make([]*HostFIB, 0, len(f.Hosts))
	for _, h := range f.Hosts {
		out = append(out, h)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].MAC < out[j].MAC })
	return out
}

// SortedTunnels returns the tunnels ordered by remote switch.
func (f *VNFIB) SortedTunnels() []*Tunnel {
	out := make([]*Tunnel, 0, len(f.Tunnels))
	for _, t := range f.Tunnels {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Remote < out[j].Remote })
	return out
}

func appendUniqueSwitch(in []model.SwitchID, id model.SwitchID) []model.SwitchID {
	for _, x := range in {
		if x == id {
			return in
		}
	}
	return append(in, id)
}
