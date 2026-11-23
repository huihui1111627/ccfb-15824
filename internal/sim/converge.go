package sim

import (
	"fmt"
	"net/netip"
	"runtime"
	"sort"
	"sync"
	"time"

	"circusnet/internal/fabric"
	"circusnet/internal/model"
)

func (w *World) log(kind EventKind, ev Event) {
	w.eventSeq++
	ev.Seq = w.eventSeq
	ev.Kind = kind
	ev.Round = w.Round
	ev.At = w.Start.Add(time.Duration(w.Round) * 250 * time.Millisecond)
	w.Events = append(w.Events, ev)
}

// buildLocalRoutes builds the complete locally-originated route set of a
// switch: one member route per VN, host routes for local single-homed
// devices, one group route per active redundancy group and external prefix
// routes from designated borders.
func (w *World) buildLocalRoutes(sw model.SwitchID) []fabric.Route {
	return w.buildLocalRoutesSeq(sw, w.localVer[sw])
}

func (w *World) buildLocalRoutesSeq(sw model.SwitchID, seq uint64) []fabric.Route {
	info := w.Topo.Switch[sw]
	if !w.SwUp[sw] {
		return nil
	}
	epoch := w.Epoch[sw]
	var routes []fabric.Route

	// 1. Member route per VN, tagged with this switch's redundancy groups.
	for _, vni := range info.VNs {
		tags := append([]model.GroupID(nil), w.idx.groupTags[sw][vni]...)
		sort.Slice(tags, func(i, j int) bool { return tags[i] < tags[j] })
		routes = append(routes, fabric.NewMemberRoute(*info, vni, tags, w.PortsOpen[sw], seq, epoch))
	}

	// 2. Host routes for local single-homed devices and for external hosts a
	// border has resolved. The external host route is /32 and therefore more
	// specific than the covering prefix route.
	for _, d := range w.idx.devicesAt[sw] {
		switch {
		case d.External && d.ExternalKnown:
			routes = append(routes, fabric.NewHostRoute(*info, d.VN, d.MAC, d.IP, w.DeviceSeq[d.ID], epoch))
		case d.External:
			continue
		case d.Kind == model.AttachSingle:
			routes = append(routes, fabric.NewHostRoute(*info, d.VN, d.MAC, d.IP, w.DeviceSeq[d.ID], epoch))
		}
	}

	// 3. One group route per active redundancy group. Both members advertise
	// the same device set under their own RD, tagged with the group ID.
	var gids []model.GroupID
	for gid, g := range w.Topo.Groups {
		if !g.HasMember(sw) || !w.AccessUp[gid][sw] || !info.InVirtualNetwork(g.VN) {
			continue
		}
		gids = append(gids, gid)
	}
	sort.Slice(gids, func(i, j int) bool { return gids[i] < gids[j] })
	for _, gid := range gids {
		g := w.Topo.Groups[gid]
		var entries []fabric.HostEntry
		for _, d := range w.idx.groupDevices[gid] {
			entries = append(entries, fabric.HostEntry{MAC: d.MAC, IP: d.IP, Seq: w.DeviceSeq[d.ID]})
		}
		sort.Slice(entries, func(i, j int) bool { return entries[i].MAC < entries[j].MAC })
		routes = append(routes, fabric.NewGroupRoute(*info, g.VN, gid, entries, w.GroupSeq[gid], epoch))
	}

	// 4. External segment prefixes from the designated border switches.
	for _, vn := range w.Topo.VNs {
		if !info.InVirtualNetwork(vn.VNI) {
			continue
		}
		for _, border := range vn.ExternalOwners {
			if border != sw {
				continue
			}
			for _, p := range vn.ExternalPrefixes {
				routes = append(routes, fabric.NewPrefixRoute(*info, vn.VNI, p, 1, epoch))
			}
		}
	}
	return routes
}

// step performs one control-plane round. It reports whether any route, tunnel,
// port or session state changed.
func (w *World) step() bool {
	changed := false

	// Node up/down changes the underlay graph.
	for _, id := range w.Topo.SwitchOrder {
		w.Underlay.SetNodeUp(id, w.SwUp[id])
	}
	// A physical topology change alters every tunnel's underlay path, so
	// every switch's forwarding table must be rebuilt this round.
	allDirty := w.graphDirty
	w.graphDirty = false

	// Every switch recomputes its local routes in parallel; results are
	// merged in switch order so the simulation stays deterministic.
	routesBySwitch := w.buildAllLocalRoutes()
	var senders []model.SwitchID
	pending := map[model.SwitchID][]fabric.Announcement{}
	for _, id := range w.Topo.SwitchOrder {
		routes, ok := routesBySwitch[id]
		if !ok {
			continue
		}
		// A new route version is issued only when the content actually
		// changes; otherwise the control plane would never quiesce.
		if prev, ok := w.localRoutes[id]; ok && !routesEqualIgnoringSeq(prev, routes) {
			w.localVer[id]++
			routes = w.buildLocalRoutesSeq(id, w.localVer[id])
		}
		w.localRoutes[id] = routes
		anns := w.RIB[id].SetLocal(id, routes)
		if len(anns) > 0 {
			pending[id] = anns
			senders = append(senders, id)
			changed = true
			w.localVerDirty[id] = true
			for _, a := range anns {
				if a.Withdraw {
					w.Withdrawals++
					if a.Route.Type == fabric.RouteGroup {
						w.GroupWithdrawals++
					}
				} else {
					w.RoutesSent++
				}
			}
		}
	}

	reach := w.computeReachability()

	// Newly reachable peers establish a session: everything the peer still
	// holds from that origin is aged out and the origin's full table is sent
	// by the session pass below. Doing it only there avoids sending the same
	// table twice on initial convergence.
	for _, origin := range w.Topo.SwitchOrder {
		if !w.SwUp[origin] {
			continue
		}
		for _, peer := range w.Topo.SwitchOrder {
			if peer == origin || !w.SwUp[peer] || !reach[origin][peer] {
				continue
			}
			if !w.reachPrev[origin][peer] {
				w.RIB[peer].MarkOriginStale(origin, w.Round, w.Times.StaleTTL)
				changed = true
			}
		}
	}

	// Deliver queued announcements to reachable peers. A peer whose
	// forwarding-relevant state changed gets its FIB rebuilt.
	dirty := map[model.SwitchID]bool{}
	for id := range pending {
		dirty[id] = true
	}
	if allDirty {
		for _, id := range w.Topo.SwitchOrder {
			dirty[id] = true
		}
	}
	// Delivery is parallelised by receiving switch: each peer's RIB is
	// written by exactly one worker, and the work inside a worker is applied
	// in sender order, so the outcome is deterministic.
	delivered := w.deliverInParallel(senders, pending, reach)
	for _, to := range w.Topo.SwitchOrder {
		if delivered[to] {
			changed = true
			dirty[to] = true
		}
	}
	// Full-table sessions deliver independently of `senders`: a new session
	// (newly reachable peer) and a peer whose epoch just advanced both require
	// the origin's complete local table. The tables are snapshotted once, not
	// once per ordered pair.
	tables := make(map[model.SwitchID][]fabric.Announcement, len(w.Topo.SwitchOrder))
	for _, from := range w.Topo.SwitchOrder {
		if !w.SwUp[from] || len(w.RIB[from].Local) == 0 {
			continue
		}
		tables[from] = localAnnouncements(w.RIB[from])
	}
	full := w.deliverFullTables(tables, reach)
	for _, to := range w.Topo.SwitchOrder {
		if full[to] {
			changed = true
			dirty[to] = true
		}
	}

	// Routes from an origin that is down or partitioned away become stale at
	// once and are deleted when the TTL expires.
	for _, id := range w.Topo.SwitchOrder {
		if !w.SwUp[id] {
			continue
		}
		rib := w.RIB[id]
		if allDirty {
			dirty[id] = true
		}
		for _, rec := range rib.Received {
			origin := rec.Route.Origin
			if rec.StaleUntil != 0 {
				continue
			}
			if !w.SwUp[origin] || !reach[origin][id] {
				rib.MarkOriginStale(origin, w.Round, w.Times.StaleTTL)
				changed = true
				dirty[id] = true
			}
		}
		if rib.Expire(w.Round) {
			changed = true
			dirty[id] = true
			w.StaleExpired++
		}
	}

	w.reachPrev = reach
	if w.rebuildFIBs(dirty) {
		changed = true
	}
	// Ports open only after the control plane has gone quiet: a switch that
	// just restarted must first converge, then pass one full round with no
	// route change at all. Any pre-restart route still in the RIB (stale,
	// awaiting its TTL) keeps the ports shut.
	if !changed {
		if w.openPorts() {
			changed = true
		}
	}
	return changed
}

func routesEqualIgnoringSeq(a, b []fabric.Route) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		x, y := a[i], b[i]
		x.Seq, y.Seq = 0, 0
		x.Stale, y.Stale = false, false
		if !fabric.SameRoute(x, y) {
			return false
		}
	}
	return true
}

// buildAllLocalRoutes computes every up switch's route set concurrently. Each
// switch writes only its own entry, so the result is identical to a serial
// loop.
func (w *World) buildAllLocalRoutes() map[model.SwitchID][]fabric.Route {
	ids := make([]model.SwitchID, 0, len(w.Topo.SwitchOrder))
	for _, id := range w.Topo.SwitchOrder {
		if w.SwUp[id] {
			ids = append(ids, id)
		}
	}
	out := make(map[model.SwitchID][]fabric.Route, len(ids))
	if len(ids) < 8 {
		for _, id := range ids {
			out[id] = w.buildLocalRoutes(id)
		}
		return out
	}
	var (
		mu   sync.Mutex
		wg   sync.WaitGroup
		next int
	)
	workers := runtime.GOMAXPROCS(0)
	if workers > len(ids) {
		workers = len(ids)
	}
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				mu.Lock()
				if next >= len(ids) {
					mu.Unlock()
					return
				}
				id := ids[next]
				next++
				mu.Unlock()
				routes := w.buildLocalRoutes(id)
				mu.Lock()
				out[id] = routes
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	return out
}

func localAnnouncements(rib *fabric.RIB) []fabric.Announcement {
	keys := make([]string, 0, len(rib.Local))
	for k := range rib.Local {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]fabric.Announcement, 0, len(keys))
	for _, k := range keys {
		out = append(out, fabric.Announcement{Route: rib.Local[k]})
	}
	return out
}

// deliverInParallel fans announcements out to every reachable peer, one
// worker per receiving switch. Returns the set of switches whose RIB changed.
func (w *World) deliverInParallel(senders []model.SwitchID, pending map[model.SwitchID][]fabric.Announcement, reach map[model.SwitchID]map[model.SwitchID]bool) map[model.SwitchID]bool {
	out := map[model.SwitchID]bool{}
	if len(senders) == 0 {
		return out
	}
	type job struct {
		to    model.SwitchID
		apply func() bool
	}
	var jobs []job
	for _, to := range w.Topo.SwitchOrder {
		if !w.SwUp[to] {
			continue
		}
		var froms []model.SwitchID
		for _, from := range senders {
			if from != to && reach[from][to] && len(pending[from]) > 0 {
				froms = append(froms, from)
			}
		}
		if len(froms) == 0 {
			continue
		}
		fromsCopy := froms
		toCopy := to
		jobs = append(jobs, job{to: toCopy, apply: func() bool {
			changedLocal := false
			for _, from := range fromsCopy {
				for _, a := range pending[from] {
					if w.RIB[toCopy].Receive(a, from, w.Round, w.Times.StaleTTL) {
						changedLocal = true
					}
				}
			}
			return changedLocal
		}})
	}
	results := make([]bool, len(jobs))
	w.runJobs(len(jobs), func(i int) { results[i] = jobs[i].apply() })
	for i, changedLocal := range results {
		if changedLocal {
			out[jobs[i].to] = true
		}
	}
	return out
}

// deliverFullTables sends each origin's complete local table to peers that
// need it (new session or epoch refresh).
func (w *World) deliverFullTables(tables map[model.SwitchID][]fabric.Announcement, reach map[model.SwitchID]map[model.SwitchID]bool) map[model.SwitchID]bool {
	out := map[model.SwitchID]bool{}
	// One unit of work per receiving switch, so all writes to a RIB happen on
	// one goroutine.
	perSwitch := map[model.SwitchID][]model.SwitchID{}
	var order []model.SwitchID
	for _, to := range w.Topo.SwitchOrder {
		if !w.SwUp[to] {
			continue
		}
		for _, from := range w.Topo.SwitchOrder {
			if from == to || !w.SwUp[from] || tables[from] == nil || !reach[from][to] {
				continue
			}
			refresh := w.RIB[to].RefreshNeeded[from]
			if w.reachPrev[from][to] && !refresh {
				continue
			}
			perSwitch[to] = append(perSwitch[to], from)
			if refresh {
				delete(w.RIB[to].RefreshNeeded, from)
				out[to] = true
			}
		}
		if len(perSwitch[to]) > 0 {
			order = append(order, to)
		}
	}
	results := make([]bool, len(order))
	w.runJobs(len(order), func(i int) {
		to := order[i]
		changedLocal := false
		for _, from := range perSwitch[to] {
			for _, a := range tables[from] {
				if w.RIB[to].Receive(a, from, w.Round, w.Times.StaleTTL) {
					changedLocal = true
				}
			}
		}
		results[i] = changedLocal
	})
	for i, changedLocal := range results {
		if changedLocal {
			out[order[i]] = true
		}
	}
	return out
}

// runJobs runs n independent jobs with a bounded worker pool.
func (w *World) runJobs(n int, run func(i int)) {
	if n <= 1 {
		for i := 0; i < n; i++ {
			run(i)
		}
		return
	}
	var (
		mu   sync.Mutex
		wg   sync.WaitGroup
		next int
	)
	workers := runtime.GOMAXPROCS(0)
	if workers > n {
		workers = n
	}
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				mu.Lock()
				if next >= n {
					mu.Unlock()
					return
				}
				i := next
				next++
				mu.Unlock()
				run(i)
			}
		}()
	}
	wg.Wait()
}

func (w *World) computeReachability() map[model.SwitchID]map[model.SwitchID]bool {
	out := map[model.SwitchID]map[model.SwitchID]bool{}
	for _, a := range w.Topo.SwitchOrder {
		if !w.SwUp[a] {
			continue
		}
		m := map[model.SwitchID]bool{}
		for _, b := range w.Topo.SwitchOrder {
			if w.SwUp[b] && w.Underlay.Reachable(a, b) {
				m[b] = true
			}
		}
		out[a] = m
	}
	return out
}

// Converge runs rounds until nothing changes. It returns the number of rounds.
func (w *World) Converge() int {
	rounds := 0
	for {
		w.Round++
		rounds++
		changed := w.step()
		if !changed {
			break
		}
		if rounds > 256 {
			panic(fmt.Sprintf("control plane failed to converge after %d rounds", rounds))
		}
	}
	w.TotalRounds += rounds
	w.ConvergenceRuns++
	w.LastRounds = rounds
	w.AvgRounds = float64(w.TotalRounds) / float64(w.ConvergenceRuns)
	return rounds
}

// rebuildFIBs derives forwarding tables from the RIBs and reports whether the
// result differs from the previous round. Switches whose RIB or underlay state
// did not change are skipped; a link change dirties every switch.
// RebuildAllFIBs forces a full rebuild, e.g. after a batch of mutations.
func (w *World) RebuildAllFIBs() {
	all := map[model.SwitchID]bool{}
	for _, id := range w.Topo.SwitchOrder {
		all[id] = true
	}
	w.rebuildFIBs(all)
}

func (w *World) rebuildFIBs(dirty map[model.SwitchID]bool) bool {
	changed := false
	for _, id := range w.Topo.SwitchOrder {
		if !dirty[id] && w.FIB[id] != nil {
			continue
		}
		next := w.buildSwitchFIB(id)
		if !vnFIBsEqual(w.FIB[id], next) {
			w.FIB[id] = next
			changed = true
		}
	}
	return changed
}

func vnFIBsEqual(a, b map[model.VNI]*fabric.VNFIB) bool {
	if len(a) != len(b) {
		return false
	}
	for vni, af := range a {
		bf := b[vni]
		if bf == nil || !vnFIBEqual(af, bf) {
			return false
		}
	}
	return true
}

func fibsEqual(a, b map[model.SwitchID]map[model.VNI]*fabric.VNFIB) bool {
	if len(a) != len(b) {
		return false
	}
	for sw, av := range a {
		bv, ok := b[sw]
		if !ok || len(av) != len(bv) {
			return false
		}
		for vni, af := range av {
			bf := bv[vni]
			if bf == nil || !vnFIBEqual(af, bf) {
				return false
			}
		}
	}
	return true
}

func vnFIBEqual(a, b *fabric.VNFIB) bool {
	if len(a.Hosts) != len(b.Hosts) || len(a.Tunnels) != len(b.Tunnels) || len(a.Prefixes) != len(b.Prefixes) {
		return false
	}
	for mac, ah := range a.Hosts {
		bh, ok := b.Hosts[mac]
		if !ok || ah.Origin != bh.Origin || ah.Egress != bh.Egress || ah.Seq != bh.Seq || ah.Type != bh.Type {
			return false
		}
	}
	for remote, at := range a.Tunnels {
		bt, ok := b.Tunnels[remote]
		if !ok || at.Up != bt.Up || at.NextHop != bt.NextHop || !samePath(at.Path, bt.Path) {
			return false
		}
	}
	for i := range a.Prefixes {
		if a.Prefixes[i] != b.Prefixes[i] {
			return false
		}
	}
	return true
}

func samePath(a, b []model.SwitchID) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// buildSwitchFIB rebuilds one switch's forwarding table from scratch. The
// table is replaced, never merged: a prefix or host binding that is no longer
// backed by an installed route must disappear, otherwise a stale /32 would
// keep shadowing a covering prefix after the device moved away.
func (w *World) buildSwitchFIB(sw model.SwitchID) map[model.VNI]*fabric.VNFIB {
	out := map[model.VNI]*fabric.VNFIB{}
	if !w.SwUp[sw] {
		return out
	}
	info := w.Topo.Switch[sw]
	for _, vni := range info.VNs {
		f := fabric.NewVNFIB(vni)
		f.SetLocalSwitch(sw)
		out[vni] = f
	}
	if !w.PortsOpen[sw] {
		// Ports are held closed until the control plane converges, so the
		// data plane forwards nothing at all.
		return out
	}
	routes := w.RIB[sw].ActiveRoutes()

	// 1. Tunnels come from remote member routes whose ports are open. They
	//    disappear when the member route is withdrawn.
	for _, r := range routes {
		if r.Type != fabric.RouteMember || r.Origin == sw || !r.Ready {
			continue
		}
		f := out[r.VNI]
		if f == nil {
			continue
		}
		path := w.Underlay.Path(sw, r.Origin)
		if path == nil {
			continue
		}
		t := &fabric.Tunnel{Local: sw, Remote: r.Origin, VNI: r.VNI, Path: path, Up: true, Ready: true}
		if len(path) > 1 {
			t.NextHop = path[1]
		}
		f.Tunnels[r.Origin] = t
	}

	// 2. Install every accepted route. Local routes win over remote copies of
	//    the same binding by construction.
	local := w.localDeviceIndex(sw)
	for _, r := range routes {
		f := out[r.VNI]
		if f == nil {
			continue
		}
		if r.Origin == sw {
			w.installLocal(f, sw, r)
		} else {
			w.installRoute(f, sw, r)
		}
	}

	// 3. Resolve each installed binding to a concrete egress. Group and prefix
	//    entries see all candidates here, so a failover is decided once, not
	//    per route.
	for _, f := range out {
		w.resolveEgress(f, sw, local)
	}
	return out
}

// resolveEgress assigns a tunnel endpoint to every remote host binding and
// external prefix. The rules are:
//   - host routes go to their origin;
//   - group entries whose member switch also has an access port into the group
//     stay local (shared segment, and the split-horizon path); otherwise they
//     go to the lowest-id reachable member that still advertises the group;
//   - prefix routes go to the lowest-id reachable border that advertises them.
//
// installRoute places one remote route into a forwarding table.
func (w *World) installRoute(f *fabric.VNFIB, sw model.SwitchID, r fabric.Route) {
	switch r.Type {
	case fabric.RouteHost:
		f.InstallHost(&fabric.HostFIB{
			MAC: r.MAC, IP: r.IP, Type: r.Type, Source: fabric.SourceEVPN,
			Origin: r.Origin, Seq: r.Seq, Epoch: r.Epoch,
			External: w.deviceIsExternal(r.MAC),
		})
	case fabric.RouteGroup:
		// A member switch with an up access link already has the whole group
		// installed locally; importing the peer's copy would point the
		// segment's own traffic back into the tunnel.
		if g := w.Topo.Groups[r.Group]; g != nil && g.HasMember(sw) && w.AccessUp[r.Group][sw] {
			return
		}
		// Both members advertise the whole device set. Remember every
		// advertiser so a link failure can move the group in one step.
		f.AddGroupOrigin(r.Group, r.Origin)
		for _, e := range r.Devices {
			h := &fabric.HostFIB{
				MAC: e.MAC, IP: e.IP, Type: r.Type, Source: fabric.SourceEVPN,
				Origin: r.Origin, Group: r.Group, Seq: e.Seq, Epoch: r.Epoch,
			}
			if existing := f.Hosts[e.MAC]; existing != nil {
				h.GroupOrgs = appendUniqueSwitch(append([]model.SwitchID(nil), existing.GroupOrgs...), r.Origin)
			} else {
				h.GroupOrgs = []model.SwitchID{r.Origin}
			}
			f.InstallHost(h)
		}
	case fabric.RoutePrefix:
		f.PrefixCandidates = append(f.PrefixCandidates, fabric.PrefixCandidate{Prefix: r.Prefix, Origin: r.Origin})
	}
}

// installLocal places one locally-originated route into a forwarding table.
func (w *World) installLocal(f *fabric.VNFIB, sw model.SwitchID, r fabric.Route) {
	eg := fabric.Egress{Switch: sw, Local: true}
	switch r.Type {
	case fabric.RouteHost:
		h := &fabric.HostFIB{
			MAC: r.MAC, IP: r.IP, Type: r.Type, Source: fabric.SourceLocalAccess,
			Origin: sw, Seq: r.Seq, Epoch: r.Epoch, Egress: eg, Local: true,
		}
		if w.deviceIsExternal(r.MAC) {
			h.External = true
		}
		f.InstallHost(h)
	case fabric.RouteGroup:
		f.MarkGroupLocal(r.Group)
		for _, e := range r.Devices {
			f.InstallHost(&fabric.HostFIB{
				MAC: e.MAC, IP: e.IP, Type: r.Type, Source: fabric.SourceLocalGroup,
				Origin: sw, Group: r.Group, Seq: e.Seq, Epoch: r.Epoch,
				Egress: eg, Local: true, GroupOrgs: []model.SwitchID{sw},
			})
		}
	case fabric.RoutePrefix:
		f.PrefixCandidates = append(f.PrefixCandidates, fabric.PrefixCandidate{Prefix: r.Prefix, Origin: sw})
	}
}

func (w *World) resolveEgress(f *fabric.VNFIB, sw model.SwitchID, local map[string]bool) {
	for mac, h := range f.Hosts {
		if h.Local || h.Egress.Switch != "" {
			continue
		}
		if local[mac] {
			continue
		}
		switch h.Type {
		case fabric.RouteHost:
			if f.Tunnels[h.Origin] != nil {
				h.Egress = fabric.Egress{Switch: h.Origin, NextHop: h.Origin}
			}
		case fabric.RouteGroup:
			if f.GroupIsLocal(h.Group) {
				continue // local install already present for this group
			}
			best := w.bestTunnelOrigin(f, f.GroupOrigins(h.Group))
			if best != "" {
				h.Egress = fabric.Egress{Switch: best, NextHop: best}
			}
		}
	}
	// Prefix routes: the local border always delivers its own external
	// segment; otherwise the lowest-id reachable border is used, which is the
	// same tie-break every switch applies.
	byPrefix := map[string][]model.SwitchID{}
	for _, c := range f.PrefixCandidates {
		key := c.Prefix.Masked().String()
		byPrefix[key] = append(byPrefix[key], c.Origin)
	}
	for key, origins := range byPrefix {
		p := netip.MustParsePrefix(key)
		target := w.bestTunnelOrigin(f, origins)
		for _, o := range origins {
			if o == sw {
				target = sw
				break
			}
		}
		if target == "" {
			continue
		}
		eg := fabric.Egress{Switch: target, NextHop: target}
		if target == sw {
			eg = fabric.Egress{Switch: sw, Local: true}
		}
		f.SetPrefix(fabric.PrefixFIB{Prefix: p, Origin: target, Egress: eg})
		f.SetPrefixTarget(p, target)
	}
}

// bestTunnelOrigin returns the lowest-id origin with a live tunnel, which is
// the deterministic tie-break every switch in the fabric applies.
func (w *World) bestTunnelOrigin(f *fabric.VNFIB, origins []model.SwitchID) model.SwitchID {
	best := model.SwitchID("")
	for _, o := range origins {
		if f.Tunnels[o] == nil {
			continue
		}
		if best == "" || o < best {
			best = o
		}
	}
	return best
}

func appendUniqueSwitch(in []model.SwitchID, id model.SwitchID) []model.SwitchID {
	for _, x := range in {
		if x == id {
			return in
		}
	}
	return append(in, id)
}

func (w *World) localDeviceIndex(sw model.SwitchID) map[string]bool {
	out := map[string]bool{}
	for _, d := range w.deliverableDevices(sw) {
		out[d.MAC] = true
	}
	for _, d := range w.idx.devicesAt[sw] {
		if d.External && d.ExternalKnown {
			out[d.MAC] = true
		}
	}
	return out
}

func (w *World) deviceByMAC(mac string) *model.Device {
	return w.macIndex[mac]
}

func (w *World) deviceIsExternal(mac string) bool {
	d := w.macIndex[mac]
	return d != nil && d.External
}

// openPorts opens a switch's ports once its routes have converged: no stale
// route remains and a member route has arrived from every reachable VN peer.
// The caller only invokes this on a quiet round, so the switch always waits
// for convergence before it starts forwarding.
func (w *World) openPorts() bool {
	opened := false
	for _, id := range w.Topo.SwitchOrder {
		if !w.SwUp[id] || w.PortsOpen[id] {
			continue
		}
		if w.bootRound[id] != 0 && w.Round-w.bootRound[id] < 2 {
			continue
		}
		if w.ribStable(id) {
			w.PortsOpen[id] = true
			opened = true
			w.log(EventSwitchUp, Event{Switch: id, Detail: "ports opened after route convergence"})
		}
	}
	return opened
}

func (w *World) ribStable(sw model.SwitchID) bool {
	rib := w.RIB[sw]
	if rib.StaleCount() > 0 {
		return false
	}
	info := w.Topo.Switch[sw]
	for _, vni := range info.VNs {
		for _, peer := range w.Topo.SwitchOrder {
			if peer == sw || !w.SwUp[peer] || !w.Topo.Switch[peer].InVirtualNetwork(vni) {
				continue
			}
			if !w.Underlay.Reachable(peer, sw) {
				continue
			}
			rec, ok := rib.Received[fabric.MemberKey(vni, peer)]
			if !ok || !rib.Policy.Accept(rec.Route) {
				return false
			}
		}
	}
	return true
}

// deliverableDevices lists devices a switch can deliver to on a local access
// port. Group devices are on the shared segment of both members; external
// hosts are L3-only and never receive L2 broadcasts.
func (w *World) deliverableDevices(sw model.SwitchID) []*model.Device {
	var out []*model.Device
	for _, d := range w.idx.devicesAt[sw] {
		if d.External {
			continue
		}
		out = append(out, d)
	}
	for gid, g := range w.Topo.Groups {
		if !g.HasMember(sw) || !w.AccessUp[gid][sw] {
			continue
		}
		out = append(out, w.idx.groupDevices[gid]...)
	}
	return out
}
