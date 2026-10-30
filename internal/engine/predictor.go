package engine

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/omorsi45/hazard/internal/change"
	"github.com/omorsi45/hazard/internal/config"
	"github.com/omorsi45/hazard/internal/control"
	"github.com/omorsi45/hazard/internal/impact"
	"github.com/omorsi45/hazard/internal/index"
	"github.com/omorsi45/hazard/internal/predict"
)

// Predictor kinds a lock may name.
const (
	kindStatic   = "static"
	kindLogistic = "logistic"
	kindJev      = "jev"
	kindEnsemble = "ensemble"
)

// The local predictors have no served model and no rubric, but a lock must
// name a model_version and a rubric_version. They are fixed names: the release
// of the algorithm and "none". What ties a lock to one fitted artifact is the
// weights digest, which Verify compares after the model release, so changing
// an artifact is reported as feature_mismatch.
const (
	releaseStatic   = "p0"
	releaseLogistic = "p1"
	noRubric        = "none"
)

// Sources of a predict.Prob that mean the predictor gave no usable score and
// the test is selected on that account (see predict.Jev).
const (
	sourceJevFailed  = "jev_failed"
	sourceBudget     = "budget_exhausted"
	sourceJevInvalid = "jev_invalid"
)

// ErrPredictorUnavailable marks a predictor that cannot be built: a missing or
// invalid artifact, a missing credential, or a privacy setting that forbids it.
// The decision is then the full suite with the reason predictor_unavailable.
var ErrPredictorUnavailable = errors.New("predictor unavailable")

// errUnknownPredictor marks a predictor kind the engine does not know.
var errUnknownPredictor = errors.New("unknown predictor kind")

// unavailableError reports cause and matches ErrPredictorUnavailable, without
// putting the sentinel's text in front of the cause.
type unavailableError struct{ cause error }

func (e unavailableError) Error() string   { return e.cause.Error() }
func (e unavailableError) Unwrap() []error { return []error{ErrPredictorUnavailable, e.cause} }

func unavailable(cause error) error { return unavailableError{cause} }

// TestScore is what a predictor said about one candidate, kept for the
// explanation of a decision.
type TestScore struct {
	TestID    string
	P         float64
	Source    string // jev, cache, logistic, static, or a failure source such as jev_failed
	Shard     int    // the request that answered, -1 when none did
	Truncated bool   // the descriptor sent for this test was cut to fit a token limit
	CostUSD   float64
}

// BuildPolicy is the selection policy a lock is calibrated under: the
// configuration that changes which tests a decision picks, and the predictor
// kind in use. Its digest goes into the lock, so any change to one of these
// fields makes a lock calibrated under the old policy fail with
// policy_mismatch.
func BuildPolicy(cfg config.Config, predictorKind string) control.Policy {
	return control.Policy{
		FeatureVersion:    impact.FeatureVersion,
		Predictor:         predictorKind,
		MaxHops:           cfg.Impact.MaxHops,
		MaxCandidates:     cfg.Impact.MaxCandidates,
		CoverageRequired:  cfg.Impact.CoverageRequired,
		MinSavingRatio:    cfg.Control.MinSavingRatio,
		CandidateRanking:  cfg.Impact.CandidateRanking,
		FullSuiteTriggers: cfg.Repo.FullSuiteTriggers,
		GeneratedGlobs:    cfg.Repo.GeneratedGlobs,
		Redact:            cfg.Privacy.Redact,
		TierEnabled:       false,
	}
}

// LoadPredictor builds the predictor of the given kind from the artifacts and
// the environment, and reports the identity it will answer with: every field
// of control.Identity except PolicyDigest, which depends on the policy and is
// the caller's to fill. static and logistic read their artifact from the
// configured path (relative to repoDir); jev needs the API key named by
// predictor.jev.api_key_env and a privacy configuration that allows an
// external predictor; ensemble is the logistic predictor and jev, combined by
// the larger probability.
//
// Every failure to build the predictor wraps ErrPredictorUnavailable, so the
// caller can answer with the full suite. A kind the engine does not know is an
// error that does not.
func LoadPredictor(repoDir string, cfg config.Config, kind string) (predict.Predictor, control.Identity, error) {
	return loadPredictor(repoDir, cfg, kind, nil)
}

// loadPredictor is LoadPredictor with the HTTP client of the Jev predictor. Nil
// means the client predict.NewJev builds; a test passes one that reaches a
// local server.
func loadPredictor(repoDir string, cfg config.Config, kind string, client *http.Client) (predict.Predictor, control.Identity, error) {
	id := control.Identity{Predictor: kind, FeatureVersion: impact.FeatureVersion}
	switch kind {
	case kindStatic:
		a, err := predict.ReadStatic(artifactPath(repoDir, cfg.Predictor.Static.Calibration))
		if err != nil {
			return nil, control.Identity{}, unavailable(err)
		}
		id.ModelRelease, id.RubricVersion, id.WeightsDigest = releaseStatic, noRubric, a.Digest
		return predict.NewStatic(a), id, nil
	case kindLogistic:
		a, err := predict.ReadLogistic(artifactPath(repoDir, cfg.Predictor.Logistic.Weights))
		if err != nil {
			return nil, control.Identity{}, unavailable(err)
		}
		id.ModelRelease, id.RubricVersion, id.WeightsDigest = releaseLogistic, noRubric, a.Digest
		return predict.NewLogistic(a), id, nil
	case kindJev:
		j, err := newJev(cfg, kind, client)
		if err != nil {
			return nil, control.Identity{}, err
		}
		id.ModelRelease, id.RubricVersion = cfg.Predictor.Jev.Model, predict.RubricR1
		return j, id, nil
	case kindEnsemble:
		a, err := predict.ReadLogistic(artifactPath(repoDir, cfg.Predictor.Logistic.Weights))
		if err != nil {
			return nil, control.Identity{}, unavailable(err)
		}
		j, err := newJev(cfg, kind, client)
		if err != nil {
			return nil, control.Identity{}, err
		}
		id.ModelRelease, id.RubricVersion, id.WeightsDigest = cfg.Predictor.Jev.Model, predict.RubricR1, a.Digest
		return predict.Ensemble{Local: predict.NewLogistic(a), Remote: j}, id, nil
	}
	return nil, control.Identity{}, fmt.Errorf("%w %q", errUnknownPredictor, kind)
}

// artifactPath resolves a configured artifact path against the repository.
func artifactPath(repoDir, configured string) string {
	if filepath.IsAbs(configured) {
		return configured
	}
	return filepath.Join(repoDir, filepath.FromSlash(configured))
}

// jevOptions are the options of the Jev client for cfg, without a key.
func jevOptions(cfg config.Config, client *http.Client) predict.JevOptions {
	return predict.JevOptions{
		Config:        cfg.Predictor.Jev,
		Privacy:       cfg.Privacy,
		RubricVersion: predict.RubricR1,
		HTTPClient:    client,
	}
}

// newJev builds the Jev client with the key from the environment. A missing
// key is not replaced by another predictor (overview, safety contract item 3):
// the locked predictor is unavailable and the answer is the full suite. A
// configuration that forbids the external predictor is reported first, since
// no key would help.
func newJev(cfg config.Config, kind string, client *http.Client) (*predict.Jev, error) {
	opts := jevOptions(cfg, client)
	env := cfg.Predictor.Jev.APIKeyEnv
	opts.APIKey = os.Getenv(env)
	if strings.TrimSpace(opts.APIKey) == "" {
		if _, err := predict.DryRunPayload(opts, &change.ChangeSet{}, nil); err != nil {
			return nil, unavailable(err)
		}
		return nil, unavailable(fmt.Errorf("%s unset: the locked predictor %s is unavailable; calibrate a logistic lock to select offline", env, kind))
	}
	j, err := predict.NewJev(opts)
	if err != nil {
		return nil, unavailable(err)
	}
	return j, nil
}

// Score asks p about cands and returns the controller's view of the answer
// and the scores kept for explanation, both in TestID order. The candidates
// are sorted first (L1 ranks them, and the local predictors require TestID
// order) without touching the caller's slice. The answer is validated: a
// count, order, NaN or range violation is an error, never a zero. A source
// that means the predictor gave no usable score (a failed or unaffordable
// shard, an invalid answer) marks the candidate Failed, which the controller
// selects. An error that wraps predict.ErrModelDrift stays comparable with
// errors.Is. With no candidates nobody is asked.
func Score(ctx context.Context, p predict.Predictor, ch *change.ChangeSet, cands []impact.Candidate) ([]control.Candidate, []TestScore, error) {
	if p == nil {
		return nil, nil, errors.New("engine: no predictor")
	}
	if len(cands) == 0 {
		return []control.Candidate{}, []TestScore{}, nil
	}
	sorted := slices.Clone(cands)
	slices.SortStableFunc(sorted, func(a, b impact.Candidate) int { return cmp.Compare(a.TestID, b.TestID) })
	probs, err := p.Predict(ctx, ch, sorted)
	if err != nil {
		return nil, nil, fmt.Errorf("%s predictor: %w", p.Name(), err)
	}
	if err := predict.ValidateProbs(sorted, probs); err != nil {
		return nil, nil, fmt.Errorf("%s predictor: %w", p.Name(), err)
	}
	out := make([]control.Candidate, len(probs))
	scores := make([]TestScore, len(probs))
	for i, pr := range probs {
		out[i] = control.Candidate{TestID: pr.TestID, P: pr.P, Failed: isFailureSource(pr.Source)}
		scores[i] = TestScore{TestID: pr.TestID, P: pr.P, Source: pr.Source, Shard: pr.Shard, Truncated: pr.Truncated, CostUSD: pr.CostUSD}
	}
	return out, scores, nil
}

func isFailureSource(source string) bool {
	switch source {
	case sourceJevFailed, sourceBudget, sourceJevInvalid:
		return true
	}
	return false
}

// ControllerInput maps the deterministic layer and the scored candidates to
// the input of control.Decide: the CERTAIN tests apart from the always-run
// ones, the candidates, the truncation and the coverage state, the package
// counts, the inventory with each test's package, the durations and the
// configured minimum saving. lambda comes from the verified lock. LockReason,
// FullReason and TriggersFull are not set: they are the caller's to give, and
// the module tier is off, so Tier is empty.
func ControllerInput(cfg config.Config, m *index.Manifest, l1 *impact.Result, cands []control.Candidate, lambda float64, testMS map[string]int, drift control.DriftStatus) control.Input {
	in := control.Input{
		Lambda:           lambda,
		Candidates:       cands,
		RawCandidates:    l1.RawCandidates,
		Truncated:        l1.Truncated,
		CoverageStale:    coverageStale(l1.CoverageState),
		CoverageRequired: cfg.Impact.CoverageRequired,
		ChangedPackages:  len(l1.ChangedPackages),
		PackageCount:     l1.PackageCount,
		AllTests:         l1.AllTests,
		TestMS:           testMS,
		MinSavingRatio:   cfg.Control.MinSavingRatio,
		Drift:            drift,
	}
	for _, mem := range l1.Certain {
		if mem.Reason == sourceAlwaysRun {
			in.AlwaysRun = append(in.AlwaysRun, mem.TestID)
		} else {
			in.Certain = append(in.Certain, mem.TestID)
		}
	}
	if m != nil {
		in.TestPackage = make(map[string]string, len(m.Tests))
		for _, t := range m.Tests {
			in.TestPackage[t.ID] = t.Package
		}
	}
	return in
}

// coverageStale reports whether the coverage state of L1 is anything but
// fresh coverage or no coverage requested. A state that is not known is not
// evidence that coverage is fresh.
func coverageStale(state string) bool {
	return state != "fresh" && state != "not_requested"
}
