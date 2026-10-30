package control

import (
	"fmt"
	"math"
	"testing"
)

const (
	monitorAlpha = 0.1
	monitorDelta = 0.1
)

// audited returns n observations under digest, the ones at the given
// positions (0-based) missed. SelectionIDs are digest-n.
func audited(digest string, n int, missedAt ...int) []Observation {
	obs := make([]Observation, n)
	for i := range obs {
		obs[i] = Observation{SelectionID: fmt.Sprintf("%s-%d", digest, i), LockDigest: digest}
	}
	for _, i := range missedAt {
		obs[i].Missed = true
	}
	return obs
}

func requireStatus(t *testing.T, got, want DriftStatus) {
	t.Helper()
	if got.State != want.State || got.N != want.N || got.Misses != want.Misses || got.Alarm != want.Alarm ||
		math.Abs(got.UCB-want.UCB) > 1e-12 {
		t.Errorf("status = %+v, want %+v", got, want)
	}
}

func TestMonitorRequirementIsTheCalibrationMinimum(t *testing.T) {
	// (1-0.1)^n <= 0.1 first holds at n = 22, so 22 audited selections with
	// no miss are enough for an "ok" and 21 are still warming.
	if got := MinCalibrationN(monitorAlpha, monitorDelta); got != 22 {
		t.Fatalf("MinCalibrationN(%v, %v) = %d, the fixtures below assume 22", monitorAlpha, monitorDelta, got)
	}
}

func TestMonitorWarmsUpWithNoObservations(t *testing.T) {
	got := Monitor(nil, "L1", 100, monitorAlpha, monitorDelta)
	requireStatus(t, got, DriftStatus{State: "warming", N: 0, Misses: 0, UCB: 1, Alarm: false})
}

func TestMonitorStaysWarmingWithoutMissesBelowTheRequirement(t *testing.T) {
	got := Monitor(audited("L1", 21), "L1", 100, monitorAlpha, monitorDelta)
	requireStatus(t, got, DriftStatus{State: "warming", N: 21, UCB: ClopperPearsonUpper(0, 21, 1-monitorDelta)})
}

func TestMonitorOneMissWhileWarmingAlarms(t *testing.T) {
	// A confirmed miss is evidence on its own: no need to wait for 22.
	got := Monitor(audited("L1", 4, 3), "L1", 100, monitorAlpha, monitorDelta)
	requireStatus(t, got, DriftStatus{State: "alarm", N: 4, Misses: 1, UCB: ClopperPearsonUpper(1, 4, 1-monitorDelta), Alarm: true})
}

func TestMonitorRequirementMetWithNoMissesIsOK(t *testing.T) {
	got := Monitor(audited("L1", 22), "L1", 100, monitorAlpha, monitorDelta)
	want := DriftStatus{State: "ok", N: 22, UCB: ClopperPearsonUpper(0, 22, 1-monitorDelta)}
	requireStatus(t, got, want)
	if got.UCB > monitorAlpha {
		t.Errorf("UCB = %v, want at most alpha %v once the requirement is met", got.UCB, monitorAlpha)
	}
}

func TestMonitorBoundAboveAlphaAlarmsOnceTheRequirementIsMet(t *testing.T) {
	// 5 misses in 52: the 90% upper bound is about 0.19, above alpha 0.1.
	got := Monitor(audited("L1", 52, 0, 10, 20, 30, 40), "L1", 100, monitorAlpha, monitorDelta)
	if !(got.UCB > monitorAlpha) {
		t.Fatalf("fixture premise: UCB = %v, want above alpha %v", got.UCB, monitorAlpha)
	}
	requireStatus(t, got, DriftStatus{State: "alarm", N: 52, Misses: 5, UCB: got.UCB, Alarm: true})
}

func TestMonitorBoundWithinAlphaIsOKDespiteMisses(t *testing.T) {
	// 2 misses in 200: the bound is about 0.04. Past the warm-up the
	// Clopper-Pearson test decides, not the mere presence of a miss.
	got := Monitor(audited("L1", 200, 3, 100), "L1", 500, monitorAlpha, monitorDelta)
	if !(got.UCB < monitorAlpha) {
		t.Fatalf("fixture premise: UCB = %v, want below alpha %v", got.UCB, monitorAlpha)
	}
	requireStatus(t, got, DriftStatus{State: "ok", N: 200, Misses: 2, UCB: got.UCB})
}

func TestMonitorAllMissesBoundIsOne(t *testing.T) {
	got := Monitor(audited("L1", 3, 0, 1, 2), "L1", 100, monitorAlpha, monitorDelta)
	requireStatus(t, got, DriftStatus{State: "alarm", N: 3, Misses: 3, UCB: 1, Alarm: true})
}

// A new lock resets the monitor's identity: observations made under another
// lock digest are neither misses nor successes.
func TestMonitorIgnoresObservationsFromOtherLockDigests(t *testing.T) {
	obs := append(audited("OLD", 40, 5), audited("NEW", 2)...)
	got := Monitor(obs, "NEW", 100, monitorAlpha, monitorDelta)
	requireStatus(t, got, DriftStatus{State: "warming", N: 2, Misses: 0, UCB: ClopperPearsonUpper(0, 2, 1-monitorDelta)})
}

func TestMonitorWithOnlyOtherDigestsIsWarmingNotOK(t *testing.T) {
	got := Monitor(audited("OLD", 40), "NEW", 100, monitorAlpha, monitorDelta)
	requireStatus(t, got, DriftStatus{State: "warming", UCB: 1})
}

func TestMonitorWindowKeepsTheLastObservations(t *testing.T) {
	tests := []struct {
		name       string
		obs        []Observation
		window     int
		wantN      int
		wantMisses int
	}{
		{"old misses fall out of the window", append(audited("L1", 3, 0, 1, 2), audited("L1", 5)...), 5, 5, 0},
		{"recent misses stay in the window", append(audited("L1", 5), audited("L1", 3, 0, 1, 2)...), 5, 5, 3},
		{"a window larger than the history keeps all of it", audited("L1", 4, 1), 50, 4, 1},
		{"a window of exactly the history", audited("L1", 4, 1), 4, 4, 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := Monitor(tc.obs, "L1", tc.window, monitorAlpha, monitorDelta)
			if got.N != tc.wantN || got.Misses != tc.wantMisses {
				t.Errorf("N=%d Misses=%d, want %d and %d", got.N, got.Misses, tc.wantN, tc.wantMisses)
			}
		})
	}
}

func TestMonitorWindowCountsOnlyObservationsOfTheCurrentLock(t *testing.T) {
	// Oldest first: two observations of this lock, a miss among them, then
	// three of another lock. The window of 2 reaches back past the other
	// lock's observations to the two of this one.
	obs := []Observation{
		{SelectionID: "a", LockDigest: "L1"}, {SelectionID: "b", LockDigest: "L1", Missed: true},
		{SelectionID: "c", LockDigest: "OLD"}, {SelectionID: "d", LockDigest: "OLD"}, {SelectionID: "e", LockDigest: "OLD"},
	}
	got := Monitor(obs, "L1", 2, monitorAlpha, monitorDelta)
	if got.N != 2 || got.Misses != 1 {
		t.Errorf("N=%d Misses=%d, want 2 and 1", got.N, got.Misses)
	}
}

func TestMonitorWithoutAUsableWindowKeepsEveryObservation(t *testing.T) {
	// A zero or negative window must not silently drop the evidence.
	for _, window := range []int{0, -3} {
		got := Monitor(audited("L1", 30, 0), "L1", window, monitorAlpha, monitorDelta)
		if got.N != 30 || got.Misses != 1 {
			t.Errorf("window %d: N=%d Misses=%d, want 30 and 1", window, got.N, got.Misses)
		}
	}
}

func TestMonitorWithUnusableParametersAlarms(t *testing.T) {
	// A bound that cannot be computed must trip the alarm, not hide it.
	nan := math.NaN()
	tests := []struct {
		name         string
		alpha, delta float64
	}{
		{"alpha zero", 0, monitorDelta},
		{"alpha one", 1, monitorDelta},
		{"alpha NaN", nan, monitorDelta},
		{"delta zero", monitorAlpha, 0},
		{"delta one", monitorAlpha, 1},
		{"delta NaN", monitorAlpha, nan},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := Monitor(audited("L1", 30), "L1", 100, tc.alpha, tc.delta)
			if !got.Alarm || got.State != "alarm" || got.UCB != 1 {
				t.Errorf("status = %+v, want an alarm with UCB 1", got)
			}
		})
	}
}
