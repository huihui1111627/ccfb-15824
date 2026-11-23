package sim

import (
	"fmt"
	"math/rand"
	"sort"
	"time"

	"circusnet/internal/fabric"
	"circusnet/internal/model"
)

// Step is one timeline entry in a stress run: a change, its convergence and
// the verification result.
type Step struct {
	Index       int         `json:"index"`
	Kind        ChangeKind  `json:"kind"`
	Detail      string      `json:"detail"`
	Round       int         `json:"round"`
	Rounds      int         `json:"rounds"`
	Checked     int         `json:"checked"`
	Failed      int         `json:"failed"`
	Skipped     int         `json:"skipped"`
	Broadcasts  int         `json:"broadcasts"`
	Withdrawals int64       `json:"withdrawals"`
	GroupWds    int64       `json:"groupWithdrawals"`
	ARPAnswered int         `json:"arpAnswered"`
	ARPRequests int         `json:"arpRequests"`
	Violations  []Violation `json:"violations,omitempty"`
	// SampleFailures captures a few concrete failures for the report.
	SampleFailures []string      `json:"sampleFailures,omitempty"`
	Elapsed        time.Duration `json:"elapsed"`
}

// StressReport is the full result of a seeded stress run.
type StressReport struct {
	Seed       int64     `json:"seed"`
	Topology   TopoStats `json:"topology"`
	Steps      []Step    `json:"steps"`
	Checks     int64     `json:"checks"`
	Failures   int64     `json:"failures"`
	Loops      int64     `json:"loops"`
	Broadcasts int64     `json:"broadcasts"`
	ARPOK      int64     `json:"arpAnswered"`
	ARPTotal   int64     `json:"arpRequests"`
	Rounds     int64     `json:"rounds"`
	MaxRounds  int       `json:"maxRounds"`
	AvgRounds  float64   `json:"avgRounds"`
	Moves      int       `json:"moves"`
	// PerChange aggregates convergence rounds by change kind.
	PerChange map[string]ChangeStat `json:"perChange"`
	// GroupFailoverCount counts link failures where exactly one group route
	// was withdrawn and the peer member took over.
	GroupFailoverCount int           `json:"groupFailovers"`
	Elapsed            time.Duration `json:"elapsed"`
	FinishedAt         time.Time     `json:"finishedAt"`
	Passed             bool          `json:"passed"`
	ExhaustiveChecked  int           `json:"exhaustiveChecked"`
}

// TopoStats summarises the generated deployment.
type TopoStats struct {
	Tents     int `json:"tents"`
	Switches  int `json:"switches"`
	Devices   int `json:"devices"`
	VNs       int `json:"vns"`
	Groups    int `json:"groups"`
	DualHomed int `json:"dualHomed"`
	Links     int `json:"links"`
	External  int `json:"externalHosts"`
}

// ChangeStat aggregates one change kind.
type ChangeStat struct {
	Count      int     `json:"count"`
	Rounds     int64   `json:"rounds"`
	Failures   int64   `json:"failures"`
	Broadcasts int64   `json:"broadcasts"`
	AvgRounds  float64 `json:"avgRounds"`
}

// StressOptions controls the seeded stress run.
type StressOptions struct {
	Config Config
	Seed   int64
	Steps  int
	Pairs  int // sampled destinations per source during each verification
	// Exhaustive performs a full all-pairs check in the final step.
	Exhaustive bool
	// Verbose prints progress lines.
	Verbose bool
}

// DefaultStressOptions is the deployment-sized run from the brief: dozens of
// tents, thousands of devices, random moves, link failures and restarts.
func DefaultStressOptions() StressOptions {
	return StressOptions{Config: DefaultConfig(), Seed: 20261124, Steps: 60, Pairs: 24}
}

// RunStress performs the seeded scenario and verifies after every change.
func RunStress(opt StressOptions) *StressReport {
	start := time.Now()
	cfg := opt.Config
	cfg.Seed = opt.Seed
	topo := GenerateTopology(cfg)
	w := NewWorld(topo, DefaultTimes())
	rng := rand.New(rand.NewSource(opt.Seed ^ 0x5eed))
	rep := &StressReport{
		Seed:      opt.Seed,
		PerChange: map[string]ChangeStat{},
		Topology: TopoStats{
			Tents: cfg.Tents, Switches: len(topo.SwitchOrder),
			Devices: len(topo.Devices), VNs: len(topo.VNs),
			Groups: len(topo.Groups), Links: len(topo.Links),
		},
	}
	for _, d := range topo.Devices {
		if d.Kind == model.AttachGroup {
			rep.Topology.DualHomed++
		}
		if d.External {
			rep.Topology.External++
		}
	}
	// Initial convergence and check.
	rounds := w.TotalRounds
	vr := w.VerifyAll(VerifyOptions{Pairs: opt.Pairs, Seed: opt.Seed})
	rep.record(Step{Index: 0, Kind: "initial", Detail: "initial convergence",
		Round: w.Round, Rounds: rounds, Checked: vr.Checked, Failed: vr.Failed,
		Skipped: vr.Skipped, Broadcasts: vr.Broadcasts,
		ARPAnswered: vr.ARPAnswered, ARPRequests: vr.ARPRequests,
		Violations: vr.Violations}, vr)
	if opt.Verbose {
		fmt.Printf("initial: rounds=%d checked=%d failed=%d\n", rounds, vr.Checked, vr.Failed)
	}

	downLinks := map[string]bool{}
	downSwitches := map[model.SwitchID]bool{}
	downAccess := map[string]bool{}
	for i := 1; i <= opt.Steps; i++ {
		stepStart := time.Now()
		before := w.snapshotCounters()
		kind, detail := applyRandomChange(w, rng, downLinks, downSwitches, downAccess)
		if kind == "" {
			continue
		}
		rounds := w.Converge()
		dl := w.diffCounters(before)
		vr := w.VerifyAll(VerifyOptions{Pairs: opt.Pairs, Seed: opt.Seed + int64(i)})
		st := Step{Index: i, Kind: kind, Detail: detail, Round: w.Round, Rounds: rounds,
			Checked: vr.Checked, Failed: vr.Failed, Skipped: vr.Skipped,
			Broadcasts: vr.Broadcasts, Withdrawals: dl.withdrawals,
			GroupWds: dl.groupWithdrawals, ARPAnswered: vr.ARPAnswered,
			ARPRequests: vr.ARPRequests, Violations: vr.Violations,
			Elapsed: time.Since(stepStart)}
		for _, v := range vr.Violations {
			if len(st.SampleFailures) >= 5 {
				break
			}
			st.SampleFailures = append(st.SampleFailures,
				fmt.Sprintf("%d->%d %s: %s (physical %v)", v.Src, v.Dst, v.DstIP, v.Result.Note, v.Result.Physical))
		}
		rep.record(st, vr)
		if opt.Verbose {
			fmt.Printf("step %2d %-14s rounds=%d checked=%d failed=%d bcast=%d  %s\n",
				i, kind, rounds, vr.Checked, vr.Failed, vr.Broadcasts, detail)
		}
		if (kind == ChangeAccessDown || kind == ChangeLinkDown) && dl.groupWithdrawals == 1 && dl.withdrawals == 1 {
			rep.GroupFailoverCount++
		}
	}

	if opt.Exhaustive {
		full := w.VerifyAll(VerifyOptions{Pairs: 0})
		rep.ExhaustiveChecked = full.Checked
		st := Step{Index: opt.Steps + 1, Kind: "exhaustive", Detail: "all ordered device pairs",
			Round: w.Round, Checked: full.Checked, Failed: full.Failed,
			Skipped: full.Skipped, Broadcasts: full.Broadcasts,
			ARPAnswered: full.ARPAnswered, ARPRequests: full.ARPRequests,
			Violations: full.Violations}
		for _, v := range full.Violations {
			if len(st.SampleFailures) >= 5 {
				break
			}
			st.SampleFailures = append(st.SampleFailures,
				fmt.Sprintf("%d->%d %s: %s (physical %v)", v.Src, v.Dst, v.DstIP, v.Result.Note, v.Result.Physical))
		}
		rep.Steps = append(rep.Steps, st)
		if opt.Verbose {
			fmt.Printf("exhaustive: checked=%d failed=%d skipped=%d\n", full.Checked, full.Failed, full.Skipped)
		}
	}

	rep.Checks = int64(0)
	for _, s := range rep.Steps {
		rep.Checks += int64(s.Checked)
		if s.Failed > 0 {
			rep.Failures += int64(s.Failed)
		}
		if s.Rounds > rep.MaxRounds {
			rep.MaxRounds = s.Rounds
		}
		rep.Rounds += int64(s.Rounds)
		rep.Broadcasts += int64(s.Broadcasts)
		rep.ARPOK += int64(s.ARPAnswered)
		rep.ARPTotal += int64(s.ARPRequests)
	}
	if n := len(rep.Steps); n > 0 {
		rep.AvgRounds = float64(rep.Rounds) / float64(n)
	}
	for k, v := range rep.PerChange {
		v.AvgRounds = float64(v.Rounds) / float64(v.Count)
		rep.PerChange[k] = v
	}
	rep.Passed = rep.Failures == 0
	rep.Elapsed = time.Since(start)
	rep.FinishedAt = time.Now()
	return rep
}

func (r *StressReport) record(s Step, vr *VerifyReport) {
	r.Steps = append(r.Steps, s)
	r.Loops += int64(vr.LoopErrors)
	r.Failures += int64(vr.Failed)
	cs := r.PerChange[string(s.Kind)]
	cs.Count++
	cs.Rounds += int64(s.Rounds)
	cs.Failures += int64(s.Failed)
	cs.Broadcasts += int64(s.Broadcasts)
	r.PerChange[string(s.Kind)] = cs
}

// applyRandomChange picks a plausible circus operation with the seeded RNG.
// Every operation is realistic: vans move devices, cables get unplugged, a
// switch loses power, a dual-homed access link is cut.
func applyRandomChange(w *World, rng *rand.Rand, downLinks map[string]bool,
	downSwitches map[model.SwitchID]bool, downAccess map[string]bool) (ChangeKind, string) {
	roll := rng.Intn(100)
	switch {
	case roll < 46:
		// Move a device or several.
		n := 1 + rng.Intn(5)
		moved := 0
		for tries := 0; tries < n*8 && moved < n; tries++ {
			d := w.Topo.Devices[rng.Intn(len(w.Topo.Devices))]
			if d.Kind != model.AttachSingle || d.External || downSwitches[d.Switch] {
				continue
			}
			if w.MoveDevice(d, rng) {
				moved++
			}
		}
		if moved == 0 {
			return "", ""
		}
		return ChangeMove, fmt.Sprintf("%d device(s) moved", moved)
	case roll < 60:
		// Link failure or repair.
		links := w.Topo.Links
		l := links[rng.Intn(len(links))]
		key := fmt.Sprint(l.A, "-", l.B)
		if downLinks[key] {
			if w.FlipLink(fabric.NewLinkKey(l.A, l.B), true) {
				delete(downLinks, key)
				return ChangeLinkUp, "link " + key + " restored"
			}
			return "", ""
		}
		if w.FlipLink(fabric.NewLinkKey(l.A, l.B), false) {
			downLinks[key] = true
			return ChangeLinkDown, "link " + key + " cut"
		}
		return "", ""
	case roll < 72:
		// Dual-homed access link failure or repair.
		var gids []model.GroupID
		for gid := range w.Topo.Groups {
			gids = append(gids, gid)
		}
		gid := gids[rng.Intn(len(gids))]
		g := w.Topo.Groups[gid]
		m := g.Members[rng.Intn(len(g.Members))]
		k := string(gid) + "/" + string(m)
		if downAccess[k] {
			if w.SetAccess(gid, m, true) {
				delete(downAccess, k)
				return ChangeAccessUp, "access " + k + " restored"
			}
			return "", ""
		}
		if w.SetAccess(gid, m, false) {
			downAccess[k] = true
			return ChangeAccessDown, "access " + k + " cut (peer takes over)"
		}
		return "", ""
	case roll < 84:
		// Switch restart: down now, up immediately. Ports stay closed while
		// the control plane reconverges.
		var cands []model.SwitchID
		for _, id := range w.Topo.SwitchOrder {
			if !downSwitches[id] {
				cands = append(cands, id)
			}
		}
		if len(cands) == 0 {
			return "", ""
		}
		sw := cands[rng.Intn(len(cands))]
		w.SwitchDown(sw)
		downSwitches[sw] = true
		return ChangeSwitchDown, "switch " + string(sw) + " powered off"
	case roll < 94:
		var cands []model.SwitchID
		for id := range downSwitches {
			cands = append(cands, id)
		}
		if len(cands) == 0 {
			return "", ""
		}
		sw := cands[rng.Intn(len(cands))]
		delete(downSwitches, sw)
		w.SwitchUp(sw)
		return ChangeSwitchUp, "switch " + string(sw) + " restarted (ports closed until converged)"
	default:
		// A switch joins or leaves a VN (tent gains/loses a service).
		sw := w.Topo.SwitchOrder[rng.Intn(len(w.Topo.SwitchOrder))]
		vn := w.Topo.VNs[rng.Intn(len(w.Topo.VNs))]
		join := !w.Topo.Switch[sw].InVirtualNetwork(vn.VNI)
		if join && rng.Intn(2) == 0 {
			return "", ""
		}
		if w.SetVNMembership(sw, vn.VNI, join) {
			if join {
				return ChangeVNJoin, fmt.Sprintf("%s joins VN %d", sw, vn.VNI)
			}
			return ChangeVNLeave, fmt.Sprintf("%s leaves VN %d", sw, vn.VNI)
		}
		return "", ""
	}
}

type counters struct {
	withdrawals      int64
	groupWithdrawals int64
	routesSent       int64
	broadcasts       int64
}

func (w *World) snapshotCounters() counters {
	return counters{w.Withdrawals, w.GroupWithdrawals, w.RoutesSent, w.Broadcasts}
}

func (w *World) diffCounters(b counters) counters {
	return counters{
		withdrawals:      w.Withdrawals - b.withdrawals,
		groupWithdrawals: w.GroupWithdrawals - b.groupWithdrawals,
		routesSent:       w.RoutesSent - b.routesSent,
		broadcasts:       w.Broadcasts - b.broadcasts,
	}
}

// SortedChangeKinds returns the change kinds in a stable order for reports.
func SortedChangeKinds(m map[string]ChangeStat) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
