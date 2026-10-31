package engine

import (
	"encoding/json"
	"sort"
	"strings"
	"testing"
)

// requestsOf splits {"requests":[...]} into the exact bytes of each request.
func requestsOf(t *testing.T, payload []byte) [][]byte {
	t.Helper()
	var doc struct {
		Requests []json.RawMessage `json:"requests"`
	}
	if err := json.Unmarshal(payload, &doc); err != nil {
		t.Fatalf("payload is not {\"requests\":[...]}: %v\n%s", err, payload)
	}
	out := make([][]byte, len(doc.Requests))
	for i, r := range doc.Requests {
		out[i] = r
	}
	return out
}

func TestSelectKeepsTheTextOfEveryHunk(t *testing.T) {
	t.Parallel()
	f := lockedFixture(t, lockedYML, nil)

	d := selectWith(t, f, nil)

	if d.Change == nil || len(d.Change.Files) != 1 || len(d.Change.Files[0].Hunks) != 1 {
		t.Fatalf("Change = %+v", d.Change)
	}
	want := "-func Core() int { return 1 }\n+func Core() int { return 1 + 0 }\n"
	if got := d.Change.Files[0].Hunks[0].Text; got != want {
		t.Errorf("hunk text = %q, want %q", got, want)
	}
	data, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "return 1 + 0") {
		t.Errorf("the decision's JSON carries source text: %s", data)
	}
}

func TestSelectDryRunPayloadOfTheLockedPredictor(t *testing.T) {
	cfgPrivacy := "privacy:\n  redact: none\n  send_hunk_text: true\n"
	f := lockedFixture(t, lockedYML+cfgPrivacy, nil)
	f.logistic(t, defaultWeight, defaultBias)
	env := f.config(t).Predictor.Jev.APIKeyEnv
	t.Setenv(env, "sk-test-engine-key")
	f.writeLock(t, "ensemble", nil)
	t.Setenv(env, "") // a dry run needs no key

	d := selectWith(t, f, func(o *Options) { o.DryRunPayload = true; o.load = failingLoader(t) })

	if d.Payload == nil || !d.FullSuite || d.AbstainReason != "dry_run" {
		t.Fatalf("Payload %q FullSuite %v reason %q warnings %v", d.Payload, d.FullSuite, d.AbstainReason, d.Warnings)
	}
	reqs := requestsOf(t, d.Payload)
	if len(reqs) != 1 {
		t.Fatalf("%d requests, want one shard for two candidates: %s", len(reqs), d.Payload)
	}
	body := string(reqs[0])
	for _, want := range []string{`"TestSolo"`, `"TestTop"`, "return 1 + 0", `"hunk_text"`} {
		if !strings.Contains(body, want) {
			t.Errorf("the request omits %s:\n%s", want, body)
		}
	}
	if strings.Contains(string(d.Payload), "sk-test") {
		t.Error("the payload holds a key")
	}
	if hasWarning(d, "nothing would be sent") {
		t.Errorf("Warnings = %v: there is a payload", d.Warnings)
	}
}

func TestSelectDryRunPayloadKeepsSourceTextOutOfAStructureRequest(t *testing.T) {
	f := lockedFixture(t, lockedYML+"privacy:\n  send_hunk_text: true\n", nil)

	d := selectWith(t, f, func(o *Options) { o.DryRunPayload = true; o.Predictor = "jev" })

	if strings.Contains(string(d.Payload), "return 1 + 0") || strings.Contains(string(d.Payload), "hunk_text") {
		t.Errorf("a structure request carries source text: %s", d.Payload)
	}
	if len(requestsOf(t, d.Payload)) != 1 {
		t.Errorf("payload = %s, want one request", d.Payload)
	}
}

func TestSelectDryRunPayloadEqualsWhatTheServerReceives(t *testing.T) {
	srv := newJevServer(t, "")
	f := lockedFixture(t, jevYML(srv, ""), extraFiles("e1", "e2", "e3"))
	t.Setenv(f.config(t).Predictor.Jev.APIKeyEnv, "sk-test-engine-key")
	f.writeLock(t, "jev", nil)

	dry := selectWith(t, f, func(o *Options) { o.DryRunPayload = true; o.load = failingLoader(t) })
	if got := srv.requests.Load(); got != 0 {
		t.Fatalf("a dry run sent %d request(s)", got)
	}
	planned := requestsOf(t, dry.Payload)
	if len(planned) != 5 {
		t.Fatalf("%d planned requests, want one per candidate: %s", len(planned), dry.Payload)
	}

	real := selectWith(t, f, func(o *Options) { o.load = clientLoader(srv.Client()) })
	requireNarrowed(t, real, "the real call")

	sent := srv.bodies()
	if len(sent) != len(planned) {
		t.Fatalf("%d requests sent, %d planned", len(sent), len(planned))
	}
	if !sameStrings(sortedStrings(sent), sortedStrings(planned)) {
		t.Error("the bytes the server received differ from the bytes of the dry run")
	}
}

func TestSelectDryRunPayloadIsEmptyWhenNothingExternalIsActive(t *testing.T) {
	t.Parallel()
	f := lockedFixture(t, lockedYML, nil)
	f.logistic(t, defaultWeight, defaultBias)
	f.writeLock(t, "logistic", nil)

	for name, mutate := range map[string]func(*Options){
		"the lock names a local predictor": func(*Options) {},
		"a local predictor is requested":   func(o *Options) { o.Predictor = "static" },
	} {
		d := selectWith(t, f, func(o *Options) { o.DryRunPayload = true; o.load = failingLoader(t); mutate(o) })

		if string(d.Payload) != `{"requests":[]}` || !hasWarning(d, "no external predictor in the active policy; nothing would be sent") {
			t.Errorf("%s: payload %q warnings %v", name, d.Payload, d.Warnings)
		}
	}
}

func TestSelectDryRunPayloadWithoutAnyPredictorNamed(t *testing.T) {
	t.Parallel()
	f := lockedFixture(t, lockedYML, nil)

	d := selectWith(t, f, func(o *Options) { o.DryRunPayload = true })

	if string(d.Payload) != `{"requests":[]}` || !hasWarning(d, "nothing would be sent") {
		t.Errorf("payload %q warnings %v, want the empty payload when neither a lock nor a request names a predictor", d.Payload, d.Warnings)
	}
}

func TestSelectDryRunPayloadIsEmptyWhenPrivacyForbidsIt(t *testing.T) {
	t.Parallel()
	for name, yml := range map[string]string{
		"external predictor disabled": lockedYML + "privacy:\n  allow_external_predictor: false\n",
		"hash redaction":              lockedYML + "privacy:\n  redact: hash\n",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := lockedFixture(t, yml, nil)

			d := selectWith(t, f, func(o *Options) { o.DryRunPayload = true; o.Predictor = "ensemble" })

			if string(d.Payload) != `{"requests":[]}` || !hasWarning(d, "dry run: no payload can be built") {
				t.Errorf("payload %q warnings %v, want the empty payload and the refusal", d.Payload, d.Warnings)
			}
		})
	}
}

func TestSelectDryRunPayloadOfNoCandidatesIsAnEmptyList(t *testing.T) {
	t.Parallel()
	f := newFixtureFiles(t, map[string]string{
		"go.mod":            "module example.com/m\n\ngo 1.21\n",
		"hazard.yml":        lockedYML,
		".gitignore":        ".hazard/\n",
		"core/core.go":      "package core\n\nfunc Core() int { return 1 }\n",
		"core/core_test.go": "package core\n\nimport \"testing\"\n\nfunc TestCore(t *testing.T) {\n\tif Core() != 1 {\n\t\tt.Fatal(\"bad\")\n\t}\n}\n",
	})
	f.write(t, map[string]string{"core/core.go": coreEdited})
	f.indexNow(t, false)

	d := selectWith(t, f, func(o *Options) { o.DryRunPayload = true; o.Predictor = "jev" })

	if d.L1 == nil || len(d.L1.Candidates) != 0 {
		t.Fatalf("premise: L1 = %+v, want no residual candidates", d.L1)
	}
	if string(d.Payload) != `{"requests":[]}` || hasWarning(d, "nothing would be sent") {
		t.Errorf("payload %q warnings %v, want an empty list without the no-predictor warning: a predictor is active and has nothing to ask", d.Payload, d.Warnings)
	}
}

// sortedStrings is the bodies as sorted strings, so two sets compare without
// regard to the order the shards were sent in.
func sortedStrings(bodies [][]byte) []string {
	out := make([]string, len(bodies))
	for i, b := range bodies {
		out[i] = string(b)
	}
	sort.Strings(out)
	return out
}
