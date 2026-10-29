package predict

import (
	"bytes"
	"errors"
	"net/http"
	"sort"
	"strings"
	"testing"
)

func sortedBodies(bodies [][]byte) []string {
	out := make([]string, len(bodies))
	for i, b := range bodies {
		out[i] = string(b)
	}
	sort.Strings(out)
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestJevDryRunPayloadEqualsWhatTheServerReceives(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*JevOptions)
		n      int
	}{
		{"three shards at structure", nil, 450},
		{"tier question", func(o *JevOptions) { o.AskTier = true }, 250},
		{"short rubric", func(o *JevOptions) { o.RubricVersion = "r1s" }, 205},
		{"none with hunk text", func(o *JevOptions) {
			o.Privacy.Redact = "none"
			o.Privacy.SendHunkText = true
		}, 3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake(t, answerAll(t, servedSnapshot, 0.0004, constant(0.3)))
			j := f.jev(t, tc.mutate)
			cands := candidatesN(tc.n)
			planned, err := j.Payload(sentinelChange(), cands)
			if err != nil {
				t.Fatalf("Payload: %v", err)
			}
			if _, err := j.Predict(t.Context(), sentinelChange(), cands); err != nil {
				t.Fatalf("Predict: %v", err)
			}
			sent := f.received()
			if len(sent) != len(planned) {
				t.Fatalf("%d requests sent, %d planned", len(sent), len(planned))
			}
			if !equalStrings(sortedBodies(sent), sortedBodies(planned)) {
				t.Fatal("the bytes the server received differ from the bytes Payload returned")
			}
		})
	}
}

func TestJevDryRunRetriesResendThePlannedBytes(t *testing.T) {
	ok := answerAll(t, servedSnapshot, 0.0004, constant(0.3))
	f := newFake(t, func(call int, body []byte, w http.ResponseWriter, r *http.Request) {
		if call <= 2 {
			http.Error(w, "busy", http.StatusBadGateway)
			return
		}
		ok(call, body, w, r)
	})
	j := f.jev(t, nil)
	planned, err := j.Payload(goldenChange(), goldenCandidates())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := j.Predict(t.Context(), goldenChange(), goldenCandidates()); err != nil {
		t.Fatal(err)
	}
	sent := f.received()
	if len(sent) != 3 {
		t.Fatalf("%d requests, want 3", len(sent))
	}
	for i, body := range sent {
		if !bytes.Equal(body, planned[0]) {
			t.Fatalf("attempt %d sent other bytes than Payload returned", i+1)
		}
	}
}

type forbiddenTransport struct{ t *testing.T }

func (f forbiddenTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	f.t.Errorf("dry run reached the network: %s", r.URL)
	return nil, errors.New("no network in a dry run")
}

func TestJevDryRunMakesNoNetworkCallAndNeverHoldsTheKey(t *testing.T) {
	j := newTestJev(t, func(o *JevOptions) {
		o.HTTPClient = &http.Client{Transport: forbiddenTransport{t}}
	})
	planned, err := j.Payload(goldenChange(), candidatesN(450))
	if err != nil {
		t.Fatal(err)
	}
	if len(planned) != 3 {
		t.Fatalf("%d shards", len(planned))
	}
	for _, body := range planned {
		if strings.Contains(string(body), testKey) {
			t.Fatal("the payload holds the API key")
		}
	}
}

func TestJevDryRunIsTheSameForEveryClient(t *testing.T) {
	a, err := newTestJev(t, nil).Payload(sentinelChange(), sentinelCandidates())
	if err != nil {
		t.Fatal(err)
	}
	b, err := newTestJev(t, func(o *JevOptions) { o.APIKey = "another-key" }).Payload(sentinelChange(), sentinelCandidates())
	if err != nil {
		t.Fatal(err)
	}
	if !equalStrings(sortedBodies(a), sortedBodies(b)) {
		t.Fatal("two clients built different payloads for the same input")
	}
}
