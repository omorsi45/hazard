package telemetry

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/omorsi45/hazard/internal/change"
	"github.com/omorsi45/hazard/internal/engine"
	"github.com/omorsi45/hazard/internal/impact"
)

func fp(v float64) *float64 { return &v }

var fixedTime = time.Date(2026, 10, 29, 22, 15, 30, 0, time.FixedZone("EDT", -4*3600))

func narrowed() engine.Decision {
	return engine.Decision{
		ID:         "dec-1",
		LockDigest: "sha256:lock",
		Lambda:     0.125,
		AllTests:   []string{"go:m/a:TestA", "go:m/a:TestB", "go:m/b:TestC", "py:t.x:test_a", "py:t.x:test_b"},
		Tests: []engine.Selected{
			{TestID: "go:m/a:TestA", Source: "certain", Reason: "direct", P: nil},
			{TestID: "go:m/b:TestC", Source: "added", Reason: "p>=lambda", P: fp(0.4)},
			{TestID: "py:t.x:test_a", Source: "always_run", Reason: "quarantined"},
			{TestID: "py:t.x:test_b", Source: "tier", Reason: "module"},
		},
		Change:        &change.ChangeSet{BaseSHA: "base1", HeadSHA: "head1"},
		EstMSSelected: 120,
		EstMSFull:     900,
		OverheadMS:    37,
		CostUSD:       0.002,
	}
}

func TestFromDecisionNarrowed(t *testing.T) {
	got := FromDecision(narrowed(), ModeRun, "9.9.9", fixedTime)
	want := Record{
		ID: "dec-1", Time: "2026-10-30T02:15:30Z", Commit: "head1", Mode: "run",
		Lambda: 0.125, LockDigest: "sha256:lock",
		NTotal: 5, NCertain: 1, NAdded: 1, NAlwaysRun: 1, NTier: 1,
		Members: []Member{
			{TestID: "go:m/a:TestA", Source: "certain"},
			{TestID: "go:m/b:TestC", Source: "added", P: fp(0.4)},
			{TestID: "py:t.x:test_a", Source: "always_run"},
			{TestID: "py:t.x:test_b", Source: "tier"},
		},
		EstMSSelected: 120, EstMSFull: 900, OverheadMS: 37, CostUSD: 0.002, HazardVersion: "9.9.9",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("FromDecision =\n%+v\nwant\n%+v", got, want)
	}
}

func TestFromDecisionFullSuiteKeepsHypotheticalMembers(t *testing.T) {
	d := engine.Decision{
		ID: "dec-2", FullSuite: true, Abstained: true, AbstainReason: "shadow",
		AllTests: []string{"go:m/a:TestA", "go:m/a:TestB"},
		Hypothetical: []engine.Selected{
			{TestID: "go:m/a:TestA", Source: "certain", Reason: "direct"},
		},
		Change: &change.ChangeSet{HeadSHA: "head2"},
	}
	got := FromDecision(d, ModeShadow, "v", fixedTime)
	if !got.FullSuite || got.AbstainReason != "shadow" || got.Mode != "shadow" || got.Commit != "head2" {
		t.Errorf("record = %+v", got)
	}
	if len(got.Members) != 1 || got.Members[0].TestID != "go:m/a:TestA" || got.NCertain != 1 || got.NTotal != 2 {
		t.Errorf("a full-suite record must keep the hypothetical selection: %+v", got)
	}
}

func TestFromDecisionWithoutChangeHasNoCommit(t *testing.T) {
	got := FromDecision(engine.Decision{ID: "x", FullSuite: true, AbstainReason: "no_merge_base"}, ModeSelect, "v", fixedTime)
	if got.Commit != "" || got.ID != "x" || !got.FullSuite {
		t.Errorf("record = %+v", got)
	}
}

func TestAppendReadRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "deep", "dir", "runs.jsonl")
	exit := 1
	first := FromDecision(narrowed(), ModeSelect, "v", fixedTime)
	second := FromDecision(narrowed(), ModeRun, "v", fixedTime.Add(time.Minute))
	second.ID, second.RunnerExit, second.JUnit = "dec-3", &exit, ".hazard/junit.xml"
	for _, r := range []Record{first, second} {
		if err := Append(path, r); err != nil {
			t.Fatal(err)
		}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if lines := bytes.Count(data, []byte("\n")); lines != 2 || !bytes.HasSuffix(data, []byte("\n")) {
		t.Errorf("want two newline-terminated lines, got %d: %q", lines, data)
	}
	got, err := Read(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, []Record{first, second}) {
		t.Errorf("round trip =\n%+v\nwant\n%+v", got, []Record{first, second})
	}
}

func TestReadSkipsATruncatedLastLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runs.jsonl")
	if err := Append(path, FromDecision(narrowed(), ModeRun, "v", fixedTime)); err != nil {
		t.Fatal(err)
	}
	appendRaw(t, path, `{"id":"half","time":"2026-10`)
	got, skipped, err := Load(path)
	if err != nil || len(got) != 1 || skipped != 1 {
		t.Fatalf("Load = %d records, skipped %d, err %v; want 1, 1, nil", len(got), skipped, err)
	}
	plain, err := Read(path)
	if err != nil || len(plain) != 1 {
		t.Errorf("Read = %d records, err %v", len(plain), err)
	}
}

func TestReadRejectsOtherMalformedLines(t *testing.T) {
	good, err := marshalLine(FromDecision(narrowed(), ModeRun, "v", fixedTime))
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]string{
		"a malformed line in the middle":        string(good) + "not json\n" + string(good),
		"a complete malformed last line":        string(good) + "not json\n",
		"a record without an id":                string(good) + "{}\n",
		"a line that is not a JSON object":      string(good) + "[1,2]\n",
		"a truncated line followed by a record": string(good) + `{"id":"half"` + "\n" + string(good),
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "runs.jsonl")
			if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
			got, err := Read(path)
			if err == nil {
				t.Fatalf("want an error, got %d records", len(got))
			}
			if !strings.Contains(err.Error(), "line") {
				t.Errorf("the error must name the line: %v", err)
			}
		})
	}
}

func TestReadIgnoresBlankLines(t *testing.T) {
	good, err := marshalLine(FromDecision(narrowed(), ModeRun, "v", fixedTime))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "runs.jsonl")
	if err := os.WriteFile(path, []byte("\n"+string(good)+"\n\n"+string(good)), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := Read(path)
	if err != nil || len(got) != 2 {
		t.Fatalf("Read = %d records, err %v; want 2, nil", len(got), err)
	}
}

func TestReadMissingFileIsNotExist(t *testing.T) {
	_, err := Read(filepath.Join(t.TempDir(), "absent.jsonl"))
	if !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("err = %v, want fs.ErrNotExist", err)
	}
}

func TestAppendAfterATruncatedTailKeepsTheFileReadable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runs.jsonl")
	if err := Append(path, FromDecision(narrowed(), ModeRun, "v", fixedTime)); err != nil {
		t.Fatal(err)
	}
	appendRaw(t, path, `{"id":"half","time":"2026-10`)
	next := FromDecision(narrowed(), ModeRun, "v", fixedTime.Add(time.Hour))
	next.ID = "dec-next"
	if err := Append(path, next); err != nil {
		t.Fatal(err)
	}
	got, skipped, err := Load(path)
	if err != nil || skipped != 0 || len(got) != 2 || got[1].ID != "dec-next" {
		t.Fatalf("Load = %+v, skipped %d, err %v; want both complete records and nothing skipped", got, skipped, err)
	}
}

func TestAppendAfterACompleteLineWithoutNewline(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runs.jsonl")
	line, err := marshalLine(FromDecision(narrowed(), ModeRun, "v", fixedTime))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, bytes.TrimSuffix(line, []byte("\n")), 0o644); err != nil {
		t.Fatal(err)
	}
	next := FromDecision(narrowed(), ModeRun, "v", fixedTime)
	next.ID = "dec-next"
	if err := Append(path, next); err != nil {
		t.Fatal(err)
	}
	got, skipped, err := Load(path)
	if err != nil || skipped != 0 || len(got) != 2 {
		t.Fatalf("Load = %d records, skipped %d, err %v; want 2, 0, nil", len(got), skipped, err)
	}
}

func TestAppendFailsWhenThePathIsADirectory(t *testing.T) {
	dir := t.TempDir()
	if err := Append(dir, FromDecision(narrowed(), ModeRun, "v", fixedTime)); err == nil {
		t.Fatal("want an error appending to a directory")
	}
}

// sentinel is text that must never reach telemetry.
const sentinel = "SENTINEL-do-not-log-4f1c9a"

func sentinelDecision() engine.Decision {
	d := narrowed()
	d.Payload = []byte(`{"requests":[{"hunk":"` + sentinel + `"}]}`)
	d.Warnings = []string{"warning " + sentinel}
	d.Tests[0].Reason = "reason " + sentinel
	d.Change = &change.ChangeSet{
		BaseSHA: "base1", HeadSHA: "head1",
		Files: []change.ChangedFile{{
			Path: "core/" + sentinel + ".go", Status: "M", Language: "go",
			Hunks: []change.Hunk{{OldStart: 1, NewStart: 1, Symbols: []string{sentinel}, Text: "+" + sentinel}},
		}},
	}
	d.L1 = &impact.Result{Candidates: []impact.Candidate{{
		TestID: "go:m/a:TestB", Purpose: "purpose " + sentinel, Asserts: []string{"asserts " + sentinel},
	}}}
	return d
}

func TestRecordNeverCarriesPayloadOrHunkText(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runs.jsonl")
	if err := Append(path, FromDecision(sentinelDecision(), ModeRun, "v", fixedTime)); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(data, []byte(sentinel)) {
		t.Fatalf("telemetry leaked text that must stay local:\n%s", data)
	}
	if !bytes.Contains(data, []byte("go:m/a:TestA")) {
		t.Fatalf("the record must still name its tests:\n%s", data)
	}
}

func appendRaw(t *testing.T, path, text string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(text); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}
