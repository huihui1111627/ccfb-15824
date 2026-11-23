package sim

import (
	"fmt"
	"net/netip"
	"sort"
	"time"

	"circusnet/internal/fabric"
	"circusnet/internal/model"
)

// Snapshot is the complete state the dashboard renders. It is produced under
// one lock so every section is mutually consistent.
type Snapshot struct {
	GeneratedAt time.Time     `json:"generatedAt"`
	Round       int           `json:"round"`
	Seed        int64         `json:"seed"`
	Topology    SnapshotTopo  `json:"topology"`
	Switches    []SwitchView  `json:"switches"`
	Groups      []GroupView   `json:"groups"`
	Timeline    []Event       `json:"timeline"`
	Stats       SnapshotStats `json:"stats"`
	VNs         []VNView      `json:"vns"`
	Scenario    string        `json:"scenario"`
}

type SnapshotTopo struct {
	Tents   int `json:"tents"`
	Links   int `json:"links"`
	Devices int `json:"devices"`
	VNs     int `json:"vns"`
	Groups  int `json:"groups"`
}

type SnapshotStats struct {
	Devices           int     `json:"devices"`
	SwitchesUp        int     `json:"switchesUp"`
	SwitchesTotal     int     `json:"switchesTotal"`
	Tunnels           int     `json:"tunnels"`
	Routes            int     `json:"routes"`
	RejectedRoutes    int64   `json:"rejectedRoutes"`
	StaleRoutes       int     `json:"staleRoutes"`
	ARPAnswered       int64   `json:"arpAnswered"`
	ARPRequests       int64   `json:"arpRequests"`
	ARPFloodAvoided   int64   `json:"arpFloodAvoided"`
	Broadcasts        int64   `json:"broadcasts"`
	BroadcastSuppress int64   `json:"broadcastSuppressed"`
	Rounds            int     `json:"rounds"`
	LastRounds        int     `json:"lastRounds"`
	Checks            int64   `json:"checks"`
	Failures          int64   `json:"failures"`
	Migrations        int     `json:"migrations"`
	GroupFailovers    int     `json:"groupFailovers"`
	AvgRounds         float64 `json:"avgRounds"`
}

type VNView struct {
	VNI              model.VNI `json:"vni"`
	Name             string    `json:"name"`
	Subnet           string    `json:"subnet"`
	Gateway          string    `json:"gateway"`
	ExternalPrefixes []string  `json:"externalPrefixes,omitempty"`
	Devices          int       `json:"devices"`
	Switches         int       `json:"switches"`
}

// SwitchView is one switch's page section.
type SwitchView struct {
	ID        model.SwitchID    `json:"id"`
	Tent      int               `json:"tent"`
	Loopback  string            `json:"loopback"`
	Up        bool              `json:"up"`
	PortsOpen bool              `json:"portsOpen"`
	Epoch     uint64            `json:"epoch"`
	VNs       []model.VNI       `json:"vns"`
	RDByVN    map[string]string `json:"rdByVn"`
	// Hosts is the MAC table per VN, with the network address and the route
	// that installed it.
	Hosts []HostView `json:"hosts"`
	// Routes is the per-route view required by the dashboard: type and source
	// for every installed route.
	Routes []RouteView `json:"routes"`
	// Tunnels is the tunnel list for this switch.
	Tunnels        []TunnelView `json:"tunnels"`
	Groups         []string     `json:"groups,omitempty"`
	StaleRoutes    int          `json:"staleRoutes"`
	RejectedRoutes int64        `json:"rejectedRoutes"`
}

// HostView is one MAC/IP binding learned by a switch.
type HostView struct {
	MAC      string             `json:"mac"`
	IP       string             `json:"ip"`
	VN       model.VNI          `json:"vn"`
	Origin   model.SwitchID     `json:"origin"`
	Local    bool               `json:"local"`
	Type     fabric.RouteType   `json:"type"`
	Source   fabric.RouteSource `json:"source"`
	Group    model.GroupID      `json:"group,omitempty"`
	Egress   string             `json:"egress"`
	External bool               `json:"external,omitempty"`
}

// RouteView shows one route's type and provenance.
type RouteView struct {
	Type        fabric.RouteType   `json:"type"`
	VN          model.VNI          `json:"vn"`
	RD          string             `json:"rd"`
	RTs         []string           `json:"rts"`
	Origin      model.SwitchID     `json:"origin"`
	Source      fabric.RouteSource `json:"source"`
	Key         string             `json:"key"`
	MAC         string             `json:"mac,omitempty"`
	IP          string             `json:"ip,omitempty"`
	Prefix      string             `json:"prefix,omitempty"`
	Group       model.GroupID      `json:"group,omitempty"`
	Seq         uint64             `json:"seq"`
	Epoch       uint64             `json:"epoch"`
	Stale       bool               `json:"stale,omitempty"`
	Ready       bool               `json:"ready,omitempty"`
	Devices     int                `json:"devices,omitempty"`
	LearnedFrom string             `json:"learnedFrom,omitempty"`
}

type TunnelView struct {
	Local   model.SwitchID   `json:"local"`
	Remote  model.SwitchID   `json:"remote"`
	VN      model.VNI        `json:"vn"`
	NextHop model.SwitchID   `json:"nextHop"`
	Path    []model.SwitchID `json:"path"`
	Up      bool             `json:"up"`
	Hops    int              `json:"hops"`
}

type GroupView struct {
	ID       model.GroupID    `json:"id"`
	VN       model.VNI        `json:"vn"`
	Members  []model.SwitchID `json:"members"`
	Devices  []string         `json:"devices"`
	AccessUp map[string]bool  `json:"accessUp"`
	Active   []model.SwitchID `json:"active"`
}

// BuildSnapshot captures the current world state for the dashboard.
func (w *World) BuildSnapshot(scenario string) *Snapshot {
	now := time.Now()
	snap := &Snapshot{
		GeneratedAt: now,
		Round:       w.Round,
		Seed:        w.Topo.Config.Seed,
		Scenario:    scenario,
	}
	snap.Topology = SnapshotTopo{
		Tents:   w.Topo.Config.Tents,
		Links:   len(w.Topo.Links),
		Devices: len(w.Topo.Devices),
		VNs:     len(w.Topo.VNs),
		Groups:  len(w.Topo.Groups),
	}
	// VN summary.
	for _, vn := range w.Topo.VNs {
		v := VNView{
			VNI: vn.VNI, Name: vn.Name,
			Subnet: vn.Subnet.String(), Gateway: vn.Gateway.String(),
		}
		for _, p := range vn.ExternalPrefixes {
			v.ExternalPrefixes = append(v.ExternalPrefixes, p.String())
		}
		for _, d := range w.Topo.Devices {
			if d.VN == vn.VNI {
				v.Devices++
			}
		}
		v.Switches = len(w.Topo.SwitchesForVN(vn.VNI))
		snap.VNs = append(snap.VNs, v)
	}

	// Switches.
	for _, id := range w.Topo.SwitchOrder {
		snap.Switches = append(snap.Switches, w.switchView(id))
	}

	// Groups.
	for gid, g := range w.Topo.Groups {
		gv := GroupView{ID: gid, VN: g.VN, Members: g.Members, AccessUp: map[string]bool{}}
		for _, m := range g.Members {
			gv.AccessUp[string(m)] = w.AccessUp[gid][m]
			if w.AccessUp[gid][m] && w.SwUp[m] {
				gv.Active = append(gv.Active, m)
			}
		}
		for _, d := range w.Topo.Devices {
			if d.Group == gid {
				gv.Devices = append(gv.Devices, d.MAC)
			}
		}
		sort.Strings(gv.Devices)
		snap.Groups = append(snap.Groups, gv)
	}
	sort.Slice(snap.Groups, func(i, j int) bool { return snap.Groups[i].ID < snap.Groups[j].ID })

	// Timeline: newest first, capped.
	events := append([]Event(nil), w.Events...)
	sort.Slice(events, func(i, j int) bool { return events[i].Seq > events[j].Seq })
	if len(events) > 300 {
		events = events[:300]
	}
	snap.Timeline = events

	snap.Stats = w.computeStats()
	return snap
}

func (w *World) switchView(id model.SwitchID) SwitchView {
	info := w.Topo.Switch[id]
	v := SwitchView{
		ID: id, Tent: info.Tent, Loopback: info.Loopback.String(),
		Up: w.SwUp[id], PortsOpen: w.PortsOpen[id], Epoch: w.Epoch[id],
		VNs: info.VNs, RDByVN: map[string]string{},
	}
	for _, vni := range info.VNs {
		v.RDByVN[vni.String()] = fabric.RD(info.Loopback, vni)
	}
	if rib := w.RIB[id]; rib != nil {
		v.StaleRoutes = rib.StaleCount()
		v.RejectedRoutes = int64(rib.Rejected)
	}
	for gid, g := range w.Topo.Groups {
		if g.HasMember(id) {
			v.Groups = append(v.Groups, string(gid))
		}
	}
	sort.Strings(v.Groups)

	if w.SwUp[id] {
		for _, vni := range info.VNs {
			f := w.FIB[id][vni]
			if f == nil {
				continue
			}
			for _, h := range f.SortedHosts() {
				eg := h.Egress.Switch.String()
				if h.Egress.Local {
					eg = "local"
				}
				v.Hosts = append(v.Hosts, HostView{
					MAC: h.MAC, IP: h.IP.String(), VN: vni,
					Origin: h.Origin, Local: h.Egress.Local, Type: h.Type, Source: h.Source,
					Group: h.Group, Egress: eg, External: h.External,
				})
			}
			for _, t := range f.SortedTunnels() {
				v.Tunnels = append(v.Tunnels, TunnelView{
					Local: id, Remote: t.Remote, VN: vni, NextHop: t.NextHop,
					Path: t.Path, Up: t.Up, Hops: len(t.Path) - 1,
				})
			}
		}
	}
	// Routes: local first, then imported, with type and source.
	if rib := w.RIB[id]; rib != nil {
		for _, r := range rib.Local {
			v.Routes = append(v.Routes, routeView(r, "local"))
		}
		for _, rec := range rib.AllReceived() {
			if !rib.Policy.Accept(rec.Route) {
				continue
			}
			from := ""
			for s := range rec.Sources {
				if from == "" || string(s) < from {
					from = string(s)
				}
			}
			rv := routeView(rec.Route, from)
			if !rec.Active() {
				rv.Stale = true
			}
			v.Routes = append(v.Routes, rv)
		}
	}
	sort.Slice(v.Routes, func(i, j int) bool {
		if v.Routes[i].Type != v.Routes[j].Type {
			return v.Routes[i].Type < v.Routes[j].Type
		}
		if v.Routes[i].VN != v.Routes[j].VN {
			return v.Routes[i].VN < v.Routes[j].VN
		}
		return v.Routes[i].Key < v.Routes[j].Key
	})
	if len(v.Routes) > 400 {
		v.Routes = v.Routes[:400]
	}
	if len(v.Hosts) > 600 {
		v.Hosts = v.Hosts[:600]
	}
	return v
}

func routeView(r fabric.Route, learnedFrom string) RouteView {
	rv := RouteView{
		Type: r.Type, VN: r.VNI, RD: r.RD, RTs: append([]string(nil), r.RTs...),
		Origin: r.Origin, Source: r.Source, Key: r.Key(), MAC: r.MAC,
		Seq: r.Seq, Epoch: r.Epoch, Ready: r.Ready, Group: r.Group,
		LearnedFrom: learnedFrom, Devices: len(r.Devices),
	}
	if r.IP.IsValid() {
		rv.IP = r.IP.String()
	}
	if r.Prefix.IsValid() {
		rv.Prefix = r.Prefix.String()
	}
	if learnedFrom != "local" {
		rv.Source = fabric.SourceEVPN
	}
	return rv
}

func (w *World) computeStats() SnapshotStats {
	st := SnapshotStats{Devices: len(w.Topo.Devices), SwitchesTotal: len(w.Topo.SwitchOrder)}
	for _, id := range w.Topo.SwitchOrder {
		if w.SwUp[id] {
			st.SwitchesUp++
			for _, vni := range w.Topo.Switch[id].VNs {
				if f := w.FIB[id][vni]; f != nil {
					st.Tunnels += len(f.Tunnels)
				}
			}
		}
		if rib := w.RIB[id]; rib != nil {
			st.Routes += len(rib.Local) + rib.ImportedCount()
			st.RejectedRoutes += int64(rib.Rejected)
			st.StaleRoutes += rib.StaleCount()
		}
	}
	st.Rounds = w.TotalRounds
	st.LastRounds = w.LastRounds
	st.AvgRounds = w.AvgRounds
	st.ARPAnswered = w.ARPAnswered
	st.ARPRequests = w.ARPRequests
	st.ARPFloodAvoided = w.ARPRequests * int64(maxInt(0, st.SwitchesUp-2))
	st.Broadcasts = w.Broadcasts
	st.BroadcastSuppress = w.BroadcastSuppressed
	st.Checks = w.VerifyChecks
	st.Failures = w.VerifyFailures
	st.Migrations = w.Migrations
	st.GroupFailovers = w.GroupFailovers
	return st
}

// SwitchSnapshot returns one switch's full page data. It is the same view the
// aggregate snapshot embeds, so the dedicated page and the overview agree.
func (w *World) SwitchSnapshot(id model.SwitchID) SwitchView { return w.switchView(id) }

// FindSwitch resolves an ID from a query string.
func (w *World) FindSwitch(id string) (model.SwitchID, bool) {
	if _, ok := w.Topo.Switch[model.SwitchID(id)]; ok {
		return model.SwitchID(id), true
	}
	return "", false
}

// QueryResult is a forwarding trace shown by the dashboard's path tool.
type QueryResult struct {
	Src    DeviceRef      `json:"src"`
	Dst    DeviceRef      `json:"dst"`
	Result *ForwardResult `json:"result"`
	Note   string         `json:"note,omitempty"`
}

type DeviceRef struct {
	ID     model.DeviceID `json:"id"`
	MAC    string         `json:"mac"`
	IP     string         `json:"ip"`
	VN     model.VNI      `json:"vn"`
	Switch model.SwitchID `json:"switch"`
	Kind   string         `json:"kind"`
	Group  model.GroupID  `json:"group,omitempty"`
}

// TraceDevices forwards one frame between two simulated devices and reports
// the result, for the dashboard's path tool.
func (w *World) TraceDevices(srcID, dstID model.DeviceID) *QueryResult {
	src, dst := w.Topo.Device(srcID), w.Topo.Device(dstID)
	if src == nil || dst == nil {
		return &QueryResult{Note: "device not found"}
	}
	res := w.Forward(src, dst)
	if res == nil {
		return &QueryResult{Src: refOf(src), Dst: refOf(dst), Note: "pair not currently testable"}
	}
	q := &QueryResult{Src: refOf(src), Dst: refOf(dst), Result: res}
	if !res.OK {
		q.Note = res.Note
	}
	return q
}

func refOf(d *model.Device) DeviceRef {
	return DeviceRef{
		ID: d.ID, MAC: d.MAC, IP: d.IP.String(), VN: d.VN,
		Switch: d.Switch, Kind: string(d.Kind), Group: d.Group,
	}
}

// FindDeviceByAddr resolves a MAC or IP to a device.
func (w *World) FindDeviceByAddr(query string) (*model.Device, bool) {
	if d, ok := w.macIndex[query]; ok {
		return d, true
	}
	for _, d := range w.Topo.Devices {
		if d.IP.String() == query {
			return d, true
		}
	}
	return nil, false
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

var _ = fmt.Sprintf
var _ = netip.Addr{}
