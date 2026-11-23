// Command simcheck runs the seeded circus stress test and prints a report.
//
// The run simulates dozens of tents and thousands of devices with a fixed
// random seed, moves devices, cuts links and restarts switches, and after
// every convergence checks that forwarding between an arbitrary pair of
// devices matches the path derived from their real current location, with no
// loops and no duplicate frames. It also reports convergence time and
// broadcast volume.
package main

import (
	"flag"
	"fmt"
	"os"
	"sort"
	"time"

	"circusnet/internal/sim"
)

func main() {
	var (
		seed       = flag.Int64("seed", 20261124, "fixed random seed")
		steps      = flag.Int("steps", 60, "number of random changes")
		pairs      = flag.Int("pairs", 24, "sampled destinations per source per check")
		tents      = flag.Int("tents", 32, "number of tents")
		devices    = flag.Int("devices", 4000, "number of devices")
		members    = flag.Int("vn-members", 12, "switches per VN when not full mesh")
		full       = flag.Bool("full-mesh-vns", true, "every switch carries every VN")
		exhaustive = flag.Bool("exhaustive", false, "finish with an all-pairs check (slow on large topologies)")
		jsonOut    = flag.String("json", "", "write the full report as JSON to this file")
		quiet      = flag.Bool("quiet", false, "only print the summary")
	)
	flag.Parse()

	cfg := sim.DefaultConfig()
	cfg.Seed = *seed
	cfg.Tents = *tents
	cfg.Devices = *devices
	cfg.FullMeshVNs = *full
	cfg.VNMembersMin, cfg.VNMembersMax = *members, *members+6
	opt := sim.StressOptions{Config: cfg, Seed: *seed, Steps: *steps, Pairs: *pairs, Verbose: !*quiet, Exhaustive: *exhaustive}

	fmt.Printf("CircusNet stress test: seed=%d tents=%d devices=%d vns=%d full-mesh-vns=%v steps=%d pairs/source=%d\n",
		*seed, cfg.Tents, cfg.Devices, cfg.VNs, cfg.FullMeshVNs, *steps, *pairs)
	rep := sim.RunStress(opt)
	printReport(rep)
	if *jsonOut != "" {
		if err := sim.WriteJSON(*jsonOut, rep); err != nil {
			fmt.Fprintln(os.Stderr, "write json:", err)
			os.Exit(2)
		}
		fmt.Println("report written to", *jsonOut)
	}
	if !rep.Passed {
		os.Exit(1)
	}
}

func printReport(rep *sim.StressReport) {
	fmt.Println()
	fmt.Println("=== summary ===")
	fmt.Printf("topology       : %d tents, %d switches, %d devices (%d dual-homed, %d external), %d VNs, %d groups, %d links\n",
		rep.Topology.Tents, rep.Topology.Switches, rep.Topology.Devices, rep.Topology.DualHomed,
		rep.Topology.External, rep.Topology.VNs, rep.Topology.Groups, rep.Topology.Links)
	fmt.Printf("forwarding     : %d pair checks, %d failures, %d loops\n",
		rep.Checks, rep.Failures, rep.Loops)
	if rep.ExhaustiveChecked > 0 {
		fmt.Printf("exhaustive     : %d ordered pairs checked at the final state\n", rep.ExhaustiveChecked)
	}
	fmt.Printf("ARP            : %d/%d requests answered locally (suppression rate %.4f%%)\n",
		rep.ARPOK, rep.ARPTotal, pct(rep.ARPOK, rep.ARPTotal))
	fmt.Printf("broadcast      : %d flooded copies across all checks\n", rep.Broadcasts)
	fmt.Printf("convergence    : %d rounds total, avg %.2f rounds/change, max %d rounds\n",
		rep.Rounds, rep.AvgRounds, rep.MaxRounds)
	fmt.Printf("group failover : %d single-withdrawal takeovers\n", rep.GroupFailoverCount)
	fmt.Printf("elapsed        : %s\n", rep.Elapsed.Round(time.Millisecond))

	kinds := make([]string, 0, len(rep.PerChange))
	for k := range rep.PerChange {
		kinds = append(kinds, k)
	}
	sort.Strings(kinds)
	fmt.Println()
	fmt.Printf("%-14s %6s %10s %10s %10s %8s\n", "change", "count", "rounds", "avgRounds", "failures", "bcast")
	for _, k := range kinds {
		c := rep.PerChange[k]
		fmt.Printf("%-14s %6d %10d %10.2f %10d %8d\n", k, c.Count, c.Rounds, c.AvgRounds, c.Failures, c.Broadcasts)
	}
	if rep.Failures > 0 {
		fmt.Println()
		fmt.Println("=== first failures ===")
		shown := 0
		for _, s := range rep.Steps {
			for _, f := range s.SampleFailures {
				fmt.Printf("step %d %s: %s\n", s.Index, s.Kind, f)
				shown++
				if shown >= 20 {
					return
				}
			}
		}
	}
	fmt.Println()
	if rep.Passed {
		fmt.Println("RESULT: PASS — every converged forwarding path matched the real device placement.")
	} else {
		fmt.Println("RESULT: FAIL — see failures above.")
	}
}

func pct(a, b int64) float64 {
	if b == 0 {
		return 0
	}
	return 100 * float64(a) / float64(b)
}
