package engine

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"slices"
	"time"

	"github.com/omorsi45/hazard/internal/change"
	"github.com/omorsi45/hazard/internal/config"
	"github.com/omorsi45/hazard/internal/control"
	"github.com/omorsi45/hazard/internal/impact"
	"github.com/omorsi45/hazard/internal/index"
	"github.com/omorsi45/hazard/internal/predict"
)

const (
	reasonLockCorrupt          = "lock_corrupt"
	reasonPredictorUnavailable = "predictor_unavailable"
	reasonPredictorError       = "predictor_error"
	reasonModelDrift           = "model_drift"
	reasonPolicyMismatch       = "policy_mismatch"
	reasonDryRun               = "dry_run"

	// supportedLockVersion is the only lock schema version control.WriteLock
	// writes and this engine trusts.
	supportedLockVersion = 1

	// defaultBudgetMS is the selection time budget when Options.BudgetMS is
	// not set: the default of --budget-ms.
	defaultBudgetMS = 5000

	// reasonPredicted is the reason of an added test whose probability reached
	// the threshold.
	reasonPredicted = "predicted"
)

// stage is the input of the controller stage: everything the deterministic
// layer produced, and the configuration with the request's overrides applied.
type stage struct {
	opts   Options
	cfg    config.Config // the overrides of opts are applied: the policy and the predictor see these
	start  time.Time
	cs     *change.ChangeSet
	m      *index.Manifest // the inventory L1 ran over, head tests included
	res    *impact.Result
	always map[string]string
	mode   impact.Mode // the mode L1 ran in: the policy digest is built with it
	lock   lockState   // the lock read before L1, which chose the mode
}

// effectiveConfig applies the overrides of a request to the configuration that
// the policy and the predictor are built from. A candidate cap or a redaction
// level that differs from the configured one changes the policy digest, so a
// lock calibrated without it fails with policy_mismatch instead of silently
// applying to another policy. The spend cap is not part of the policy.
func effectiveConfig(cfg config.Config, opts Options) config.Config {
	if opts.MaxCandidates > 0 {
		cfg.Impact.MaxCandidates = opts.MaxCandidates
	}
	if opts.Redact != "" {
		cfg.Privacy.Redact = opts.Redact
	}
	if opts.BudgetUSD > 0 {
		cfg.Predictor.Jev.BudgetUSDPerRun = opts.BudgetUSD
	}
	return cfg
}

// run is the controller stage: it reads and verifies the lock, loads and asks
// the predictor, judges drift and durations and lets control.Decide choose.
// Every refusal is a full-suite decision; the error is non-nil only for a
// canceled context.
func (s stage) run(ctx context.Context, d *Decision) error {
	if s.opts.DryRunPayload {
		s.dryRun(d)
		return nil
	}
	lock := s.lock.lock
	if reason := s.lock.reason; reason != "" {
		d.full(reason, nonEmpty(s.lock.detail)...)
		return nil
	}
	if !isKnownKind(lock.Predictor) {
		d.full(reasonLockCorrupt, fmt.Sprintf("the lock names the predictor %q, which this version does not know", lock.Predictor))
		return nil
	}

	kind := s.opts.Predictor
	if kind == "" {
		kind = lock.Predictor
	}
	load := s.opts.load
	if load == nil {
		load = LoadPredictor
	}
	p, id, err := load(s.opts.RepoDir, s.cfg, kind)
	if err != nil {
		d.full(reasonPredictorUnavailable, err.Error())
		return nil
	}
	id.PolicyDigest = BuildPolicy(s.cfg, kind, s.mode).Digest()
	if reason := lock.Verify(id); reason != "" {
		d.full(reason, mismatchDetail(lock, id, reason))
		return nil
	}
	d.LockDigest, d.Lambda = lock.Digest, lock.Lambda

	cands, scores, ok, err := s.score(ctx, p, d)
	if err != nil || !ok {
		return err
	}
	d.Scores = scores
	for _, sc := range scores {
		d.CostUSD += sc.CostUSD
	}

	status, warnings := driftFor(s.opts.RepoDir, s.cfg, lock)
	d.Warnings = append(d.Warnings, warnings...)

	testMS, err := testDurations(ctx, s.opts.RepoDir)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		d.Warnings = append(d.Warnings, "test durations are unknown and imputed: "+err.Error())
	}

	in := ControllerInput(s.cfg, s.m, s.res, cands, lock.Lambda, testMS, status)
	in.TriggersFull, in.FullReason = s.cs.TriggersFull, s.res.FullReason
	dec := control.Decide(in)

	d.EstMSSelected, d.EstMSFull = dec.EstMSSelected, dec.EstMSFull
	d.Warnings = append(d.Warnings, dec.Warnings...)
	if dec.Members != nil { // Decide built a selection, even if it then abstained
		d.Hypothetical = s.selected(dec.Members, scores)
	}
	if dec.FullSuite {
		d.full(dec.Reason)
		return nil
	}
	d.Tests = slices.Clone(d.Hypothetical)
	return nil
}

// selectionBudget is the time a selection may take: Options.BudgetMS, else the
// default of --budget-ms.
func selectionBudget(opts Options) time.Duration {
	ms := opts.BudgetMS
	if ms <= 0 {
		ms = defaultBudgetMS
	}
	return time.Duration(ms) * time.Millisecond
}

// score asks the predictor within the time budget that is left. ok is false
// when the decision is already made (a full suite) or err is set. An answer
// that cannot be used is the full suite, never a narrowed selection: a spent
// budget and a deadline are predictor_error, a served model of another
// release is model_drift. A canceled context is an error, not a decision.
func (s stage) score(ctx context.Context, p predict.Predictor, d *Decision) (cands []control.Candidate, scores []TestScore, ok bool, err error) {
	budget := selectionBudget(s.opts)
	remaining := budget - time.Since(s.start)
	if remaining <= 0 {
		d.full(reasonPredictorError, fmt.Sprintf("deadline: the %d ms budget was spent before the predictor ran", budget.Milliseconds()))
		return nil, nil, false, nil
	}
	pctx, cancel := context.WithTimeout(ctx, remaining)
	defer cancel()

	cands, scores, err = Score(pctx, p, s.cs, s.res.Candidates)
	d.Warnings = append(d.Warnings, predictorWarnings(p)...)
	switch {
	case err == nil:
		return cands, scores, true, nil
	case ctx.Err() != nil:
		return nil, nil, false, ctx.Err()
	case errors.Is(err, predict.ErrModelDrift):
		d.full(reasonModelDrift, err.Error())
	case errors.Is(err, context.DeadlineExceeded) || pctx.Err() != nil:
		d.full(reasonPredictorError, "deadline: "+err.Error())
	default:
		d.full(reasonPredictorError, err.Error())
	}
	return nil, nil, false, nil
}

// readLock reads the lock at path. A non-empty reason means no lock can be
// used: no_lock (there is none), lock_error (it exists but cannot be read) or
// lock_corrupt (it cannot be parsed, or is of a version this engine does not
// know). detail explains the reason, if there is more to say.
func readLock(path string) (l control.Lock, reason, detail string) {
	_, err := os.Stat(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return l, reasonNoLock, ""
	case err != nil:
		return l, reasonLockError, "lock file could not be inspected: " + err.Error()
	}
	l, err = control.ReadLock(path)
	var pathErr *fs.PathError
	switch {
	case errors.As(err, &pathErr):
		return l, reasonLockError, "lock file could not be read: " + err.Error()
	case err != nil:
		return l, reasonLockCorrupt, err.Error()
	case l.Version != supportedLockVersion:
		return l, reasonLockCorrupt, fmt.Sprintf("lock %s has version %d, want %d", path, l.Version, supportedLockVersion)
	case !(l.Lambda > 0 && l.Lambda < 1):
		// control.WriteLock never writes such a lock. A threshold of one or more
		// would select no candidate at all, and the digest only detects damage:
		// it cannot vouch for a file somebody edited and re-sealed.
		return l, reasonLockCorrupt, fmt.Sprintf("lock %s has the threshold lambda %v, want a value in (0, 1)", path, l.Lambda)
	case l.L1Mode != control.L1ModeGraph && l.L1Mode != control.L1ModeCoverage:
		return l, reasonLockCorrupt, fmt.Sprintf("lock %s names the L1 mode %q, want %q or %q", path, l.L1Mode, control.L1ModeGraph, control.L1ModeCoverage)
	}
	return l, "", ""
}

// lockState is what reading the lock gave: the lock, or the reason none can
// be used. It is read once, before L1, because the lock decides the L1 mode.
type lockState struct {
	lock   control.Lock
	reason string // as readLock: no_lock, lock_error or lock_corrupt; empty when lock is usable
	detail string
}

// loadLock reads the lock the controller will verify for opts.
func loadLock(opts Options) lockState {
	l, reason, detail := readLock(controllerLockPath(opts))
	return lockState{lock: l, reason: reason, detail: detail}
}

// l1Mode is the mode L1 runs in. An explicit Options.Mode wins (the evaluation
// harness, which never uses a lock). Otherwise a lock that parses decides:
// its threshold was certified over the floor of the mode it was calibrated
// in, and only that floor may be narrowed from. A lock calibrated with
// coverage cannot be honored by an index without it, which is a
// policy_mismatch. With no usable lock the mode follows the index: coverage
// when the index has it, else graph.
func l1Mode(opts Options, hasCoverage bool, ls lockState) (mode impact.Mode, reason, detail string) {
	switch {
	case opts.ModeSet:
		return opts.Mode, "", ""
	case ls.reason == "" && ls.lock.L1Mode == control.L1ModeGraph:
		return impact.ModeGraph, "", ""
	case ls.reason == "" && ls.lock.L1Mode == control.L1ModeCoverage:
		if !hasCoverage {
			return impact.ModeGraph, reasonPolicyMismatch, "the lock was calibrated in coverage mode but the index has no coverage"
		}
		return impact.ModeCoverage, "", ""
	case hasCoverage:
		return impact.ModeCoverage, "", ""
	}
	return impact.ModeGraph, "", ""
}

func isKnownKind(kind string) bool {
	switch kind {
	case kindStatic, kindLogistic, kindJev, kindEnsemble:
		return true
	}
	return false
}

func nonEmpty(s string) []string {
	if s == "" {
		return nil
	}
	return []string{s}
}

// mismatchDetail says which part of the lock disagrees with the running
// system: the guarantee of the threshold does not transfer, and the user
// should see why.
func mismatchDetail(l control.Lock, id control.Identity, reason string) string {
	switch reason {
	case reasonLockCorrupt:
		return "the digest of the lock does not match its content"
	case reasonModelDrift:
		return fmt.Sprintf("the lock was fitted for the model %q but the predictor reports %q", l.ModelVersion, id.ModelRelease)
	case "rubric_mismatch":
		return fmt.Sprintf("the lock was fitted under the rubric %q but the running rubric is %q", l.RubricVersion, id.RubricVersion)
	case "feature_mismatch":
		return fmt.Sprintf("the lock was fitted for the features %q with the weights %q but the running ones are %q and %q",
			l.FeatureVersion, l.WeightsDigest, id.FeatureVersion, id.WeightsDigest)
	case "policy_mismatch":
		return fmt.Sprintf("the lock was fitted for the predictor %q under the policy %q but the running ones are %q and %q",
			l.Predictor, l.PolicyDigest, id.Predictor, id.PolicyDigest)
	}
	return "the lock does not match the running system: " + reason
}

// predictorWarnings are the warnings the last call of p recorded: a failed
// shard, a spent budget, a judgment that could not be cached.
func predictorWarnings(p predict.Predictor) []string {
	switch v := p.(type) {
	case *predict.Jev:
		return v.LastStats().Warnings
	case predict.Ensemble:
		return predictorWarnings(v.Remote)
	}
	return nil
}

// selected turns the controller's members into selections, enriched from the
// inventory. A certain test keeps the reason L1 gave it, an always-run test
// the reason of the history database, and an added test says whether its
// probability reached the threshold or the predictor gave no usable score.
func (s stage) selected(members []control.Member, scores []TestScore) []Selected {
	byID := make(map[string]index.Test, len(s.m.Tests))
	for _, t := range s.m.Tests {
		byID[t.ID] = t
	}
	certain := make(map[string]string, len(s.res.Certain))
	for _, mem := range s.res.Certain {
		certain[mem.TestID] = mem.Reason
	}
	source := make(map[string]string, len(scores))
	for _, sc := range scores {
		source[sc.TestID] = sc.Source
	}
	out := make([]Selected, 0, len(members))
	for _, m := range members {
		sel := Selected{TestID: m.TestID, Source: m.Source, P: m.P}
		switch m.Source {
		case sourceCertain:
			sel.Reason = certain[m.TestID]
		case sourceAlwaysRun:
			sel.Reason = s.always[m.TestID]
			if sel.Reason == "" {
				sel.Reason = sourceAlwaysRun
			}
		case "added":
			sel.Reason = reasonPredicted
			if isFailureSource(source[m.TestID]) {
				sel.Reason = source[m.TestID]
			}
		default:
			sel.Reason = m.Source
		}
		if t, ok := byID[m.TestID]; ok {
			sel.Package, sel.RunnerID = t.Package, t.RunnerID
		}
		out = append(out, sel)
	}
	return out
}

// dryRun builds the payload that would be sent, and nothing else: no lock is
// verified, no key is needed and no request is made. The decision is the full
// suite. The active predictor is the one the request names, else the one the
// lock names; only jev and ensemble send anything. Without a payload the
// caller reports an empty one.
func (s stage) dryRun(d *Decision) {
	d.full(reasonDryRun)
	kind := s.opts.Predictor
	if kind == "" {
		if s.lock.reason == "" {
			kind = s.lock.lock.Predictor
		}
	}
	if kind != kindJev && kind != kindEnsemble {
		return
	}
	bodies, err := predict.DryRunPayload(jevOptions(s.cfg, nil), s.cs, s.res.Candidates)
	if err != nil {
		d.Warnings = append(d.Warnings, "dry run: no payload can be built: "+err.Error())
		return
	}
	d.Payload = joinRequests(bodies)
}

// joinRequests is {"requests":[<body>,...]} with every body byte for byte as
// it would be sent: the bodies are not decoded and encoded again, which would
// change their escaping.
func joinRequests(bodies [][]byte) []byte {
	var b bytes.Buffer
	b.WriteString(`{"requests":[`)
	for i, body := range bodies {
		if i > 0 {
			b.WriteByte(',')
		}
		b.Write(body)
	}
	b.WriteString(`]}`)
	return b.Bytes()
}
