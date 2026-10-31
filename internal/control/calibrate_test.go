package control

import (
	"fmt"
	"math/rand/v2"
	"path/filepath"
	"reflect"
	"slices"
	"testing"
	"time"
)

// calUnit is a commit whose only regression is t00, a residual candidate with
// probability p. baseInput selects nothing else and never abstains, so t00 is
// selected exactly when lambda <= p.
func calUnit(id string, p float64) CalUnit {
	return CalUnit{ID: id, Failing: []string{"t00"}, Input: withCandidates(baseInput(), Candidate{TestID: "t00", P: p})}
}

// calUnits returns n units named ci:c000.., the first misses of them with
// probability pMiss and the rest with pHit.
func calUnits(n, misses int, pHit, pMiss float64) []CalUnit {
	units := make([]CalUnit, n)
	for i := range units {
		p := pHit
		if i < misses {
			p = pMiss
		}
		units[i] = calUnit(fmt.Sprintf("ci:c%03d", i), p)
	}
	return units
}

func calibrationParams(t *testing.T) CalibrationParams {
	t.Helper()
	grid, err := ParseGrid("0.005:0.500:0.005")
	if err != nil {
		t.Fatal(err)
	}
	return CalibrationParams{
		Alpha: 0.01, Delta: 0.05, Grid: grid, Unit: "commit", Split: "temporal", SplitAt: "2026-06-01", L1Mode: L1ModeGraph,
		Identity: testIdentity(), Now: time.Date(2026, 10, 14, 18, 22, 4, 0, time.UTC),
	}
}

func TestUnitLossIsOneOnlyWhenARegressionIsNotSelected(t *testing.T) {
	certain := calUnit("ci:certain", 0)
	certain.Input.Certain = []string{"t00"}
	forgotten := calUnit("ci:forgotten", 0.9)
	forgotten.Failing = []string{"t05"} // neither certain nor a candidate: the controller never saw it
	failedScore := calUnit("ci:failed", 0)
	failedScore.Input.Candidates[0].Failed = true
	noRegression := calUnit("ci:none", 0)
	noRegression.Failing = nil
	partial := calUnit("ci:partial", 0.9)
	partial.Failing = []string{"t00", "t05"} // one selected, one not: the commit still missed

	tests := []struct {
		name   string
		unit   CalUnit
		lambda float64
		want   float64
	}{
		{"selected at the threshold", calUnit("ci:a", 0.3), 0.3, 0},
		{"selected above the threshold", calUnit("ci:a", 0.9), 0.3, 0},
		{"below the threshold is a miss", calUnit("ci:a", 0.29), 0.3, 1},
		{"a certain test is never missed", certain, 0.5, 0},
		{"a failed score selects the test", failedScore, 0.5, 0},
		{"a regression the controller never saw is a miss", forgotten, 0.1, 1},
		{"all regressions must be selected", partial, 0.1, 1},
		{"a commit without regressions cannot miss", noRegression, 0.5, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := UnitLoss(tc.unit, tc.lambda); got != tc.want {
				t.Errorf("UnitLoss(lambda %v) = %v, want %v", tc.lambda, got, tc.want)
			}
		})
	}
}

// An abstention selects the full suite, which cannot miss: loss 0 whatever
// the predictor said, through the same rules production applies.
func TestUnitLossIsZeroWhenTheControllerAbstains(t *testing.T) {
	trigger := calUnit("ci:trigger", 0)
	trigger.Input.TriggersFull = []string{"go.mod"}
	l1Full := calUnit("ci:l1", 0)
	l1Full.Input.FullReason = "graph_unavailable"
	notWorthIt := calUnit("ci:worthless", 0)
	for i := 1; i < 20; i++ {
		notWorthIt.Input.Certain = append(notWorthIt.Input.Certain, fmt.Sprintf("t%02d", i)) // 19 of 20 tests run: no saving
	}
	broad := calUnit("ci:broad", 0)
	broad.Input.ChangedPackages = 4
	for name, u := range map[string]CalUnit{"trigger": trigger, "L1 full-suite reason": l1Full, "saving below the minimum": notWorthIt, "too many packages": broad} {
		if got := UnitLoss(u, 0.5); got != 0 {
			t.Errorf("%s: UnitLoss = %v, want 0 (the full suite runs)", name, got)
		}
	}
	// Control: the same unit without the abstention condition misses.
	if got := UnitLoss(calUnit("ci:control", 0), 0.5); got != 1 {
		t.Errorf("control unit loss = %v, want 1", got)
	}
}

func TestUnitLossDoesNotChangeTheUnit(t *testing.T) {
	u := calUnit("ci:a", 0.4)
	u.Input.Lambda = 0.123
	UnitLoss(u, 0.5)
	if u.Input.Lambda != 0.123 {
		t.Errorf("UnitLoss changed the unit's lambda to %v", u.Input.Lambda)
	}
}

func TestCalibrateSelectsTheLargestRejectedLambda(t *testing.T) {
	p := calibrationParams(t)
	// 297 commits keep their regression above every lambda of the grid; 3 are
	// only caught up to 0.1. From 0.105 on the empirical miss rate is 3/300,
	// which is alpha, and the p-value is 1.
	units := calUnits(300, 3, 0.9, 0.1)
	got := Calibrate(units, p)

	if got.LTT.Refused {
		t.Fatalf("refused: %+v", got.LTT)
	}
	if got.Lock.Lambda != 0.1 || got.LTT.Lambda != 0.1 {
		t.Errorf("lambda = lock %v ltt %v, want 0.1", got.Lock.Lambda, got.LTT.Lambda)
	}
	if got.Lock.EmpiricalMissRate != 0 || got.LTT.EmpiricalMiss != 0 {
		t.Errorf("empirical miss = lock %v ltt %v, want 0 at lambda 0.1", got.Lock.EmpiricalMissRate, got.LTT.EmpiricalMiss)
	}
	wantPoints := slices.Index(p.Grid, 0.105) + 1
	if len(got.LTT.Points) != wantPoints {
		t.Fatalf("evaluated %d lambdas, want %d (stop at the first non-rejection, 0.105)", len(got.LTT.Points), wantPoints)
	}

	// The p-values of every evaluated lambda are in the lock, keyed by lambda.
	rejected := HBPValue(300, 0, 0.01)
	if rejected > 0.05 {
		t.Fatalf("fixture: p-value %v of a loss-free sample of 300 must reject at delta 0.05", rejected)
	}
	if len(got.Lock.PValues) != wantPoints {
		t.Errorf("lock holds %d p-values, want %d", len(got.Lock.PValues), wantPoints)
	}
	for _, lambda := range p.Grid[:wantPoints-1] {
		if v, ok := got.Lock.PValues[PValueKey(lambda)]; !ok || v != rejected {
			t.Errorf("p-value of %v = %v (present %v), want %v", lambda, v, ok, rejected)
		}
	}
	if v := got.Lock.PValues[PValueKey(0.105)]; v != 1 {
		t.Errorf("p-value of the first non-rejected lambda 0.105 = %v, want 1", v)
	}
}

func TestCalibrateLockCarriesEveryInputAndPassesVerify(t *testing.T) {
	p := calibrationParams(t)
	units := calUnits(300, 0, 0.9, 0)
	got := Calibrate(units, p)
	if got.LTT.Refused {
		t.Fatalf("refused: %+v", got.LTT)
	}

	wantIDs := make([]string, len(units))
	for i, u := range units {
		wantIDs[i] = u.ID
	}
	l := got.Lock
	checks := []struct {
		field     string
		got, want any
	}{
		{"Version", l.Version, 1},
		{"CreatedAt", l.CreatedAt, "2026-10-14T18:22:04Z"},
		{"Alpha", l.Alpha, p.Alpha}, {"Delta", l.Delta, p.Delta}, {"Unit", l.Unit, "commit"},
		{"Lambda", l.Lambda, 0.5}, // loss free everywhere: the largest lambda of the grid
		{"Predictor", l.Predictor, p.Identity.Predictor},
		{"ModelVersion", l.ModelVersion, p.Identity.ModelRelease},
		{"RubricVersion", l.RubricVersion, p.Identity.RubricVersion},
		{"FeatureVersion", l.FeatureVersion, p.Identity.FeatureVersion},
		{"WeightsDigest", l.WeightsDigest, p.Identity.WeightsDigest},
		{"PolicyDigest", l.PolicyDigest, p.Identity.PolicyDigest},
		{"L1Mode", l.L1Mode, L1ModeGraph},
		{"NCal", l.NCal, 300}, {"Split", l.Split, "temporal"}, {"SplitAt", l.SplitAt, "2026-06-01"},
		{"Grid", l.Grid, p.Grid}, {"CalibrationIDs", l.CalibrationIDs, wantIDs},
	}
	for _, c := range checks {
		if !reflect.DeepEqual(c.got, c.want) {
			t.Errorf("Lock.%s = %v, want %v", c.field, c.got, c.want)
		}
	}
	if l.Digest != "" {
		t.Errorf("Calibrate set the digest %q; WriteLock owns it", l.Digest)
	}

	// The lock is accepted by WriteLock and then by the controller's Verify
	// for the identity it was fitted under.
	path := filepath.Join(t.TempDir(), "hazard.lock")
	if err := WriteLock(path, l); err != nil {
		t.Fatalf("WriteLock: %v", err)
	}
	read, err := ReadLock(path)
	if err != nil {
		t.Fatal(err)
	}
	if reason := read.Verify(p.Identity); reason != "" {
		t.Errorf("Verify(identity of the calibration) = %q, want the guarantee to transfer", reason)
	}
}

func TestCalibrateIgnoresTheOrderOfTheUnits(t *testing.T) {
	p := calibrationParams(t)
	units := calUnits(300, 3, 0.9, 0.1)
	want := Calibrate(units, p)

	shuffled := slices.Clone(units)
	rand.New(rand.NewPCG(7, 0)).Shuffle(len(shuffled), func(i, j int) { shuffled[i], shuffled[j] = shuffled[j], shuffled[i] })
	got := Calibrate(shuffled, p)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("a shuffled sample calibrated differently:\n got %+v\nwant %+v", got.Lock, want.Lock)
	}
	if !slices.IsSorted(got.Lock.CalibrationIDs) {
		t.Errorf("calibration IDs are not sorted: %v", got.Lock.CalibrationIDs[:5])
	}
}

// With fewer commits than MinCalibrationN no lambda can be certified however
// well the predictor did, and nothing resembling a lock is produced.
func TestCalibrateRefusesBelowTheMinimumSample(t *testing.T) {
	p := calibrationParams(t)
	need := MinCalibrationN(p.Alpha, p.Delta)
	for _, n := range []int{0, 1, need - 1} {
		got := Calibrate(calUnits(n, 0, 0.9, 0), p)
		if !got.LTT.Refused || got.LTT.NeedN != need || got.LTT.HaveN != n {
			t.Errorf("n=%d: ltt = refused %v need %d have %d, want refused, need %d, have %d", n, got.LTT.Refused, got.LTT.NeedN, got.LTT.HaveN, need, n)
		}
		if !reflect.DeepEqual(got.Lock, Lock{}) {
			t.Errorf("n=%d: a refused calibration produced the lock %+v", n, got.Lock)
		}
	}
	// The boundary itself is enough for a loss-free sample.
	if got := Calibrate(calUnits(need, 0, 0.9, 0), p); got.LTT.Refused || got.Lock.NCal != need {
		t.Errorf("n=%d (the minimum): refused %v ncal %d, want accepted", need, got.LTT.Refused, got.Lock.NCal)
	}
}

// A sample that is large enough but misses too often certifies nothing either.
func TestCalibrateRefusesWhenNoLambdaKeepsTheMissRateBelowAlpha(t *testing.T) {
	p := calibrationParams(t)
	got := Calibrate(calUnits(300, 300, 0.9, 0), p) // every regression has probability 0: missed at every lambda
	if !got.LTT.Refused || got.LTT.HaveN != 300 || got.LTT.NeedN != MinCalibrationN(p.Alpha, p.Delta) {
		t.Fatalf("ltt = %+v, want refused with need %d have 300", got.LTT, MinCalibrationN(p.Alpha, p.Delta))
	}
	if !reflect.DeepEqual(got.Lock, Lock{}) {
		t.Errorf("a refused calibration produced the lock %+v", got.Lock)
	}
}
