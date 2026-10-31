package engine

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/omorsi45/hazard/internal/control"
	"github.com/omorsi45/hazard/internal/impact"
	"github.com/omorsi45/hazard/internal/predict"
)

// The tests below guard properties the selection's safety rests on. Each one
// first proves that the same repository narrows when nothing is wrong, so a
// full-suite answer can only come from the guard it names.

func requireNarrowed(t *testing.T, d Decision, context string) {
	t.Helper()
	if d.FullSuite {
		t.Fatalf("%s: reason %q warnings %v, want a narrowed decision", context, d.AbstainReason, d.Warnings)
	}
}

func requireFullSuite(t *testing.T, d Decision, reason string) {
	t.Helper()
	if !d.FullSuite || !d.Abstained || d.AbstainReason != reason {
		t.Fatalf("FullSuite %v Abstained %v reason %q warnings %v, want the full suite with %q", d.FullSuite, d.Abstained, d.AbstainReason, d.Warnings, reason)
	}
	if len(d.Tests) != 0 {
		t.Errorf("Tests = %v, want none while FullSuite", ids(d.Tests))
	}
}

// flipDigestByte changes one hexadecimal character of the digest of the lock
// file at path.
func flipDigestByte(t *testing.T, path string) {
	t.Helper()
	data := readFileString(t, path)
	const marker = `"digest": "sha256:`
	i := strings.Index(data, marker)
	if i < 0 {
		t.Fatalf("no digest in %s:\n%s", path, data)
	}
	i += len(marker)
	flipped := byte('0')
	if data[i] == '0' {
		flipped = '1'
	}
	if err := os.WriteFile(path, []byte(data[:i]+string(flipped)+data[i+1:]), 0o644); err != nil {
		t.Fatal(err)
	}
}

// G01: a lock whose digest does not match its content is corrupt.
func TestGuardG01LockDigest(t *testing.T) {
	t.Parallel()
	f, _ := logisticFixture(t, lockedYML, nil)
	requireNarrowed(t, selectWith(t, f, nil), "the intact lock")

	flipDigestByte(t, f.lockPath())

	d := selectWith(t, f, nil)
	requireFullSuite(t, d, "lock_corrupt")
	if !hasWarning(d, "digest of the lock") {
		t.Errorf("Warnings = %v, want the digest named", d.Warnings)
	}
	if d.L1 == nil || !sameStrings(ids(d.Hypothetical), certainIDs) {
		t.Errorf("L1 %v Hypothetical %v, want the CERTAIN set kept for the record", d.L1, ids(d.Hypothetical))
	}
}

// G02: the model release moved since the threshold was fitted.
func TestGuardG02ModelRelease(t *testing.T) {
	f := lockedFixture(t, lockedYML+"predictor:\n  jev:\n    model: typesafe/jev-1.14\n", nil)
	f.logistic(t, defaultWeight, defaultBias)
	t.Setenv(f.config(t).Predictor.Jev.APIKeyEnv, "sk-test-engine-key")
	loader := func(o *Options) { o.load = stubLoader(fixedP(0)) } // no request is made

	f.writeLock(t, "ensemble", nil)
	requireNarrowed(t, selectWith(t, f, loader), "a lock for typesafe/jev-1.14")

	f.writeLock(t, "ensemble", func(l *control.Lock) { l.ModelVersion = "typesafe/jev-1.13" })

	d := selectWith(t, f, loader)
	requireFullSuite(t, d, "model_drift")
	if !hasWarning(d, "typesafe/jev-1.13") || !hasWarning(d, "typesafe/jev-1.14") {
		t.Errorf("Warnings = %v, want both releases named", d.Warnings)
	}
}

// G03: the rubric the lock was fitted under is not the running rubric.
func TestGuardG03Rubric(t *testing.T) {
	f := lockedFixture(t, lockedYML, nil)
	f.logistic(t, defaultWeight, defaultBias)
	t.Setenv(f.config(t).Predictor.Jev.APIKeyEnv, "sk-test-engine-key")
	loader := func(o *Options) { o.load = stubLoader(fixedP(0)) }

	f.writeLock(t, "ensemble", nil)
	requireNarrowed(t, selectWith(t, f, loader), "a lock for rubric r1")

	f.writeLock(t, "ensemble", func(l *control.Lock) { l.RubricVersion = predict.RubricR1 + "-bumped" })

	d := selectWith(t, f, loader)
	requireFullSuite(t, d, "rubric_mismatch")
	if !hasWarning(d, "rubric") {
		t.Errorf("Warnings = %v, want the rubric named", d.Warnings)
	}
}

// G04: the weights of the artifact are not the weights the threshold was
// fitted for. The replacement is sealed, so it reads without complaint.
func TestGuardG04FeatureWeights(t *testing.T) {
	t.Parallel()
	f, _ := logisticFixture(t, lockedYML, nil)
	requireNarrowed(t, selectWith(t, f, nil), "the artifact the lock was fitted for")

	f.logistic(t, defaultWeight, defaultBias+0.1)

	d := selectWith(t, f, nil)
	requireFullSuite(t, d, "feature_mismatch")
	if !hasWarning(d, "weights") {
		t.Errorf("Warnings = %v, want the weights named", d.Warnings)
	}
}

// G05: the predictor can add tests and never remove one. A predictor that is
// sure nothing breaks still leaves every CERTAIN and every always-run test.
func TestGuardG05MonotoneAuthority(t *testing.T) {
	t.Parallel()
	f, _ := logisticFixture(t, lockedYML, nil)
	quarantine(t, f, idSolo, "example.com/m/solo", "TestSolo")
	p := fixedP(0.0)

	d := selectWith(t, f, func(o *Options) { o.load = stubLoader(p) })

	requireNarrowed(t, d, "a predictor that answers 0 for every candidate")
	if p.calls != 1 {
		t.Fatalf("the predictor ran %d times, want once", p.calls)
	}
	if want := []string{idCore, idLeft, idRght, idSolo}; !sameStrings(ids(d.Tests), want) {
		t.Errorf("Tests = %v, want every CERTAIN test and the always-run test %v", ids(d.Tests), want)
	}
	if _, ok := byID(d.Tests, idTop); ok {
		t.Error("top is not CERTAIN and scored 0: it must not be selected")
	}
}

// G06: a shard the Jev API could not answer selects its candidates.
func TestGuardG06FailedShardSelectsItsCandidates(t *testing.T) {
	srv := newJevServer(t, "TestE2")
	f := lockedFixture(t, jevYML(srv, ""), extraFiles("e1", "e2", "e3"))
	t.Setenv(f.config(t).Predictor.Jev.APIKeyEnv, "sk-test-engine-key")
	f.writeLock(t, "jev", nil)
	e2 := extraID("e2")
	load := func(o *Options) { o.load = clientLoader(srv.Client()) }

	d := selectWith(t, f, load)

	requireNarrowed(t, d, "one failed shard out of five")
	if srv.requests.Load() < 6 { // five shards, and the failed one is tried twice
		t.Errorf("%d requests reached the server, want a request per shard and a retry of the failed one", srv.requests.Load())
	}
	if want := []string{idCore, e2, idLeft, idRght}; !sameStrings(ids(d.Tests), want) {
		t.Fatalf("Tests = %v, want the CERTAIN tests and the candidate of the failed shard %v", ids(d.Tests), want)
	}
	s, _ := byID(d.Tests, e2)
	if s.Source != "added" || s.P == nil || *s.P != 1 || s.Reason != "jev_failed" {
		t.Errorf("e2 = %+v (p %v), want an added test at p = 1 because its shard failed", s, s.P)
	}
	failed, scored := 0, 0
	for _, sc := range d.Scores {
		switch sc.Source {
		case "jev_failed":
			failed++
			if sc.TestID != e2 || sc.P != 1 {
				t.Errorf("failed score %+v, want e2 at 1", sc)
			}
		case "jev":
			scored++
		}
	}
	if failed != 1 || scored != 4 {
		t.Errorf("%d failed and %d scored candidates, want 1 and 4: %+v", failed, scored, d.Scores)
	}
	if !hasWarning(d, "503") {
		t.Errorf("Warnings = %v, want the failed shard reported", d.Warnings)
	}
	if d.CostUSD <= 0 {
		t.Errorf("CostUSD = %v, want the cost of the answered shards", d.CostUSD)
	}
}

// G07: a full-suite trigger wins over a valid lock and a predictor that is
// sure a test breaks. go.mod changes no package, so with the trigger gone the
// selection is only what the predictor names, and that one test out of five
// saves far more than min_saving_ratio: removing any trigger layer shows as a
// narrowed decision, not as an unrelated abstention.
func TestGuardG07TriggerForcesTheFullSuite(t *testing.T) {
	t.Parallel()
	touchedGoMod := map[string]string{"go.mod": "module example.com/m\n\ngo 1.21\n\n// touched\n"}
	build := func(t *testing.T, yml string) *fixture {
		t.Helper()
		f := newFixture(t, map[string]string{"hazard.yml": yml})
		f.write(t, touchedGoMod)
		f.indexNow(t, false)
		f.logistic(t, defaultWeight, defaultBias)
		f.writeLock(t, "logistic", nil)
		return f
	}
	// A predictor that is sure top breaks and that nothing else does.
	sureOfTop := func() *stubPredictor {
		return &stubPredictor{fn: func(_ context.Context, cands []impact.Candidate) ([]predict.Prob, error) {
			out := make([]predict.Prob, len(cands))
			for i, c := range cands {
				out[i] = predict.Prob{TestID: c.TestID, Source: "stub", Shard: -1}
				if c.TestID == idTop {
					out[i].P = 1
				}
			}
			return out, nil
		}}
	}

	// The premise: the same change and predictor, with go.mod not a trigger
	// (a pattern that matches nothing here), narrow to the one test.
	open := build(t, lockedYML+"repo:\n  full_suite_triggers: [\"Dockerfile*\"]\n")
	requireNarrowed(t, selectWith(t, open, func(o *Options) { o.load = stubLoader(sureOfTop()) }), "go.mod is not a trigger")

	f := build(t, lockedYML)
	p := sureOfTop()

	d := selectWith(t, f, func(o *Options) { o.load = stubLoader(p) })

	requireFullSuite(t, d, "trigger:go.mod:go.mod")
	if d.Hypothetical != nil {
		t.Errorf("Hypothetical = %v, want nil when L1 itself demands the full suite", ids(d.Hypothetical))
	}
	if d.L1 == nil || d.L1.FullReason != "trigger:go.mod:go.mod" {
		t.Errorf("L1 = %+v", d.L1)
	}
}

// TestGuardAuditLogThatCannotBeReadIsDrift is the guard of the ruling that
// unknown drift never narrows: a damaged audit log must not be read as an
// empty history.
func TestGuardAuditLogThatCannotBeReadIsDrift(t *testing.T) {
	t.Parallel()
	f, lock := logisticFixture(t, lockedYML, nil)
	writeAuditsFile(t, f, auditLine("s1", lock.Digest, nil, true))
	requireNarrowed(t, selectWith(t, f, nil), "an intact audit log")

	writeAuditsFile(t, f, auditLine("s1", lock.Digest, nil, true), "this line is not json", auditLine("s2", lock.Digest, nil, true))

	d := selectWith(t, f, nil)
	requireFullSuite(t, d, "drift")
	if !hasWarning(d, "drift_unknown: ") {
		t.Errorf("Warnings = %v, want drift_unknown", d.Warnings)
	}
}
