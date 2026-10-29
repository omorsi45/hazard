package control

// States of the drift monitor.
const (
	driftWarming = "warming"
	driftOK      = "ok"
	driftAlarm   = "alarm"
)

// Observation is one audited regression-bearing selection: whether the
// selection missed a regression the full suite found, under the lock that
// made it. Runs that were never audited are not observations, and are never
// counted as successes.
type Observation struct {
	SelectionID string
	LockDigest  string
	Missed      bool
}

// DriftStatus is the drift monitor's verdict. State is "warming" (too few
// audited selections for the bound to mean anything), "ok" or "alarm"; Alarm
// is true exactly when State is "alarm". N and Misses count the observations
// used and UCB is their Clopper-Pearson upper bound on the field miss rate.
type DriftStatus struct {
	State  string
	N      int
	Misses int
	UCB    float64
	Alarm  bool
}

// Monitor reports whether the field miss rate of the selections made under
// lockDigest has drifted past alpha (seed 10.5, with the warm-up correction).
// obs runs oldest first. Only observations under lockDigest count, so a new
// lock starts from nothing, and only the last window of those: a window of
// zero or less keeps all of them rather than dropping evidence.
//
// With need = MinCalibrationN(alpha, delta) observations the bound is
// meaningful. Below it the monitor is warming, but a confirmed miss alarms at
// once, since a miss is evidence on its own. At need or more it alarms when
// the (1-delta) Clopper-Pearson upper bound exceeds alpha. Parameters that
// cannot define a bound (alpha or delta outside (0, 1)) alarm: a bound that
// cannot be computed must not hide drift.
func Monitor(obs []Observation, lockDigest string, window int, alpha, delta float64) DriftStatus {
	var current []Observation
	for _, o := range obs {
		if o.LockDigest == lockDigest {
			current = append(current, o)
		}
	}
	if window > 0 && len(current) > window {
		current = current[len(current)-window:]
	}
	misses := 0
	for _, o := range current {
		if o.Missed {
			misses++
		}
	}
	status := DriftStatus{State: driftWarming, N: len(current), Misses: misses}

	if !(alpha > 0 && alpha < 1) || !(delta > 0 && delta < 1) {
		status.State, status.UCB, status.Alarm = driftAlarm, 1, true
		return status
	}
	// ClopperPearsonUpper(0, 0, ...) is 1: no observations, no evidence.
	status.UCB = ClopperPearsonUpper(misses, status.N, 1-delta)
	need := MinCalibrationN(alpha, delta)
	switch {
	case status.N < need && misses > 0, status.N >= need && status.UCB > alpha:
		status.State, status.Alarm = driftAlarm, true
	case status.N >= need:
		status.State = driftOK
	}
	return status
}
