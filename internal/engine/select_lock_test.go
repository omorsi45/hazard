package engine

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/omorsi45/hazard/internal/control"
	"github.com/omorsi45/hazard/internal/impact"
	"github.com/omorsi45/hazard/internal/predict"
	"github.com/omorsi45/hazard/internal/store"
)

func sigmoid(x float64) float64 { return 1 / (1 + math.Exp(-x)) }

func TestSelectNarrowsWithAValidLock(t *testing.T) {
	t.Parallel()
	f, lock := logisticFixture(t, lockedYML, nil)

	d := selectWith(t, f, nil)

	if d.FullSuite || d.Abstained || d.AbstainReason != "" {
		t.Fatalf("FullSuite %v Abstained %v reason %q warnings %v, want a narrowed decision", d.FullSuite, d.Abstained, d.AbstainReason, d.Warnings)
	}
	if want := []string{idCore, idLeft, idRght, idTop}; !sameStrings(ids(d.Tests), want) {
		t.Fatalf("Tests = %v, want the CERTAIN tests plus the candidate scored above lambda %v", ids(d.Tests), want)
	}
	for _, s := range d.Tests[:3] {
		if s.Source != "certain" || s.Reason != "graph" || s.P != nil {
			t.Errorf("%s: source %q reason %q p %v, want certain/graph/nil", s.TestID, s.Source, s.Reason, s.P)
		}
	}
	top := d.Tests[3]
	if top.Source != "added" || top.Reason != "predicted" || top.P == nil || math.Abs(*top.P-sigmoid(-2)) > 1e-9 {
		t.Errorf("top = %+v (p %v), want an added test with p = sigmoid(-2)", top, top.P)
	}
	if top.Package != "example.com/m/top" || top.RunnerID != "TestTop" {
		t.Errorf("inventory fields of top = %q, %q", top.Package, top.RunnerID)
	}
	if d.LockDigest != lock.Digest || d.Lambda != lockLambda {
		t.Errorf("LockDigest %q Lambda %v, want the lock's %q and %v", d.LockDigest, d.Lambda, lock.Digest, lockLambda)
	}
	if !sameStrings(ids(d.Hypothetical), ids(d.Tests)) {
		t.Errorf("Hypothetical = %v, want what Decide selected", ids(d.Hypothetical))
	}
	if len(d.Scores) != 2 || d.Scores[0].TestID != idSolo || d.Scores[1].TestID != idTop || d.Scores[1].Source != "logistic" || d.Scores[1].Shard != -1 {
		t.Errorf("Scores = %+v, want solo then top, scored by the logistic model", d.Scores)
	}
	if d.EstMSSelected != 4 || d.EstMSFull != 5 || !hasWarning(d, "durations_unknown:5") {
		t.Errorf("estimates %d of %d, warnings %v; want 4 of 5 tests and the durations_unknown warning", d.EstMSSelected, d.EstMSFull, d.Warnings)
	}
	if d.CostUSD != 0 || d.L1 == nil || d.Change == nil {
		t.Errorf("CostUSD %v L1 %v Change %v", d.CostUSD, d.L1, d.Change)
	}
}

func TestSelectEstimatesFromTheRecordedDurations(t *testing.T) {
	t.Parallel()
	f, _ := logisticFixture(t, lockedYML, nil)
	seedDurations(t, f.dir, []durationRow{
		{idCore, "pass", 100}, {idLeft, "pass", 100}, {idRght, "pass", 100}, {idTop, "pass", 100}, {idSolo, "pass", 1000},
	})

	d := selectWith(t, f, nil)

	if d.FullSuite {
		t.Fatalf("reason %q warnings %v", d.AbstainReason, d.Warnings)
	}
	if d.EstMSSelected != 400 || d.EstMSFull != 1400 {
		t.Errorf("estimates %d of %d, want 400 of 1400 from the recorded medians", d.EstMSSelected, d.EstMSFull)
	}
	if hasWarning(d, "durations_unknown") {
		t.Errorf("Warnings = %v, want no durations_unknown when every test is measured", d.Warnings)
	}
}

func TestSelectResolvesARelativeLockPathAgainstTheRepository(t *testing.T) {
	t.Parallel()
	f, lock := logisticFixture(t, lockedYML, nil)
	if err := os.MkdirAll(filepath.Join(f.dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(f.lockPath(), filepath.Join(f.dir, "sub", "my.lock")); err != nil {
		t.Fatal(err)
	}

	d := selectWith(t, f, func(o *Options) { o.LockPath = filepath.Join("sub", "my.lock") })

	if d.FullSuite || d.LockDigest != lock.Digest {
		t.Errorf("FullSuite %v reason %q digest %q, want the lock found at sub/my.lock", d.FullSuite, d.AbstainReason, d.LockDigest)
	}
}

// writeRawLock writes l with a correct digest, whatever else is wrong with it.
func writeRawLock(t *testing.T, path string, l control.Lock) {
	t.Helper()
	l.Digest = l.ComputeDigest()
	data, err := json.MarshalIndent(l, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestSelectLockFailuresAreTheFullSuiteWithL1Kept: every refusal of the lock
// still carries the deterministic layer, because train and calibrate read the
// candidates from abstaining decisions.
func TestSelectLockFailuresAreTheFullSuiteWithL1Kept(t *testing.T) {
	t.Parallel()
	f, valid := logisticFixture(t, lockedYML, nil)
	dir := t.TempDir()

	for _, tc := range []struct {
		name   string
		reason string
		setup  func(t *testing.T, path string)
	}{
		{"no lock file", "no_lock", func(*testing.T, string) {}},
		{"not JSON", "lock_corrupt", func(t *testing.T, path string) {
			if err := os.WriteFile(path, []byte("this is not json"), 0o644); err != nil {
				t.Fatal(err)
			}
		}},
		{"an empty object", "lock_corrupt", func(t *testing.T, path string) {
			if err := os.WriteFile(path, []byte("{}"), 0o644); err != nil {
				t.Fatal(err)
			}
		}},
		{"an unknown field", "lock_corrupt", func(t *testing.T, path string) {
			if err := os.WriteFile(path, []byte(`{"version":1,"surprise":true}`), 0o644); err != nil {
				t.Fatal(err)
			}
		}},
		{"an unsupported version with an intact digest", "lock_corrupt", func(t *testing.T, path string) {
			l := valid
			l.Version = 2
			writeRawLock(t, path, l)
		}},
		{"a predictor kind the engine does not know", "lock_corrupt", func(t *testing.T, path string) {
			l := valid
			l.Predictor = "xgboost"
			writeRawLock(t, path, l)
		}},
		{"a lambda above one with an intact digest", "lock_corrupt", func(t *testing.T, path string) {
			l := valid
			l.Lambda = 2
			writeRawLock(t, path, l)
		}},
		{"a lambda of zero with an intact digest", "lock_corrupt", func(t *testing.T, path string) {
			l := valid
			l.Lambda = 0
			writeRawLock(t, path, l)
		}},
		{"a directory in place of the lock", "lock_error", func(t *testing.T, path string) {
			if err := os.Mkdir(path, 0o755); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(dir, strings.ReplaceAll(tc.name, " ", "_")+".lock")
			tc.setup(t, path)

			d := selectWith(t, f, func(o *Options) { o.LockPath = path })

			if !d.FullSuite || !d.Abstained || d.AbstainReason != tc.reason {
				t.Fatalf("FullSuite %v Abstained %v reason %q warnings %v, want the full suite with %s", d.FullSuite, d.Abstained, d.AbstainReason, d.Warnings, tc.reason)
			}
			if len(d.Tests) != 0 {
				t.Errorf("Tests = %v, want none while FullSuite", ids(d.Tests))
			}
			if d.L1 == nil || d.Change == nil || len(d.L1.Certain) != 3 {
				t.Errorf("L1 %+v Change %v, want the deterministic layer kept", d.L1, d.Change)
			}
			if want := certainIDs; !sameStrings(ids(d.Hypothetical), want) {
				t.Errorf("Hypothetical = %v, want the CERTAIN set %v", ids(d.Hypothetical), want)
			}
			if d.LockDigest != "" || d.Lambda != 0 {
				t.Errorf("LockDigest %q Lambda %v: a lock that did not verify is not in force", d.LockDigest, d.Lambda)
			}
		})
	}
}

func TestSelectPredictorUnavailableWhenTheArtifactIsGone(t *testing.T) {
	t.Parallel()
	f, _ := logisticFixture(t, lockedYML, nil)
	if err := os.Remove(filepath.Join(f.dir, ".hazard", "logistic.json")); err != nil {
		t.Fatal(err)
	}

	d := selectWith(t, f, nil)

	if !d.FullSuite || d.AbstainReason != "predictor_unavailable" {
		t.Fatalf("FullSuite %v reason %q, want predictor_unavailable", d.FullSuite, d.AbstainReason)
	}
	if !hasWarning(d, "logistic.json") {
		t.Errorf("Warnings = %v, want the artifact error", d.Warnings)
	}
	if d.L1 == nil || !sameStrings(ids(d.Hypothetical), certainIDs) || len(d.Tests) != 0 {
		t.Errorf("L1 %v Hypothetical %v Tests %v", d.L1, ids(d.Hypothetical), ids(d.Tests))
	}
}

func TestSelectMissingKeyDoesNotReplaceTheLockedPredictor(t *testing.T) {
	f := lockedFixture(t, lockedYML, nil)
	f.logistic(t, defaultWeight, defaultBias)
	env := f.config(t).Predictor.Jev.APIKeyEnv
	t.Setenv(env, "sk-test-engine-key")
	f.writeLock(t, "ensemble", nil)
	t.Setenv(env, "")

	d := selectWith(t, f, nil)

	if !d.FullSuite || d.AbstainReason != "predictor_unavailable" {
		t.Fatalf("FullSuite %v reason %q, want predictor_unavailable and not the logistic predictor in its place", d.FullSuite, d.AbstainReason)
	}
	want := env + " unset: the locked predictor ensemble is unavailable; calibrate a logistic lock to select offline"
	if !hasWarning(d, want) {
		t.Errorf("Warnings = %v, want %q", d.Warnings, want)
	}
	if len(d.Scores) != 0 {
		t.Errorf("Scores = %+v: nothing was asked", d.Scores)
	}
}

func TestSelectPredictorErrors(t *testing.T) {
	t.Parallel()
	f, _ := logisticFixture(t, lockedYML, nil)
	boom := errors.New("boom")

	for _, tc := range []struct {
		name    string
		predict func(context.Context, []impact.Candidate) ([]predict.Prob, error)
		reason  string
		warning string
	}{
		{"a probability that is NaN", func(_ context.Context, c []impact.Candidate) ([]predict.Prob, error) {
			out := make([]predict.Prob, len(c))
			for i := range c {
				out[i] = predict.Prob{TestID: c[i].TestID, P: math.NaN(), Source: "stub", Shard: -1}
			}
			return out, nil
		}, "predictor_error", "not finite"},
		{"an answer for the wrong number of tests", func(context.Context, []impact.Candidate) ([]predict.Prob, error) {
			return []predict.Prob{{TestID: "x", P: 0, Source: "stub"}}, nil
		}, "predictor_error", "probabilities for"},
		{"an error", func(context.Context, []impact.Candidate) ([]predict.Prob, error) { return nil, boom }, "predictor_error", "boom"},
		{"a deadline that the predictor reports", func(context.Context, []impact.Candidate) ([]predict.Prob, error) {
			return nil, context.DeadlineExceeded
		}, "predictor_error", "deadline"},
		{"a served model of another release", func(context.Context, []impact.Candidate) ([]predict.Prob, error) {
			return nil, predict.ErrModelDrift
		}, "model_drift", "served model"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p := &stubPredictor{fn: tc.predict}

			d := selectWith(t, f, func(o *Options) { o.load = stubLoader(p) })

			if !d.FullSuite || !d.Abstained || d.AbstainReason != tc.reason {
				t.Fatalf("FullSuite %v reason %q warnings %v, want the full suite with %s", d.FullSuite, d.AbstainReason, d.Warnings, tc.reason)
			}
			if !hasWarning(d, tc.warning) {
				t.Errorf("Warnings = %v, want one containing %q", d.Warnings, tc.warning)
			}
			if len(d.Tests) != 0 || d.L1 == nil || !sameStrings(ids(d.Hypothetical), certainIDs) {
				t.Errorf("Tests %v L1 %v Hypothetical %v; an unusable answer selects nothing narrower than the CERTAIN set", ids(d.Tests), d.L1, ids(d.Hypothetical))
			}
			if d.LockDigest == "" || d.Lambda != lockLambda {
				t.Errorf("LockDigest %q Lambda %v: the lock was verified before the predictor ran", d.LockDigest, d.Lambda)
			}
		})
	}
}

func TestSelectBudgetIsADeadlineForThePredictor(t *testing.T) {
	t.Parallel()
	f, _ := logisticFixture(t, lockedYML, nil)

	t.Run("the predictor gets the remaining budget as its deadline", func(t *testing.T) {
		t.Parallel()
		p := fixedP(0)

		d := selectWith(t, f, func(o *Options) { o.load = stubLoader(p); o.BudgetMS = 60000 })

		if d.FullSuite {
			t.Fatalf("reason %q warnings %v", d.AbstainReason, d.Warnings)
		}
		if p.calls != 1 {
			t.Fatalf("the predictor ran %d times, want once", p.calls)
		}
		deadline, ok := p.ctx.Deadline()
		if left := time.Until(deadline); !ok || left > 60*time.Second || left < 50*time.Second {
			t.Errorf("the predictor's deadline is in %v (set %v), want just under the 60 s budget", left, ok)
		}
	})
	t.Run("a spent budget is predictor_error, never a narrowed selection", func(t *testing.T) {
		t.Parallel()
		p := fixedP(0)

		d := selectWith(t, f, func(o *Options) { o.load = stubLoader(p); o.BudgetMS = 1 })

		if p.calls != 0 {
			t.Errorf("the predictor ran %d time(s) after the budget was spent", p.calls)
		}
		if !d.FullSuite || d.AbstainReason != "predictor_error" || !hasWarning(d, "deadline") || len(d.Tests) != 0 {
			t.Errorf("FullSuite %v reason %q tests %v warnings %v, want predictor_error with a deadline warning", d.FullSuite, d.AbstainReason, ids(d.Tests), d.Warnings)
		}
	})
}

func TestSelectionBudget(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		ms   int
		want time.Duration
	}{{0, 5 * time.Second}, {-3, 5 * time.Second}, {700, 700 * time.Millisecond}, {60000, time.Minute}} {
		if got := selectionBudget(Options{BudgetMS: tc.ms}); got != tc.want {
			t.Errorf("BudgetMS %d: budget %v, want %v", tc.ms, got, tc.want)
		}
	}
}

func TestSelectCancelDuringPredictIsAnErrorNotADecision(t *testing.T) {
	t.Parallel()
	f, _ := logisticFixture(t, lockedYML, nil)
	ctx, cancel := context.WithCancel(context.Background())
	p := &stubPredictor{fn: func(ctx context.Context, _ []impact.Candidate) ([]predict.Prob, error) {
		cancel()
		<-ctx.Done()
		return nil, ctx.Err()
	}}

	_, err := Select(ctx, Options{RepoDir: f.dir, BudgetMS: generousBudgetMS, load: stubLoader(p)})

	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
}

func TestSelectDriftAbstains(t *testing.T) {
	t.Parallel()
	f, lock := logisticFixture(t, lockedYML, nil)
	miss := []string{idTop}

	t.Run("a confirmed miss under this lock", func(t *testing.T) {
		writeAuditsFile(t, f, auditLine("s1", lock.Digest, miss, true))
		defer func() { _ = os.Remove(auditsPath(f.dir)) }()

		d := selectWith(t, f, nil)

		if !d.FullSuite || d.AbstainReason != "drift" || len(d.Tests) != 0 {
			t.Fatalf("FullSuite %v reason %q tests %v, want drift", d.FullSuite, d.AbstainReason, ids(d.Tests))
		}
		if want := []string{idCore, idLeft, idRght, idTop}; !sameStrings(ids(d.Hypothetical), want) {
			t.Errorf("Hypothetical = %v, want what the controller built before it abstained %v", ids(d.Hypothetical), want)
		}
		if !hasWarning(d, "drift: alarm") {
			t.Errorf("Warnings = %v, want the drift alarm", d.Warnings)
		}
	})
	t.Run("a miss under another lock is another lock's evidence", func(t *testing.T) {
		writeAuditsFile(t, f, auditLine("s1", "sha256:an-older-lock", miss, true))
		defer func() { _ = os.Remove(auditsPath(f.dir)) }()

		if d := selectWith(t, f, nil); d.FullSuite {
			t.Errorf("reason %q warnings %v, want a narrowed decision", d.AbstainReason, d.Warnings)
		}
	})
	t.Run("an audit log that cannot be read is an unknown drift", func(t *testing.T) {
		writeAuditsFile(t, f, auditLine("s1", lock.Digest, nil, true), "{this line is broken", auditLine("s2", lock.Digest, nil, true))
		defer func() { _ = os.Remove(auditsPath(f.dir)) }()

		d := selectWith(t, f, nil)

		if !d.FullSuite || d.AbstainReason != "drift" {
			t.Fatalf("FullSuite %v reason %q, want drift: unknown never narrows", d.FullSuite, d.AbstainReason)
		}
		if !hasWarning(d, "drift_unknown: ") {
			t.Errorf("Warnings = %v, want drift_unknown", d.Warnings)
		}
	})
	t.Run("a truncated last line is skipped with a warning", func(t *testing.T) {
		writeAudits(t, f.dir, auditLine("s1", lock.Digest, nil, true)+"\n"+`{"selection_id":"s2","lock_`)
		defer func() { _ = os.Remove(auditsPath(f.dir)) }()

		d := selectWith(t, f, nil)

		if d.FullSuite || !hasWarning(d, "truncated") {
			t.Errorf("FullSuite %v warnings %v, want a narrowed decision and a warning about the truncated line", d.FullSuite, d.Warnings)
		}
	})
}

func TestSelectDriftThatIsNotEscalatedOnlyWarns(t *testing.T) {
	t.Parallel()
	yml := lockedYML + "control:\n  drift:\n    escalate_if_ucb_above_alpha: false\n"
	f, lock := logisticFixture(t, yml, nil)

	for name, lines := range map[string][]string{
		"a confirmed miss": {auditLine("s1", lock.Digest, []string{idTop}, true)},
	} {
		writeAuditsFile(t, f, lines...)

		d := selectWith(t, f, nil)

		if d.FullSuite {
			t.Errorf("%s: reason %q, want a narrowed decision because drift is not escalated", name, d.AbstainReason)
		}
		if !hasWarning(d, "drift") {
			t.Errorf("%s: Warnings = %v, want the drift status as a warning", name, d.Warnings)
		}
	}
}

// TestSelectUnreadableAuditLogIsDriftEvenWhenNotEscalated: the flag
// control.drift.escalate_if_ucb_above_alpha governs the escalation of an upper
// bound above alpha. An audit log that cannot be read is no bound at all but an
// unknown, and unknown never narrows.
func TestSelectUnreadableAuditLogIsDriftEvenWhenNotEscalated(t *testing.T) {
	t.Parallel()
	yml := lockedYML + "control:\n  drift:\n    escalate_if_ucb_above_alpha: false\n"
	f, lock := logisticFixture(t, yml, nil)
	writeAuditsFile(t, f, auditLine("s1", lock.Digest, nil, true))
	requireNarrowed(t, selectWith(t, f, nil), "an intact audit log")

	writeAuditsFile(t, f, "{broken")

	d := selectWith(t, f, nil)
	requireFullSuite(t, d, "drift")
	if !hasWarning(d, "drift_unknown: ") {
		t.Errorf("Warnings = %v, want drift_unknown", d.Warnings)
	}
}

func TestSelectControllerAbstentionKeepsWhatItWouldHaveSelected(t *testing.T) {
	t.Parallel()
	f, _ := logisticFixture(t, lockedYML+"control:\n  min_saving_ratio: 0.9\n", nil)

	d := selectWith(t, f, nil)

	if !d.FullSuite || d.AbstainReason != "not_worth_it" || len(d.Tests) != 0 {
		t.Fatalf("FullSuite %v reason %q tests %v, want not_worth_it", d.FullSuite, d.AbstainReason, ids(d.Tests))
	}
	if want := []string{idCore, idLeft, idRght, idTop}; !sameStrings(ids(d.Hypothetical), want) {
		t.Errorf("Hypothetical = %v, want the narrowed selection %v", ids(d.Hypothetical), want)
	}
	if d.LockDigest == "" || d.Lambda != lockLambda || len(d.Scores) != 2 {
		t.Errorf("LockDigest %q Lambda %v Scores %v", d.LockDigest, d.Lambda, d.Scores)
	}
}

func TestSelectOptionsThatChangeThePolicyVoidTheLock(t *testing.T) {
	t.Parallel()
	f, _ := logisticFixture(t, lockedYML, nil)
	staticPath := filepath.Join(f.dir, ".hazard", "static.json")
	writeStaticArtifact(t, staticPath, 0.01, 0.5)

	for name, tc := range map[string]struct {
		mutate func(*Options)
		reason string // "" means the lock still applies
	}{
		"no override":                  {func(*Options) {}, ""},
		"the predictor the lock names": {func(o *Options) { o.Predictor = "logistic" }, ""},
		"a spend cap":                  {func(o *Options) { o.BudgetUSD = 0.5 }, ""},
		"a candidate cap":              {func(o *Options) { o.MaxCandidates = 1 }, "policy_mismatch"},
		"a redaction level":            {func(o *Options) { o.Redact = "none" }, "policy_mismatch"},
		"another predictor":            {func(o *Options) { o.Predictor = "static" }, "model_drift"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			d := selectWith(t, f, tc.mutate)

			if tc.reason == "" && d.FullSuite {
				t.Errorf("reason %q warnings %v, want the lock to apply", d.AbstainReason, d.Warnings)
			}
			if tc.reason != "" && (!d.FullSuite || d.AbstainReason != tc.reason || len(d.Tests) != 0) {
				t.Errorf("FullSuite %v reason %q tests %v, want %s", d.FullSuite, d.AbstainReason, ids(d.Tests), tc.reason)
			}
		})
	}
}

func TestSelectRecallOverrideIsRejected(t *testing.T) {
	t.Parallel()
	f, _ := logisticFixture(t, lockedYML, nil)

	d, err := Select(context.Background(), Options{RepoDir: f.dir, Recall: 0.9})

	if !errors.Is(err, ErrRecallOverride) {
		t.Fatalf("err = %v, want ErrRecallOverride", err)
	}
	if !strings.Contains(err.Error(), "recall override is not supported without recalibration") {
		t.Errorf("message = %q", err)
	}
	if d.ID != "" || d.FullSuite || d.Tests != nil {
		t.Errorf("an invalid invocation must not yield a decision: %+v", d)
	}
}

func TestSelectShadowKeepsTheNarrowedSelection(t *testing.T) {
	t.Parallel()
	f, _ := logisticFixture(t, lockedYML, nil)

	d := selectWith(t, f, func(o *Options) { o.Shadow = true })

	if !d.FullSuite || d.AbstainReason != "shadow" || len(d.Tests) != 0 {
		t.Fatalf("FullSuite %v reason %q tests %v, want the shadow run set", d.FullSuite, d.AbstainReason, ids(d.Tests))
	}
	if want := []string{idCore, idLeft, idRght, idTop}; !sameStrings(ids(d.Hypothetical), want) {
		t.Errorf("Hypothetical = %v, want %v", ids(d.Hypothetical), want)
	}
}

func TestSelectAlwaysRunTestsJoinTheSelection(t *testing.T) {
	t.Parallel()
	f, _ := logisticFixture(t, lockedYML, nil)
	quarantine(t, f, idSolo, "example.com/m/solo", "TestSolo")

	d := selectWith(t, f, func(o *Options) { o.load = stubLoader(fixedP(0)) })

	if d.FullSuite {
		t.Fatalf("reason %q warnings %v", d.AbstainReason, d.Warnings)
	}
	solo, ok := byID(d.Tests, idSolo)
	if !ok || solo.Source != "always_run" || solo.Reason != "quarantined" || solo.P != nil {
		t.Errorf("solo = %+v (selected %v), want an always_run test with the history reason", solo, ok)
	}
}

// quarantine marks a test as quarantined in the history database.
func quarantine(t *testing.T, f *fixture, id, pkg, name string) {
	t.Helper()
	s, err := store.Open(filepath.Join(f.dir, ".hazard", "hazard.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB().Exec(`INSERT INTO tests (id, lang, package, name, file, first_seen, last_seen, quarantined, unstable_id) VALUES (?, 'go', ?, ?, 'x_test.go', '2026-01-01', '2026-01-01', 1, 0)`,
		id, pkg, name); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
}
