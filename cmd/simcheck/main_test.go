package main

import (
	"testing"

	"circusnet/internal/sim"
)

// The reported stress scenario must be reproducible: the same seed produces
// identical statistics.
func TestStressIsDeterministic(t *testing.T) {
	cfg := sim.DefaultConfig()
	cfg.Devices = 300
	cfg.Tents = 6
	opt := sim.StressOptions{Config: cfg, Seed: 12345, Steps: 10, Pairs: 4}
	a := sim.RunStress(opt)
	b := sim.RunStress(opt)
	if !a.Passed || !b.Passed {
		t.Fatalf("stress failed: %v/%v", a.Failures, b.Failures)
	}
	if a.Checks != b.Checks || a.Rounds != b.Rounds || a.Failures != b.Failures || a.Broadcasts != b.Broadcasts {
		t.Fatalf("non-deterministic: a=%+v b=%+v", a, b)
	}
	if len(a.Steps) != len(b.Steps) {
		t.Fatalf("step count differs: %d vs %d", len(a.Steps), len(b.Steps))
	}
	for i := range a.Steps {
		if a.Steps[i].Kind != b.Steps[i].Kind || a.Steps[i].Rounds != b.Steps[i].Rounds {
			t.Fatalf("step %d differs: %+v vs %+v", i, a.Steps[i], b.Steps[i])
		}
	}
}
