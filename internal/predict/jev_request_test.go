package predict

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/omorsi45/hazard/internal/config"
)

func TestJevRequestEstimateTokens(t *testing.T) {
	cases := []struct{ bytes, want int }{
		{0, 0}, {1, 1}, {3, 1}, {4, 2}, {7, 2}, {8, 3}, {35, 10}, {36, 11}, {7000, 2000}, {7001, 2001},
	}
	for _, tc := range cases {
		if got := estimateTokens(tc.bytes); got != tc.want {
			t.Errorf("estimateTokens(%d) = %d, want %d", tc.bytes, got, tc.want)
		}
	}
}

func TestJevRequestNewRefusals(t *testing.T) {
	base := func() JevOptions {
		return JevOptions{Config: jevConfig(), Privacy: config.Defaults().Privacy, RubricVersion: "r1", APIKey: testKey}
	}
	cases := []struct {
		name    string
		mutate  func(*JevOptions)
		wantErr string // empty means accepted
	}{
		{"structure", nil, ""},
		{"none", func(o *JevOptions) { o.Privacy.Redact = "none" }, ""},
		{"short rubric", func(o *JevOptions) { o.RubricVersion = "r1s" }, ""},
		{"empty rubric defaults", func(o *JevOptions) { o.RubricVersion = "" }, ""},
		{"external predictor disallowed", func(o *JevOptions) { o.Privacy.AllowExternalPredictor = false }, "allow_external_predictor"},
		{"hash", func(o *JevOptions) { o.Privacy.Redact = "hash" }, "hash"},
		{"unknown level fails closed", func(o *JevOptions) { o.Privacy.Redact = "bogus" }, "bogus"},
		{"empty level fails closed", func(o *JevOptions) { o.Privacy.Redact = "" }, "privacy.redact"},
		{"no key", func(o *JevOptions) { o.APIKey = "" }, "API key"},
		{"blank key", func(o *JevOptions) { o.APIKey = "  \t" }, "API key"},
		{"unknown rubric", func(o *JevOptions) { o.RubricVersion = "r9" }, "r9"},
		{"no questions per request", func(o *JevOptions) { o.Config.QuestionsPerRequest = 0 }, "questions_per_request"},
		{"no state tokens", func(o *JevOptions) { o.Config.MaxStateTokens = 0 }, "max_state_tokens"},
		{"no request tokens", func(o *JevOptions) { o.Config.MaxRequestTokens = 0 }, "max_request_tokens"},
		{"no concurrency", func(o *JevOptions) { o.Config.Concurrency = 0 }, "concurrency"},
		{"no timeout", func(o *JevOptions) { o.Config.TimeoutMS = 0 }, "timeout_ms"},
		{"negative retries", func(o *JevOptions) { o.Config.Retries = -1 }, "retries"},
		{"no model", func(o *JevOptions) { o.Config.Model = "" }, "model"},
		{"no endpoint", func(o *JevOptions) { o.Config.Endpoint = "" }, "endpoint"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			opts := base()
			if tc.mutate != nil {
				tc.mutate(&opts)
			}
			j, err := NewJev(opts)
			if tc.wantErr == "" {
				if err != nil || j == nil {
					t.Fatalf("NewJev = %v, %v, want a client", j, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("NewJev accepted %s, want an error mentioning %q", tc.name, tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error = %q, want it to mention %q", err, tc.wantErr)
			}
			if strings.Contains(err.Error(), testKey) {
				t.Fatalf("error leaks the key: %q", err)
			}
		})
	}
}

func TestJevRequestIdentity(t *testing.T) {
	j := newTestJev(t, nil)
	if j.Name() != "jev" || j.Version() != "typesafe/jev-1.13" {
		t.Fatalf("name %q version %q", j.Name(), j.Version())
	}
	var _ Predictor = j
}

func TestJevRequestGolden(t *testing.T) {
	cases := []struct {
		name    string
		fixture string
		mutate  func(*JevOptions)
	}{
		{"structure r1", "request-structure-r1.json", nil},
		{"structure r1s", "request-structure-r1s.json", func(o *JevOptions) { o.RubricVersion = "r1s" }},
		{"none with hunk text", "request-none-text.json", func(o *JevOptions) {
			o.Privacy.Redact = "none"
			o.Privacy.SendHunkText = true
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var want bytes.Buffer
			if err := json.Compact(&want, readFixture(t, tc.fixture)); err != nil {
				t.Fatalf("compact fixture: %v", err)
			}
			shards, err := newTestJev(t, tc.mutate).Payload(goldenChange(), goldenCandidates())
			if err != nil {
				t.Fatalf("Payload: %v", err)
			}
			if len(shards) != 1 {
				t.Fatalf("shards = %d, want 1", len(shards))
			}
			if !bytes.Equal(shards[0], want.Bytes()) {
				t.Fatalf("request differs from %s\n got: %s\nwant: %s", tc.fixture, shards[0], want.Bytes())
			}
		})
	}
}

func TestJevRequestEmptyCandidatesMakeNoShard(t *testing.T) {
	shards, err := newTestJev(t, nil).Payload(goldenChange(), nil)
	if err != nil || len(shards) != 0 {
		t.Fatalf("Payload = %d shards, %v, want none", len(shards), err)
	}
}

func TestJevRequestNilChangeIsAnError(t *testing.T) {
	if _, err := newTestJev(t, nil).Payload(nil, candidatesN(1)); err == nil {
		t.Fatal("a nil change set must be an error")
	}
}

func TestJevRequestShardsByQuestionCount(t *testing.T) {
	shards, err := newTestJev(t, nil).Payload(goldenChange(), candidatesN(450))
	if err != nil {
		t.Fatalf("Payload: %v", err)
	}
	if len(shards) != 3 {
		t.Fatalf("450 candidates with 200 questions per request gave %d shards, want 3", len(shards))
	}
	wantCounts := []int{200, 200, 50}
	seen := map[string]bool{}
	for i, body := range shards {
		r := decodeRequest(t, body)
		if len(r.Questions) != wantCounts[i] || len(r.State.Tests) != wantCounts[i] {
			t.Fatalf("shard %d: %d questions and %d tests, want %d", i, len(r.Questions), len(r.State.Tests), wantCounts[i])
		}
		for _, key := range sortedKeys(r.Questions) {
			if _, ok := r.State.Tests[key]; !ok {
				t.Fatalf("shard %d: question %s has no test", i, key)
			}
		}
		for _, raw := range r.State.Tests {
			var d struct{ Name string }
			if err := json.Unmarshal(raw, &d); err != nil {
				t.Fatal(err)
			}
			if seen[d.Name] {
				t.Fatalf("test %s sent twice", d.Name)
			}
			seen[d.Name] = true
		}
		if _, ok := r.State.Tests["t_001"]; !ok {
			t.Fatalf("shard %d does not number its tests from t_001", i)
		}
	}
	if len(seen) != 450 {
		t.Fatalf("%d distinct tests sent, want 450", len(seen))
	}
}

func TestJevRequestShardsFollowTestIDOrder(t *testing.T) {
	cands := candidatesN(5)
	shuffled := []int{3, 0, 4, 1, 2}
	mixed := make([]string, 0, len(cands))
	reordered := cands[:0:0]
	for _, i := range shuffled {
		reordered = append(reordered, cands[i])
	}
	j := newTestJev(t, func(o *JevOptions) { o.Config.QuestionsPerRequest = 2 })
	shards, err := j.Payload(goldenChange(), reordered)
	if err != nil {
		t.Fatal(err)
	}
	for _, body := range shards {
		r := decodeRequest(t, body)
		for _, key := range sortedKeys(r.State.Tests) {
			var d struct{ Name string }
			if err := json.Unmarshal(r.State.Tests[key], &d); err != nil {
				t.Fatal(err)
			}
			mixed = append(mixed, d.Name)
		}
	}
	if got, want := strings.Join(mixed, ","), "Test0001,Test0002,Test0003,Test0004,Test0005"; got != want {
		t.Fatalf("tests were sharded in the order %s, want TestID order %s", got, want)
	}
	if len(shards) != 3 {
		t.Fatalf("5 candidates with 2 per request gave %d shards, want 3", len(shards))
	}
}

func TestJevRequestShardsByRequestTokens(t *testing.T) {
	const limit = 1800
	j := newTestJev(t, func(o *JevOptions) { o.Config.MaxRequestTokens = limit })
	shards, err := j.Payload(goldenChange(), candidatesN(60))
	if err != nil {
		t.Fatalf("Payload: %v", err)
	}
	if len(shards) < 3 {
		t.Fatalf("a %d token limit gave only %d shards for 60 candidates", limit, len(shards))
	}
	total := 0
	for i, body := range shards {
		if got := estimateTokens(len(body)); got > limit {
			t.Fatalf("shard %d estimates %d tokens, over the limit %d", i, got, limit)
		}
		r := decodeRequest(t, body)
		if len(r.Questions) == 0 {
			t.Fatalf("shard %d has no question", i)
		}
		total += len(r.Questions)
	}
	if total != 60 {
		t.Fatalf("%d questions sent, want 60", total)
	}
	if one := (len(shards[0])); one >= int(float64(limit)*3.5)+1 {
		t.Fatalf("first shard is %d bytes", one)
	}
}

func TestJevRequestShardsByStateTokens(t *testing.T) {
	const limit = 900
	j := newTestJev(t, func(o *JevOptions) {
		o.Config.MaxStateTokens = limit
		o.Config.MaxRequestTokens = 28000
	})
	shards, err := j.Payload(goldenChange(), candidatesN(60))
	if err != nil {
		t.Fatalf("Payload: %v", err)
	}
	if len(shards) < 2 {
		t.Fatalf("a %d token state limit gave only %d shard for 60 candidates", limit, len(shards))
	}
	for i, body := range shards {
		if got := estimateTokens(len(stateBytes(t, body))); got > limit {
			t.Fatalf("shard %d state estimates %d tokens, over the limit %d", i, got, limit)
		}
	}
}

func TestJevRequestTruncatesLongDescriptor(t *testing.T) {
	cands := candidatesN(3)
	cands[1].Purpose = strings.Repeat("long purpose text ", 2000)
	cands[1].Asserts = []string{"first == 1", strings.Repeat("x", 5000)}
	j := newTestJev(t, func(o *JevOptions) { o.Privacy.Redact = "none" })
	shards, err := j.Payload(goldenChange(), cands)
	if err != nil {
		t.Fatalf("Payload: %v", err)
	}
	if len(shards) != 1 {
		t.Fatalf("shards = %d, want 1", len(shards))
	}
	r := decodeRequest(t, shards[0])
	longest, shortest := 0, 1<<30
	for _, raw := range r.State.Tests {
		longest = max(longest, len(raw))
		shortest = min(shortest, len(raw))
	}
	if got := estimateTokens(longest); got > 2000 {
		t.Fatalf("a descriptor of %d bytes (%d tokens) was not cut to 2000 tokens", longest, got)
	}
	if longest < 6000 {
		t.Fatalf("the long descriptor was cut to %d bytes, want it filled up to the limit", longest)
	}
	if shortest > 400 {
		t.Fatalf("a short descriptor grew to %d bytes", shortest)
	}
	var cut struct{ Name, Purpose string }
	if err := json.Unmarshal(r.State.Tests["t_002"], &cut); err != nil {
		t.Fatal(err)
	}
	if cut.Name != "Test0002" || !strings.HasPrefix(cut.Purpose, "long purpose text") {
		t.Fatalf("truncation must keep the name and the start of the purpose: %+v", cut)
	}
}

func TestJevRequestChangeBlockTooLargeIsAnError(t *testing.T) {
	j := newTestJev(t, func(o *JevOptions) { o.Config.MaxStateTokens = 100 })
	_, err := j.Payload(goldenChange(), candidatesN(2))
	if err == nil || !strings.Contains(err.Error(), "max_state_tokens") {
		t.Fatalf("Payload = %v, want an error naming max_state_tokens", err)
	}
	j = newTestJev(t, func(o *JevOptions) { o.Config.MaxRequestTokens = 100 })
	_, err = j.Payload(goldenChange(), candidatesN(2))
	if err == nil || !strings.Contains(err.Error(), "max_request_tokens") {
		t.Fatalf("Payload = %v, want an error naming max_request_tokens", err)
	}
}

func TestJevRequestRubricForms(t *testing.T) {
	long := decodeRequest(t, mustPayload(t, newTestJev(t, nil), 1)[0])
	short := decodeRequest(t, mustPayload(t, newTestJev(t, func(o *JevOptions) { o.RubricVersion = "r1s" }), 1)[0])
	if !strings.HasPrefix(long.State.Rubric, "A test BREAKS when the change alters any behavior") {
		t.Fatalf("rubric = %q", long.State.Rubric)
	}
	if short.State.Rubric != long.State.Rubric {
		t.Fatal("both forms share one rubric text")
	}
	if got, want := long.Questions["t_001"].Instructions, "Applying state.rubric to state.change, could the change cause state.tests.t_001 to go from passing to failing?"; got != want {
		t.Fatalf("r1 instructions = %q, want %q", got, want)
	}
	if got, want := short.Questions["t_001"].Instructions, "Break state.tests.t_001? Use state.rubric."; got != want {
		t.Fatalf("r1s instructions = %q, want %q", got, want)
	}
	if long.Questions["t_001"].Type != "noul" || len(long.Questions["t_001"].Criteria) != 2 {
		t.Fatalf("question = %+v", long.Questions["t_001"])
	}
}

func TestJevRequestTierQuestionOnlyInTheFirstShard(t *testing.T) {
	asked := newTestJev(t, func(o *JevOptions) { o.AskTier = true })
	shards, err := asked.Payload(goldenChange(), candidatesN(250))
	if err != nil {
		t.Fatal(err)
	}
	if len(shards) != 2 {
		t.Fatalf("250 candidates and a tier question gave %d shards, want 2", len(shards))
	}
	first, second := decodeRequest(t, shards[0]), decodeRequest(t, shards[1])
	tier, ok := first.Questions["tier"]
	if !ok {
		t.Fatalf("no tier question in the first shard: %v", sortedKeys(first.Questions))
	}
	if tier.Type != "choice" || len(tier.Criteria) != 3 || tier.Criteria["targeted"] == "" || tier.Criteria["module"] == "" || tier.Criteria["suite"] == "" {
		t.Fatalf("tier question = %+v", tier)
	}
	if len(first.Questions) != 200 || len(first.State.Tests) != 199 {
		t.Fatalf("first shard: %d questions and %d tests, the tier question counts as a question", len(first.Questions), len(first.State.Tests))
	}
	if _, ok := second.Questions["tier"]; ok {
		t.Fatal("tier question repeated in the second shard")
	}
	none, err := newTestJev(t, nil).Payload(goldenChange(), candidatesN(2))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := decodeRequest(t, none[0]).Questions["tier"]; ok {
		t.Fatal("tier question asked without AskTier")
	}
}

func TestJevRequestIsDeterministic(t *testing.T) {
	j := newTestJev(t, nil)
	a, err := j.Payload(goldenChange(), candidatesN(300))
	if err != nil {
		t.Fatal(err)
	}
	b, err := j.Payload(goldenChange(), candidatesN(300))
	if err != nil {
		t.Fatal(err)
	}
	if len(a) != len(b) || len(a) != 2 {
		t.Fatalf("%d and %d shards, want 2 each", len(a), len(b))
	}
	for i := range a {
		if !bytes.Equal(a[i], b[i]) {
			t.Fatalf("shard %d differs between two builds", i)
		}
	}
}

func mustPayload(t *testing.T, j *Jev, n int) [][]byte {
	t.Helper()
	shards, err := j.Payload(goldenChange(), candidatesN(n))
	if err != nil {
		t.Fatalf("Payload: %v", err)
	}
	if len(shards) == 0 {
		t.Fatal("Payload built no shard")
	}
	return shards
}

func TestJevRequestSizeArithmeticMatchesBodies(t *testing.T) {
	for _, ask := range []bool{false, true} {
		j := newTestJev(t, func(o *JevOptions) {
			o.AskTier = ask
			o.Config.QuestionsPerRequest = 5
		})
		b, err := j.prepare(goldenChange(), candidatesN(12))
		if err != nil {
			t.Fatal(err)
		}
		shards, err := b.shards(b.descs)
		if err != nil {
			t.Fatal(err)
		}
		if len(shards) < 3 {
			t.Fatalf("ask tier %v: %d shards", ask, len(shards))
		}
		for _, s := range shards {
			predicted := b.baseRequest + s.testBytes + max(len(s.members)-1, 0) + s.questionBytes + max(s.questions-1, 0)
			if predicted != len(s.body) {
				t.Fatalf("ask tier %v, shard %d: predicted %d bytes, serialized %d", ask, s.index, predicted, len(s.body))
			}
			state := b.baseState + s.testBytes + max(len(s.members)-1, 0)
			if got := len(stateBytes(t, s.body)); got != state {
				t.Fatalf("ask tier %v, shard %d: predicted state %d bytes, serialized %d", ask, s.index, state, got)
			}
		}
	}
}
