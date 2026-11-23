// Package sim builds and runs the CircusNet simulation: a multi-tent underlay,
// thousands of devices, an EVPN-style control plane and a data-plane checker.
package sim

import (
	"fmt"
	"math/rand"
	"net/netip"
	"sort"

	"circusnet/internal/fabric"
	"circusnet/internal/model"
)

// Config describes a whole simulated circus deployment.
type Config struct {
	Seed         int64
	Tents        int
	SwitchesMin  int // switches per tent (inclusive)
	SwitchesMax  int // inclusive
	VNs          int
	VNMembersMin int // participating switches per VN
	VNMembersMax int
	Devices      int
	DualHomedPct int  // percentage of devices attached through a redundancy group
	ExternalPct  int  // percentage of VNs that have an external prefix
	CoreSwitch   bool // one extra backbone switch that carries every VN
	// FullMeshVNs gives every switch every VN, which is what allows any
	// switch to route between any two VNs locally with the same anycast
	// gateway. When false, VNs are assigned to random subsets and the
	// membership churn tests are meaningful.
	FullMeshVNs bool
}

// DefaultConfig is the stress-test size asked for in the brief: dozens of
// tents, thousands of devices.
func DefaultConfig() Config {
	return Config{
		Seed:         20261124,
		Tents:        32,
		SwitchesMin:  3,
		SwitchesMax:  7,
		VNs:          6,
		VNMembersMin: 8,
		VNMembersMax: 18,
		Devices:      4000,
		DualHomedPct: 35,
		ExternalPct:  100,
		CoreSwitch:   true,
		FullMeshVNs:  true,
	}
}

type Topology struct {
	Config      Config
	Switch      map[model.SwitchID]*model.SwitchInfo
	Devices     []*model.Device
	VNs         []model.VirtualNetwork
	Groups      map[model.GroupID]*model.RedundancyGroup
	Links       []model.Link
	SwitchOrder []model.SwitchID

	externalBorders [][]model.SwitchID
}

// VNByID returns the virtual network with the given VNI.
func (t *Topology) VNByID(vni model.VNI) (model.VirtualNetwork, bool) {
	for _, vn := range t.VNs {
		if vn.VNI == vni {
			return vn, true
		}
	}
	return model.VirtualNetwork{}, false
}

func (t *Topology) Device(id model.DeviceID) *model.Device {
	if id < 0 || int(id) >= len(t.Devices) {
		return nil
	}
	return t.Devices[id]
}

// switchesInTent returns the switch IDs of one tent in stable order.
func switchesInTent(tents [][]model.SwitchID, idx int) []model.SwitchID {
	if idx < 0 || idx >= len(tents) {
		return nil
	}
	return tents[idx]
}

// TentSwitches groups switch IDs by tent.
func (t *Topology) TentSwitches() [][]model.SwitchID {
	tents := make([][]model.SwitchID, t.Config.Tents)
	for _, id := range t.SwitchOrder {
		sw := t.Switch[id]
		tents[sw.Tent] = append(tents[sw.Tent], id)
	}
	return tents
}

// GenerateTopology creates the underlay, VNs, redundancy groups and initial
// device placement deterministically from the seed.
func GenerateTopology(cfg Config) *Topology {
	rng := rand.New(rand.NewSource(cfg.Seed))
	t := &Topology{
		Config: cfg,
		Switch: map[model.SwitchID]*model.SwitchInfo{},
		Groups: map[model.GroupID]*model.RedundancyGroup{},
	}
	tents := make([][]model.SwitchID, cfg.Tents)
	tentOf := map[model.SwitchID]int{}
	next := 0
	loopback := netip.MustParseAddr("10.255.0.1")
	for tent := 0; tent < cfg.Tents; tent++ {
		n := cfg.SwitchesMin
		if cfg.SwitchesMax > cfg.SwitchesMin {
			n += rng.Intn(cfg.SwitchesMax - cfg.SwitchesMin + 1)
		}
		for i := 0; i < n; i++ {
			id := model.SwitchID(fmt.Sprintf("sw%03d", next))
			next++
			t.Switch[id] = &model.SwitchInfo{ID: id, Tent: tent, Loopback: loopback}
			loopback = loopback.Next()
			t.SwitchOrder = append(t.SwitchOrder, id)
			tents[tent] = append(tents[tent], id)
			tentOf[id] = tent
		}
	}
	_ = tentOf
	core := model.SwitchID("")
	if cfg.CoreSwitch {
		core = model.SwitchID(fmt.Sprintf("sw%03d", next))
		t.Switch[core] = &model.SwitchInfo{ID: core, Tent: -1, Loopback: loopback}
		t.SwitchOrder = append(t.SwitchOrder, core)
	}

	// Underlay: a ring of tents, a spanning tree between tent gateways and a
	// tree inside each tent. Rings/trees keep the path set loop-free so the
	// checker can assert exactly one physical path per switch pair.
	for tent := 0; tent < cfg.Tents; tent++ {
		gw := tents[tent][0]
		nextGW := tents[(tent+1)%cfg.Tents][0]
		t.Links = append(t.Links, model.Link{A: gw, B: nextGW})
		if core != "" {
			t.Links = append(t.Links, model.Link{A: gw, B: core})
		}
		for i := 1; i < len(tents[tent]); i++ {
			parent := tents[tent][rng.Intn(i)]
			t.Links = append(t.Links, model.Link{A: tents[tent][i], B: parent})
		}
	}
	// A few diagonal links inside large tents give the tree some depth
	// without creating cycles.
	for tent := 0; tent < cfg.Tents; tent++ {
		sws := tents[tent]
		if len(sws) >= 5 {
			t.Links = append(t.Links, model.Link{A: sws[1], B: sws[len(sws)-1]})
		}
	}
	t.Links = dedupeLinks(t.Links)

	// Virtual networks with auto-generated subnets.
	for v := 0; v < cfg.VNs; v++ {
		vni := model.VNI(10000 + v*100)
		subnet := netip.PrefixFrom(netip.AddrFrom4([4]byte{10, byte(v / 64), byte((v % 64) * 4), 0}), 22).Masked()
		gateway := subnet.Addr().Next()
		vn := model.VirtualNetwork{
			VNI:     vni,
			Name:    fmt.Sprintf("vn-%d", v),
			Subnet:  subnet,
			Gateway: gateway,
		}
		if rng.Intn(100) < cfg.ExternalPct {
			// External segments live well away from every VN subnet
			// (10.0.0.0/10), so host and prefix routes never collide.
			ext := netip.PrefixFrom(netip.AddrFrom4([4]byte{172, 16 + byte(v), 0, 0}), 16).Masked()
			vn.ExternalPrefixes = []netip.Prefix{ext}
		}
		t.VNs = append(t.VNs, vn)
	}

	if cfg.FullMeshVNs {
		for _, id := range t.SwitchOrder {
			for _, vn := range t.VNs {
				t.Switch[id].VNs = append(t.Switch[id].VNs, vn.VNI)
			}
			sort.Slice(t.Switch[id].VNs, func(i, j int) bool { return t.Switch[id].VNs[i] < t.Switch[id].VNs[j] })
		}
	}
	// Otherwise each VN is carried by a stable random subset (plus the core).
	for v := range t.VNs {
		if cfg.FullMeshVNs {
			break
		}
		want := cfg.VNMembersMin
		if cfg.VNMembersMax > cfg.VNMembersMin {
			want += rng.Intn(cfg.VNMembersMax - cfg.VNMembersMin + 1)
		}
		if want > len(t.SwitchOrder) {
			want = len(t.SwitchOrder)
		}
		perm := rng.Perm(len(t.SwitchOrder))
		for i := 0; i < want; i++ {
			id := t.SwitchOrder[perm[i]]
			t.Switch[id].VNs = append(t.Switch[id].VNs, t.VNs[v].VNI)
		}
	}
	if core != "" {
		for v := range t.VNs {
			t.Switch[core].VNs = append(t.Switch[core].VNs, t.VNs[v].VNI)
		}
	}
	for _, id := range t.SwitchOrder {
		sort.Slice(t.Switch[id].VNs, func(i, j int) bool { return t.Switch[id].VNs[i] < t.Switch[id].VNs[j] })
	}

	// External segments: two border switches per VN advertise the prefix;
	// external hosts are attached to the second one so that a /32 host route
	// and the covering /16 prefix route point at different switches. This is
	// what proves host-route preference in the verifier.
	t.externalBorders = make([][]model.SwitchID, len(t.VNs))
	for v := range t.VNs {
		if len(t.VNs[v].ExternalPrefixes) == 0 {
			continue
		}
		members := t.SwitchesForVN(t.VNs[v].VNI)
		if len(members) < 2 {
			continue
		}
		a := members[0]
		b := members[1]
		for _, m := range members[2:] {
			if t.Switch[m].Tent != t.Switch[a].Tent {
				b = m
				break
			}
		}
		t.VNs[v].ExternalOwners = []model.SwitchID{a, b}
		t.externalBorders[v] = []model.SwitchID{a, b}
	}

	// Devices. Dual-homed devices are grouped: each redundancy group is a
	// pair of switches in the same tent carrying several devices, so losing
	// one access link flips a whole group with a single route withdrawal.
	t.Devices = make([]*model.Device, 0, cfg.Devices+2*len(t.VNs))
	perVN := make([]int, len(t.VNs))
	nextID := 0
	groupSeq := 0
	alloc := func(v int, vni model.VNI) (string, netip.Addr) {
		perVN[v]++
		ip := t.VNs[v].Subnet.Addr()
		for i := 0; i <= perVN[v]; i++ { // .1 is the anycast gateway
			ip = ip.Next()
		}
		mac := fmt.Sprintf("02:%02x:%02x:%02x:%02x:%02x", byte(nextID>>24), byte(nextID>>16), byte(nextID>>8), byte(nextID), byte(nextID*7))
		return mac, ip
	}
	dualTarget := cfg.Devices * cfg.DualHomedPct / 100
	if dualTarget > 0 && len(t.VNs) > 0 {
		groupSize := 8
		for len(t.Devices) < dualTarget {
			v := rng.Intn(len(t.VNs))
			vni := t.VNs[v].VNI
			candidates := t.SwitchesForVN(vni)
			var a, b model.SwitchID
			found := false
			for tries := 0; tries < 64 && !found; tries++ {
				a = candidates[rng.Intn(len(candidates))]
				b = candidates[rng.Intn(len(candidates))]
				if a == b {
					continue
				}
				if t.Switch[a].Tent == t.Switch[b].Tent {
					found = true
				}
			}
			if !found {
				continue
			}
			if a > b {
				a, b = b, a
			}
			gid := model.GroupID(fmt.Sprintf("esi-%04d", groupSeq))
			groupSeq++
			t.Groups[gid] = &model.RedundancyGroup{ID: gid, VN: vni, Members: []model.SwitchID{a, b}}
			n := groupSize
			if n > dualTarget-len(t.Devices) {
				n = dualTarget - len(t.Devices)
			}
			for i := 0; i < n; i++ {
				mac, ip := alloc(v, vni)
				t.Devices = append(t.Devices, &model.Device{
					ID: model.DeviceID(nextID), MAC: mac, IP: ip, VN: vni,
					Kind: model.AttachGroup, Switch: a, Group: gid,
					GroupMembers: []model.SwitchID{a, b},
					Port:         fmt.Sprintf("p%d", nextID%48),
				})
				nextID++
			}
		}
	}
	for len(t.Devices) < cfg.Devices {
		v := rng.Intn(len(t.VNs))
		vni := t.VNs[v].VNI
		candidates := t.SwitchesForVN(vni)
		mac, ip := alloc(v, vni)
		t.Devices = append(t.Devices, &model.Device{
			ID: model.DeviceID(nextID), MAC: mac, IP: ip, VN: vni,
			Kind:   model.AttachSingle,
			Switch: candidates[rng.Intn(len(candidates))],
			Port:   fmt.Sprintf("p%d", nextID%48),
		})
		nextID++
	}

	// External hosts on each external segment.
	for v := range t.VNs {
		if len(t.VNs[v].ExternalPrefixes) == 0 || len(t.externalBorders[v]) < 2 {
			continue
		}
		owner := t.externalBorders[v][0]
		prefix := t.VNs[v].ExternalPrefixes[0]
		for i := 0; i < 2; i++ {
			d := len(t.Devices)
			ip := prefix.Addr()
			for n := 0; n <= i+2; n++ {
				ip = ip.Next()
			}
			dev := &model.Device{
				ID:            model.DeviceID(d),
				MAC:           fmt.Sprintf("02:ee:%02x:%02x:%02x:%02x", byte(v), byte(i), byte(d>>8), byte(d)),
				IP:            ip,
				VN:            t.VNs[v].VNI,
				Kind:          model.AttachSingle,
				Switch:        owner,
				Port:          fmt.Sprintf("x%d", i),
				External:      true,
				ExternalKnown: i == 0,
			}
			t.Devices = append(t.Devices, dev)
		}
	}
	return t
}

// SwitchesForVN lists the switches carrying a VN in stable order.
func (t *Topology) SwitchesForVN(vni model.VNI) []model.SwitchID {
	var out []model.SwitchID
	for _, id := range t.SwitchOrder {
		if t.Switch[id].InVirtualNetwork(vni) {
			out = append(out, id)
		}
	}
	return out
}

func indexOf(ids []model.SwitchID, want model.SwitchID) int {
	for i, id := range ids {
		if id == want {
			return i
		}
	}
	return 0
}

func dedupeLinks(in []model.Link) []model.Link {
	seen := map[fabric.LinkKey]bool{}
	var out []model.Link
	for _, l := range in {
		if l.A == l.B {
			continue
		}
		k := fabric.NewLinkKey(l.A, l.B)
		if seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, model.Link{A: model.SwitchID(k.A), B: model.SwitchID(k.B)})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].A != out[j].A {
			return out[i].A < out[j].A
		}
		return out[i].B < out[j].B
	})
	return out
}

var _ = switchesInTent
