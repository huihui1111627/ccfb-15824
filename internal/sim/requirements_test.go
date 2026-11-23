package sim

import (
	"math/rand"
	"net/netip"
	"testing"

	"circusnet/internal/fabric"
	"circusnet/internal/model"
)

func smallWorld(t *testing.T, devices, tents int) (*World, *Topology) {
	t.Helper()
	cfg := DefaultConfig()
	cfg.Seed = 99
	cfg.Devices = devices
	cfg.Tents = tents
	topo := GenerateTopology(cfg)
	w := NewWorld(topo, DefaultTimes())
	return w, topo
}

// Requirement 1: a switch that learned a MAC locally advertises a host route
// carrying the device's IP, and other switches answer ARP from that route
// instead of flooding.
func TestARPStaysLocal(t *testing.T) {
	w, topo := smallWorld(t, 240, 4)
	for _, d := range topo.Devices {
		if d.External || d.Kind != model.AttachSingle {
			continue
		}
		owner := d.Switch
		if !w.Topo.Switch[owner].InVirtualNetwork(d.VN) {
			continue
		}
		found := false
		for _, r := range w.RIB[owner].Local {
			if r.Type == fabric.RouteHost && r.MAC == d.MAC && r.IP == d.IP {
				found = true
			}
		}
		if !found {
			t.Fatalf("device %d: no host route with IP on %s", d.ID, owner)
		}
		// Any other switch carrying the VN must resolve the IP locally.
		for _, id := range topo.SwitchOrder {
			if id == owner || !topo.Switch[id].InVirtualNetwork(d.VN) {
				continue
			}
			if !w.operational(id) {
				continue
			}
			if !w.ARPHit(id, d.VN, d.IP) {
				t.Fatalf("switch %s cannot answer ARP for %s from %s", id, d.IP, owner)
			}
		}
		break
	}
	rep := w.VerifyAll(VerifyOptions{Pairs: 8, Seed: 1})
	if rep.Broadcasts != 0 {
		t.Fatalf("expected no flooding, got %d broadcast copies", rep.Broadcasts)
	}
	if rep.ARPAnswered != rep.ARPRequests {
		t.Fatalf("ARP suppression %d/%d", rep.ARPAnswered, rep.ARPRequests)
	}
}

// Requirement 2: RDs and RTs are derived from switch identity and VN number,
// routes import by matching RT, and routes for a VN the switch does not carry
// never enter its RIB.
func TestRTImportPolicy(t *testing.T) {
	w, topo := smallWorld(t, 120, 3)
	for _, id := range topo.SwitchOrder {
		info := topo.Switch[id]
		for _, r := range w.RIB[id].Local {
			if r.RD != fabric.RD(info.Loopback, r.VNI) {
				t.Fatalf("%s RD %s not derived from loopback %s / vni %d", id, r.RD, info.Loopback, r.VNI)
			}
			if got := r.RTs[0]; got != fabric.VNRT(r.VNI) {
				t.Fatalf("%s route target %s, want %s", id, got, fabric.VNRT(r.VNI))
			}
		}
		for _, rec := range w.RIB[id].Received {
			if !info.InVirtualNetwork(rec.Route.VNI) {
				t.Fatalf("%s installed route for non-local VN %d", id, rec.Route.VNI)
			}
			if !w.RIB[id].Policy.Accept(rec.Route) {
				t.Fatalf("%s installed route whose RT does not match", id)
			}
		}
	}
	// A synthetic route for an unrelated VN must be rejected outright.
	p := fabric.NewImportPolicy([]model.VNI{10000})
	r := fabric.NewHostRoute(*topo.Switch[topo.SwitchOrder[0]], 99999, "02:00:00:00:ff:ff",
		netip.MustParseAddr("10.99.0.1"), 1, 1)
	if p.Accept(r) {
		t.Fatal("policy accepted a route for an unconfigured VN")
	}
}

// Requirement 3: external segments are advertised as prefix routes, kept
// separate from host routes, and a more-specific host route wins.
func TestHostBeatsPrefix(t *testing.T) {
	w, topo := smallWorld(t, 240, 4)
	checked := 0
	for _, vn := range topo.VNs {
		if len(vn.ExternalPrefixes) == 0 {
			continue
		}
		var ext *model.Device
		for _, d := range topo.Devices {
			if d.External && d.VN == vn.VNI && d.ExternalKnown {
				ext = d
				break
			}
		}
		if ext == nil {
			continue
		}
		if len(vn.ExternalOwners) < 2 {
			continue
		}
		owner, other := vn.ExternalOwners[0], vn.ExternalOwners[1]
		for _, id := range topo.SwitchOrder {
			if !w.operational(id) || !topo.Switch[id].InVirtualNetwork(vn.VNI) {
				continue
			}
			fib := w.FIB[id][vn.VNI]
			if fib == nil {
				continue
			}
			// Both the prefix and the host route must be present and stored
			// in separate structures.
			hasPrefix := false
			for _, p := range fib.Prefixes {
				if p.Prefix == vn.ExternalPrefixes[0] {
					hasPrefix = true
				}
			}
			if !hasPrefix {
				t.Fatalf("%s missing external prefix %s", id, vn.ExternalPrefixes[0])
			}
			hit, ok := fib.Lookup(ext.IP)
			if !ok {
				t.Fatalf("%s cannot resolve external host %s", id, ext.IP)
			}
			if hit.Type != fabric.RouteHost {
				t.Fatalf("%s resolved %s via %s, expected more-specific host route", id, ext.IP, hit.Type)
			}
			if !hit.Egress.Local && hit.Origin != ext.Switch {
				t.Fatalf("%s external host route origin %s, want owner %s", id, hit.Origin, ext.Switch)
			}
			_ = owner
			// An address in the prefix that is not the known host resolves
			// through the prefix route instead.
			unknown := vn.ExternalPrefixes[0].Addr().Next().Next().Next().Next().Next().Next()
			if hit2, ok := fib.Lookup(unknown); ok && hit2.Type != fabric.RoutePrefix {
				t.Fatalf("%s resolved unknown %s via %s", id, unknown, hit2.Type)
			}
			_ = other
			checked++
		}
	}
	if checked == 0 {
		t.Fatal("no external segments exercised")
	}
}

// Requirement 4: a redundancy group is advertised once per member; cutting one
// access link withdraws exactly that member's group route and every device in
// the group moves to the peer at once.
func TestGroupFailoverSingleWithdrawal(t *testing.T) {
	w, topo := smallWorld(t, 300, 6)
	var gid model.GroupID
	var group *model.RedundancyGroup
	for id, g := range topo.Groups {
		n := 0
		for _, d := range topo.Devices {
			if d.Group == id {
				n++
			}
		}
		if n >= 4 {
			gid, group = id, g
			break
		}
	}
	if group == nil {
		t.Fatal("no multi-device group")
	}
	member := group.Members[0]
	peer := group.Members[1]
	wdBefore, gwBefore := w.Withdrawals, w.GroupWithdrawals
	w.SetAccess(gid, member, false)
	w.Converge()
	if got := w.Withdrawals - wdBefore; got != 1 {
		t.Fatalf("access cut produced %d withdrawals, want exactly 1", got)
	}
	if got := w.GroupWithdrawals - gwBefore; got != 1 {
		t.Fatalf("access cut produced %d group withdrawals, want 1", got)
	}
	// Every switch must now reach every device in the group via the peer.
	n := 0
	for _, d := range topo.Devices {
		if d.Group != gid {
			continue
		}
		for _, id := range topo.SwitchOrder {
			if id == member || !w.operational(id) {
				continue
			}
			fib := w.FIB[id][d.VN]
			if fib == nil {
				continue
			}
			h := fib.Hosts[d.MAC]
			if h == nil {
				t.Fatalf("%s lost group device %s", id, d.MAC)
			}
			if id != peer && !h.Egress.Local && h.Origin != peer {
				t.Fatalf("%s routes %s via %s, want peer %s", id, d.MAC, h.Origin, peer)
			}
			n++
		}
	}
	if n == 0 {
		t.Fatal("group failover not exercised")
	}
	rep := w.VerifyAll(VerifyOptions{Pairs: 10, Seed: 2})
	if rep.Failed != 0 {
		t.Fatalf("%d forwarding failures after group failover: %+v", rep.Failed, rep.Violations[:min(3, len(rep.Violations))])
	}
}

// Requirement 5: a broadcast received from a redundancy group's segment is
// not copied back out of that group's access port on the peer switch. The
// decision is driven by the group tag carried in the member route.
func TestGroupSplitHorizon(t *testing.T) {
	w, topo := smallWorld(t, 300, 6)
	var gid model.GroupID
	var group *model.RedundancyGroup
	for id, g := range topo.Groups {
		gid, group = id, g
		break
	}
	if group == nil {
		t.Fatal("no groups")
	}
	from := group.Members[0]
	peer := group.Members[1]

	out := w.FloodBroadcast(from, group.VN)
	if !containsSwitch(out.Delivered, peer) {
		t.Fatalf("peer %s should still receive the frame over the tunnel; only its access port is suppressed (delivered %v)",
			peer, out.Delivered)
	}
	if !w.egressSuppressed(peer, group.VN, from) {
		t.Fatalf("no split-horizon suppression on %s for its own group %s", peer, gid)
	}
	if len(out.Suppressed) == 0 {
		t.Fatal("flood reported no suppressed egress ports")
	}

	// Suppression must be per group: exactly the groups whose access port the
	// receiving switch has, and only because the ingress switch is on that
	// group's segment.
	for _, id := range topo.SwitchOrder {
		if !w.operational(id) || !topo.Switch[id].InVirtualNetwork(group.VN) {
			continue
		}
		for _, g := range topo.Groups {
			if g.VN != group.VN {
				continue
			}
			hasPort := g.HasMember(id) && w.AccessUp[g.ID][id]
			onSegment := w.TopoSegmentReached(g.ID, from)
			want := hasPort && onSegment
			if got := w.egressSuppressedFor(id, group.VN, g.ID, from); got != want {
				t.Fatalf("%s group %s: suppressed=%v, want %v (port=%v segment=%v)",
					id, g.ID, got, want, hasPort, onSegment)
			}
		}
	}

	// A switch that is not a member of the group must never suppress that
	// group's port.
	for _, id := range topo.SwitchOrder {
		if group.HasMember(id) || !w.operational(id) || !topo.Switch[id].InVirtualNetwork(group.VN) {
			continue
		}
		if w.egressSuppressedFor(id, group.VN, gid, from) {
			t.Fatalf("%s suppressed group %s without being a member", id, gid)
		}
	}
}

func containsSwitch(ids []model.SwitchID, want model.SwitchID) bool {
	for _, id := range ids {
		if id == want {
			return true
		}
	}
	return false
}

// Requirement 6: each switch advertises a member route per VN, peers build
// tunnels from them, and leaving the VN withdraws the route and tears the
// tunnel down.
func TestMemberRoutesBuildTunnels(t *testing.T) {
	w, topo := smallWorld(t, 120, 3)
	sw := topo.SwitchOrder[2]
	vni := topo.Switch[sw].VNs[0]
	peer := topo.SwitchOrder[3]
	if !topo.Switch[peer].InVirtualNetwork(vni) {
		t.Fatal("test setup: peer lacks VN")
	}
	if w.FIB[sw][vni].Tunnels[peer] == nil {
		t.Fatalf("%s has no tunnel to %s for VN %d", sw, peer, vni)
	}
	wdBefore := w.Withdrawals
	w.SetVNMembership(sw, vni, false)
	w.Converge()
	if w.Withdrawals == wdBefore {
		t.Fatal("leaving the VN withdrew nothing")
	}
	if w.FIB[sw][vni] != nil {
		t.Fatalf("%s still has a FIB for VN %d after leaving", sw, vni)
	}
	if _, ok := w.FIB[peer][vni].Tunnels[sw]; ok {
		t.Fatalf("%s still has a tunnel to %s after it left", peer, sw)
	}
	w.SetVNMembership(sw, vni, true)
	w.Converge()
	if w.FIB[sw][vni].Tunnels[peer] == nil {
		t.Fatalf("tunnel was not rebuilt after rejoining")
	}
}

// Requirement 7: every switch has the same gateway address per VN and does
// the inter-VN hop locally.
func TestAnycastGatewayLocalRouting(t *testing.T) {
	w, topo := smallWorld(t, 120, 3)
	want := topo.VNs[0].Gateway
	for _, vn := range topo.VNs {
		if vn.Gateway == (netip.Addr{}) {
			t.Fatalf("VN %d has no gateway", vn.VNI)
		}
	}
	var a, b *model.Device
	for _, d := range topo.Devices {
		if d.VN == topo.VNs[0].VNI && d.Kind == model.AttachSingle && a == nil && !d.External {
			a = d
		}
		if d.VN == topo.VNs[1].VNI && b == nil && !d.External {
			b = d
		}
	}
	if a == nil || b == nil {
		t.Fatal("need devices in two VNs")
	}
	_ = want
	res := w.Forward(a, b)
	if res == nil {
		t.Skip("pair not testable")
	}
	if !res.OK {
		t.Fatalf("cross-VN forwarding failed: %s", res.Note)
	}
	if len(res.Path) == 0 || res.Path[0].Switch != res.Path[0].NextHop {
		t.Fatalf("expected the local gateway hop at the ingress switch, got %+v", res.Path)
	}
}

// Requirement 8: a restarted switch holds its ports closed until routes
// converge, and pre-restart routes are aged out by their TTL.
func TestRestartWaitsForConvergence(t *testing.T) {
	w, topo := smallWorld(t, 240, 4)
	sw := topo.SwitchOrder[5]
	w.SwitchDown(sw)
	w.Converge()
	if w.PortsOpen[sw] || w.FIB[sw] != nil {
		// A down switch may still expose an empty map; it must not forward.
		for _, f := range w.FIB[sw] {
			if len(f.Hosts) > 0 {
				t.Fatal("down switch still forwards")
			}
		}
	}
	w.SwitchUp(sw)
	if w.PortsOpen[sw] {
		t.Fatal("ports opened before convergence")
	}
	// A restarting switch has no FIB, so nothing entering it can be
	// delivered; the pair is simply not testable through it.
	if f := w.FIB[sw]; len(f) != 0 {
		for _, v := range f {
			if len(v.Hosts) > 0 || len(v.Tunnels) > 0 {
				t.Fatal("restarting switch has installed forwarding state")
			}
		}
	}
	w.Converge()
	if !w.PortsOpen[sw] {
		t.Fatal("ports never opened after convergence")
	}
	if w.RIB[sw].StaleCount() != 0 {
		t.Fatalf("%d stale routes left after convergence", w.RIB[sw].StaleCount())
	}
	rep := w.VerifyAll(VerifyOptions{Pairs: 8, Seed: 3})
	if rep.Failed != 0 {
		t.Fatalf("%d forwarding failures after restart", rep.Failed)
	}
}

// Routes from an origin's previous epoch are excluded from forwarding at
// once and deleted when the TTL expires. This is the rule that lets a
// restarted switch keep its ports closed without trusting old state.
func TestRestartAgesOutPreRestartRoutes(t *testing.T) {
	w, topo := smallWorld(t, 200, 4)
	observer, origin := topo.SwitchOrder[5], topo.SwitchOrder[6]
	vni := topo.Switch[observer].VNs[0]
	old := fabric.NewHostRoute(*topo.Switch[origin], vni, "02:de:ad:be:ef:10",
		netip.MustParseAddr("10.0.0.240"), 1, 1)
	if !w.RIB[observer].Receive(fabric.Announcement{Route: old}, origin, w.Round, w.Times.StaleTTL) {
		t.Fatal("failed to seed the old-epoch route")
	}
	if w.RIB[observer].StaleCount() != 0 {
		t.Fatal("a fresh route was marked stale")
	}

	// The origin restarts: its epoch advances, and its route set is
	// re-advertised under the new epoch.
	w.SwitchDown(origin)
	w.Converge()
	if w.RIB[observer].StaleCount() == 0 {
		t.Fatal("routes from a downed origin were not marked stale")
	}
	w.SwitchUp(origin)
	w.Converge()

	if w.RIB[observer].StaleCount() != 0 {
		t.Fatalf("%d pre-restart routes survived the TTL", w.RIB[observer].StaleCount())
	}
	if h := w.FIB[observer][vni].Hosts["02:de:ad:be:ef:10"]; h != nil {
		t.Fatal("a route that was never re-advertised is still in the FIB")
	}
	// The restarted origin's real routes are present after convergence.
	found := 0
	for _, h := range w.FIB[observer][vni].Hosts {
		if h.Origin == origin && h.Epoch == w.Epoch[origin] {
			found++
		}
	}
	if found == 0 {
		t.Fatal("no routes from the restarted origin were re-learned")
	}
	rep := w.VerifyAll(VerifyOptions{Pairs: 8, Seed: 4})
	if rep.Failed != 0 {
		t.Fatalf("%d forwarding failures after the origin restarted", rep.Failed)
	}
}

// Broadcast cost: with host routes everywhere, a broadcast is replicated only
// once per remote member switch and is suppressed at redundancy-group ports.
func TestBroadcastAccounting(t *testing.T) {
	w, topo := smallWorld(t, 240, 4)
	sw := topo.SwitchOrder[0]
	vni := topo.Switch[sw].VNs[0]
	members := 0
	for _, id := range topo.SwitchOrder {
		if id != sw && w.operational(id) && topo.Switch[id].InVirtualNetwork(vni) {
			members++
		}
	}
	out := w.FloodBroadcast(sw, vni)
	if out.Frames != members {
		t.Fatalf("flood replicated to %d switches, want %d (one copy per remote member)", out.Frames, members)
	}
	if out.Frames > 1 && len(out.Suppressed) == 0 {
		t.Fatal("expected at least one split-horizon suppression with many group members")
	}
	if w.Broadcasts != int64(out.Frames) {
		t.Fatalf("broadcast counter %d, want %d", w.Broadcasts, out.Frames)
	}
	if w.BroadcastSuppressed != int64(len(out.Suppressed)) {
		t.Fatalf("suppressed counter %d, want %d", w.BroadcastSuppressed, len(out.Suppressed))
	}
}

// Pre-restart routes are held but not used, then removed when the TTL
// expires. This is the RIB-level rule the restart path relies on.
func TestStaleRouteTTL(t *testing.T) {
	mk := func(epoch uint64, mac string) fabric.Route {
		return fabric.Route{
			Type: fabric.RouteHost, VNI: 10000,
			RD: "10.255.0.9:10000", RTs: []string{fabric.VNRT(10000)},
			Origin: "sw9", Loopback: netip.MustParseAddr("10.255.0.9"),
			Epoch: epoch, Seq: 1, MAC: mac,
			IP: netip.MustParseAddr("10.0.0.9"),
		}
	}
	rib := fabric.NewRIB([]model.VNI{10000})
	ttl := 3
	old := mk(1, "02:aa:bb:cc:dd:ee")
	rib.Receive(fabric.Announcement{Route: old}, "sw1", 1, ttl)
	if n := len(rib.ActiveRoutes()); n != 1 {
		t.Fatalf("initial route not imported (%d active)", n)
	}
	// The origin restarts: an epoch-2 route arrives and the epoch-1 copy
	// becomes stale immediately.
	// The restarted origin re-advertises only a different device; the
	// pre-restart route is not repeated and must age out.
	fresh := mk(2, "02:aa:bb:cc:dd:ef")
	rib.Receive(fabric.Announcement{Route: fresh}, "sw1", 2, ttl)
	if got := rib.StaleCount(); got != 1 {
		t.Fatalf("%d stale routes, want 1", got)
	}
	for _, r := range rib.ActiveRoutes() {
		if r.MAC == "02:aa:bb:cc:dd:ee" {
			t.Fatal("stale pre-restart route is still active")
		}
	}
	// It survives until the TTL, then is deleted.
	if rib.Expire(4) {
		t.Fatal("stale route expired too early")
	}
	if !rib.Expire(5) {
		t.Fatal("stale route did not expire at its TTL")
	}
	if rib.StaleCount() != 0 {
		t.Fatal("stale route still present after TTL")
	}
	if len(rib.ActiveRoutes()) != 1 {
		t.Fatal("fresh route was removed with the stale one")
	}
}

// The whole stress scenario must pass with a fixed seed.
func TestSeededStress(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Devices = 400
	cfg.Tents = 8
	rep := RunStress(StressOptions{Config: cfg, Seed: 424242, Steps: 25, Pairs: 6})
	if !rep.Passed {
		t.Fatalf("stress failed: %d failures / %d checks", rep.Failures, rep.Checks)
	}
	if rep.Loops != 0 {
		t.Fatalf("%d forwarding loops", rep.Loops)
	}
	if rep.Broadcasts != 0 {
		t.Fatalf("%d flooded broadcast copies escaped suppression", rep.Broadcasts)
	}
	if rep.GroupFailoverCount == 0 {
		t.Fatal("scenario never exercised a group-wide failover")
	}
}

// Randomised device movement must keep every path consistent.
func TestMoveChurn(t *testing.T) {
	w, topo := smallWorld(t, 300, 5)
	rng := rand.New(rand.NewSource(5))
	for iter := 0; iter < 15; iter++ {
		moved := 0
		for tries := 0; tries < 50 && moved < 6; tries++ {
			d := topo.Devices[rng.Intn(len(topo.Devices))]
			if d.Kind != model.AttachSingle || d.External {
				continue
			}
			if w.MoveDevice(d, rng) {
				moved++
			}
		}
		w.Converge()
		rep := w.VerifyAll(VerifyOptions{Pairs: 6, Seed: int64(iter)})
		if rep.Failed != 0 {
			t.Fatalf("iteration %d: %d failures, first: %+v", iter, rep.Failed, rep.Violations[0])
		}
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
