package engine

import (
	"context"
	"errors"
	"fmt"
	"math"
	"reflect"
	"testing"

	"github.com/omorsi45/hazard/internal/change"
	"github.com/omorsi45/hazard/internal/impact"
	"github.com/omorsi45/hazard/internal/predict"
)

// stubPredictor answers with fn and records the candidates it was asked about.
type stubPredictor struct {
	fn    func(ctx context.Context, cands []impact.Candidate) ([]predict.Prob, error)
	calls int
	got   []string
	ctx   context.Context
}

func (s *stubPredictor) Name() string    { return "stub" }
func (s *stubPredictor) Version() string { return "stub/1" }
func (s *stubPredictor) Predict(ctx context.Context, _ *change.ChangeSet, cands []impact.Candidate) ([]predict.Prob, error) {
	s.calls++
	s.ctx = ctx
	s.got = s.got[:0]
	for _, c := range cands {
		s.got = append(s.got, c.TestID)
	}
	return s.fn(ctx, cands)
}

// constantP answers every candidate with p from source.
func constantP(p float64, source string) func(context.Context, []impact.Candidate) ([]predict.Prob, error) {
	return func(_ context.Context, cands []impact.Candidate) ([]predict.Prob, error) {
		out := make([]predict.Prob, len(cands))
		for i, c := range cands {
			out[i] = predict.Prob{TestID: c.TestID, P: p, Source: source, Shard: -1}
		}
		return out, nil
	}
}

func candsOf(ids ...string) []impact.Candidate {
	out := make([]impact.Candidate, len(ids))
	for i, id := range ids {
		out[i] = impact.Candidate{TestID: id, Rank: float64(len(ids) - i)}
	}
	return out
}

func TestScoreMapsFailureSourcesToFailed(t *testing.T) {
	t.Parallel()
	for source, failed := range map[string]bool{
		"jev_failed": true, "budget_exhausted": true, "jev_invalid": true,
		"jev": false, "cache": false, "logistic": false, "static": false, "": false,
	} {
		p := &stubPredictor{fn: constantP(0.5, source)}

		cands, scores, err := Score(context.Background(), p, &change.ChangeSet{}, candsOf("b", "a"))

		if err != nil {
			t.Fatalf("source %q: %v", source, err)
		}
		if len(cands) != 2 || cands[0].Failed != failed || cands[1].Failed != failed {
			t.Errorf("source %q: candidates %+v, want Failed = %v", source, cands, failed)
		}
		if len(scores) != 2 || scores[0].Source != source {
			t.Errorf("source %q: scores %+v", source, scores)
		}
	}
}

func TestScoreCopiesWhatTheExplanationNeeds(t *testing.T) {
	t.Parallel()
	p := &stubPredictor{fn: func(_ context.Context, cands []impact.Candidate) ([]predict.Prob, error) {
		return []predict.Prob{
			{TestID: cands[0].TestID, P: 0.25, Source: "jev", Shard: 3, Truncated: true, CostUSD: 0.002, LatencyMS: 9},
			{TestID: cands[1].TestID, P: 1, Source: "jev_failed", Shard: -1},
		}, nil
	}}

	cands, scores, err := Score(context.Background(), p, &change.ChangeSet{}, candsOf("a", "b"))

	if err != nil {
		t.Fatal(err)
	}
	wantScores := []TestScore{
		{TestID: "a", P: 0.25, Source: "jev", Shard: 3, Truncated: true, CostUSD: 0.002},
		{TestID: "b", P: 1, Source: "jev_failed", Shard: -1},
	}
	if !reflect.DeepEqual(scores, wantScores) {
		t.Errorf("scores\n got: %+v\nwant: %+v", scores, wantScores)
	}
	if cands[0].TestID != "a" || cands[0].P != 0.25 || cands[0].Failed || cands[1].TestID != "b" || cands[1].P != 1 || !cands[1].Failed {
		t.Errorf("candidates = %+v", cands)
	}
}

// TestScoreAsksInTestIDOrder: L1 ranks candidates, and the local predictors
// require TestID order, so Score must sort before it asks.
func TestScoreAsksInTestIDOrder(t *testing.T) {
	t.Parallel()
	p := &stubPredictor{fn: constantP(0.1, "stub")}

	cands, scores, err := Score(context.Background(), p, &change.ChangeSet{}, candsOf("c", "a", "b"))

	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"a", "b", "c"}; !reflect.DeepEqual(p.got, want) {
		t.Errorf("the predictor was asked about %v, want %v", p.got, want)
	}
	if cands[0].TestID != "a" || cands[2].TestID != "c" || scores[0].TestID != "a" || scores[2].TestID != "c" {
		t.Errorf("answers are not in TestID order: %+v %+v", cands, scores)
	}
}

func TestScoreDoesNotReorderTheCallersSlice(t *testing.T) {
	t.Parallel()
	in := candsOf("c", "a", "b")
	if _, _, err := Score(context.Background(), &stubPredictor{fn: constantP(0.1, "stub")}, &change.ChangeSet{}, in); err != nil {
		t.Fatal(err)
	}
	if in[0].TestID != "c" || in[1].TestID != "a" || in[2].TestID != "b" {
		t.Errorf("Score reordered its input: %+v", in)
	}
}

func TestScoreModelDriftIsComparableWithErrorsIs(t *testing.T) {
	t.Parallel()
	p := &stubPredictor{fn: func(context.Context, []impact.Candidate) ([]predict.Prob, error) {
		return nil, fmt.Errorf("jev: %w: configured a, served b", predict.ErrModelDrift)
	}}

	cands, scores, err := Score(context.Background(), p, &change.ChangeSet{}, candsOf("a"))

	if !errors.Is(err, predict.ErrModelDrift) {
		t.Fatalf("err = %v, want predict.ErrModelDrift", err)
	}
	if cands != nil || scores != nil {
		t.Errorf("an error must not carry a partial answer: %+v %+v", cands, scores)
	}
}

func TestScoreReturnsThePredictorsOwnError(t *testing.T) {
	t.Parallel()
	boom := errors.New("boom")
	p := &stubPredictor{fn: func(context.Context, []impact.Candidate) ([]predict.Prob, error) { return nil, boom }}

	if _, _, err := Score(context.Background(), p, &change.ChangeSet{}, candsOf("a")); !errors.Is(err, boom) {
		t.Errorf("err = %v, want the predictor's error", err)
	}
}

func TestScoreRejectsAnAnswerThatBreaksTheContract(t *testing.T) {
	t.Parallel()
	for name, fn := range map[string]func(context.Context, []impact.Candidate) ([]predict.Prob, error){
		"NaN": func(_ context.Context, c []impact.Candidate) ([]predict.Prob, error) {
			return []predict.Prob{{TestID: c[0].TestID, P: math.NaN()}}, nil
		},
		"infinite": func(_ context.Context, c []impact.Candidate) ([]predict.Prob, error) {
			return []predict.Prob{{TestID: c[0].TestID, P: math.Inf(1)}}, nil
		},
		"below zero": func(_ context.Context, c []impact.Candidate) ([]predict.Prob, error) {
			return []predict.Prob{{TestID: c[0].TestID, P: -0.1}}, nil
		},
		"above one": func(_ context.Context, c []impact.Candidate) ([]predict.Prob, error) {
			return []predict.Prob{{TestID: c[0].TestID, P: 1.1}}, nil
		},
		"too few": func(context.Context, []impact.Candidate) ([]predict.Prob, error) {
			return nil, nil
		},
		"another test": func(context.Context, []impact.Candidate) ([]predict.Prob, error) {
			return []predict.Prob{{TestID: "somebody else", P: 0.5}}, nil
		},
	} {
		cands, scores, err := Score(context.Background(), &stubPredictor{fn: fn}, &change.ChangeSet{}, candsOf("a"))
		if err == nil || cands != nil || scores != nil {
			t.Errorf("%s: got %+v %+v %v, want an error and no answer", name, cands, scores, err)
		}
	}
}

func TestScoreWithNothingToScoreAsksNobody(t *testing.T) {
	t.Parallel()
	p := &stubPredictor{fn: constantP(0.1, "stub")}

	cands, scores, err := Score(context.Background(), p, &change.ChangeSet{}, nil)

	if err != nil || p.calls != 0 {
		t.Errorf("err %v, %d calls; want no call for no candidates", err, p.calls)
	}
	if cands == nil || scores == nil || len(cands) != 0 || len(scores) != 0 {
		t.Errorf("got %#v %#v, want empty non-nil slices", cands, scores)
	}
}

func TestScoreNeedsAPredictor(t *testing.T) {
	t.Parallel()
	if _, _, err := Score(context.Background(), nil, &change.ChangeSet{}, candsOf("a")); err == nil {
		t.Error("no predictor must be an error, not an empty answer")
	}
}

func TestScorePassesTheContextToThePredictor(t *testing.T) {
	t.Parallel()
	type key struct{}
	ctx := context.WithValue(context.Background(), key{}, "v")
	p := &stubPredictor{fn: constantP(0.1, "stub")}

	if _, _, err := Score(ctx, p, &change.ChangeSet{}, candsOf("a")); err != nil {
		t.Fatal(err)
	}
	if p.ctx.Value(key{}) != "v" {
		t.Error("the predictor did not receive the caller's context")
	}
}
