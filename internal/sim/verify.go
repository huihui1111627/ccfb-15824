package sim

import (
	"fmt"
	"net/netip"
	"runtime"
	"sync"

	"circusnet/internal/fabric"
	"circusnet/internal/model"
)

// Hop is one overlay forwarding step. A tunnel is a single logical hop: the
// switch records where it sends the frame, and the physical underlay path it
// takes is checked separately.
type Hop struct {
	Switch  model.SwitchID `json:"switch"`
	NextHop model.SwitchID `json:"nextHop,omitempty"`
	Access  bool           `json:"access,omitempty"` // delivered on a local access port
}

// ForwardResult is the outcome of forwarding one frame end to end.
type ForwardResult struct {
	OK        bool  `json:"ok"`
	Delivered bool  `json:"delivered"`
	Path      []Hop `json:"path"`
	// Broadcasts counts simulated ARP/unknown-unicast frames used to resolve
	// the destination. Host routes and ARP suppression should make this 0.
	Broadcasts int `json:"broadcasts"`
	// Note explains a failure.
	Note string `json:"note,omitempty"`
	// Egress is the final tunnel endpoint switch.
	Egress model.SwitchID `json:"egress,omitempty"`
	// Physical is the stitched underlay walk from the ingress switch to the
	// egress switch.
	Physical []model.SwitchID `json:"physical,omitempty"`
}

// Forward simulates one frame from src to dst through the current FIBs and
// compares the result with the independent truth model. It returns nil when
// the pair is not currently testable (e.g. one end is down).
func (w *World) Forward(src, dst *model.Device) *ForwardResult {
	return w.forwardWith(src, dst, nil)
}

// forwardWith lets the caller reuse one truth graph for many pairs.
func (w *World) forwardWith(src, dst *model.Device, tg *truthGraph) *ForwardResult {
	if src.VN != dst.VN {
		return w.forwardRoutedWith(src, dst, tg)
	}
	injSrc, ok := w.sourceInjection(src)
	if !ok {
		return nil
	}
	if tg == nil {
		tg = w.newTruthGraph()
	}
	wantEgress, ok := w.expectedEgress(injSrc, dst, tg)
	if !ok {
		return nil
	}
	res := &ForwardResult{}
	at := injSrc
	visited := map[model.SwitchID]bool{}
	for {
		if visited[at] {
			res.Note = fmt.Sprintf("forwarding loop at %s", at)
			return res
		}
		visited[at] = true
		if !w.operational(at) {
			res.Note = fmt.Sprintf("frame reached non-operational switch %s", at)
			return res
		}
		fib := w.FIB[at][src.VN]
		if fib == nil {
			res.Note = fmt.Sprintf("no FIB for VN %d on %s", src.VN, at)
			return res
		}
		hit, found := fib.Lookup(dst.IP)
		if !found {
			res.Broadcasts += w.countBroadcast(at, src.VN)
			res.Note = fmt.Sprintf("no route for %s on %s (would flood)", dst.IP, at)
			return res
		}
		if hit.Egress.Local {
			if at != wantEgress {
				res.Path = append(res.Path, Hop{Switch: at, Access: true})
				res.Note = fmt.Sprintf("delivered on %s but expected egress %s", at, wantEgress)
				return res
			}
			res.Delivered = true
			res.OK = true
			res.Egress = at
			res.Path = append(res.Path, Hop{Switch: at, Access: true})
			if w.finishPhysical(res, src.VN, injSrc) {
				w.checkPhysical(res, tg, injSrc, wantEgress)
			}
			return res
		}
		remote := hit.Egress.Switch
		if remote == "" {
			res.Note = fmt.Sprintf("route on %s has no tunnel endpoint", at)
			return res
		}
		tun := fib.Tunnels[remote]
		if tun == nil {
			res.Note = fmt.Sprintf("route on %s points at %s but no tunnel exists", at, remote)
			return res
		}
		if !tun.Up {
			res.Note = fmt.Sprintf("tunnel %s->%s is not ready", at, remote)
			return res
		}
		res.Path = append(res.Path, Hop{Switch: at, NextHop: remote})
		at = remote
	}
}

// finishPhysical stitches the overlay path's tunnel paths into one physical
// switch walk.
func (w *World) finishPhysical(res *ForwardResult, vni model.VNI, injSrc model.SwitchID) bool {
	cur := injSrc
	physical := []model.SwitchID{cur}
	for _, h := range res.Path {
		if h.NextHop == "" || h.NextHop == h.Switch {
			continue
		}
		tun := w.FIB[cur][vni].Tunnels[h.NextHop]
		if tun == nil {
			res.OK = false
			res.Note = fmt.Sprintf("missing tunnel %s->%s", cur, h.NextHop)
			return false
		}
		physical = append(physical, tun.Path[1:]...)
		cur = h.NextHop
	}
	res.Physical = physical
	return true
}

// checkPhysical validates the overlay path against the truth model:
//   - the last switch is the expected access switch;
//   - no switch repeats (no loops, no duplicate frames);
//   - every physical link traversed is up;
//   - the physical walk equals the truth graph's shortest path.
func (w *World) checkPhysical(res *ForwardResult, tg *truthGraph, injSrc, wantEgress model.SwitchID) {
	if res == nil || !res.OK {
		return
	}
	if res.Egress != wantEgress {
		res.OK = false
		res.Note = fmt.Sprintf("egress %s, expected %s", res.Egress, wantEgress)
		return
	}
	seen := map[model.SwitchID]bool{}
	for _, sw := range res.Physical {
		if seen[sw] {
			res.OK = false
			res.Note = fmt.Sprintf("loop: switch %s appears twice in %v", sw, res.Physical)
			return
		}
		seen[sw] = true
	}
	for i := 0; i+1 < len(res.Physical); i++ {
		key := fabric.NewLinkKey(res.Physical[i], res.Physical[i+1])
		if !w.linkUp[key] {
			res.OK = false
			res.Note = fmt.Sprintf("physical path uses down link %s-%s", res.Physical[i], res.Physical[i+1])
			return
		}
	}
	want := tg.path(injSrc, wantEgress)
	if !equalPath(res.Physical, want) {
		res.OK = false
		res.Note = fmt.Sprintf("physical path %v, truth %v", res.Physical, want)
	}
}

func equalPath(a, b []model.SwitchID) bool {
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

// forwardRouted handles the cross-VN case. Every switch has the same anycast
// gateway, so the source's own switch performs the inter-VN hop locally and
// then follows the destination VN's FIB from that same switch.
func (w *World) forwardRouted(src, dst *model.Device) *ForwardResult {
	return w.forwardRoutedWith(src, dst, nil)
}

func (w *World) forwardRoutedWith(src, dst *model.Device, tg *truthGraph) *ForwardResult {
	injSrc, ok := w.sourceInjection(src)
	if !ok {
		return nil
	}
	sw := w.Topo.Switch[injSrc]
	if sw == nil || !sw.InVirtualNetwork(src.VN) || !sw.InVirtualNetwork(dst.VN) {
		// The ingress switch lacks one of the two VNs; routing would need a
		// VN border. Not testable in this configuration.
		return nil
	}
	if tg == nil {
		tg = w.newTruthGraph()
	}
	wantEgress, ok := w.expectedEgress(injSrc, dst, tg)
	if !ok {
		return nil
	}
	res := &ForwardResult{}
	res.Path = append(res.Path, Hop{Switch: injSrc, NextHop: injSrc})
	at := injSrc
	visited := map[model.SwitchID]bool{}
	for {
		if visited[at] {
			res.Note = fmt.Sprintf("forwarding loop at %s", at)
			return res
		}
		visited[at] = true
		fib := w.FIB[at][dst.VN]
		if fib == nil {
			res.Note = fmt.Sprintf("no FIB for VN %d on %s", dst.VN, at)
			return res
		}
		hit, found := fib.Lookup(dst.IP)
		if !found {
			res.Broadcasts += w.countBroadcast(at, dst.VN)
			res.Note = fmt.Sprintf("no route for %s on %s (would flood)", dst.IP, at)
			return res
		}
		if hit.Egress.Local {
			if at != wantEgress {
				res.Path = append(res.Path, Hop{Switch: at, Access: true})
				res.Note = fmt.Sprintf("routed delivery on %s but expected egress %s", at, wantEgress)
				return res
			}
			res.Delivered = true
			res.OK = true
			res.Egress = at
			res.Path = append(res.Path, Hop{Switch: at, Access: true})
			if w.finishPhysical(res, dst.VN, injSrc) {
				w.checkPhysical(res, tg, injSrc, wantEgress)
			}
			return res
		}
		remote := hit.Egress.Switch
		tun := fib.Tunnels[remote]
		if tun == nil || !tun.Up {
			res.Note = fmt.Sprintf("no usable tunnel %s->%s for VN %d", at, remote, dst.VN)
			return res
		}
		res.Path = append(res.Path, Hop{Switch: at, NextHop: remote})
		at = remote
	}
}

// countBroadcast estimates how many switches would receive a flooded frame on
// a VN given the current membership.
func (w *World) countBroadcast(from model.SwitchID, vni model.VNI) int {
	n := 0
	for _, id := range w.Topo.SwitchOrder {
		if id == from || !w.operational(id) {
			continue
		}
		if !w.Topo.Switch[id].InVirtualNetwork(vni) {
			continue
		}
		if w.Underlay.Reachable(from, id) {
			n++
		}
	}
	w.Broadcasts += int64(n)
	return n
}

// ARPHit reports whether an ARP for ip is answered by local knowledge (a host
// route or the anycast gateway) instead of being flooded.
func (w *World) ARPHit(sw model.SwitchID, vni model.VNI, ip netip.Addr) bool {
	fib := w.FIB[sw][vni]
	if fib == nil {
		return false
	}
	_, ok := fib.Lookup(ip)
	return ok
}

// VerifyOptions selects how many device pairs to check.
type VerifyOptions struct {
	// Pairs is the number of sampled destinations per source. Zero means
	// check every ordered pair (quadratic; use only on small topologies).
	Pairs int
	Seed  int64
}

// Violation is one forwarding mismatch.
type Violation struct {
	Src       model.DeviceID `json:"src"`
	Dst       model.DeviceID `json:"dst"`
	SrcMAC    string         `json:"srcMac"`
	DstMAC    string         `json:"dstMac"`
	DstIP     string         `json:"dstIp"`
	SrcSwitch model.SwitchID `json:"srcSwitch"`
	DstEgress model.SwitchID `json:"dstEgress"`
	Result    *ForwardResult `json:"result"`
}

// VerifyReport is the result of a full data-plane check.
type VerifyReport struct {
	Checked     int         `json:"checked"`
	Skipped     int         `json:"skipped"`
	Failed      int         `json:"failed"`
	LoopErrors  int         `json:"loopErrors"`
	Broadcasts  int         `json:"broadcasts"`
	ARPRequests int         `json:"arpRequests"`
	ARPAnswered int         `json:"arpAnswered"`
	Violations  []Violation `json:"violations"`
}

// VerifyAll checks forwarding for every testable ordered device pair, or a
// deterministic sample when Pairs > 0, and audits ARP suppression.
func (w *World) VerifyAll(opt VerifyOptions) *VerifyReport {
	rep := w.VerifyAllNoCount(opt)
	return rep
}

// VerifyAllNoCount runs the same checks without touching the reporting
// counters (the stress runner keeps its own aggregates).
func (w *World) VerifyAllNoCount(opt VerifyOptions) *VerifyReport {
	rep := &VerifyReport{}
	devices := w.Topo.Devices
	type pair struct{ a, b *model.Device }
	var pairs []pair
	if opt.Pairs <= 0 {
		for _, a := range devices {
			for _, b := range devices {
				if a.ID != b.ID {
					pairs = append(pairs, pair{a, b})
				}
			}
		}
	} else {
		for _, a := range devices {
			for k := 0; k < opt.Pairs; k++ {
				idx := (int(a.ID)*7 + k*131 + int(opt.Seed)) % len(devices)
				b := devices[idx]
				if b.ID != a.ID {
					pairs = append(pairs, pair{a, b})
				}
			}
		}
	}
	tg := w.newTruthGraph()
	type outcome struct {
		checked    bool
		skipped    bool
		ok         bool
		broadcasts int
		note       string
		physical   []model.SwitchID
	}
	results := make([]outcome, len(pairs))
	runPair := func(i int) {
		src, dst := pairs[i].a, pairs[i].b
		if _, ok := w.sourceInjection(src); !ok {
			results[i] = outcome{skipped: true}
			return
		}
		res := w.forwardWith(src, dst, tg)
		if res == nil {
			results[i] = outcome{skipped: true}
			return
		}
		results[i] = outcome{
			checked: true, ok: res.OK, broadcasts: res.Broadcasts,
			note: res.Note, physical: res.Physical,
		}
	}
	if len(pairs) >= 256 {
		var (
			mu   sync.Mutex
			wg   sync.WaitGroup
			next int
		)
		workers := runtime.GOMAXPROCS(0)
		for i := 0; i < workers; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for {
					mu.Lock()
					if next >= len(pairs) {
						mu.Unlock()
						return
					}
					i := next
					next++
					mu.Unlock()
					runPair(i)
				}
			}()
		}
		wg.Wait()
	} else {
		for i := range pairs {
			runPair(i)
		}
	}
	for i, o := range results {
		if o.skipped {
			rep.Skipped++
			continue
		}
		if !o.checked {
			continue
		}
		src, dst := pairs[i].a, pairs[i].b
		injSrc, _ := w.sourceInjection(src)
		rep.Checked++
		rep.Broadcasts += o.broadcasts
		if o.broadcasts > 0 {
			rep.ARPRequests++
		}
		if !o.ok {
			rep.Failed++
			seen := map[model.SwitchID]bool{}
			for _, sw := range o.physical {
				if seen[sw] {
					rep.LoopErrors++
					break
				}
				seen[sw] = true
			}
			if len(rep.Violations) < 50 {
				wantEgress, _ := w.expectedEgress(injSrc, dst, tg)
				res := &ForwardResult{OK: false, Note: o.note, Physical: o.physical}
				rep.Violations = append(rep.Violations, Violation{
					Src: src.ID, Dst: dst.ID, SrcMAC: src.MAC, DstMAC: dst.MAC,
					DstIP: dst.IP.String(), SrcSwitch: injSrc, DstEgress: wantEgress,
					Result: res,
				})
			}
		}
	}

	// ARP suppression audit: every switch carrying a VN must resolve every
	// host it knows from its own table, without flooding.
	for _, id := range w.Topo.SwitchOrder {
		if !w.operational(id) {
			continue
		}
		for _, vni := range w.Topo.Switch[id].VNs {
			fib := w.FIB[id][vni]
			if fib == nil {
				continue
			}
			for _, h := range fib.SortedHosts() {
				if h.External {
					continue
				}
				rep.ARPRequests++
				if _, ok := fib.ByIP[h.IP]; ok {
					rep.ARPAnswered++
				}
			}
		}
	}
	w.VerifyChecks += int64(rep.Checked)
	w.VerifyFailures += int64(rep.Failed)
	w.ARPRequests += int64(rep.ARPRequests)
	w.ARPAnswered += int64(rep.ARPAnswered)
	w.Broadcasts += int64(rep.Broadcasts)
	return rep
}
