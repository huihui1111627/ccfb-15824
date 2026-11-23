// Package fabric implements the EVPN-style control plane used by CircusNet:
// route types, route-target import policy, RIBs, FIBs and tunnel computation.
package fabric

import (
	"fmt"
	"net/netip"
	"sort"
	"strconv"

	"circusnet/internal/model"
)

// RouteType distinguishes the address families carried by the control plane.
// Host routes describe a single end device, prefix routes describe an external
// segment, group routes describe a whole redundancy group and member routes
// describe a switch's membership in a virtual network.
type RouteType string

const (
	RouteHost   RouteType = "host"
	RoutePrefix RouteType = "prefix"
	RouteGroup  RouteType = "group"
	RouteMember RouteType = "member"
)

// RouteSource explains why a route exists, for the dashboard.
type RouteSource string

const (
	SourceLocalAccess RouteSource = "local-access" // learned on an access port
	SourceLocalGroup  RouteSource = "local-group"  // one member's view of a redundancy group
	SourceExternal    RouteSource = "external"     // external segment prefix
	SourceEVPN        RouteSource = "evpn"         // imported from a remote switch
	SourceLocalMember RouteSource = "member"       // local VN membership
)

// HostEntry is one MAC/IP binding inside a group route. Seq is the MAC
// mobility sequence assigned when the device was attached, which lets the
// receiver prefer the newest binding across a move.
type HostEntry struct {
	MAC string     `json:"mac"`
	IP  netip.Addr `json:"ip"`
	Seq uint64     `json:"seq"`
}

// Route is one advertisement. RD/RT values are generated, never configured.
type Route struct {
	Type     RouteType      `json:"type"`
	VNI      model.VNI      `json:"vni"`
	RD       string         `json:"rd"`
	RTs      []string       `json:"rts"`
	Origin   model.SwitchID `json:"origin"`
	Loopback netip.Addr     `json:"loopback"`

	// Epoch increments when the origin switch restarts. Seq increments for
	// each local change (and for host routes carries MAC mobility order).
	Epoch uint64 `json:"epoch"`
	Seq   uint64 `json:"seq"`

	MAC     string          `json:"mac,omitempty"`
	IP      netip.Addr      `json:"ip,omitempty"`
	Prefix  netip.Prefix    `json:"prefix,omitempty"`
	Group   model.GroupID   `json:"group,omitempty"`
	Devices []HostEntry     `json:"devices,omitempty"`
	Groups  []model.GroupID `json:"groups,omitempty"` // redundancy-group tags on a member route
	Ready   bool            `json:"ready"`            // member route: ports are open

	Source RouteSource `json:"source"`
	Stale  bool        `json:"stale,omitempty"`
	// cacheKey memoises Key(); it is never serialised and is filled in by the
	// constructors so the control plane does not rebuild strings per peer.
	cacheKey string
	// StaleUntil is a local receive-side timer, in convergence rounds.
	StaleUntil int `json:"-"`
	Received   int `json:"-"`
}

// RD derives the route distinguisher from the switch loopback and the VN.
// Every switch+VN pair gets a distinct RD without any operator input.
func RD(loopback netip.Addr, vni model.VNI) string {
	return fmt.Sprintf("%s:%d", loopback, uint32(vni))
}

// VNRT is the route target shared by every member of a virtual network. This
// is the target used for import decisions.
func VNRT(vni model.VNI) string { return fmt.Sprintf("target:%d", uint32(vni)) }

// OriginRT additionally names the advertising switch; it is carried for
// observability and per-origin policy but is not required to import.
func OriginRT(sw model.SwitchID, vni model.VNI) string {
	return fmt.Sprintf("target:%s:%d", sw, uint32(vni))
}

// RTs returns the auto-generated route-target set for a switch/VN pair. The
// import decision only needs the VN target; the origin-specific target is
// carried for observability and per-origin policy (the string is still built
// because routes are rendered in the dashboard).
func RTs(sw model.SwitchID, vni model.VNI) []string {
	return []string{VNRT(vni), OriginRT(sw, vni)}
}

// MemberKey is the RIB key of the member route for a switch/VN pair.
func MemberKey(vni model.VNI, origin model.SwitchID) string {
	return "M|" + strconv.FormatUint(uint64(vni), 10) + "|" + string(origin)
}

// Key is the RIB identity of a route. Member and group routes from the same
// switch/VN are distinct route types under the same RD.
func (r Route) Key() string {
	if r.cacheKey != "" {
		return r.cacheKey
	}
	return r.computeKey()
}

func (r Route) computeKey() string {
	switch r.Type {
	case RouteHost:
		return "H|" + strconv.FormatUint(uint64(r.VNI), 10) + "|" + string(r.Origin) + "|" + LowerMAC(r.MAC)
	case RoutePrefix:
		return "P|" + strconv.FormatUint(uint64(r.VNI), 10) + "|" + string(r.Origin) + "|" + r.Prefix.String()
	case RouteGroup:
		return "G|" + strconv.FormatUint(uint64(r.VNI), 10) + "|" + string(r.Origin) + "|" + string(r.Group)
	case RouteMember:
		return MemberKey(r.VNI, r.Origin)
	}
	return "?"
}

// withKey returns the route with its key memoised.
func (r Route) withKey() Route {
	r.cacheKey = r.computeKey()
	return r
}

// Clone returns a deep enough copy for storage (RTs/Devices/Groups slices).
func (r Route) Clone() Route {
	r.RTs = append([]string(nil), r.RTs...)
	r.Devices = append([]HostEntry(nil), r.Devices...)
	r.Groups = append([]model.GroupID(nil), r.Groups...)
	return r
}

// NewHostRoute builds a locally-learned host route.
func NewHostRoute(sw model.SwitchInfo, vni model.VNI, mac string, ip netip.Addr, seq, epoch uint64) Route {
	return Route{
		Type: RouteHost, VNI: vni,
		RD: RD(sw.Loopback, vni), RTs: RTs(sw.ID, vni),
		Origin: sw.ID, Loopback: sw.Loopback,
		Epoch: epoch, Seq: seq,
		MAC: LowerMAC(mac), IP: ip,
		Source: SourceLocalAccess,
	}.withKey()
}

// NewGroupRoute builds one member switch's advertisement for a redundancy
// group. Both members advertise the same device set under their own RD.
func NewGroupRoute(sw model.SwitchInfo, vni model.VNI, group model.GroupID, devices []HostEntry, seq, epoch uint64) Route {
	devices = append([]HostEntry(nil), devices...)
	sort.Slice(devices, func(i, j int) bool { return devices[i].MAC < devices[j].MAC })
	return Route{
		Type: RouteGroup, VNI: vni,
		RD: RD(sw.Loopback, vni), RTs: RTs(sw.ID, vni),
		Origin: sw.ID, Loopback: sw.Loopback,
		Epoch: epoch, Seq: seq,
		Group: group, Devices: devices,
		Source: SourceLocalGroup,
	}.withKey()
}

// NewMemberRoute builds a switch's membership advertisement for a VN. The
// Groups tags are used by receivers for split-horizon suppression.
func NewMemberRoute(sw model.SwitchInfo, vni model.VNI, groups []model.GroupID, ready bool, seq, epoch uint64) Route {
	return Route{
		Type: RouteMember, VNI: vni,
		RD: RD(sw.Loopback, vni), RTs: RTs(sw.ID, vni),
		Origin: sw.ID, Loopback: sw.Loopback,
		Epoch: epoch, Seq: seq,
		Groups: append([]model.GroupID(nil), groups...),
		Ready:  ready,
		Source: SourceLocalMember,
	}.withKey()
}

// NewPrefixRoute builds a local external-prefix advertisement.
func NewPrefixRoute(sw model.SwitchInfo, vni model.VNI, prefix netip.Prefix, seq, epoch uint64) Route {
	return Route{
		Type: RoutePrefix, VNI: vni,
		RD: RD(sw.Loopback, vni), RTs: RTs(sw.ID, vni),
		Origin: sw.ID, Loopback: sw.Loopback,
		Epoch: epoch, Seq: seq,
		Prefix: prefix.Masked(),
		Source: SourceExternal,
	}.withKey()
}

// Preference returns a comparable freshness tuple for host-route selection.
// MAC mobility sequence dominates, then the origin epoch, then switch ID for
// deterministic tie-breaking.
func (r Route) fresherThan(o Route) bool {
	if r.Seq != o.Seq {
		return r.Seq > o.Seq
	}
	if r.Epoch != o.Epoch {
		return r.Epoch > o.Epoch
	}
	return r.Origin < o.Origin
}
