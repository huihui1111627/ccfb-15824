package sim

import (
	"fmt"
	"sync"
	"time"

	"circusnet/internal/fabric"
	"circusnet/internal/model"
)

// EventKind labels timeline entries.
type EventKind string

const (
	EventMove         EventKind = "move"
	EventLinkDown     EventKind = "link-down"
	EventLinkUp       EventKind = "link-up"
	EventAccessDown   EventKind = "access-down"
	EventAccessUp     EventKind = "access-up"
	EventSwitchDown   EventKind = "switch-down"
	EventSwitchUp     EventKind = "switch-up"
	EventReboot       EventKind = "reboot"
	EventVNJoin       EventKind = "vn-join"
	EventVNLeave      EventKind = "vn-leave"
	EventConverged    EventKind = "converged"
	EventVerification EventKind = "verification"
)

// Event is one entry in the device-migration and network-change timeline.
type Event struct {
	Seq    int            `json:"seq"`
	Round  int            `json:"round"`
	At     time.Time      `json:"at"`
	Kind   EventKind      `json:"kind"`
	Device model.DeviceID `json:"device,omitempty"`
	MAC    string         `json:"mac,omitempty"`
	From   string         `json:"from,omitempty"`
	To     string         `json:"to,omitempty"`
	Group  model.GroupID  `json:"group,omitempty"`
	Switch model.SwitchID `json:"switch,omitempty"`
	VN     model.VNI      `json:"vn,omitempty"`
	Detail string         `json:"detail,omitempty"`
}

// Times are the behavioural knobs of the simulated control plane.
type Times struct {
	// StaleTTL is how many convergence rounds a route from an older origin
	// epoch is retained before being deleted.
	StaleTTL int
}

func DefaultTimes() Times { return Times{StaleTTL: 4} }

// World owns all mutable state of one simulated circus network.
type World struct {
	Topo     *Topology
	Underlay *fabric.Underlay
	Times    Times

	RIB       map[model.SwitchID]*fabric.RIB
	FIB       map[model.SwitchID]map[model.VNI]*fabric.VNFIB
	SwUp      map[model.SwitchID]bool
	PortsOpen map[model.SwitchID]bool
	Epoch     map[model.SwitchID]uint64
	DeviceSeq map[model.DeviceID]uint64
	GroupSeq  map[model.GroupID]uint64

	// AccessUp[group][member] is the state of that member's access link into
	// the redundancy group.
	AccessUp map[model.GroupID]map[model.SwitchID]bool

	configuredLinks map[fabric.LinkKey]bool
	linkUp          map[fabric.LinkKey]bool

	Round         int
	Events        []Event
	eventSeq      int
	Start         time.Time
	bootRound     map[model.SwitchID]int
	ribChangedAt  map[model.SwitchID]int
	localVer      map[model.SwitchID]uint64
	seenVer       map[model.SwitchID]map[model.SwitchID]uint64
	reachPrev     map[model.SwitchID]map[model.SwitchID]bool
	localRoutes   map[model.SwitchID][]fabric.Route
	localVerDirty map[model.SwitchID]bool
	graphDirty    bool
	mu            sync.Mutex

	// Counters for the dashboard / report.
	TotalRounds         int
	ConvergenceRuns     int
	LastRounds          int
	AvgRounds           float64
	ARPAnswered         int64
	ARPRequests         int64
	BroadcastSuppressed int64
	VerifyChecks        int64
	VerifyFailures      int64
	Migrations          int
	GroupFailovers      int
	RoutesSent          int64
	RoutesRejected      int64
	Withdrawals         int64
	GroupWithdrawals    int64
	StaleExpired        int64
	Broadcasts          int64

	idx      *indexes
	macIndex map[string]*model.Device
}

// NewWorld builds the initial state and converges it.
func NewWorld(topo *Topology, times Times) *World {
	w := &World{
		Topo:            topo,
		Underlay:        fabric.NewUnderlay(),
		Times:           times,
		RIB:             map[model.SwitchID]*fabric.RIB{},
		FIB:             map[model.SwitchID]map[model.VNI]*fabric.VNFIB{},
		SwUp:            map[model.SwitchID]bool{},
		PortsOpen:       map[model.SwitchID]bool{},
		Epoch:           map[model.SwitchID]uint64{},
		DeviceSeq:       map[model.DeviceID]uint64{},
		GroupSeq:        map[model.GroupID]uint64{},
		AccessUp:        map[model.GroupID]map[model.SwitchID]bool{},
		configuredLinks: map[fabric.LinkKey]bool{},
		linkUp:          map[fabric.LinkKey]bool{},
		bootRound:       map[model.SwitchID]int{},
		macIndex:        map[string]*model.Device{},
		ribChangedAt:    map[model.SwitchID]int{},
		localVer:        map[model.SwitchID]uint64{},
		seenVer:         map[model.SwitchID]map[model.SwitchID]uint64{},
		reachPrev:       map[model.SwitchID]map[model.SwitchID]bool{},
		localRoutes:     map[model.SwitchID][]fabric.Route{},
		localVerDirty:   map[model.SwitchID]bool{},
		Start:           time.Now(),
	}
	for _, id := range topo.SwitchOrder {
		sw := topo.Switch[id]
		w.Underlay.AddNode(id)
		w.SwUp[id] = true
		w.Epoch[id] = 1
		w.DeviceSeq[model.DeviceID(0)] = 1
		w.RIB[id] = fabric.NewRIB(sw.VNs)
		w.RIB[id].KnownEpoch[id] = 1
		w.seenVer[id] = map[model.SwitchID]uint64{}
		w.reachPrev[id] = map[model.SwitchID]bool{}
	}
	for _, d := range topo.Devices {
		w.DeviceSeq[d.ID] = 1
	}
	for gid := range topo.Groups {
		w.GroupSeq[gid] = 1
	}
	for _, l := range topo.Links {
		k := fabric.NewLinkKey(l.A, l.B)
		w.configuredLinks[k] = true
		w.linkUp[k] = true
		w.Underlay.SetLink(l.A, l.B, true)
	}
	for gid, g := range topo.Groups {
		w.AccessUp[gid] = map[model.SwitchID]bool{}
		for _, m := range g.Members {
			w.AccessUp[gid][m] = true
		}
	}
	w.reindex()
	w.Converge()
	w.log(EventConverged, Event{Detail: fmt.Sprintf("initial convergence in %d rounds", w.TotalRounds)})
	return w
}
