package fabric

import (
	"sort"
	"strings"
	"sync"

	"circusnet/internal/model"
)

// Announcement is one route update sent by a switch to a control-plane peer.
// Withdraw removes the route identified by Route.Key().
type Announcement struct {
	Route    Route
	Withdraw bool
}

// ImportPolicy is derived from a switch's VN membership. A received route is
// imported only when its VNI is local and its VN route target matches a local
// target.
type ImportPolicy struct {
	VNs map[model.VNI]bool
}

func NewImportPolicy(vns []model.VNI) ImportPolicy {
	p := ImportPolicy{VNs: make(map[model.VNI]bool, len(vns))}
	for _, v := range vns {
		p.VNs[v] = true
	}
	return p
}

// Accept implements route-target import: only routes for a locally
// configured VN are eligible. The RT carried on the route is what identifies
// the VN, so a route whose RT does not match any local target is dropped.
func (p ImportPolicy) Accept(r Route) bool {
	if !p.VNs[r.VNI] {
		return false
	}
	// The first route target of a route is always its VN target (see RTs);
	// checking it directly avoids parsing on the hot path.
	if vni, ok := ParseVNRT(r.RTs[0]); ok {
		return p.VNs[vni]
	}
	for _, rt := range r.RTs[1:] {
		if vni, ok := ParseVNRT(rt); ok && p.VNs[vni] {
			return true
		}
	}
	return false
}

// ParseVNRT extracts the VN number from a "target:<vni>" route target.
func ParseVNRT(rt string) (model.VNI, bool) {
	const prefix = "target:"
	if !strings.HasPrefix(rt, prefix) {
		return 0, false
	}
	rest := rt[len(prefix):]
	// Origin-specific targets look like target:<switch>:<vni>; they are not
	// import targets.
	if strings.Contains(rest, ":") {
		return 0, false
	}
	var v uint64
	for i := 0; i < len(rest); i++ {
		c := rest[i]
		if c < '0' || c > '9' {
			return 0, false
		}
		v = v*10 + uint64(c-'0')
	}
	return model.VNI(v), true
}

// Received is a route held in the Adj-RIB-In.
type Received struct {
	Route      Route
	Sources    map[model.SwitchID]bool
	ReceivedAt int
	// StaleUntil is the round at which a route from an older origin epoch is
	// deleted. A stale route is never used for forwarding.
	StaleUntil int
}

// Active reports whether the route may be used for forwarding. Routes from a
// previous origin epoch are retained for the TTL but excluded immediately.
func (r Received) Active() bool { return r.StaleUntil == 0 }

type tombstone struct {
	Version Route
	Until   int
}

// RIB is a per-switch routing information base: locally originated routes,
// routes accepted from peers, import policy, stale-route timers and
// withdrawal tombstones.
type RIB struct {
	Local      map[string]Route
	Received   map[string]Received
	Tombstones map[string]tombstone
	Policy     ImportPolicy
	KnownEpoch map[model.SwitchID]uint64
	// RefreshNeeded marks origins whose routes were invalidated by a newer
	// epoch. The control plane pulls their full table again.
	RefreshNeeded map[model.SwitchID]bool

	Rejected   int
	Expired    int
	StaleMarks int
}

func NewRIB(vns []model.VNI) *RIB {
	return &RIB{
		Local:         map[string]Route{},
		Received:      map[string]Received{},
		Tombstones:    map[string]tombstone{},
		Policy:        NewImportPolicy(vns),
		KnownEpoch:    map[model.SwitchID]uint64{},
		RefreshNeeded: map[model.SwitchID]bool{},
	}
}

// SetPolicy changes the import policy. The Adj-RIB-In is untouched: leaving a
// VN only stops the routes being imported, and re-joining re-imports the
// routes already learned.
func (r *RIB) SetPolicy(vns []model.VNI) { r.Policy = NewImportPolicy(vns) }

// ClearReceived forgets everything learned from peers.
func (r *RIB) ClearReceived() {
	r.Received = map[string]Received{}
	r.KnownEpoch = map[model.SwitchID]uint64{}
}

// SetLocal replaces the set of locally originated routes and returns the
// announcements expressing the difference.
func (r *RIB) SetLocal(sw model.SwitchID, routes []Route) []Announcement {
	next := make(map[string]Route, len(routes))
	for _, route := range routes {
		if route.Origin != sw {
			continue
		}
		next[route.Key()] = route
	}
	var out []Announcement
	for k, old := range r.Local {
		if _, ok := next[k]; !ok {
			out = append(out, Announcement{Route: old, Withdraw: true})
		}
	}
	for k, route := range next {
		old, ok := r.Local[k]
		if !ok || !SameRoute(old, route) {
			out = append(out, Announcement{Route: route})
		}
	}
	r.Local = next
	sortAnnouncements(out)
	return out
}

// SameRoute reports content equality, excluding bookkeeping fields.
func SameRoute(a, b Route) bool {
	if a.Type != b.Type || a.VNI != b.VNI || a.Origin != b.Origin ||
		a.Epoch != b.Epoch || a.Seq != b.Seq || a.MAC != b.MAC ||
		a.IP != b.IP || a.Prefix != b.Prefix || a.Group != b.Group ||
		a.Ready != b.Ready ||
		len(a.Devices) != len(b.Devices) || len(a.Groups) != len(b.Groups) {
		return false
	}
	for i := range a.Devices {
		if a.Devices[i] != b.Devices[i] {
			return false
		}
	}
	for i := range a.Groups {
		if a.Groups[i] != b.Groups[i] {
			return false
		}
	}
	return true
}

func sortAnnouncements(in []Announcement) {
	if len(in) < 2 {
		return
	}
	sort.Slice(in, func(i, j int) bool {
		if in[i].Route.Key() != in[j].Route.Key() {
			return in[i].Route.Key() < in[j].Route.Key()
		}
		return !in[i].Withdraw && in[j].Withdraw
	})
}

func versionLess(a, b Route) bool {
	if a.Epoch != b.Epoch {
		return a.Epoch < b.Epoch
	}
	return a.Seq < b.Seq
}

// Receive applies one announcement from a neighbour and stores it in the
// Adj-RIB-In. Import policy is applied when the forwarding table is built, so
// a route that does not match a local target today can still be imported after
// the switch joins a VN, without waiting for a re-advertisement.
func (r *RIB) Receive(a Announcement, from model.SwitchID, round, ttl int) bool {
	key := a.Route.Key()
	if a.Withdraw {
		if cur, ok := r.Received[key]; ok {
			if versionLess(cur.Route, a.Route) {
				return false
			}
			delete(r.Received, key)
			r.Tombstones[key] = tombstone{Version: a.Route, Until: round + ttl}
			return true
		}
		if old, ok := r.Tombstones[key]; !ok || versionLess(old.Version, a.Route) {
			r.Tombstones[key] = tombstone{Version: a.Route, Until: round + ttl}
		}
		return false
	}

	route := a.Route
	if tomb, ok := r.Tombstones[key]; ok && !versionLess(tomb.Version, route) {
		return false
	}

	known := r.KnownEpoch[route.Origin]
	if route.Epoch > known {
		if known != 0 || len(r.Received) > 0 {
			r.markOriginStale(route.Origin, route.Epoch, round, ttl)
		}
		r.KnownEpoch[route.Origin] = route.Epoch
	} else if route.Epoch < known {
		if _, ok := r.Received[key]; !ok {
			r.Received[key] = Received{
				Route:      route,
				Sources:    map[model.SwitchID]bool{from: true},
				ReceivedAt: round,
				StaleUntil: round + ttl,
			}
			r.StaleMarks++
		}
		return false
	}

	cur, ok := r.Received[key]
	if ok && SameRoute(cur.Route, route) {
		if cur.Sources == nil {
			cur.Sources = map[model.SwitchID]bool{}
		}
		cur.Sources[from] = true
		if cur.StaleUntil != 0 {
			cur.StaleUntil = 0
			r.Received[key] = cur
			return true
		}
		r.Received[key] = cur
		return false
	}
	r.Received[key] = Received{
		Route:      route,
		Sources:    map[model.SwitchID]bool{from: true},
		ReceivedAt: round,
	}
	return true
}

func (r *RIB) markOriginStale(origin model.SwitchID, newEpoch uint64, round, ttl int) {
	r.RefreshNeeded[origin] = true
	for k, rec := range r.Received {
		if rec.Route.Origin != origin || rec.Route.Epoch >= newEpoch || rec.StaleUntil != 0 {
			continue
		}
		rec.StaleUntil = round + ttl
		r.Received[k] = rec
		r.StaleMarks++
	}
}

// MarkOriginStale is used when a peer goes down: its routes are retained for
// the TTL but excluded from forwarding at once.
func (r *RIB) MarkOriginStale(origin model.SwitchID, round, ttl int) {
	r.RefreshNeeded[origin] = true
	for k, rec := range r.Received {
		if rec.Route.Origin != origin || rec.StaleUntil != 0 {
			continue
		}
		rec.StaleUntil = round + ttl
		r.Received[k] = rec
		r.StaleMarks++
	}
}

// DropOrigin removes every route learned from an origin.
func (r *RIB) DropOrigin(origin model.SwitchID) bool {
	changed := false
	for k, rec := range r.Received {
		if rec.Route.Origin == origin {
			delete(r.Received, k)
			changed = true
		}
	}
	return changed
}

// Expire removes stale routes whose TTL has elapsed.
func (r *RIB) Expire(round int) bool {
	changed := false
	for k, rec := range r.Received {
		if rec.StaleUntil != 0 && round >= rec.StaleUntil {
			delete(r.Received, k)
			r.Expired++
			changed = true
		}
	}
	for k, t := range r.Tombstones {
		if round >= t.Until {
			delete(r.Tombstones, k)
		}
	}
	return changed
}

// ActiveRoutes returns locally originated routes and the fresh received
// routes that the current import policy accepts.
func (r *RIB) ActiveRoutes() []Route {
	out := make([]Route, 0, len(r.Local)+len(r.Received))
	for _, rt := range r.Local {
		out = append(out, rt)
	}
	for _, rec := range r.Received {
		if rec.Active() && r.Policy.Accept(rec.Route) {
			out = append(out, rec.Route)
		}
	}
	return out
}

// ImportedCount counts received routes the current policy imports.
func (r *RIB) ImportedCount() int {
	n := 0
	for _, rec := range r.Received {
		if rec.Active() && r.Policy.Accept(rec.Route) {
			n++
		}
	}
	return n
}

// StaleCount counts currently retained stale routes.
func (r *RIB) StaleCount() int {
	n := 0
	for _, rec := range r.Received {
		if rec.StaleUntil != 0 {
			n++
		}
	}
	return n
}

// AllReceived returns a snapshot of received routes (including stale).
func (r *RIB) AllReceived() []Received {
	out := make([]Received, 0, len(r.Received))
	for _, rec := range r.Received {
		out = append(out, rec)
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i].Route, out[j].Route
		if a.Type != b.Type {
			return a.Type < b.Type
		}
		if a.VNI != b.VNI {
			return a.VNI < b.VNI
		}
		if a.Origin != b.Origin {
			return a.Origin < b.Origin
		}
		return a.Key() < b.Key()
	})
	return out
}

// macPool interns lower-cased MAC strings so route keys do not allocate.
var macPool sync.Map

// LowerMAC returns an interned lower-case MAC string.
func LowerMAC(mac string) string {
	if v, ok := macPool.Load(mac); ok {
		return v.(string)
	}
	lower := strings.ToLower(mac)
	macPool.Store(mac, lower)
	macPool.Store(lower, lower)
	return lower
}
