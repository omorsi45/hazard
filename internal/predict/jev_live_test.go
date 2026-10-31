package predict

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/omorsi45/hazard/internal/change"
	"github.com/omorsi45/hazard/internal/config"
	"github.com/omorsi45/hazard/internal/impact"
)

// TestJevLiveSmoke sends one tiny request to the real endpoint. It is the only
// test that needs the network, and it is skipped unless OPENROUTER_API_KEY is
// set. The request carries a synthetic change, never repository content.
func TestJevLiveSmoke(t *testing.T) {
	key := os.Getenv("OPENROUTER_API_KEY")
	if key == "" {
		t.Skip("OPENROUTER_API_KEY is not set")
	}
	cfg := config.Defaults().Predictor.Jev
	j, err := NewJev(JevOptions{
		Config: cfg, Privacy: config.Defaults().Privacy, RubricVersion: RubricR1, APIKey: key,
	})
	if err != nil {
		t.Fatalf("NewJev: %v", err)
	}
	ch := &change.ChangeSet{
		BaseSHA: "1111111", HeadSHA: "2222222", LinesAdded: 1, LinesRemoved: 1,
		Files: []change.ChangedFile{{
			Path: "calc/add.go", Kind: change.KindSource, Status: "M", Language: "go", Package: "example.com/calc",
			Hunks: []change.Hunk{{OldStart: 3, OldLines: 1, NewStart: 3, NewLines: 1, Symbols: []string{"Add"}, Text: "-return a + b\n+return a - b\n"}},
		}},
	}
	cands := []impact.Candidate{{
		TestID: "go:example.com/calc:TestAdd", TestName: "TestAdd", Package: "example.com/calc",
		Features: impact.Features{Reachable: true, Hops: 0},
	}}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	probs, err := j.Predict(ctx, ch, cands)
	if err != nil {
		t.Fatalf("Predict: %s", strings.ReplaceAll(err.Error(), key, "[redacted]"))
	}
	if err := ValidateProbs(cands, probs); err != nil {
		t.Fatal(err)
	}
	st := j.LastStats()
	if probs[0].Source != sourceJev {
		t.Fatalf("source = %q, want a real answer; stats requests=%d failed=%d warnings=%v", probs[0].Source, st.Requests, st.FailedShards, st.Warnings)
	}
	if probs[0].P < 0 || probs[0].P > 1 {
		t.Fatalf("probability %v is outside [0, 1]", probs[0].P)
	}
	if st.ServedModel == "" {
		t.Fatal("the response did not name the served model")
	}
}
