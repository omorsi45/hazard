package mine

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

func githubRequest() Request {
	return Request{Provider: "github", Project: "owner/repo", Branch: "main", Since: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), Until: time.Date(2026, 1, 1, 0, 0, 4, 0, time.UTC), MaxPipelines: 20}
}

func githubFixture(t *testing.T, request Request, handler http.HandlerFunc) *githubProvider {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	client := mustHTTPClient(t, server.Client(), server.URL, "github")
	provider, err := newGitHubProvider(request, client)
	if err != nil {
		t.Fatal(err)
	}
	return provider
}

func githubRunJSON(id int64, seconds int, branch, project string) map[string]any {
	return map[string]any{"id": id, "head_sha": strings.Repeat("a", 40), "head_branch": branch, "status": "completed", "created_at": githubRequest().Since.Add(time.Duration(seconds) * time.Second).Format(time.RFC3339), "run_attempt": 2, "repository": map[string]any{"full_name": project}}
}

func githubJSON(t *testing.T, w http.ResponseWriter, value any) {
	t.Helper()
	if err := json.NewEncoder(w).Encode(value); err != nil {
		t.Error(err)
	}
}

func githubNext(w http.ResponseWriter, r *http.Request, page int) {
	q := r.URL.Query()
	q.Set("page", strconv.Itoa(page))
	w.Header().Set("Link", "<"+r.URL.Path+"?"+q.Encode()+`>; rel="next"`)
}

func TestGitHubConstructor(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("invalid constructor performed acquisition") }))
	defer server.Close()
	client := mustHTTPClient(t, server.Client(), server.URL, "github")
	if _, err := newGitHubProvider(githubRequest(), client); err != nil {
		t.Fatal("valid source request rejected")
	}
	for _, tc := range []struct {
		name   string
		change func(*Request)
	}{
		{"provider", func(r *Request) { r.Provider = "gitlab" }},
		{"project", func(r *Request) { r.Project = "owner/repo/extra" }},
		{"project_syntax", func(r *Request) { r.Project = "owner/../repo" }},
		{"window", func(r *Request) { r.Until = r.Since.Add(-time.Second) }},
		{"limit", func(r *Request) { r.MaxPipelines = 0 }},
		{"language", func(r *Request) { r.Language = "ruby" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := githubRequest()
			tc.change(&r)
			if _, err := newGitHubProvider(r, client); err == nil {
				t.Fatal("invalid source request accepted")
			}
		})
	}
	if _, err := newGitHubProvider(githubRequest(), nil); err == nil {
		t.Fatal("missing HTTP source accepted")
	}
	other := mustHTTPClient(t, server.Client(), server.URL, "gitlab")
	if _, err := newGitHubProvider(githubRequest(), other); err == nil {
		t.Fatal("wrong HTTP provider accepted")
	}
}

func TestGitHubRunHistory(t *testing.T) {
	calls := 0
	p := githubFixture(t, githubRequest(), func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Path != "/repos/owner/repo/actions/runs" || r.URL.Query().Get("branch") != "main" || r.URL.Query().Get("per_page") != "100" || r.URL.Query().Get("created") == "" {
			t.Error("run request lost endpoint or historical filters")
		}
		if r.URL.Query().Get("page") == "2" {
			githubJSON(t, w, map[string]any{"total_count": 4, "workflow_runs": []any{githubRunJSON(17, 1, "main", "owner/repo"), githubRunJSON(9007199254740993, 2, "main", "owner/repo"), githubRunJSON(19, 4, "main", "owner/repo")}})
			return
		}
		githubNext(w, r, 2)
		githubJSON(t, w, map[string]any{"total_count": 4, "workflow_runs": []any{githubRunJSON(17, 1, "main", "owner/repo")}})
	})
	runs, gaps, err := p.listRuns(context.Background())
	if err != nil || len(gaps) != 0 || len(runs) != 2 || calls != 2 || runs[0].ID != "17" || runs[1].ID != "9007199254740993" || runs[0].RunAttempt == nil || *runs[0].RunAttempt != 2 {
		t.Fatalf("history lost exact IDs, attempt, boundaries or pagination: runs=%d gaps=%d calls=%d err=%v", len(runs), len(gaps), calls, err)
	}
}

func TestGitHubRunScope(t *testing.T) {
	for _, tc := range []struct {
		name, branch, project string
		invalid               bool
	}{
		{"wrong_branch", "other", "owner/repo", false},
		{"wrong_source", "main", "other/repo", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := githubFixture(t, githubRequest(), func(w http.ResponseWriter, r *http.Request) {
				githubJSON(t, w, map[string]any{"total_count": 1, "workflow_runs": []any{githubRunJSON(17, 1, tc.branch, tc.project)}})
			})
			runs, gaps, err := p.listRuns(context.Background())
			if len(runs) != 0 || (tc.invalid && (err == nil || len(gaps) == 0)) {
				t.Fatal("out-of-scope source became an inspected run")
			}
		})
	}
}

func TestGitHubRunPagination(t *testing.T) {
	for _, tc := range []struct{ name, link string }{
		{"filter_switch", `<?page=2>; rel="next"`},
		{"endpoint_switch", `</repos/owner/repo/actions/artifacts?page=2>; rel="next"`},
		{"loop", "loop"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			p := githubFixture(t, githubRequest(), func(w http.ResponseWriter, r *http.Request) {
				calls++
				if tc.link == "loop" {
					page := 2
					if r.URL.Query().Get("page") == "2" {
						page = 1
					}
					githubNext(w, r, page)
				} else {
					w.Header().Set("Link", tc.link)
				}
				githubJSON(t, w, map[string]any{"total_count": 1, "workflow_runs": []any{githubRunJSON(17, 1, "main", "owner/repo")}})
			})
			runs, gaps, err := p.listRuns(context.Background())
			limit := 1
			if tc.name == "loop" {
				limit = 2
			}
			if err == nil || len(gaps) == 0 || len(runs) != 1 || calls > limit {
				t.Fatal("unsafe or looping pagination claimed complete history")
			}
		})
	}
}

func TestGitHubRunCaps(t *testing.T) {
	t.Run("split", func(t *testing.T) {
		calls := 0
		p := githubFixture(t, githubRequest(), func(w http.ResponseWriter, r *http.Request) {
			calls++
			bounds := strings.Split(r.URL.Query().Get("created"), "..")
			if len(bounds) != 2 {
				t.Error("missing bounded window")
				return
			}
			start, _ := time.Parse(time.RFC3339, bounds[0])
			end, _ := time.Parse(time.RFC3339, bounds[1])
			total := 2
			data := []any{githubRunJSON(17, 0, "main", "owner/repo"), githubRunJSON(18, 2, "main", "owner/repo")}
			if end.Sub(start) > 2*time.Second {
				total = 1000
				data = []any{githubRunJSON(19, 3, "main", "owner/repo")}
			} else if start.Equal(githubRequest().Since.Add(2 * time.Second)) {
				data = []any{githubRunJSON(18, 2, "main", "owner/repo"), githubRunJSON(19, 3, "main", "owner/repo")}
			}
			githubJSON(t, w, map[string]any{"total_count": total, "workflow_runs": data})
		})
		runs, gaps, err := p.listRuns(context.Background())
		if err != nil || len(gaps) != 0 || len(runs) != 3 || calls != 3 {
			t.Fatal("filtered API cap did not split shrinking windows and deduplicate boundaries")
		}
	})
	t.Run("irreducible", func(t *testing.T) {
		r := githubRequest()
		r.Until = r.Since.Add(time.Second)
		p := githubFixture(t, r, func(w http.ResponseWriter, q *http.Request) {
			githubJSON(t, w, map[string]any{"total_count": 1000, "workflow_runs": []any{githubRunJSON(17, 0, "main", "owner/repo")}})
		})
		runs, gaps, err := p.listRuns(context.Background())
		if err != nil || len(runs) != 1 || len(gaps) == 0 || gaps[0].Reason != "discovery_capped" {
			t.Fatal("irreducible filtered cap claimed exhaustive history")
		}
	})
	t.Run("caller", func(t *testing.T) {
		r := githubRequest()
		r.MaxPipelines = 1
		p := githubFixture(t, r, func(w http.ResponseWriter, q *http.Request) {
			githubJSON(t, w, map[string]any{"total_count": 2, "workflow_runs": []any{githubRunJSON(17, 0, "main", "owner/repo"), githubRunJSON(18, 1, "main", "owner/repo")}})
		})
		runs, gaps, err := p.listRuns(context.Background())
		if err != nil || len(runs) != 1 || len(gaps) == 0 || gaps[0].Reason != "discovery_capped" {
			t.Fatal("caller limit was ignored or reported as complete")
		}
	})
}

func TestGitHubRunCancellation(t *testing.T) {
	p := githubFixture(t, githubRequest(), func(http.ResponseWriter, *http.Request) { t.Error("canceled discovery performed HTTP acquisition") })
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	runs, gaps, err := p.listRuns(ctx)
	if !errors.Is(err, context.Canceled) || len(runs) != 0 || len(gaps) == 0 {
		t.Fatal("cancellation claimed complete acquisition")
	}
}

func TestGitHubRunMetadata(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(map[string]any)
	}{
		{"missing_id", func(v map[string]any) { v["id"] = 0 }},
		{"missing_time", func(v map[string]any) { v["created_at"] = nil }},
		{"invalid_source_sha", func(v map[string]any) { v["head_sha"] = "not-a-sha" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := githubFixture(t, githubRequest(), func(w http.ResponseWriter, r *http.Request) {
				v := githubRunJSON(17, 1, "main", "owner/repo")
				tc.change(v)
				githubJSON(t, w, map[string]any{"total_count": 1, "workflow_runs": []any{v}})
			})
			runs, gaps, err := p.listRuns(context.Background())
			if len(runs) != 0 || len(gaps) == 0 || err == nil {
				t.Fatal("malformed source metadata established requested history")
			}
		})
	}
}

func githubSourceRun() sourceRun {
	r := githubRequest()
	attempt := 2
	created := r.Since.Add(time.Second)
	return sourceRun{ID: "17", SourceHeadSHA: strings.Repeat("a", 40), Branch: "main", Status: "completed", CreatedAt: &created, RunAttempt: &attempt}
}

func githubGapSink(gaps *[]acquisitionGap) acquisitionSink {
	return acquisitionSink{Artifact: func(acquiredArtifact) error { return nil }, Gap: func(g acquisitionGap) error { *gaps = append(*gaps, g); return nil }}
}

func githubArtifactJSON(id int64) map[string]any {
	return map[string]any{"id": id, "size_in_bytes": 21, "digest": "sha256:" + strings.Repeat("b", 64), "expired": false, "created_at": githubRequest().Since.Add(time.Second).Format(time.RFC3339), "workflow_run": map[string]any{"id": 17, "head_sha": strings.Repeat("a", 40), "head_branch": "main"}}
}

func TestGitHubAttempts(t *testing.T) {
	var paths []string
	p := githubFixture(t, githubRequest(), func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		attempt := 1
		if strings.Contains(r.URL.Path, "/attempts/2/") {
			attempt = 2
		}
		if !strings.Contains(r.URL.Path, "/attempts/") || r.URL.Query().Get("per_page") != "100" {
			t.Error("jobs did not use exact attempt scope")
		}
		githubJSON(t, w, map[string]any{"total_count": 1, "jobs": []any{map[string]any{"id": 200 + attempt, "run_id": 17, "run_attempt": attempt, "status": "completed"}}})
	})
	var gaps []acquisitionGap
	outcome := runOutcome{State: "complete"}
	jobs, err := p.listJobs(context.Background(), githubSourceRun(), githubGapSink(&gaps), &outcome)
	if err != nil || len(jobs) != 2 || len(paths) != 2 || len(gaps) != 0 || jobs[0].ID != 201 || jobs[1].ID != 202 || jobs[0].Attempt != 1 || jobs[1].Attempt != 2 || len(outcome.Attempts) != 2 {
		t.Fatal("actual jobs or attempts were lost or guessed")
	}
}

func TestGitHubJobGaps(t *testing.T) {
	for _, tc := range []struct {
		name     string
		mismatch bool
	}{{"missing_attempt", false}, {"wrong_attempt", true}} {
		t.Run(tc.name, func(t *testing.T) {
			p := githubFixture(t, githubRequest(), func(w http.ResponseWriter, r *http.Request) {
				if strings.Contains(r.URL.Path, "/attempts/2/") {
					if !tc.mismatch {
						w.WriteHeader(http.StatusNotFound)
						return
					}
					githubJSON(t, w, map[string]any{"total_count": 1, "jobs": []any{map[string]any{"id": 202, "run_id": 17, "run_attempt": 1, "status": "completed"}}})
					return
				}
				githubJSON(t, w, map[string]any{"total_count": 1, "jobs": []any{map[string]any{"id": 201, "run_id": 17, "run_attempt": 1, "status": "completed"}}})
			})
			var gaps []acquisitionGap
			outcome := runOutcome{State: "complete"}
			jobs, err := p.listJobs(context.Background(), githubSourceRun(), githubGapSink(&gaps), &outcome)
			if err != nil || len(jobs) != 1 || len(gaps) != 1 || outcome.State == "complete" || gaps[0].Attempt == nil || *gaps[0].Attempt != 2 {
				t.Fatal("missing or inconsistent attempt became complete execution evidence")
			}
		})
	}
}

func TestGitHubArtifactListing(t *testing.T) {
	calls := 0
	p := githubFixture(t, githubRequest(), func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Path != "/repos/owner/repo/actions/runs/17/artifacts" || r.URL.Query().Get("per_page") != "100" {
			t.Error("artifact listing used guessed job or attempt association")
		}
		if r.URL.Query().Get("page") == "2" {
			v := githubArtifactJSON(302)
			v["expired"] = true
			githubJSON(t, w, map[string]any{"total_count": 2, "artifacts": []any{v}})
			return
		}
		githubNext(w, r, 2)
		githubJSON(t, w, map[string]any{"total_count": 2, "artifacts": []any{githubArtifactJSON(301)}})
	})
	var gaps []acquisitionGap
	outcome := runOutcome{State: "complete"}
	artifacts, err := p.listArtifacts(context.Background(), githubSourceRun(), githubGapSink(&gaps), &outcome)
	if err != nil || calls != 2 || len(artifacts) != 2 || len(gaps) != 0 || artifacts[0].ID != 301 || artifacts[0].CreatedAt == nil || artifacts[0].Size == nil || *artifacts[0].Size != 21 || !artifacts[1].Expired {
		t.Fatal("independent artifact source facts were discarded or joined to current attempt")
	}
}

func TestGitHubListingBounds(t *testing.T) {
	t.Run("attempts", func(t *testing.T) {
		calls := 0
		p := githubFixture(t, githubRequest(), func(w http.ResponseWriter, r *http.Request) {
			calls++
			githubJSON(t, w, map[string]any{"total_count": 0, "jobs": []any{}})
		})
		run := githubSourceRun()
		attempt := 101
		run.RunAttempt = &attempt
		var gaps []acquisitionGap
		outcome := runOutcome{State: "complete"}
		_, err := p.listJobs(context.Background(), run, githubGapSink(&gaps), &outcome)
		if err != nil || calls != 100 || outcome.State != "capped" || len(gaps) == 0 {
			t.Fatal("attempt policy cap was ignored or reported complete")
		}
	})
	t.Run("jobs", func(t *testing.T) {
		calls := 0
		p := githubFixture(t, githubRequest(), func(w http.ResponseWriter, r *http.Request) {
			calls++
			page, _ := strconv.Atoi(r.URL.Query().Get("page"))
			data := make([]any, 100)
			for i := range data {
				data[i] = map[string]any{"id": page*100 + i + 1, "run_id": 17, "run_attempt": 1, "status": "completed"}
			}
			if page < 11 {
				githubNext(w, r, page+1)
			}
			githubJSON(t, w, map[string]any{"total_count": 1100, "jobs": data})
		})
		run := githubSourceRun()
		attempt := 1
		run.RunAttempt = &attempt
		var gaps []acquisitionGap
		outcome := runOutcome{State: "complete"}
		jobs, err := p.listJobs(context.Background(), run, githubGapSink(&gaps), &outcome)
		if err != nil || calls != 10 || len(jobs) != 1000 || outcome.State != "capped" || len(gaps) == 0 {
			t.Fatal("per-run job policy cap was ignored or reported complete")
		}
	})
	t.Run("artifacts", func(t *testing.T) {
		calls := 0
		p := githubFixture(t, githubRequest(), func(w http.ResponseWriter, r *http.Request) {
			calls++
			page, _ := strconv.Atoi(r.URL.Query().Get("page"))
			data := make([]any, 100)
			for i := range data {
				data[i] = githubArtifactJSON(int64(page*100 + i + 1))
			}
			if page < 11 {
				githubNext(w, r, page+1)
			}
			githubJSON(t, w, map[string]any{"total_count": 1100, "artifacts": data})
		})
		var gaps []acquisitionGap
		outcome := runOutcome{State: "complete"}
		artifacts, err := p.listArtifacts(context.Background(), githubSourceRun(), githubGapSink(&gaps), &outcome)
		if err != nil || calls != 10 || len(artifacts) != 1000 || outcome.State != "capped" || len(gaps) == 0 {
			t.Fatal("artifact policy cap was ignored or reported complete")
		}
	})
}

func TestGitHubGapCallbackStops(t *testing.T) {
	sentinel := errors.New("fixture writer failure")
	calls := 0
	p := githubFixture(t, githubRequest(), func(w http.ResponseWriter, r *http.Request) { calls++; w.WriteHeader(http.StatusNotFound) })
	sink := acquisitionSink{Artifact: func(acquiredArtifact) error { return nil }, Gap: func(acquisitionGap) error { return sentinel }}
	outcome := runOutcome{State: "complete"}
	_, err := p.listJobs(context.Background(), githubSourceRun(), sink, &outcome)
	if !errors.Is(err, sentinel) || calls != 1 || outcome.State == "complete" {
		t.Fatal("gap writer failure was swallowed or acquisition continued")
	}
}

type githubAcquireFixture struct {
	Raw                                                                                     []byte
	ReportName                                                                              string
	Language                                                                                string
	NoProducer, Ambiguous, MissingTime, Expired, WrongRun, WrongDigest, WrongSize, GoReport bool
	TwoArtifacts, SecondProducer                                                            bool
	DuplicateJobs, ReusedJobID                                                              bool
	LogStatus                                                                               int
	JobStatus                                                                               string
	LogBytes                                                                                int
	Jobs                                                                                    int
	ZipCalls, LogCalls, ModuleCalls, CommitCalls                                            int
	FailEndpoint                                                                            string
	FailStatus                                                                              int
	FailedCalls                                                                             int
	Oversized                                                                               bool
	CommitStatus                                                                            int
}

func githubReportZIP(t *testing.T, name, report string) []byte {
	t.Helper()
	var b bytes.Buffer
	z := zip.NewWriter(&b)
	f, err := z.Create(name)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte(report)); err != nil {
		t.Fatal(err)
	}
	if err := z.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func githubAcquireProvider(t *testing.T, c *githubAcquireFixture) *githubProvider {
	t.Helper()
	r := githubRequest()
	r.Language = c.Language
	name := c.ReportName
	if name == "" {
		name = "reports/junit.xml"
	}
	report := `<testsuite><testcase classname="tests.test_value" name="test_value[param]" time="0.012"><failure type="AssertionError" message="actual assertion">raw diagnostic</failure></testcase></testsuite>`
	if c.GoReport {
		report = `<testsuite><testcase classname="example.org/project/pkg" name="TestValue"/></testsuite>`
	}
	if c.Raw == nil {
		c.Raw = githubReportZIP(t, name, report)
	}
	digest := sha256.Sum256(c.Raw)
	hash := hex.EncodeToString(digest[:])
	return githubFixture(t, r, func(w http.ResponseWriter, q *http.Request) {
		if c.FailEndpoint != "" && strings.HasSuffix(q.URL.Path, c.FailEndpoint) {
			c.FailedCalls++
			w.WriteHeader(c.FailStatus)
			_, _ = w.Write([]byte("fixture-private-response"))
			return
		}
		switch {
		case strings.HasSuffix(q.URL.Path, "/jobs"):
			attempt := 1
			if strings.Contains(q.URL.Path, "/attempts/2/") {
				attempt = 2
			}
			status := c.JobStatus
			if status == "" {
				status = "completed"
			}
			count := 1
			if c.Jobs > 0 {
				count = c.Jobs
			}
			data := make([]any, count)
			for i := range data {
				data[i] = map[string]any{"id": 200 + attempt + i*10, "run_id": 17, "run_attempt": attempt, "status": status}
				if c.ReusedJobID {
					data[i].(map[string]any)["id"] = 201
				}
			}
			if c.DuplicateJobs {
				data = append(data, data...)
				count = len(data)
			}
			githubJSON(t, w, map[string]any{"total_count": count, "jobs": data})
		case strings.HasSuffix(q.URL.Path, "/artifacts"):
			v := githubArtifactJSON(301)
			v["digest"] = "sha256:" + hash
			v["size_in_bytes"] = len(c.Raw)
			if c.Oversized {
				v["size_in_bytes"] = (64 << 20) + 1
			}
			v["expired"] = c.Expired
			v["created_at"] = r.Since.Add(2 * time.Second).Format(time.RFC3339)
			if c.MissingTime {
				v["created_at"] = nil
			}
			if c.WrongDigest {
				v["digest"] = "sha256:" + strings.Repeat("f", 64)
			}
			if c.WrongSize {
				v["size_in_bytes"] = len(c.Raw) + 1
			}
			if c.WrongRun {
				v["workflow_run"] = map[string]any{"id": 18}
			}
			data := []any{v}
			if c.TwoArtifacts {
				copy := githubArtifactJSON(302)
				copy["digest"] = "sha256:" + hash
				copy["size_in_bytes"] = len(c.Raw)
				data = append(data, copy)
			}
			githubJSON(t, w, map[string]any{"total_count": len(data), "artifacts": data})
		case strings.HasSuffix(q.URL.Path, "/zip"):
			c.ZipCalls++
			_, _ = w.Write(c.Raw)
		case strings.HasSuffix(q.URL.Path, "/logs"):
			c.LogCalls++
			if c.LogStatus != 0 {
				w.WriteHeader(c.LogStatus)
				return
			}
			if c.LogBytes > 0 {
				_, _ = w.Write(bytes.Repeat([]byte("x"), c.LogBytes))
				return
			}
			log, _ := provenanceFixture()
			log = bytes.ReplaceAll(log, []byte("10715173308"), []byte("301"))
			log = bytes.ReplaceAll(log, []byte("e88639bc11ac82b50d7d908ba1b596b43a99c8aa3cdcf16447332033cb11c515"), []byte(hash))
			log = bytes.ReplaceAll(log, []byte("572407 bytes"), []byte(strconv.Itoa(len(c.Raw))+" bytes"))
			if c.SecondProducer && !strings.Contains(q.URL.Path, "/201/") {
				log = bytes.ReplaceAll(log, []byte("Artifact ID 301"), []byte("Artifact ID 302"))
				log = bytes.ReplaceAll(log, []byte("Artifact ID is 301"), []byte("Artifact ID is 302"))
			} else if !c.Ambiguous && !strings.Contains(q.URL.Path, "/201/") {
				log = bytes.ReplaceAll(log, []byte("Artifact ID 301"), []byte("Artifact ID 999"))
				log = bytes.ReplaceAll(log, []byte("Artifact ID is 301"), []byte("Artifact ID is 999"))
			}
			if c.NoProducer {
				log = []byte("unsupported fixture log\n")
			}
			if c.GoReport {
				log = bytes.ReplaceAll(log, []byte("uv run --no-sync pytest --junitxml=reports/junit.xml"), []byte("go test ./..."))
			}
			_, _ = w.Write(log)
		case strings.HasSuffix(q.URL.Path, "/contents/go.mod"):
			c.ModuleCalls++
			if q.URL.Query().Get("ref") != "3329023300bb33fd3fa6430e2dafbb158deb0508" {
				t.Error("module lookup substituted source head or branch")
			}
			githubJSON(t, w, map[string]any{"type": "file", "path": "go.mod", "encoding": "base64", "content": base64.StdEncoding.EncodeToString([]byte("module example.org/project\n"))})
		case strings.Contains(q.URL.Path, "/commits/"):
			c.CommitCalls++
			if c.CommitStatus != 0 {
				w.WriteHeader(c.CommitStatus)
				_, _ = w.Write([]byte("fixture-private-response"))
				return
			}
			if !strings.HasSuffix(q.URL.Path, "/3329023300bb33fd3fa6430e2dafbb158deb0508") {
				t.Error("commit lookup substituted source head or branch")
			}
			githubJSON(t, w, map[string]any{"sha": "3329023300bb33fd3fa6430e2dafbb158deb0508", "commit": map[string]any{"author": map[string]any{"date": r.Since.Format(time.RFC3339)}}, "parents": []any{map[string]any{"sha": strings.Repeat("d", 40)}}, "stats": map[string]any{"additions": 2, "deletions": 1}, "files": []any{map[string]any{"filename": "a.go", "additions": 2, "deletions": 1}}})
		default:
			t.Error("fixture received unexpected acquisition endpoint")
			w.WriteHeader(http.StatusNotFound)
		}
	})
}

func TestGitHubAcquireEvidence(t *testing.T) {
	c := &githubAcquireFixture{}
	p := githubAcquireProvider(t, c)
	var artifacts []acquiredArtifact
	var gaps []acquisitionGap
	sink := githubGapSink(&gaps)
	sink.Artifact = func(a acquiredArtifact) error { artifacts = append(artifacts, a); return nil }
	_, err := p.acquireRun(context.Background(), githubSourceRun(), sink)
	if err != nil || len(artifacts) != 1 {
		t.Fatal("actual parsed artifact was not synchronously emitted")
	}
	a := artifacts[0]
	o := a.Observation
	digest := sha256.Sum256(c.Raw)
	if !bytes.Equal(a.Raw, c.Raw) || o.ContentSHA256 != hex.EncodeToString(digest[:]) || o.SourceHeadSHA != strings.Repeat("a", 40) || o.TestedSHA == nil || *o.TestedSHA != "3329023300bb33fd3fa6430e2dafbb158deb0508" || o.JobID == nil || *o.JobID != "201" || o.Attempt == nil || *o.Attempt != 1 || o.RunAttempt == nil || *o.RunAttempt != 2 || !o.ObservedAt.Equal(githubRequest().Since.Add(2*time.Second)) || o.EnvironmentID != nil || o.RawBlobPath != "" || len(o.Entries) != 1 || len(o.Entries[0].Cases) != 1 {
		t.Fatal("source, tested tree, real attempt, time or raw-byte evidence was fabricated or lost")
	}
	v := o.Entries[0].Cases[0]
	if v.Case.Status != "fail" || v.Case.Message != "actual assertion\nraw diagnostic" || v.Identity.ID != "py:tests.test_value:test_value[param]" || v.Identity.RunnerID != "" {
		t.Fatal("raw failure or report-linked identity was changed")
	}
}

func TestGitHubAcquireUnknownProducer(t *testing.T) {
	for _, tc := range []struct {
		name string
		c    githubAcquireFixture
	}{{"absent", githubAcquireFixture{NoProducer: true}}, {"ambiguous", githubAcquireFixture{Ambiguous: true}}, {"incomplete_job", githubAcquireFixture{JobStatus: "queued"}}} {
		t.Run(tc.name, func(t *testing.T) {
			c := tc.c
			p := githubAcquireProvider(t, &c)
			var got []acquiredArtifact
			var gaps []acquisitionGap
			sink := githubGapSink(&gaps)
			sink.Artifact = func(a acquiredArtifact) error { got = append(got, a); return nil }
			outcome, err := p.acquireRun(context.Background(), githubSourceRun(), sink)
			if err != nil || len(got) != 1 || got[0].Observation.JobID != nil || got[0].Observation.Attempt != nil || got[0].Observation.TestedSHA != nil || got[0].Observation.ExclusionReason == "" || (tc.name == "incomplete_job" && outcome.State == "complete") || (tc.name == "absent" && outcome.State != "complete") {
				t.Fatal("unknown producer or current attempt manufactured factual provenance")
			}
		})
	}
}

func TestGitHubAcquireReportLink(t *testing.T) {
	for _, tc := range []struct {
		name           string
		c              githubAcquireFixture
		wantID, reason string
	}{
		{"full_path", githubAcquireFixture{ReportName: "other/junit.xml"}, "", "language_unavailable"},
		{"operator_fallback", githubAcquireFixture{ReportName: "other/junit.xml", Language: "python"}, "py:tests.test_value:test_value[param]", ""},
		{"language_conflict", githubAcquireFixture{Language: "go"}, "", "language_conflict"},
		{"tested_module", githubAcquireFixture{Language: "go", GoReport: true}, "go:example.org/project/pkg:TestValue", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := tc.c
			p := githubAcquireProvider(t, &c)
			var got []acquiredArtifact
			var gaps []acquisitionGap
			sink := githubGapSink(&gaps)
			sink.Artifact = func(a acquiredArtifact) error { got = append(got, a); return nil }
			_, err := p.acquireRun(context.Background(), githubSourceRun(), sink)
			if err != nil || len(got) != 1 || got[0].Observation.Entries[0].Cases[0].Identity.ID != tc.wantID || got[0].Observation.Entries[0].Cases[0].IdentityError != tc.reason || (tc.name == "tested_module" && c.ModuleCalls != 1) || (tc.name == "language_conflict" && c.ModuleCalls != 0) {
				t.Fatal("exact report/module linkage or explicit language evidence was replaced by a guess")
			}
		})
	}
}

func TestGitHubAcquireRawGaps(t *testing.T) {
	for _, tc := range []struct {
		name string
		c    githubAcquireFixture
		raw  bool
	}{
		{"source_time", githubAcquireFixture{MissingTime: true}, true},
		{"empty_source_time", githubAcquireFixture{MissingTime: true, Raw: []byte{}}, true},
		{"parse", githubAcquireFixture{Raw: []byte("PKbroken")}, true},
		{"digest", githubAcquireFixture{WrongDigest: true}, true},
		{"size", githubAcquireFixture{WrongSize: true}, true},
		{"run", githubAcquireFixture{WrongRun: true}, false},
		{"expired", githubAcquireFixture{Expired: true}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := tc.c
			p := githubAcquireProvider(t, &c)
			var got []acquiredArtifact
			var gaps []acquisitionGap
			sink := githubGapSink(&gaps)
			sink.Artifact = func(a acquiredArtifact) error { got = append(got, a); return nil }
			outcome, err := p.acquireRun(context.Background(), githubSourceRun(), sink)
			if err != nil || len(got) != 0 || len(gaps) == 0 || outcome.State == "complete" {
				t.Fatal("missing source facts or failed artifact became synthetic success")
			}
			var raw []byte
			for _, g := range gaps {
				if g.Raw != nil {
					raw = g.Raw
				}
			}
			if tc.raw && (raw == nil || !bytes.Equal(raw, c.Raw)) {
				t.Fatal("successfully acquired bytes were lost on a gap")
			}
			if !tc.raw && c.ZipCalls != 0 {
				t.Fatal("expired or foreign artifact bytes were requested")
			}
		})
	}
}

func TestGitHubArtifactCallbackStops(t *testing.T) {
	c := &githubAcquireFixture{TwoArtifacts: true}
	p := githubAcquireProvider(t, c)
	sentinel := errors.New("fixture writer failure")
	var gaps []acquisitionGap
	calls := 0
	sink := githubGapSink(&gaps)
	sink.Artifact = func(acquiredArtifact) error { calls++; return sentinel }
	outcome, err := p.acquireRun(context.Background(), githubSourceRun(), sink)
	if !errors.Is(err, sentinel) || calls != 1 || c.ZipCalls != 1 || outcome.State == "complete" {
		t.Fatal("artifact callback failure was swallowed or another report was processed")
	}
}

func TestGitHubAcquireCancellation(t *testing.T) {
	c := &githubAcquireFixture{}
	p := githubAcquireProvider(t, c)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var gaps []acquisitionGap
	sink := githubGapSink(&gaps)
	sink.Artifact = func(acquiredArtifact) error { cancel(); return nil }
	outcome, err := p.acquireRun(ctx, githubSourceRun(), sink)
	if !errors.Is(err, context.Canceled) || c.ZipCalls != 1 || outcome.State == "complete" {
		t.Fatal("cancellation continued acquisition or claimed complete")
	}
}

func TestGitHubAcquireLogBudget(t *testing.T) {
	c := &githubAcquireFixture{LogBytes: 16 << 20, Jobs: 5}
	p := githubAcquireProvider(t, c)
	var gaps []acquisitionGap
	sink := githubGapSink(&gaps)
	run := githubSourceRun()
	attempt := 1
	run.RunAttempt = &attempt
	outcome, err := p.acquireRun(context.Background(), run, sink)
	if err != nil || c.LogCalls != 4 || outcome.State != "capped" {
		t.Fatal("run log byte budget was exceeded or reported complete")
	}
}

func TestGitHubProducerConflictHandoff(t *testing.T) {
	c := &githubAcquireFixture{Ambiguous: true}
	p := githubAcquireProvider(t, c)
	var order []string
	sink := acquisitionSink{Gap: func(g acquisitionGap) error {
		if g.Reason == "acquisition_conflict" {
			order = append(order, "gap")
			if g.Detail != "multiple_verified_producers" || g.RunID != "17" || g.UnitKey != "artifact:301" || g.ArtifactID == nil || *g.ArtifactID != "301" || g.JobID != nil || g.Attempt != nil || g.Raw != nil || g.SourceTime == nil || !g.SourceTime.Equal(githubRequest().Since.Add(2*time.Second)) {
				t.Error("producer conflict gap fabricated scope or discarded actual source facts")
			}
		}
		return nil
	}, Artifact: func(a acquiredArtifact) error {
		order = append(order, "artifact")
		o := a.Observation
		if !bytes.Equal(a.Raw, c.Raw) || o.ExclusionReason != "ambiguous_producer" || o.JobID != nil || o.Attempt != nil || o.TestedSHA != nil || len(o.Entries) != 1 || o.Entries[0].Cases[0].Case.Status != "fail" || o.Entries[0].Cases[0].InfraReason != "" {
			t.Error("ambiguous producer altered raw cases or selected a conflicting execution")
		}
		return nil
	}}
	outcome, err := p.acquireRun(context.Background(), githubSourceRun(), sink)
	if err != nil || outcome.State != "incomplete" || strings.Join(order, ",") != "gap,artifact" {
		t.Fatal("explicit producer conflict was not handed off before raw evidence")
	}
}

func TestGitHubProducerConflictCallbackStops(t *testing.T) {
	c := &githubAcquireFixture{Ambiguous: true}
	p := githubAcquireProvider(t, c)
	sentinel := errors.New("fixture conflict writer failure")
	gaps, artifacts := 0, 0
	sink := acquisitionSink{Gap: func(g acquisitionGap) error {
		if g.Reason == "acquisition_conflict" {
			gaps++
			return sentinel
		}
		return nil
	}, Artifact: func(acquiredArtifact) error { artifacts++; return nil }}
	outcome, err := p.acquireRun(context.Background(), githubSourceRun(), sink)
	if !errors.Is(err, sentinel) || gaps != 1 || artifacts != 0 || outcome.State == "complete" {
		t.Fatal("producer-conflict writer failure did not stop before Artifact")
	}
}

func TestGitHubProducerContextDedup(t *testing.T) {
	c := &githubAcquireFixture{DuplicateJobs: true}
	p := githubAcquireProvider(t, c)
	var got []acquiredArtifact
	var gaps []acquisitionGap
	sink := githubGapSink(&gaps)
	sink.Artifact = func(a acquiredArtifact) error { got = append(got, a); return nil }
	_, err := p.acquireRun(context.Background(), githubSourceRun(), sink)
	if err != nil || len(got) != 1 || got[0].Observation.JobID == nil || *got[0].Observation.JobID != "201" || c.LogCalls != 2 {
		t.Fatal("identical job contexts duplicated verified producers")
	}
	for _, g := range gaps {
		if g.Reason == "acquisition_conflict" {
			t.Fatal("identical verified contexts manufactured producer conflict")
		}
	}
}

func TestGitHubProducerDistinctAttempt(t *testing.T) {
	c := &githubAcquireFixture{Ambiguous: true, ReusedJobID: true}
	p := githubAcquireProvider(t, c)
	var got []acquiredArtifact
	var gaps []acquisitionGap
	sink := githubGapSink(&gaps)
	sink.Artifact = func(a acquiredArtifact) error { got = append(got, a); return nil }
	outcome, err := p.acquireRun(context.Background(), githubSourceRun(), sink)
	conflicts := 0
	for _, g := range gaps {
		if g.Reason == "acquisition_conflict" {
			conflicts++
		}
	}
	if err != nil || len(got) != 1 || conflicts != 1 || got[0].Observation.Attempt != nil || got[0].Observation.JobID != nil || outcome.State == "complete" {
		t.Fatal("materially different verified attempt contexts selected the first execution")
	}
}

func githubCommitJSON(sha string, files []any) map[string]any {
	return map[string]any{"sha": sha, "commit": map[string]any{"author": map[string]any{"date": githubRequest().Since.Format(time.RFC3339)}}, "parents": []any{map[string]any{"sha": strings.Repeat("d", 40)}, map[string]any{"sha": strings.Repeat("e", 40)}}, "stats": map[string]any{"additions": 2, "deletions": 1}, "files": files}
}

func TestGitHubCommitMetadata(t *testing.T) {
	sha := strings.Repeat("c", 40)
	calls := 0
	p := githubFixture(t, githubRequest(), func(w http.ResponseWriter, q *http.Request) {
		calls++
		if q.URL.Path != "/repos/owner/repo/commits/"+sha || q.URL.Query().Get("per_page") != "100" {
			t.Error("commit lookup substituted a revision or page size")
		}
		page := q.URL.Query().Get("page")
		if page == "1" {
			githubNext(w, q, 2)
			githubJSON(t, w, githubCommitJSON(sha, []any{map[string]any{"filename": "a.go"}}))
		} else {
			githubJSON(t, w, githubCommitJSON(sha, []any{map[string]any{"filename": "b.go"}}))
		}
	})
	m, err := p.commitMetadata(context.Background(), sha)
	if err != nil || calls != 2 || m == nil || m.AuthoredAt == nil || !m.AuthoredAt.Equal(githubRequest().Since) || len(m.ParentSHAs) != 2 || m.ParentSHAs[1] != strings.Repeat("e", 40) || m.FilesChanged == nil || *m.FilesChanged != 2 || m.LinesAdded == nil || *m.LinesAdded != 2 || m.LinesRemoved == nil || *m.LinesRemoved != 1 || m.Kinds != nil {
		t.Fatal("exact commit metadata was lost, fabricated or repeatedly summed")
	}
}

func TestGitHubCommitIncomplete(t *testing.T) {
	for _, mode := range []string{"authored", "parents", "stats", "duplicate", "missing_files", "sha", "partial", "filter", "loop", "missing_fields", "root", "missing_then_known", "oversized_files", "removed"} {
		t.Run(mode, func(t *testing.T) {
			sha := strings.Repeat("c", 40)
			calls := 0
			p := githubFixture(t, githubRequest(), func(w http.ResponseWriter, q *http.Request) {
				calls++
				page := q.URL.Query().Get("page")
				if mode == "partial" && page == "2" {
					w.WriteHeader(http.StatusNotFound)
					return
				}
				v := githubCommitJSON(sha, []any{map[string]any{"filename": "file" + page}})
				if mode == "missing_then_known" && page == "1" {
					delete(v, "commit")
				}
				if mode == "missing_fields" {
					delete(v, "commit")
					delete(v, "parents")
					delete(v, "stats")
				}
				if mode == "root" {
					v["parents"] = []any{}
				}
				if mode == "missing_files" {
					delete(v, "files")
				}
				if mode == "oversized_files" {
					rows := []any{}
					for i := 0; i < 101; i++ {
						rows = append(rows, map[string]any{"filename": strconv.Itoa(i)})
					}
					v["files"] = rows
				}
				if mode == "sha" {
					v["sha"] = strings.Repeat("a", 40)
				}
				if page == "1" && mode != "missing_fields" && mode != "root" && mode != "missing_files" && mode != "sha" && mode != "oversized_files" {
					githubNext(w, q, 2)
				}
				if page == "2" {
					switch mode {
					case "authored":
						v["commit"] = map[string]any{"author": map[string]any{"date": githubRequest().Until.Format(time.RFC3339)}}
					case "parents":
						v["parents"] = []any{map[string]any{"sha": strings.Repeat("f", 40)}}
					case "stats":
						v["stats"] = map[string]any{"additions": 7, "deletions": 1}
					case "removed":
						v["stats"] = map[string]any{"additions": 2, "deletions": 7}
					case "duplicate":
						v["files"] = []any{map[string]any{"filename": "file1"}}
					case "filter":
						w.Header().Set("Link", "<"+q.URL.String()+"&ref=other>; rel=\"next\"")
					case "loop":
						githubNext(w, q, 1)
					}
				}
				githubJSON(t, w, v)
			})
			m, err := p.commitMetadata(context.Background(), sha)
			if mode == "root" {
				if err != nil || m == nil || m.ParentSHAs == nil || len(m.ParentSHAs) != 0 || m.FilesChanged == nil || *m.FilesChanged != 1 {
					t.Fatal("known root metadata became unknown")
				}
				return
			}
			if err == nil || m == nil || (mode != "missing_fields" && m.FilesChanged != nil) || (mode == "missing_fields" && (m.FilesChanged == nil || *m.FilesChanged != 1)) {
				t.Fatal("incomplete commit walk fabricated or discarded an independent file count")
			}
			if mode == "sha" {
				if m.AuthoredAt != nil || m.LinesAdded != nil || m.ParentSHAs != nil {
					t.Fatal("wrong revision supplied commit facts")
				}
				return
			}
			if mode == "authored" || mode == "missing_fields" || mode == "missing_then_known" {
				if m.AuthoredAt != nil {
					t.Fatal("conflicting or missing authored date remained known")
				}
			} else if m.AuthoredAt == nil {
				t.Fatal("independent authored date was discarded")
			}
			if mode == "parents" || mode == "missing_fields" {
				if m.ParentSHAs != nil {
					t.Fatal("conflicting or missing parents remained known")
				}
			} else if len(m.ParentSHAs) != 2 {
				t.Fatal("independent parents were discarded")
			}
			if mode == "stats" || mode == "missing_fields" {
				if m.LinesAdded != nil {
					t.Fatal("conflicting or missing additions remained known")
				}
			} else if m.LinesAdded == nil || *m.LinesAdded != 2 {
				t.Fatal("independent additions were discarded")
			}
			if mode == "loop" && calls != 2 {
				t.Fatal("repeated commit page performed another acquisition")
			}
			if mode == "removed" {
				if m.LinesRemoved != nil {
					t.Fatal("conflicting deletions remained known")
				}
			} else if mode != "missing_fields" && (m.LinesRemoved == nil || *m.LinesRemoved != 1) {
				t.Fatal("independent deletions were discarded")
			}
		})
	}
}

func TestGitHubCommitCaps(t *testing.T) {
	for _, mode := range []string{"files", "pages"} {
		t.Run(mode, func(t *testing.T) {
			sha := strings.Repeat("c", 40)
			calls := 0
			p := githubFixture(t, githubRequest(), func(w http.ResponseWriter, q *http.Request) {
				calls++
				files := []any{map[string]any{"filename": "file" + strconv.Itoa(calls)}}
				if mode == "files" {
					files = nil
					for i := 0; i < 100; i++ {
						files = append(files, map[string]any{"filename": strconv.Itoa(calls*100 + i)})
					}
				}
				if (mode == "pages" && calls < 31) || calls < 30 {
					githubNext(w, q, calls+1)
				}
				githubJSON(t, w, githubCommitJSON(sha, files))
			})
			m, err := p.commitMetadata(context.Background(), sha)
			if err == nil || calls != 30 || m == nil || m.FilesChanged != nil || m.LinesAdded == nil || *m.LinesAdded != 2 {
				t.Fatal("commit ceiling claimed exact completeness or exceeded finite work")
			}
		})
	}
}

func TestGitHubCommitFailureHandoff(t *testing.T) {
	for _, status := range []int{http.StatusNotFound, http.StatusUnauthorized, http.StatusTooManyRequests} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			c := &githubAcquireFixture{CommitStatus: status}
			p := githubAcquireProvider(t, c)
			var gaps []acquisitionGap
			var got []acquiredArtifact
			sink := githubGapSink(&gaps)
			sink.Artifact = func(a acquiredArtifact) error { got = append(got, a); return nil }
			outcome, err := p.acquireRun(context.Background(), githubSourceRun(), sink)
			if c.CommitCalls == 0 || len(got) != 1 || got[0].Observation.TestedSHA == nil || got[0].Observation.Entries[0].Cases[0].Identity.ID == "" || outcome.State == "complete" {
				t.Fatal("missing commit metadata discarded factual evidence or claimed completeness")
			}
			if status == http.StatusNotFound {
				if err != nil {
					t.Fatal("individual unavailable metadata stopped independent acquisition")
				}
			} else if !githubFatal(err) {
				t.Fatal("authentication or rate-limit error did not stop acquisition")
			}
			if err != nil && strings.Contains(err.Error(), "fixture-private-response") {
				t.Fatal("HTTP response body leaked into provider error")
			}
			for _, g := range gaps {
				if g.Raw != nil || strings.Contains(g.Detail, "fixture-private-response") {
					t.Fatal("HTTP response body entered evidence gap")
				}
			}
		})
	}
}

func TestGitHubMetadataCache(t *testing.T) {
	for _, kind := range []string{"commit", "module", "logs"} {
		t.Run(kind, func(t *testing.T) {
			c := &githubAcquireFixture{GoReport: true, Language: "go", TwoArtifacts: true, SecondProducer: true}
			p := githubAcquireProvider(t, c)
			var got []acquiredArtifact
			var gaps []acquisitionGap
			sink := githubGapSink(&gaps)
			sink.Artifact = func(a acquiredArtifact) error { got = append(got, a); return nil }
			outcome, err := p.acquireRun(context.Background(), githubSourceRun(), sink)
			if err != nil || outcome.State != "complete" || len(got) != 2 || got[1].Observation.TestedSHA == nil || got[1].Observation.Commit == nil || got[1].Observation.Entries[0].Cases[0].Identity.ID != "go:example.org/project/pkg:TestValue" {
				t.Fatal("cached acquisition changed actual report facts")
			}
			switch kind {
			case "commit":
				if c.CommitCalls != 1 {
					t.Fatal("tested commit was refetched within one run")
				}
			case "module":
				if c.ModuleCalls != 1 {
					t.Fatal("tested module was refetched within one run")
				}
			case "logs":
				if c.LogCalls != 2 {
					t.Fatal("producer log contexts were refetched within one run")
				}
			}
		})
	}
}

func TestGitHubModuleEvidence(t *testing.T) {
	for _, mode := range []string{"valid", "type", "path", "encoding", "base64", "size"} {
		t.Run(mode, func(t *testing.T) {
			sha := strings.Repeat("c", 40)
			p := githubFixture(t, githubRequest(), func(w http.ResponseWriter, q *http.Request) {
				if q.URL.Path != "/repos/owner/repo/contents/go.mod" || q.URL.Query().Get("ref") != sha {
					t.Error("tested root module lookup changed exact reference")
				}
				v := map[string]any{"type": "file", "path": "go.mod", "encoding": "base64", "content": base64.StdEncoding.EncodeToString([]byte("module example.org/project\n"))}
				switch mode {
				case "type":
					v["type"] = "dir"
				case "path":
					v["path"] = "nested/go.mod"
				case "encoding":
					v["encoding"] = "none"
				case "base64":
					v["content"] = "!not-base64!"
				case "size":
					v["content"] = base64.StdEncoding.EncodeToString(append([]byte("module example.org/project\n//"), bytes.Repeat([]byte("x"), 1<<20)...))
				}
				githubJSON(t, w, v)
			})
			module, err := p.readTestedModule(context.Background(), sha)
			if mode == "valid" {
				if err != nil || module != "example.org/project" {
					t.Fatal("valid tested module was lost")
				}
			} else if err == nil || module != "" {
				t.Fatal("unsupported or oversized module evidence supplied identity")
			}
		})
	}
}

func TestGitHubAcquisitionRequirements(t *testing.T) {
	for _, mode := range []string{"artifact_callback", "gap_callback", "cancelled"} {
		t.Run(mode, func(t *testing.T) {
			calls := 0
			p := githubFixture(t, githubRequest(), func(w http.ResponseWriter, q *http.Request) {
				calls++
				if strings.HasSuffix(q.URL.Path, "/jobs") {
					githubJSON(t, w, map[string]any{"total_count": 0, "jobs": []any{}})
				} else {
					githubJSON(t, w, map[string]any{"total_count": 0, "artifacts": []any{}})
				}
			})
			var gaps []acquisitionGap
			sink := githubGapSink(&gaps)
			if mode == "artifact_callback" {
				sink.Artifact = nil
			}
			if mode == "gap_callback" {
				sink.Gap = nil
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if mode == "cancelled" {
				cancel()
			}
			outcome, err := p.acquireRun(ctx, githubSourceRun(), sink)
			if mode == "cancelled" && len(gaps) != 0 {
				t.Fatal("cancellation before acquisition invented a source gap")
			}
			if err == nil || calls != 0 || outcome.State == "complete" {
				t.Fatal("invalid callbacks or cancellation performed acquisition or claimed complete")
			}
		})
	}
}

func TestGitHubReportCompleteness(t *testing.T) {
	for _, mode := range []string{"no_match", "incomplete", "truncated", "run_incomplete", "size"} {
		t.Run(mode, func(t *testing.T) {
			c := &githubAcquireFixture{NoProducer: true}
			run := githubSourceRun()
			switch mode {
			case "no_match":
				c.ReportName = "reports/output.xml"
			case "incomplete":
				c.Raw = githubReportZIP(t, "reports/junit.xml", `<testsuite tests="2"><testcase classname="tests.value" name="test_value"/></testsuite>`)
			case "truncated":
				c.Raw = githubReportZIP(t, "reports/junit.xml", `<testsuite><testcase classname="tests.value" name="test_value"><failure>`+strings.Repeat("x", (64<<10)+1)+`</failure></testcase></testsuite>`)
			case "run_incomplete":
				run.Status = "in_progress"
			case "size":
				c.Oversized = true
			}
			p := githubAcquireProvider(t, c)
			var gaps []acquisitionGap
			var got []acquiredArtifact
			sink := githubGapSink(&gaps)
			sink.Artifact = func(a acquiredArtifact) error { got = append(got, a); return nil }
			outcome, err := p.acquireRun(context.Background(), run, sink)
			if err != nil || outcome.State == "complete" || len(gaps) == 0 {
				t.Fatal("incomplete or bounded-out evidence claimed complete acquisition")
			}
			if mode == "size" {
				if c.ZipCalls != 0 || len(got) != 0 || outcome.State != "capped" {
					t.Fatal("oversized artifact bytes were acquired")
				}
				return
			}
			if len(got) != 1 || !bytes.Equal(got[0].Raw, c.Raw) {
				t.Fatal("actual report bytes were discarded")
			}
			o := got[0].Observation
			if mode == "no_match" && (len(o.Entries) != 0 || o.ExclusionReason != "no_matching_report") {
				t.Fatal("nonmatching archive fabricated cases")
			}
			if mode == "incomplete" && (!o.SuiteIncomplete || !o.Entries[0].Cases[0].Case.SuiteIncomplete) {
				t.Fatal("incomplete suite flag was lost")
			}
			if mode == "truncated" && (!o.DiagnosticsTruncated || !o.Entries[0].Cases[0].Case.DiagnosticsTruncated || o.Entries[0].Cases[0].Case.Status != "fail") {
				t.Fatal("diagnostic truncation changed factual failure")
			}
		})
	}
}

func TestGitHubListingSafety(t *testing.T) {
	for _, kind := range []string{"runs", "jobs", "artifacts"} {
		for _, mode := range []string{"missing_total", "negative_total", "oversized", "changed_total", "missing_link", "loop", "pages", "filter", "invalid_id", "wrong_job_run", "unknown_attempt"} {
			if (mode == "wrong_job_run" || mode == "unknown_attempt") && kind != "jobs" {
				continue
			}
			t.Run(kind+"/"+mode, func(t *testing.T) {
				calls := 0
				p := githubFixture(t, githubRequest(), func(w http.ResponseWriter, q *http.Request) {
					calls++
					page, _ := strconv.Atoi(q.URL.Query().Get("page"))
					total := 11
					data := []any{}
					makeRow := func(id int) any {
						if kind == "runs" {
							return githubRunJSON(int64(id), 1, "main", "owner/repo")
						}
						if kind == "jobs" {
							runID := 17
							if mode == "wrong_job_run" {
								runID = 18
							}
							return map[string]any{"id": id, "run_id": runID, "run_attempt": 1, "status": "completed"}
						}
						return githubArtifactJSON(int64(id))
					}
					id := 17 + page
					if mode == "invalid_id" {
						id = 0
					}
					data = append(data, makeRow(id))
					if mode == "oversized" {
						total = 101
						data = nil
						for i := 0; i < 101; i++ {
							data = append(data, makeRow(100+i))
						}
					}
					if mode == "changed_total" && page == 2 {
						total = 12
					}
					if mode == "negative_total" {
						total = -1
					}
					switch mode {
					case "loop":
						next := 1
						if kind != "runs" && page == 1 {
							next = 2
						}
						githubNext(w, q, next)
					case "pages":
						if page < 11 {
							githubNext(w, q, page+1)
						}
					case "changed_total":
						if page < 3 {
							githubNext(w, q, page+1)
						}
					case "filter":
						w.Header().Set("Link", "<"+q.URL.String()+"&per_page=99>; rel=\"next\"")
					}
					key := kind
					if kind == "runs" {
						key = "workflow_runs"
					}
					v := map[string]any{"total_count": total, key: data}
					if mode == "missing_total" {
						delete(v, "total_count")
					}
					githubJSON(t, w, v)
				})
				var gaps []acquisitionGap
				outcome := runOutcome{State: "complete"}
				run := githubSourceRun()
				attempt := 1
				run.RunAttempt = &attempt
				if mode == "unknown_attempt" {
					run.RunAttempt = nil
				}
				acquired := 0
				switch kind {
				case "runs":
					rows, discovery, err := p.listRuns(context.Background())
					acquired = len(rows)
					gaps = discovery
					if err == nil && len(gaps) == 0 {
						t.Fatal("partial or malformed discovery claimed completeness")
					}
				case "jobs":
					rows, err := p.listJobs(context.Background(), run, githubGapSink(&gaps), &outcome)
					acquired = len(rows)
					if err != nil {
						t.Fatal(err)
					}
				default:
					rows, err := p.listArtifacts(context.Background(), run, githubGapSink(&gaps), &outcome)
					acquired = len(rows)
					if err != nil {
						t.Fatal(err)
					}
				}
				if len(gaps) == 0 || (kind != "runs" && outcome.State == "complete") || calls > 10 {
					t.Fatal("unsafe listing established complete acquisition or exceeded page bound")
				}
				if (mode == "invalid_id" || mode == "wrong_job_run") && acquired != 0 {
					t.Fatal("invalid execution or artifact identity became source evidence")
				}
				loopLimit := 1
				if kind != "runs" {
					loopLimit = 2
				}
				if mode == "changed_total" && calls > 2 || mode == "loop" && calls != loopLimit {
					t.Fatal("inconsistent or repeated pagination performed another acquisition")
				}
				if mode == "unknown_attempt" && calls != 0 {
					t.Fatal("unknown attempt fabricated job lookup")
				}
			})
		}
	}
}

type githubCountedBody struct {
	io.ReadCloser
	total, maximum *int64
	consumed       int64
}

func (b *githubCountedBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	*b.total += int64(n)
	b.consumed += int64(n)
	*b.maximum = max(*b.maximum, b.consumed)
	return n, err
}

func TestGitHubLogDownloadedBudget(t *testing.T) {
	c := &githubAcquireFixture{LogBytes: (17 << 20) + 1, Jobs: 5}
	p := githubAcquireProvider(t, c)
	var downloaded, maximum int64
	transport := p.client.api.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}
	p.client.api.Transport = httpRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		response, err := transport.RoundTrip(r)
		if err == nil && strings.HasSuffix(r.URL.Path, "/logs") {
			response.Body = &githubCountedBody{ReadCloser: response.Body, total: &downloaded, maximum: &maximum}
		}
		return response, err
	})
	var gaps []acquisitionGap
	run := githubSourceRun()
	attempt := 1
	run.RunAttempt = &attempt
	outcome, err := p.acquireRun(context.Background(), run, githubGapSink(&gaps))
	if err != nil || downloaded > 64<<20 || maximum > (16<<20)+1 || c.LogCalls != 4 || outcome.State != "capped" {
		t.Fatal("log overflow probes exceeded the actual run download budget")
	}
}

func TestGitHubFatalOperations(t *testing.T) {
	for _, endpoint := range []string{"/jobs", "/artifacts", "/zip", "/logs", "/contents/go.mod"} {
		t.Run(strings.TrimPrefix(endpoint, "/"), func(t *testing.T) {
			c := &githubAcquireFixture{FailEndpoint: endpoint, FailStatus: http.StatusUnauthorized}
			if endpoint == "/contents/go.mod" {
				c.GoReport = true
				c.Language = "go"
			}
			p := githubAcquireProvider(t, c)
			var got []acquiredArtifact
			var gaps []acquisitionGap
			sink := githubGapSink(&gaps)
			sink.Artifact = func(a acquiredArtifact) error { got = append(got, a); return nil }
			outcome, err := p.acquireRun(context.Background(), githubSourceRun(), sink)
			if !githubFatal(err) || c.FailedCalls != 1 || outcome.State == "complete" || len(gaps) == 0 {
				t.Fatal("authentication failure continued source acquisition or claimed complete")
			}
			want := 0
			if endpoint == "/logs" || endpoint == "/contents/go.mod" {
				want = 1
			}
			if len(got) != want {
				t.Fatal("actual acquired report evidence was lost or fabricated after authentication failure")
			}
			if strings.Contains(err.Error(), "fixture-private-response") {
				t.Fatal("service error body leaked")
			}
			for _, g := range gaps {
				if g.Raw != nil || strings.Contains(g.Detail, "fixture-private-response") {
					t.Fatal("service error body entered evidence")
				}
			}
		})
	}
}

func TestGitHubOperationGapCallbackStops(t *testing.T) {
	for _, endpoint := range []string{"/jobs", "/artifacts", "/zip", "/logs", "/contents/go.mod", "/commits/3329023300bb33fd3fa6430e2dafbb158deb0508"} {
		t.Run(strings.TrimPrefix(endpoint, "/"), func(t *testing.T) {
			c := &githubAcquireFixture{FailEndpoint: endpoint, FailStatus: http.StatusNotFound}
			if endpoint == "/contents/go.mod" {
				c.GoReport = true
				c.Language = "go"
			}
			p := githubAcquireProvider(t, c)
			sentinel := errors.New("fixture gap writer failure")
			artifacts := 0
			sink := acquisitionSink{Gap: func(acquisitionGap) error { return sentinel }, Artifact: func(acquiredArtifact) error { artifacts++; return nil }}
			outcome, err := p.acquireRun(context.Background(), githubSourceRun(), sink)
			if !errors.Is(err, sentinel) || artifacts != 0 || c.FailedCalls != 1 || outcome.State == "complete" {
				t.Fatal("failed synchronous gap callback allowed evidence acquisition to continue")
			}
		})
	}
}

func TestGitHubDuplicateRunConflict(t *testing.T) {
	p := githubFixture(t, githubRequest(), func(w http.ResponseWriter, q *http.Request) {
		v := githubRunJSON(17, 1, "main", "owner/repo")
		if q.URL.Query().Get("page") == "1" {
			githubNext(w, q, 2)
		} else {
			v["head_sha"] = strings.Repeat("f", 40)
		}
		githubJSON(t, w, map[string]any{"total_count": 2, "workflow_runs": []any{v}})
	})
	_, gaps, err := p.listRuns(context.Background())
	if err == nil || len(gaps) == 0 {
		t.Fatal("conflicting duplicate run selected arbitrary source facts")
	}
}

func TestGitHubArtifactDedup(t *testing.T) {
	p := githubFixture(t, githubRequest(), func(w http.ResponseWriter, q *http.Request) {
		githubJSON(t, w, map[string]any{"total_count": 2, "artifacts": []any{githubArtifactJSON(301), githubArtifactJSON(301)}})
	})
	var gaps []acquisitionGap
	outcome := runOutcome{State: "complete"}
	rows, err := p.listArtifacts(context.Background(), githubSourceRun(), githubGapSink(&gaps), &outcome)
	if err != nil || len(rows) != 1 || len(gaps) != 0 || outcome.State != "complete" {
		t.Fatal("identical artifact listing duplicated evidence units")
	}
}

func TestGitHubJobsAcrossAttempts(t *testing.T) {
	calls := 0
	p := githubFixture(t, githubRequest(), func(w http.ResponseWriter, q *http.Request) {
		calls++
		attempt := 1
		if strings.Contains(q.URL.Path, "/attempts/2/") {
			attempt = 2
		}
		page, _ := strconv.Atoi(q.URL.Query().Get("page"))
		rows := make([]any, 100)
		for i := range rows {
			rows[i] = map[string]any{"id": attempt*10000 + page*100 + i, "run_id": 17, "run_attempt": attempt, "status": "completed"}
		}
		if page < 6 {
			githubNext(w, q, page+1)
		}
		githubJSON(t, w, map[string]any{"total_count": 600, "jobs": rows})
	})
	var gaps []acquisitionGap
	outcome := runOutcome{State: "complete"}
	jobs, err := p.listJobs(context.Background(), githubSourceRun(), githubGapSink(&gaps), &outcome)
	if err != nil || calls != 10 || len(jobs) != 1000 || outcome.State != "capped" || len(gaps) == 0 {
		t.Fatal("job budget restarted at each attempt or claimed complete")
	}
}

func TestGitHubExactArtifactCeiling(t *testing.T) {
	p := githubFixture(t, githubRequest(), func(w http.ResponseWriter, q *http.Request) {
		page, _ := strconv.Atoi(q.URL.Query().Get("page"))
		rows := make([]any, 100)
		for i := range rows {
			rows[i] = githubArtifactJSON(int64(page*100 + i + 1))
		}
		if page < 10 {
			githubNext(w, q, page+1)
		}
		githubJSON(t, w, map[string]any{"total_count": 1000, "artifacts": rows})
	})
	var gaps []acquisitionGap
	outcome := runOutcome{State: "complete"}
	rows, err := p.listArtifacts(context.Background(), githubSourceRun(), githubGapSink(&gaps), &outcome)
	if err != nil || len(rows) != 1000 || len(gaps) == 0 || outcome.State != "capped" {
		t.Fatal("exact artifact ceiling falsely certified exhaustive acquisition")
	}
}

func TestGitHubCommitFieldGuards(t *testing.T) {
	for _, field := range []string{"authored", "parents", "added", "removed"} {
		t.Run(field, func(t *testing.T) {
			sha := strings.Repeat("c", 40)
			p := githubFixture(t, githubRequest(), func(w http.ResponseWriter, q *http.Request) {
				v := githubCommitJSON(sha, []any{map[string]any{"filename": "a.go"}})
				switch field {
				case "authored":
					v["commit"] = map[string]any{"author": map[string]any{"date": "0001-01-01T00:00:00Z"}}
				case "parents":
					v["parents"] = []any{map[string]any{"sha": "not-a-sha"}}
				case "added":
					v["stats"] = map[string]any{"additions": -1, "deletions": 1}
				case "removed":
					v["stats"] = map[string]any{"additions": 2, "deletions": -1}
				}
				githubJSON(t, w, v)
			})
			m, err := p.commitMetadata(context.Background(), sha)
			if err == nil || m == nil || m.FilesChanged == nil || *m.FilesChanged != 1 {
				t.Fatal("invalid commit field fabricated complete metadata or discarded independent files")
			}
			switch field {
			case "authored":
				if m.AuthoredAt != nil {
					t.Fatal("zero authored timestamp became known")
				}
			case "parents":
				if m.ParentSHAs != nil {
					t.Fatal("malformed parent revision became known")
				}
			case "added":
				if m.LinesAdded != nil {
					t.Fatal("negative addition count became known")
				}
			case "removed":
				if m.LinesRemoved != nil {
					t.Fatal("negative deletion count became known")
				}
			}
		})
	}
}

// This fixture counts successful-status response-body reads, including failed retries.
type githubRetryReadBody struct {
	remaining  int64
	fail       bool
	downloaded *int64
}

func (b *githubRetryReadBody) Read(p []byte) (int, error) {
	if b.remaining == 0 {
		if b.fail {
			return 0, io.ErrUnexpectedEOF
		}
		return 0, io.EOF
	}
	n := min(int64(len(p)), b.remaining)
	for i := range p[:int(n)] {
		p[i] = 'x'
	}
	b.remaining -= n
	*b.downloaded += n
	return int(n), nil
}

func TestGitHubLogRetryReadBudget(t *testing.T) {
	c := &githubAcquireFixture{Jobs: 3}
	p := githubAcquireProvider(t, c)
	transport := p.client.api.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}
	var downloaded int64
	attempts := make(map[string]int)
	p.client.api.Transport = httpRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		response, err := transport.RoundTrip(r)
		if err == nil && response.StatusCode >= 200 && response.StatusCode < 300 && strings.HasSuffix(r.URL.Path, "/logs") {
			_ = response.Body.Close()
			attempts[r.URL.Path]++
			size, fail := int64(1<<20), false
			if attempts[r.URL.Path] <= 2 {
				size, fail = 16<<20, true
			}
			response.Body = io.NopCloser(&githubRetryReadBody{remaining: size, fail: fail, downloaded: &downloaded})
			response.ContentLength = -1
			response.Header.Del("Content-Length")
		}
		return response, err
	})
	run := githubSourceRun()
	attempt := 1
	run.RunAttempt = &attempt
	var got []acquiredArtifact
	var gaps []acquisitionGap
	sink := githubGapSink(&gaps)
	sink.Artifact = func(a acquiredArtifact) error { got = append(got, a); return nil }
	outcome, err := p.acquireRun(context.Background(), run, sink)
	if err != nil || len(got) != 1 {
		t.Fatal("retry-byte fixture did not reach raw artifact handoff")
	}
	t.Logf("successful-status log response-body reads=%d; actual log HTTP calls=%d; known jobs=%d; final state=%s; gaps=%d", downloaded, c.LogCalls, len(attempts), outcome.State, len(gaps))
	if downloaded > 64<<20 {
		t.Errorf("failed retries exceeded the shared run log body-read budget: got=%d limit=%d", downloaded, 64<<20)
	}
	if outcome.State != "capped" || c.LogCalls != 5 || attempts["/repos/owner/repo/actions/jobs/201/logs"] != 3 || attempts["/repos/owner/repo/actions/jobs/211/logs"] != 2 || attempts["/repos/owner/repo/actions/jobs/221/logs"] != 0 {
		t.Error("exhausted retry budget claimed complete or requested a later retry/job")
	}
	capped := 0
	for _, g := range gaps {
		if g.Reason == "acquisition_capped" && g.Detail == "run_log_budget" {
			capped++
			if g.JobID == nil || *g.JobID != "211" || g.Raw != nil {
				t.Fatal("retry exhaustion gap lost actual constrained job or retained trace bytes")
			}
		}
	}
	if capped != 1 || got[0].Observation.Entries[0].Cases[0].Case.Status != "fail" {
		t.Error("retry exhaustion lost its capped gap or actual artifact cases")
	}

}

func TestGitHubLogExhaustionStopsNextJob(t *testing.T) {
	c := &githubAcquireFixture{Jobs: 5}
	p := githubAcquireProvider(t, c)
	transport := p.client.api.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}
	var downloaded int64
	p.client.api.Transport = httpRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		response, err := transport.RoundTrip(r)
		if err == nil && strings.HasSuffix(r.URL.Path, "/logs") {
			_ = response.Body.Close()
			size := int64(16 << 20)
			if strings.Contains(r.URL.Path, "/221/") {
				size--
			} else if strings.Contains(r.URL.Path, "/231/") {
				size += 2
			}
			response.Body = io.NopCloser(&githubRetryReadBody{remaining: size, downloaded: &downloaded})
		}
		return response, err
	})
	run := githubSourceRun()
	attempt := 1
	run.RunAttempt = &attempt
	var gaps []acquisitionGap
	outcome, err := p.acquireRun(context.Background(), run, githubGapSink(&gaps))
	if err != nil || downloaded != 64<<20 || c.LogCalls != 4 || outcome.State != "capped" {
		t.Fatal("a consumed overflow probe allowed the next job HTTP request")
	}
	capped := 0
	for _, g := range gaps {
		if g.Reason == "acquisition_capped" && g.Detail == "run_log_budget" {
			capped++
			if g.JobID == nil || *g.JobID != "241" || g.Raw != nil {
				t.Fatal("next-job exhaustion lost its scope or retained log bytes")
			}
		}
	}
	if capped != 1 {
		t.Fatal("next-job exhaustion did not emit its capped gap")
	}
}
