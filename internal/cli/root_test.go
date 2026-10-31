package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func invoke(ctx context.Context, args ...string) (int, string, string) {
	var out, errOut bytes.Buffer
	code := Execute(ctx, args, &out, &errOut)
	return code, out.String(), errOut.String()
}

func TestHelpTreeAndVersion(t *testing.T) {
	code, out, errOut := invoke(context.Background(), "--help")
	if code != 0 || errOut != "" {
		t.Fatalf("help code=%d out=%q err=%q", code, out, errOut)
	}
	for _, name := range []string{"select", "run", "ci-mine", "labels", "mutate", "calibrate", "eval", "audit", "explain"} {
		if !strings.Contains(out, name) {
			t.Errorf("help omitted %s", name)
		}
	}
	code, out, errOut = invoke(context.Background(), "labels", "--help")
	if code != 0 || errOut != "" {
		t.Fatalf("labels help code=%d out=%q err=%q", code, out, errOut)
	}
	for _, name := range []string{"build", "flaky", "stats"} {
		if !strings.Contains(out, name) {
			t.Errorf("labels help omitted %s", name)
		}
	}
	code, out, errOut = invoke(context.Background(), "version")
	if code != 0 || !strings.Contains(out, "hazard") || errOut != "" {
		t.Fatalf("version code=%d out=%q err=%q", code, out, errOut)
	}
}

func TestDocumentedFlagsParse(t *testing.T) {
	cases := [][]string{}
	for _, args := range cases {
		t.Run(strings.Join(args[:min(len(args), 2)], "_"), func(t *testing.T) {
			code, _, errOut := invoke(context.Background(), args...)
			if code != 70 || !strings.Contains(errOut, "not implemented") {
				t.Fatalf("%v: code=%d err=%q", args, code, errOut)
			}
		})
	}
}

func TestUsageAndJSONErrors(t *testing.T) {
	for _, args := range [][]string{{"select", "--no-such-flag"}, {"select", "--budget-ms", "wrong"}, {"mutate", "--timeout", "wrong"}} {
		code, _, _ := invoke(context.Background(), args...)
		if code != 2 {
			t.Errorf("%v returned %d, want 2", args, code)
		}
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	code, out, errOut := invoke(canceled, "eval", "--json")
	if code != 70 || out != "" {
		t.Fatalf("JSON leaf code=%d stdout=%q stderr=%q", code, out, errOut)
	}
	var payload struct {
		Error string `json:"error"`
		Code  int    `json:"code"`
	}
	if err := json.Unmarshal([]byte(errOut), &payload); err != nil || payload.Code != 70 || payload.Error == "" {
		t.Fatalf("bad JSON error %q: %v", errOut, err)
	}
	code, out, errOut = invoke(context.Background(), "select", "--json", "--help")
	if code != 0 || out == "" || errOut != "" {
		t.Fatalf("help contaminated code=%d out=%q err=%q", code, out, errOut)
	}
}

func TestCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for _, command := range []string{"select", "eval", "run"} {
		code, _, errOut := invoke(ctx, command)
		if code != 70 || !strings.Contains(errOut, "canceled") {
			t.Fatalf("%s canceled code=%d err=%q", command, code, errOut)
		}
	}
}

func TestRecallOverrideRequiresExplicitConfirmation(t *testing.T) {
	for _, args := range [][]string{
		{"select", "--recall", "0.9"},
		{"run", "--recall", "0.9"},
		{"select", "--recall", "NaN", "--i-know-what-im-doing"},
		{"select", "--recall", "1.1", "--i-know-what-im-doing"},
	} {
		code, _, _ := invoke(context.Background(), args...)
		if code != 2 {
			t.Errorf("%v: code=%d, want 2", args, code)
		}
	}
	// A confirmed override passes the usage gate and reaches the selector.
	r := newRig(t, narrowedDecision())
	code, _, errOut := r.run("--recall", "0.9", "--i-know-what-im-doing")
	if code != 0 || r.sel.calls != 1 || r.sel.got.Recall != 0.9 {
		t.Fatalf("confirmed override: code=%d err=%q Select calls=%d recall=%v", code, errOut, r.sel.calls, r.sel.got.Recall)
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("injected write failure") }

func TestVersionWriterFailureReturnsInternalCode(t *testing.T) {
	var errOut bytes.Buffer
	code := Execute(context.Background(), []string{"version"}, failingWriter{}, &errOut)
	if code != 70 {
		t.Fatalf("writer failure code=%d, want 70; stderr=%q", code, errOut.String())
	}
}

func TestSelectionHelpEffectiveDefaults(t *testing.T) {
	code, out, errOut := invoke(context.Background(), "select", "--help")
	if code != 0 || errOut != "" {
		t.Fatalf("help code=%d err=%q", code, errOut)
	}
	for _, phrase := range []string{
		"merge-base with the default branch",
		"working tree, including uncommitted changes",
		"hazard.yml, then .hazard/hazard.yml",
		"from lock",
		"from config",
		"print the full suite as the run set",
	} {
		if !strings.Contains(out, phrase) {
			t.Errorf("help omitted %q", phrase)
		}
	}
}

func TestJSONUsageErrorsIndependentOfFlagOrder(t *testing.T) {
	for _, args := range [][]string{
		{"select", "--unknown", "--json"},
		{"select", "--runner", "--json"},
		{"--json", "bogus"},
		{"select", "--budget-ms", "bad", "--json"},
	} {
		code, out, errOut := invoke(context.Background(), args...)
		var payload struct {
			Error string `json:"error"`
			Code  int    `json:"code"`
		}
		if err := json.Unmarshal([]byte(errOut), &payload); err != nil || code != 2 || out != "" || payload.Code != 2 || payload.Error == "" {
			t.Errorf("%v: code=%d stdout=%q stderr=%q parse=%v", args, code, out, errOut, err)
		}
	}
	for _, args := range [][]string{
		{"select", "--format", "--json", "--unknown"},
		{"run", "--runner", "--json", "--unknown"},
		{"run", "--runner", "bogus", "--", "--json"},
	} {
		_, _, errOut := invoke(context.Background(), args...)
		if strings.HasPrefix(errOut, "{") {
			t.Errorf("%v incorrectly requested JSON: %q", args, errOut)
		}
	}
}
