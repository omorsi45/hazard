package predict

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/omorsi45/hazard/internal/change"
	"github.com/omorsi45/hazard/internal/config"
	"github.com/omorsi45/hazard/internal/impact"
)

const (
	sentinel = "SECRET_SENTINEL_42"
	literal  = "hunter2-literal-value"
	testKey  = "sk-test-key-do-not-log-7731"
)

func fixedNow() time.Time { return time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC) }

// jevConfig is the default Jev configuration with timings short enough for
// tests.
func jevConfig() config.Jev {
	c := config.Defaults().Predictor.Jev
	c.TimeoutMS = 30000
	c.RetryBackoffMS = 1
	c.Retries = 2
	return c
}

func newTestJev(t *testing.T, mutate func(*JevOptions)) *Jev {
	t.Helper()
	opts := JevOptions{
		Config:        jevConfig(),
		Privacy:       config.Defaults().Privacy,
		RubricVersion: "r1",
		APIKey:        testKey,
		Now:           fixedNow,
	}
	if mutate != nil {
		mutate(&opts)
	}
	j, err := NewJev(opts)
	if err != nil {
		t.Fatalf("NewJev: %v", err)
	}
	return j
}

// candidatesN returns n candidates whose test ids sort in ascending order.
func candidatesN(n int) []impact.Candidate {
	out := make([]impact.Candidate, n)
	for i := range out {
		name := fmt.Sprintf("Test%04d", i+1)
		out[i] = impact.Candidate{
			TestID: "go:m/p:" + name, TestName: name, Package: "m/p",
			Features: impact.Features{Reachable: true, Hops: 1},
		}
	}
	return out
}

// goldenChange is the change of the golden request fixtures.
func goldenChange() *change.ChangeSet {
	return &change.ChangeSet{
		BaseSHA: "a1b2c3d4e5f6", HeadSHA: "e4f5a6b7c8d9", LinesAdded: 34, LinesRemoved: 12,
		Files: []change.ChangedFile{
			{
				Path: "internal/auth/token.go", Kind: change.KindSource, Status: "M", Language: "go",
				Package: "github.com/omorsi45/x/internal/auth",
				Hunks: []change.Hunk{{
					OldStart: 40, OldLines: 4, NewStart: 40, NewLines: 6,
					Symbols: []string{"RefreshToken", "(*Session).Renew"},
					Text:    "-\tif t.Expiry > now {\n+\tif t.Expiry >= now {\n+\t\tgrace := 30\n",
				}},
			},
			{
				Path: "config/defaults.yaml", Kind: change.KindConfig, Status: "M",
				Hunks: []change.Hunk{{
					OldStart: 3, OldLines: 1, NewStart: 3, NewLines: 1,
					Symbols: []string{"__file__"},
					Text:    "-token_grace_seconds: 0\n+token_grace_seconds: 30\n",
				}},
			},
		},
	}
}

// goldenCandidates are the two tests of the golden request fixtures.
func goldenCandidates() []impact.Candidate {
	return []impact.Candidate{
		{
			TestID: "go:github.com/omorsi45/x/internal/auth:TestRefreshNearExpiry", TestName: "TestRefreshNearExpiry",
			Package: "github.com/omorsi45/x/internal/auth",
			Purpose: "refresh succeeds when the token is one second from expiry",
			Asserts: []string{"session.Valid", "err == nil"},
			Features: impact.Features{
				Hops: 0, Reachable: true, CoverageKnown: true, CoverageOverlap: 0.5, LinesAdded: 34, LinesRemoved: 12,
			},
		},
		{
			TestID: "go:github.com/omorsi45/x/internal/billing:TestBillingInvoicePDF", TestName: "TestBillingInvoicePDF",
			Package: "github.com/omorsi45/x/internal/billing",
			Purpose: "renders an invoice to PDF with the right totals",
			Asserts: []string{"pdf.PageCount == 2"},
			Features: impact.Features{
				Hops: 4, Reachable: true, LinesAdded: 34, LinesRemoved: 12,
			},
		},
	}
}

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "testdata", "jev", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return b
}

// wireRequest is the part of a request the tests inspect.
type wireRequest struct {
	Model string `json:"model"`
	State struct {
		Rubric string                     `json:"rubric"`
		Change json.RawMessage            `json:"change"`
		Tests  map[string]json.RawMessage `json:"tests"`
	} `json:"state"`
	Questions map[string]struct {
		Type         string            `json:"type"`
		Instructions string            `json:"instructions"`
		Criteria     map[string]string `json:"criteria"`
	} `json:"questions"`
}

func decodeRequest(t *testing.T, body []byte) wireRequest {
	t.Helper()
	var r wireRequest
	if err := json.Unmarshal(body, &r); err != nil {
		t.Fatalf("decode request: %v\n%s", err, body)
	}
	return r
}

// stateBytes returns the serialized state of a request body.
func stateBytes(t *testing.T, body []byte) []byte {
	t.Helper()
	var r struct {
		State json.RawMessage `json:"state"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		t.Fatal(err)
	}
	return r.State
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
