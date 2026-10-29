package predict

import (
	"bytes"
	"testing"

	"github.com/omorsi45/hazard/internal/change"
	"github.com/omorsi45/hazard/internal/config"
	"github.com/omorsi45/hazard/internal/impact"
)

const assertLiteral = "assert-literal-value"

// sentinelChange has a hunk whose text carries the sentinel in a comment and
// a string literal in code.
func sentinelChange() *change.ChangeSet {
	return &change.ChangeSet{
		BaseSHA: "a1b2c3d4e5f6", HeadSHA: "e4f5a6b7c8d9", LinesAdded: 2, LinesRemoved: 1,
		Files: []change.ChangedFile{{
			Path: "internal/auth/token.go", Kind: change.KindSource, Status: "M", Language: "go",
			Package: "m/auth",
			Hunks: []change.Hunk{{
				OldStart: 10, OldLines: 1, NewStart: 10, NewLines: 2, Symbols: []string{"RefreshToken"},
				Text: "-\tapiKey := \"old\"\n+\tapiKey := \"" + literal + "\" // " + sentinel + "\n+\tretry := 3\n",
			}},
		}},
	}
}

// sentinelCandidates carry the sentinel in the purpose, in an assertion and in
// a parameter of the test name.
func sentinelCandidates() []impact.Candidate {
	return []impact.Candidate{
		{
			TestID: "go:m/auth:TestRefresh", TestName: "TestRefresh", Package: "m/auth",
			Purpose:  "refresh with literal " + sentinel,
			Asserts:  []string{"session.Key == \"" + assertLiteral + "\"", sentinel},
			Features: impact.Features{Reachable: true, Hops: 1},
		},
		{
			TestID: "py:tests/test_login.py::test_login[" + sentinel + "-x]", TestName: "test_login[" + sentinel + "-x]",
			Package: "tests", Purpose: "login " + sentinel,
			Features: impact.Features{Reachable: true, Hops: 2},
		},
	}
}

func payloadString(t *testing.T, j *Jev) string {
	t.Helper()
	shards, err := j.Payload(sentinelChange(), sentinelCandidates())
	if err != nil {
		t.Fatalf("Payload: %v", err)
	}
	return string(bytes.Join(shards, []byte("\n")))
}

func TestJevPrivacyStructureNeverSendsSourceText(t *testing.T) {
	for _, sendText := range []bool{false, true} {
		got := payloadString(t, newTestJev(t, func(o *JevOptions) {
			o.Privacy.Redact = "structure"
			o.Privacy.SendHunkText = sendText
		}))
		for _, banned := range []string{sentinel, literal, assertLiteral, "apiKey", "retry", "hunk_text", "\"asserts\""} {
			if bytes.Contains([]byte(got), []byte(banned)) {
				t.Fatalf("send_hunk_text=%v: the request leaks %q:\n%s", sendText, banned, got)
			}
		}
		for _, want := range []string{"internal/auth/token.go", "RefreshToken", "TestRefresh", "\"purpose\":\"refresh\""} {
			if !bytes.Contains([]byte(got), []byte(want)) {
				t.Fatalf("send_hunk_text=%v: the request lost %q:\n%s", sendText, want, got)
			}
		}
	}
}

func TestJevPrivacyNoneSendsHunkTextOnlyWhenAsked(t *testing.T) {
	with := payloadString(t, newTestJev(t, func(o *JevOptions) {
		o.Privacy = config.Privacy{Redact: "none", SendHunkText: true, SendSymbolNames: true, SendPaths: true, AllowExternalPredictor: true}
	}))
	for _, want := range []string{sentinel, literal, assertLiteral, "hunk_text"} {
		if !bytes.Contains([]byte(with), []byte(want)) {
			t.Fatalf("none with send_hunk_text: the request lacks %q:\n%s", want, with)
		}
	}
	without := payloadString(t, newTestJev(t, func(o *JevOptions) {
		o.Privacy = config.Privacy{Redact: "none", SendHunkText: false, SendSymbolNames: true, SendPaths: true, AllowExternalPredictor: true}
	}))
	for _, banned := range []string{literal, "apiKey", "hunk_text"} {
		if bytes.Contains([]byte(without), []byte(banned)) {
			t.Fatalf("none without send_hunk_text: the request leaks hunk text %q:\n%s", banned, without)
		}
	}
	if !bytes.Contains([]byte(without), []byte(assertLiteral)) {
		t.Fatalf("none keeps the test purposes and assertions:\n%s", without)
	}
}

func TestJevPrivacyHashIsRefused(t *testing.T) {
	_, err := NewJev(JevOptions{Config: jevConfig(), Privacy: config.Privacy{Redact: "hash", SendSymbolNames: true, SendPaths: true, AllowExternalPredictor: true}, APIKey: testKey})
	if err == nil {
		t.Fatal("NewJev accepted the hash level, which cannot go to the external predictor")
	}
}
