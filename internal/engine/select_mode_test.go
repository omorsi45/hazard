package engine

import (
	"testing"

	"github.com/omorsi45/hazard/internal/control"
	"github.com/omorsi45/hazard/internal/impact"
)

// The tests below guard the L1 mode of a locked selection. A threshold is
// certified for the floor it was measured over, so a selection under a lock
// runs L1 in the mode the lock records, whatever the index could also do.

// otherEdited changes only Other, which no test of core or left executes.
const otherEdited = "package core\n\nfunc Core() int { return 1 }\n\nfunc Other() int { return 1 + 0 }\n"

// coverageFixture is the diamond module with core split in two functions:
// left reaches Core and right reaches Other. The index is built with coverage
// on the committed tree, then Other is edited in the working tree. With
// max_hops 1 the graph floor is core, left and right; the coverage floor is
// right and top, because no test of core or left executes the edited line
// (top reaches Other through right). The lock is ignored, as a committed lock
// would not be an added file of the change.
func coverageFixture(t *testing.T) *fixture {
	t.Helper()
	f := newFixture(t, map[string]string{
		"hazard.yml":     lockedYML,
		".gitignore":     ".hazard/\nhazard.lock\n",
		"core/core.go":   "package core\n\nfunc Core() int { return 1 }\n\nfunc Other() int { return 1 }\n",
		"right/right.go": "package right\n\nimport \"example.com/m/core\"\n\nfunc Right() int { return core.Other() }\n",
	})
	f.indexNow(t, true)
	f.logistic(t, defaultWeight, defaultBias)
	f.write(t, map[string]string{"core/core.go": otherEdited})
	return f
}

// coverageLock writes the lock of a calibration done in coverage mode: the
// recorded mode and the policy digest built for it.
func coverageLock(t *testing.T, f *fixture) control.Lock {
	t.Helper()
	cfg := f.config(t)
	return f.writeLock(t, "logistic", func(l *control.Lock) {
		l.L1Mode = control.L1ModeCoverage
		l.PolicyDigest = BuildPolicy(cfg, "logistic", impact.ModeCoverage).Digest()
	})
}

func TestSelectLockedGraphModeIsNotNarrowedByCoverage(t *testing.T) {
	t.Parallel()
	f := coverageFixture(t)

	// The premise: without a lock the two modes disagree.
	if got := selectOK(t, Options{RepoDir: f.dir, Mode: impact.ModeCoverage, ModeSet: true}); !sameStrings(ids(got.Hypothetical), []string{idRght, idTop}) {
		t.Fatalf("coverage floor = %v (L1 %+v), want right and top", ids(got.Hypothetical), got.L1)
	}
	if got := selectOK(t, Options{RepoDir: f.dir, Mode: impact.ModeGraph, ModeSet: true}); !sameStrings(ids(got.Hypothetical), certainIDs) {
		t.Fatalf("graph floor = %v, want %v", ids(got.Hypothetical), certainIDs)
	}

	lock := f.writeLock(t, "logistic", nil) // what hazard calibrate writes: graph mode
	if lock.L1Mode != control.L1ModeGraph {
		t.Fatalf("the lock records the mode %q, want graph", lock.L1Mode)
	}
	d := selectWith(t, f, func(o *Options) { o.load = stubLoader(fixedP(0)) })

	requireNarrowed(t, d, "a graph lock over an index that also has coverage")
	if d.L1 == nil || d.L1.CoverageState != "not_requested" {
		t.Errorf("L1 = %+v, want the graph mode the lock was calibrated in", d.L1)
	}
	if !sameStrings(ids(d.Tests), certainIDs) {
		t.Errorf("Tests = %v, want the graph CERTAIN set %v: the threshold was measured over it", ids(d.Tests), certainIDs)
	}
	if d.LockDigest != lock.Digest {
		t.Errorf("LockDigest = %q, want the lock in force %q", d.LockDigest, lock.Digest)
	}
}

func TestSelectLockedCoverageModeUsesCoverage(t *testing.T) {
	t.Parallel()
	f := coverageFixture(t)
	lock := coverageLock(t, f)

	d := selectWith(t, f, func(o *Options) { o.load = stubLoader(fixedP(0)) })

	requireNarrowed(t, d, "a coverage lock over an index with fresh coverage")
	if d.L1 == nil || d.L1.CoverageState != "fresh" {
		t.Errorf("L1 = %+v, want the coverage mode the lock was calibrated in", d.L1)
	}
	if !sameStrings(ids(d.Tests), []string{idRght, idTop}) || d.LockDigest != lock.Digest {
		t.Errorf("Tests = %v digest %q, want the coverage floor and the lock in force", ids(d.Tests), d.LockDigest)
	}
}

func TestSelectLockedCoverageModeNeedsCoverageInTheIndex(t *testing.T) {
	t.Parallel()
	f := lockedFixture(t, lockedYML, nil) // an index without coverage
	f.logistic(t, defaultWeight, defaultBias)
	coverageLock(t, f)

	d := selectWith(t, f, func(o *Options) { o.load = failingLoader(t) })

	requireFullSuite(t, d, "policy_mismatch")
	if !hasWarning(d, "coverage") {
		t.Errorf("Warnings = %v, want the missing coverage named", d.Warnings)
	}
}

// TestSelectLockModeEditedAfterCalibrationIsAPolicyMismatch: the lock records
// graph; somebody edits the mode to coverage and re-seals the digest. The
// policy digest still describes the graph policy, so the lock does not verify.
func TestSelectLockModeEditedAfterCalibrationIsAPolicyMismatch(t *testing.T) {
	t.Parallel()
	f := coverageFixture(t)
	f.writeLock(t, "logistic", nil)
	requireNarrowed(t, selectWith(t, f, func(o *Options) { o.load = stubLoader(fixedP(0)) }), "the unedited graph lock")

	f.writeLock(t, "logistic", func(l *control.Lock) { l.L1Mode = control.L1ModeCoverage })

	d := selectWith(t, f, func(o *Options) { o.load = stubLoader(fixedP(0)) })
	requireFullSuite(t, d, "policy_mismatch")
	if d.LockDigest != "" || d.Lambda != 0 {
		t.Errorf("LockDigest %q Lambda %v: a lock that did not verify is not in force", d.LockDigest, d.Lambda)
	}
}

// TestSelectExplicitModeDoesNotTransferALock: an explicit mode (eval, which
// never uses a lock) wins over the lock's, and then the digest built for the
// mode in use differs from the lock's.
func TestSelectExplicitModeDoesNotTransferALock(t *testing.T) {
	t.Parallel()
	f := coverageFixture(t)
	f.writeLock(t, "logistic", nil)

	d := selectWith(t, f, func(o *Options) {
		o.load = stubLoader(fixedP(0))
		o.Mode, o.ModeSet = impact.ModeCoverage, true
	})

	requireFullSuite(t, d, "policy_mismatch")
	if d.L1 == nil || d.L1.CoverageState != "fresh" {
		t.Errorf("L1 = %+v, want the explicit coverage mode to win over the lock's", d.L1)
	}
}

func TestSelectLockNamingAnUnknownModeIsCorrupt(t *testing.T) {
	t.Parallel()
	f, valid := logisticFixture(t, lockedYML, nil)
	valid.L1Mode = "hybrid"
	writeRawLock(t, f.lockPath(), valid)

	d := selectWith(t, f, nil)

	requireFullSuite(t, d, "lock_corrupt")
	if !hasWarning(d, "hybrid") {
		t.Errorf("Warnings = %v, want the unknown mode named", d.Warnings)
	}
}
