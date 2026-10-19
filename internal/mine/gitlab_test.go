package mine

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
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

func gitlabRequest() Request {
	return Request{Provider: "gitlab", Project: "group/nested/repo", Branch: "main", Since: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), Until: time.Date(2026, 1, 1, 0, 0, 4, 0, time.UTC), MaxPipelines: 20}
}

func gitlabJSON(t *testing.T, w http.ResponseWriter, value any) {
	t.Helper()
	if err := json.NewEncoder(w).Encode(value); err != nil {
		t.Error(err)
	}
}

func gitlabFixture(t *testing.T, request Request, handler http.HandlerFunc) *gitlabProvider {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	client := mustHTTPClient(t, server.Client(), server.URL+"/api/v4", "gitlab")
	p, err := newGitLabProvider(request, client)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestGitLabConstructor(t *testing.T) {
	client := mustHTTPClient(t, http.DefaultClient, "https://fixture.invalid/api/v4", "gitlab")
	for _, change := range []func(*Request){
		func(r *Request) { r.Provider = "github" },
		func(r *Request) { r.Project = "group/../repo" },
		func(r *Request) { r.Project = "repo" },
		func(r *Request) { r.Project = "0" },
		func(r *Request) { r.Until = r.Since.Add(-time.Second) },
		func(r *Request) { r.Since = time.Time{} },
		func(r *Request) { r.MaxPipelines = 0 },
		func(r *Request) { r.Language = "ruby" },
	} {
		r := gitlabRequest()
		change(&r)
		if _, err := newGitLabProvider(r, client); err == nil {
			t.Error("invalid GitLab request accepted")
		}
	}
	if _, err := newGitLabProvider(gitlabRequest(), nil); err == nil {
		t.Error("nil HTTP source accepted")
	}
	client.provider = "github"
	if _, err := newGitLabProvider(gitlabRequest(), client); err == nil {
		t.Error("wrong HTTP provider accepted")
	}
}

func TestGitLabProjectAndVersion(t *testing.T) {
	for _, project := range []string{"group/nested/repo", "42"} {
		t.Run(project, func(t *testing.T) {
			request := gitlabRequest()
			request.Project = project
			projects, versions, pipelines := 0, 0, 0
			p := gitlabFixture(t, request, func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("X-GitHub-Api-Version") != "" {
					t.Error("GitHub API header escaped provider scope")
				}
				projectPath := "/api/v4/projects/group%2Fnested%2Frepo"
				if project == "42" {
					projectPath = "/api/v4/projects/42"
				}
				switch r.URL.EscapedPath() {
				case projectPath:
					projects++
					gitlabJSON(t, w, map[string]any{"id": 42, "path_with_namespace": "group/nested/repo"})
				case "/api/v4/version":
					versions++
					gitlabJSON(t, w, map[string]any{"version": "19.4.0-ee", "kas": map[string]any{"version": "1.0.0"}})
				case "/api/v4/projects/42/pipelines":
					pipelines++
					if r.URL.Query().Get("ref") != "main" || r.URL.Query().Get("per_page") != "100" || r.URL.Query().Get("scope") != "" || r.URL.Query().Get("created_after") != "2026-01-01T00:00:00Z" || r.URL.Query().Get("created_before") != "2026-01-01T00:00:04Z" {
						t.Error("historical pipeline scope changed")
					}
					gitlabJSON(t, w, []any{})
				default:
					t.Error("unexpected project/version acquisition route")
					w.WriteHeader(404)
				}
			})
			for i := 0; i < 2; i++ {
				runs, gaps, err := p.listRuns(context.Background())
				if err != nil || len(runs) != 0 || len(gaps) != 0 {
					t.Fatal("empty scoped history was not complete")
				}
			}
			if projects != 1 || versions != 1 || pipelines != 2 || p.projectID != 42 || !p.versionKnown || p.major != 19 || p.minor != 4 {
				t.Fatalf("project/version discovery missing or repeated: project=%d version=%d pipelines=%d", projects, versions, pipelines)
			}
		})
	}
}

func TestGitLabProjectBinding(t *testing.T) {
	for _, mode := range []string{"id", "path", "auth"} {
		t.Run(mode, func(t *testing.T) {
			r := gitlabRequest()
			if mode == "id" {
				r.Project = "42"
			}
			calls := 0
			p := gitlabFixture(t, r, func(w http.ResponseWriter, q *http.Request) {
				calls++
				if mode == "auth" {
					w.WriteHeader(401)
					_, _ = w.Write([]byte("fixture-private-body"))
					return
				}
				id, path := 42, "group/nested/repo"
				if mode == "id" {
					id = 43
				} else {
					path = "other/repo"
				}
				gitlabJSON(t, w, map[string]any{"id": id, "path_with_namespace": path})
			})
			runs, gaps, err := p.listRuns(context.Background())
			if err == nil || len(runs) != 0 || len(gaps) != 1 || calls != 1 {
				t.Fatal("mismatched/unavailable project became acquisition scope")
			}
			if strings.Contains(err.Error(), "fixture-private-body") || strings.Contains(err.Error(), "http") && strings.Contains(err.Error(), "://") {
				t.Fatal("service details leaked")
			}
		})
	}
}

func TestGitLabOptionalVersion(t *testing.T) {
	for _, tc := range []struct {
		name      string
		status    int
		body      string
		transport error
		fatal     bool
		calls     int
	}{
		{"401", 401, "", nil, false, 1}, {"403", 403, "", nil, false, 1}, {"404", 404, "", nil, false, 1},
		{"server", 500, "", nil, false, 3}, {"network", 0, "", errors.New("fixture network failure"), false, 3},
		{"malformed", 200, "{bad", nil, false, 1}, {"missing", 200, "{}", nil, false, 1},
		{"invalid", 200, "{\"version\":\"19.4.not-a-version\"}", nil, false, 1},
		{"runner", 200, "{\"kas\":{\"version\":\"19.4.0\"}}", nil, false, 1},
		{"rate", 429, "", nil, true, 1}, {"canceled", 0, "", context.Canceled, true, 1}, {"deadline", 0, "", context.DeadlineExceeded, true, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			versions, pipelines := 0, 0
			p := gitlabFixture(t, gitlabRequest(), func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.EscapedPath() {
				case "/api/v4/projects/group%2Fnested%2Frepo":
					gitlabJSON(t, w, map[string]any{"id": 42, "path_with_namespace": "group/nested/repo"})
				case "/api/v4/version":
					versions++
					w.WriteHeader(tc.status)
					_, _ = w.Write([]byte(tc.body))
				case "/api/v4/projects/42/pipelines":
					pipelines++
					gitlabJSON(t, w, []any{})
				default:
					t.Error("unexpected optional metadata workaround")
					w.WriteHeader(404)
				}
			})
			if tc.transport != nil {
				base := p.client.api.Transport
				if base == nil {
					base = http.DefaultTransport
				}
				p.client.api.Transport = httpRoundTripFunc(func(r *http.Request) (*http.Response, error) {
					if r.URL.Path == "/api/v4/version" {
						versions++
						return nil, tc.transport
					}
					return base.RoundTrip(r)
				})
			}
			runs, gaps, err := p.listRuns(context.Background())
			wantPipelines := 1
			if tc.fatal {
				wantPipelines = 0
			}
			if (err != nil) != tc.fatal || len(runs) != 0 || versions != tc.calls || pipelines != wantPipelines || p.versionKnown || (!tc.fatal && len(gaps) != 0) {
				t.Fatalf("optional metadata boundary changed: err=%v versions=%d pipelines=%d gaps=%d", err, versions, pipelines, len(gaps))
			}
			if tc.transport == context.Canceled && !errors.Is(err, context.Canceled) || tc.transport == context.DeadlineExceeded && !errors.Is(err, context.DeadlineExceeded) {
				t.Fatal("context identity lost")
			}
		})
	}
}

func gitlabPipelineJSON(id int64, second int, ref string) map[string]any {
	return map[string]any{"id": id, "project_id": 42, "sha": strings.Repeat("a", 40), "ref": ref, "status": "success", "created_at": gitlabRequest().Since.Add(time.Duration(second) * time.Second).Format(time.RFC3339)}
}

func gitlabDiscoveryFixture(t *testing.T, request Request, version string, handler http.HandlerFunc) *gitlabProvider {
	t.Helper()
	return gitlabFixture(t, request, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.EscapedPath() {
		case "/api/v4/projects/group%2Fnested%2Frepo":
			gitlabJSON(t, w, map[string]any{"id": 42, "path_with_namespace": "group/nested/repo"})
		case "/api/v4/version":
			gitlabJSON(t, w, map[string]any{"version": version})
		default:
			handler(w, r)
		}
	})
}

func TestGitLabPipelineHistory(t *testing.T) {
	rootCalls := 0
	p := gitlabDiscoveryFixture(t, gitlabRequest(), "19.4.0", func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v4/projects/42/pipelines":
			rootCalls++
			if r.URL.Query().Get("page") == "1" {
				w.Header().Set("X-Next-Page", "2")
				gitlabJSON(t, w, []any{gitlabPipelineJSON(17, 0, "main"), gitlabPipelineJSON(18, 1, "main")})
			} else {
				gitlabJSON(t, w, []any{gitlabPipelineJSON(17, 0, "main"), gitlabPipelineJSON(19, 4, "main"), gitlabPipelineJSON(20, 2, "other")})
			}
		case "/api/v4/projects/42/pipelines/17/trigger_jobs":
			gitlabJSON(t, w, []any{map[string]any{"id": 1, "downstream_pipeline": map[string]any{"id": 21, "project_id": 42}}, map[string]any{"id": 2, "downstream_pipeline": map[string]any{"id": 22, "project_id": 43}}})
		case "/api/v4/projects/42/pipelines/18/trigger_jobs":
			gitlabJSON(t, w, []any{})
		case "/api/v4/projects/42/pipelines/21":
			gitlabJSON(t, w, gitlabPipelineJSON(21, 2, "main"))
		case "/api/v4/projects/42/pipelines/21/trigger_jobs":
			gitlabJSON(t, w, []any{map[string]any{"id": 3, "downstream_pipeline": map[string]any{"id": 17, "project_id": 42}}})
		default:
			t.Error("unexpected/outside-scope pipeline request")
			w.WriteHeader(404)
		}
	})
	runs, gaps, err := p.listRuns(context.Background())
	ids := []string{}
	for _, r := range runs {
		ids = append(ids, r.ID)
		if r.RunAttempt != nil || r.SourceHeadSHA != strings.Repeat("a", 40) {
			t.Error("pipeline execution facts were fabricated")
		}
	}
	if err != nil || strings.Join(ids, ",") != "17,18,21" || rootCalls != 2 || len(gaps) != 1 || gaps[0].Reason != "downstream_outside_scope" {
		t.Fatalf("pipeline history/scope lost: ids=%v gaps=%v err=%v", ids, gaps, err)
	}
}

func TestGitLabTriggerCapabilities(t *testing.T) {
	for _, tc := range []struct {
		name, version string
		status        int
		malformed     bool
		legacy, fatal bool
	}{
		{"modern_old_backport", "19.1.0", 200, false, false, false},
		{"legacy404", "19.1.0", 404, false, true, false}, {"legacy405", "19.1.0", 405, false, true, false},
		{"old400", "19.1.0", 400, false, false, false}, {"unknown", "", 404, false, false, false},
		{"new404", "19.2.0", 404, false, false, false}, {"auth", "19.1.0", 403, false, false, true},
		{"rate", "19.1.0", 429, false, false, true}, {"server", "19.1.0", 500, false, false, false},
		{"malformed", "19.1.0", 200, true, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			modern, legacy := 0, 0
			p := gitlabDiscoveryFixture(t, gitlabRequest(), tc.version, func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/v4/projects/42/pipelines":
					gitlabJSON(t, w, []any{gitlabPipelineJSON(17, 0, "main")})
				case "/api/v4/projects/42/pipelines/17/trigger_jobs":
					modern++
					w.WriteHeader(tc.status)
					if tc.status == 200 {
						if tc.malformed {
							_, _ = w.Write([]byte("{bad"))
						} else {
							gitlabJSON(t, w, []any{})
						}
					}
				case "/api/v4/projects/42/pipelines/17/bridges":
					legacy++
					gitlabJSON(t, w, []any{})
				default:
					t.Error("unexpected capability request")
					w.WriteHeader(404)
				}
			})
			runs, gaps, err := p.listRuns(context.Background())
			wantModern, wantLegacy := 1, 0
			if tc.status == 500 {
				wantModern = 3
			}
			if tc.legacy {
				wantLegacy = 1
			}
			wantGap := tc.status != 200 || tc.malformed
			if tc.legacy {
				wantGap = false
			}
			if modern != wantModern || legacy != wantLegacy || (err != nil) != tc.fatal || len(runs) != 1 || (len(gaps) > 0) != wantGap {
				t.Fatalf("trigger fallback boundary changed: modern=%d legacy=%d runs=%d gaps=%d err=%v", modern, legacy, len(runs), len(gaps), err)
			}
		})
	}
}

func TestGitLabPipelineScopeAndCaps(t *testing.T) {
	for _, mode := range []string{"project", "id", "sha", "time", "loop", "filter", "pages", "oversized", "caller", "child_scope", "child_id"} {
		t.Run(mode, func(t *testing.T) {
			request := gitlabRequest()
			if mode == "caller" {
				request.MaxPipelines = 2
			}
			roots, children := 0, 0
			p := gitlabDiscoveryFixture(t, request, "19.4.0", func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.URL.Path == "/api/v4/projects/42/pipelines":
					roots++
					row := gitlabPipelineJSON(17, 0, "main")
					switch mode {
					case "project":
						row["project_id"] = 43
					case "id":
						row["id"] = 0
					case "sha":
						row["sha"] = "short"
					case "time":
						delete(row, "created_at")
					case "loop":
						if roots == 1 {
							w.Header().Set("X-Next-Page", "2")
						} else {
							w.Header().Set("X-Next-Page", "1")
						}
					case "filter":
						w.Header().Set("Link", "<?page=2>; rel=\"next\"")
					case "pages":
						w.Header().Set("X-Next-Page", strconv.Itoa(roots+1))
					}
					rows := []any{row}
					if mode == "pages" {
						rows = []any{}
					}
					if mode == "oversized" {
						rows = make([]any, 101)
						for i := range rows {
							rows[i] = gitlabPipelineJSON(int64(100+i), 0, "main")
						}
					}
					gitlabJSON(t, w, rows)
				case strings.HasSuffix(r.URL.Path, "/trigger_jobs"):
					if strings.Contains(r.URL.Path, "/17/") && (mode == "caller" || mode == "child_scope" || mode == "child_id") {
						rows := []any{map[string]any{"id": 1, "downstream_pipeline": map[string]any{"id": 18, "project_id": 42}}}
						if mode == "caller" {
							rows = append(rows, map[string]any{"id": 2, "downstream_pipeline": map[string]any{"id": 19, "project_id": 42}})
						}
						gitlabJSON(t, w, rows)
					} else {
						gitlabJSON(t, w, []any{})
					}
				case r.URL.Path == "/api/v4/projects/42/pipelines/18":
					children++
					row := gitlabPipelineJSON(18, 2, "main")
					if mode == "child_scope" {
						row["ref"] = "other"
					}
					if mode == "child_id" {
						row["id"] = 19
					}
					gitlabJSON(t, w, row)
				default:
					t.Error("unbounded/outside-scope child acquisition")
					w.WriteHeader(404)
				}
			})
			runs, gaps, _ := p.listRuns(context.Background())
			if len(gaps) == 0 {
				t.Fatal("invalid/partial/capped pipeline discovery claimed completeness")
			}
			if roots > 10 || mode == "loop" && roots != 2 || mode == "filter" && roots != 1 || mode == "caller" && (len(runs) != 2 || children != 1) || (mode == "child_scope" || mode == "child_id") && len(runs) != 1 {
				t.Fatal("pipeline scope/cap/loop guard did not bound actual work")
			}
			if (mode == "project" || mode == "id" || mode == "sha" || mode == "time" || mode == "oversized") && len(runs) != 0 {
				t.Fatal("untrusted pipeline metadata became source evidence")
			}
		})
	}
}

func TestGitLabTriggerPaging(t *testing.T) {
	for _, mode := range []string{"pages", "loop", "filter", "cap"} {
		t.Run(mode, func(t *testing.T) {
			calls := 0
			p := gitlabDiscoveryFixture(t, gitlabRequest(), "19.4.0", func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/v4/projects/42/pipelines" {
					gitlabJSON(t, w, []any{gitlabPipelineJSON(17, 0, "main")})
					return
				}
				calls++
				switch mode {
				case "loop":
					if calls == 1 {
						w.Header().Set("X-Next-Page", "2")
					} else {
						w.Header().Set("X-Next-Page", "1")
					}
				case "filter":
					w.Header().Set("Link", "<?page=2>; rel=\"next\"")
				default:
					w.Header().Set("X-Next-Page", strconv.Itoa(calls+1))
				}
				rows := []any{}
				if mode == "cap" {
					for i := 0; i < 100; i++ {
						rows = append(rows, map[string]any{"id": calls*100 + i + 1})
					}
				}
				gitlabJSON(t, w, rows)
			})
			runs, gaps, err := p.listRuns(context.Background())
			if err != nil || len(runs) != 1 || len(gaps) != 1 || calls > 10 || mode == "loop" && calls != 2 || mode == "filter" && calls != 1 {
				t.Fatal("trigger pagination/cap claimed exhaustive discovery or repeated acquisition")
			}
		})
	}
}

func gitlabSourceRun() sourceRun {
	created := gitlabRequest().Since.Add(time.Second)
	return sourceRun{ID: "17", SourceHeadSHA: strings.Repeat("a", 40), Branch: "main", Status: "success", CreatedAt: &created}
}

type gitlabAcquisitionFixture struct {
	Version                                                                   string
	Raw, Archive                                                              []byte
	ReportStatus, TraceStatus, CommitStatus, Jobs                             int
	AdvertisedArchive                                                         bool
	JobChange                                                                 func(map[string]any)
	CommitChange                                                              func(map[string]any)
	TraceChange                                                               func(string) string
	DirectCalls, ArchiveCalls, TraceCalls, CommitCalls, ModuleCalls, JobCalls int
}

func gitlabAcquisitionProvider(t *testing.T, c *gitlabAcquisitionFixture) *gitlabProvider {
	t.Helper()
	xml := "<testsuite><testcase classname=\"tests.test_value\" name=\"test_value[param]\" time=\"0.012\"><failure type=\"AssertionError\" message=\"actual assertion\">raw diagnostic</failure></testcase></testsuite>"
	if c.Raw == nil {
		c.Raw = []byte(xml)
	}
	if c.Archive == nil {
		c.Archive = githubReportZIP(t, "reports/junit.xml", xml)
	}
	return gitlabDiscoveryFixture(t, gitlabRequest(), c.Version, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/v4/projects/42/pipelines/17/jobs":
			c.JobCalls++
			if r.URL.Query().Get("include_retried") != "true" || r.URL.Query().Get("per_page") != "100" {
				t.Error("retry-preserving job query lost")
			}
			count := max(1, c.Jobs)
			rows := []any{}
			for i := 0; i < count; i++ {
				row := map[string]any{"id": 201 + i, "name": "same-test-name", "status": "success", "ref": "main", "finished_at": "2026-01-01T00:00:03Z", "erased_at": nil, "project": map[string]any{"id": 42}, "pipeline": map[string]any{"id": 17, "project_id": 42, "sha": strings.Repeat("a", 40), "ref": "main"}, "commit": map[string]any{"id": strings.Repeat("a", 40)}}
				if c.AdvertisedArchive {
					row["artifacts_file"] = map[string]any{"filename": "artifacts.zip", "size": len(c.Archive)}
				}
				if c.JobChange != nil {
					c.JobChange(row)
				}
				rows = append(rows, row)
			}
			gitlabJSON(t, w, rows)
		case strings.HasSuffix(r.URL.Path, "/artifacts"):
			if r.URL.Query().Get("file_type") == "junit" {
				c.DirectCalls++
				status := c.ReportStatus
				if status == 0 {
					status = 200
				}
				w.WriteHeader(status)
				if status == 200 {
					_, _ = w.Write(c.Raw)
				}
			} else {
				c.ArchiveCalls++
				_, _ = w.Write(c.Archive)
			}
		case strings.HasSuffix(r.URL.Path, "/trace"):
			c.TraceCalls++
			if c.TraceStatus != 0 {
				w.WriteHeader(c.TraceStatus)
				return
			}
			parts := strings.Split(r.URL.Path, "/")
			trace := strings.ReplaceAll(gitLabFixtureTrace(), " token=fixture-secret", "")
			trace = strings.ReplaceAll(trace, "id=42", "id="+parts[len(parts)-2])
			if c.TraceChange != nil {
				trace = c.TraceChange(trace)
			}
			_, _ = w.Write([]byte(trace))
		case strings.Contains(r.URL.Path, "/repository/commits/"):
			c.CommitCalls++
			if !strings.HasSuffix(r.URL.Path, "/"+gitLabFixtureSHA) {
				t.Error("commit metadata substituted source revision")
			}
			if c.CommitStatus != 0 {
				w.WriteHeader(c.CommitStatus)
				return
			}
			row := map[string]any{"id": gitLabFixtureSHA, "authored_date": "2026-01-01T00:00:00Z", "parent_ids": []string{strings.Repeat("b", 40), strings.Repeat("c", 40)}, "stats": map[string]any{"additions": 2, "deletions": 1, "total": 3}}
			if c.CommitChange != nil {
				c.CommitChange(row)
			}
			gitlabJSON(t, w, row)
		case strings.Contains(r.URL.Path, "/repository/files/"):
			c.ModuleCalls++
			t.Error("unsupported Go producer caused module acquisition")
			w.WriteHeader(404)
		default:
			t.Error("unexpected acquisition route")
			w.WriteHeader(404)
		}
	})
}

func gitlabCapture(gaps *[]acquisitionGap, artifacts *[]acquiredArtifact) acquisitionSink {
	return acquisitionSink{Gap: func(g acquisitionGap) error { *gaps = append(*gaps, g); return nil }, Artifact: func(a acquiredArtifact) error { *artifacts = append(*artifacts, a); return nil }}
}

func TestGitLabAcquireEvidence(t *testing.T) {
	for _, mode := range []string{"xml", "gzip", "failed"} {
		t.Run(mode, func(t *testing.T) {
			c := &gitlabAcquisitionFixture{Version: "19.4.0"}
			if mode == "gzip" {
				var raw bytes.Buffer
				z := gzip.NewWriter(&raw)
				_, _ = z.Write([]byte("<testsuite><testcase classname=\"tests.test_value\" name=\"test_value[param]\"><failure>failed</failure></testcase></testsuite>"))
				if err := z.Close(); err != nil {
					t.Fatal(err)
				}
				c.Raw = raw.Bytes()
			}
			if mode == "failed" {
				c.JobChange = func(v map[string]any) { v["status"] = "failed"; v["failure_reason"] = "script_failure" }
				c.TraceChange = func(trace string) string {
					trace = strings.ReplaceAll(trace, "upload_artifacts_on_success", "upload_artifacts_on_failure")
					return strings.Replace(trace, "Job succeeded", "ERROR: Job failed: exit code 1", 1)
				}
			}
			p := gitlabAcquisitionProvider(t, c)
			var gaps []acquisitionGap
			var got []acquiredArtifact
			outcome, err := p.acquireRun(context.Background(), gitlabSourceRun(), gitlabCapture(&gaps, &got))
			if err != nil || outcome.State != "complete" || len(got) != 1 || c.DirectCalls != 1 || c.TraceCalls != 1 || len(outcome.Attempts) != 0 {
				t.Fatalf("job report was not acquired completely: artifacts=%d gaps=%d state=%s err=%v", len(got), len(gaps), outcome.State, err)
			}
			o := got[0].Observation
			hash := sha256.Sum256(c.Raw)
			if !bytes.Equal(got[0].Raw, c.Raw) || o.ContentSHA256 != hex.EncodeToString(hash[:]) || o.JobID == nil || *o.JobID != "201" || o.ArtifactID != nil || o.Attempt != nil || o.RunAttempt != nil || o.TestedSHA == nil || *o.TestedSHA != "0123456789abcdef0123456789abcdef01234567" || o.SourceHeadSHA != strings.Repeat("a", 40) || o.UnitKey != "job:201:junit" || o.RawBlobPath != "" || !o.ObservedAt.Equal(gitlabRequest().Since.Add(3*time.Second)) || o.EnvironmentID != nil {
				t.Fatal("job/source/tested/time/byte facts lost or fabricated")
			}
			if len(o.Entries) != 1 || o.Entries[0].Name != "junit.xml" || o.ExecutionEvidence == nil || !strings.Contains(*o.ExecutionEvidence, "reports/result.xml") || o.Entries[0].Cases[0].Case.Status != "fail" || o.Entries[0].Cases[0].Identity.ID != "py:tests.test_value:test_value[param]" || o.Entries[0].Cases[0].Identity.RunnerID != "" {
				t.Fatal("logical report name, producer path or factual failing identity changed")
			}
		})
	}
}

func TestGitLabReportCapabilities(t *testing.T) {
	for _, tc := range []struct {
		name, version                         string
		status                                int
		advertised, malformed, archive, fatal bool
	}{
		{"backport", "19.3.0", 200, false, false, false, false},
		{"legacy400", "19.3.0", 400, false, false, true, false}, {"legacy404", "19.3.0", 404, false, false, true, false}, {"legacy405", "19.3.0", 405, false, false, true, false},
		{"unknown", "", 404, false, false, false, false}, {"modern_missing", "19.4.0", 404, false, false, false, false},
		{"advertised", "", 404, true, false, true, false}, {"advertised400", "", 400, true, false, false, false},
		{"auth", "19.3.0", 401, true, false, false, true}, {"rate", "19.3.0", 429, true, false, false, true},
		{"server", "19.3.0", 500, true, false, false, false}, {"malformed", "19.3.0", 200, true, true, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := &gitlabAcquisitionFixture{Version: tc.version, ReportStatus: tc.status, AdvertisedArchive: tc.advertised}
			if tc.malformed {
				c.Raw = []byte("<html>wrong body</html>")
			}
			p := gitlabAcquisitionProvider(t, c)
			var gaps []acquisitionGap
			var got []acquiredArtifact
			outcome, err := p.acquireRun(context.Background(), gitlabSourceRun(), gitlabCapture(&gaps, &got))
			wantDirect, wantArchive := 1, 0
			if tc.status == 500 {
				wantDirect = 3
			}
			if tc.archive {
				wantArchive = 1
			}
			wantArtifact := tc.archive || tc.status == 200 && !tc.malformed
			if c.DirectCalls != wantDirect || c.ArchiveCalls != wantArchive || (err != nil) != tc.fatal || (len(got) == 1) != wantArtifact {
				t.Fatalf("report fallback boundary changed: direct=%d archive=%d got=%d err=%v", c.DirectCalls, c.ArchiveCalls, len(got), err)
			}
			if tc.archive && (got[0].Observation.UnitKey != "job:201:archive" || got[0].Observation.TestedSHA != nil || c.TraceCalls != 0 || got[0].Observation.Entries[0].Name != "reports/junit.xml") {
				t.Fatal("archive source invented direct-report provenance")
			}
			if !wantArtifact && (outcome.State == "complete" || len(gaps) == 0) {
				t.Fatal("missing report became complete")
			}
			if tc.malformed && (len(gaps) != 1 || !bytes.Equal(gaps[0].Raw, c.Raw)) {
				t.Fatal("malformed acquired bytes were discarded or alternate source was guessed")
			}
		})
	}
}

func TestGitLabJobAndRawGaps(t *testing.T) {
	for _, mode := range []string{"finished_at", "empty_bytes", "zero_time", "erased", "project", "pipeline", "commit", "ref", "queued", "unsupported", "wrong_trace_job"} {
		t.Run(mode, func(t *testing.T) {
			c := &gitlabAcquisitionFixture{Version: "19.4.0"}
			c.JobChange = func(v map[string]any) {
				switch mode {
				case "finished_at", "empty_bytes":
					delete(v, "finished_at")
				case "zero_time":
					v["finished_at"] = "0001-01-01T00:00:00Z"
				case "erased":
					v["erased_at"] = "2026-01-01T00:00:03Z"
				case "project":
					v["project"] = map[string]any{"id": 43}
				case "pipeline":
					v["pipeline"] = map[string]any{"id": 18}
				case "commit":
					v["commit"] = map[string]any{"id": strings.Repeat("f", 40)}
				case "ref":
					v["ref"] = "other"
				case "queued":
					v["status"] = "pending"
				}
			}
			if mode == "empty_bytes" {
				c.Raw = []byte{}
			}
			if mode == "unsupported" {
				c.TraceChange = func(string) string { return "unsupported nonsecret producer\n" }
			}
			if mode == "wrong_trace_job" {
				c.TraceChange = func(v string) string { return strings.ReplaceAll(v, "id=201", "id=999") }
			}
			p := gitlabAcquisitionProvider(t, c)
			var gaps []acquisitionGap
			var got []acquiredArtifact
			outcome, err := p.acquireRun(context.Background(), gitlabSourceRun(), gitlabCapture(&gaps, &got))
			if err != nil {
				t.Fatal(err)
			}
			if mode == "unsupported" || mode == "wrong_trace_job" {
				if len(got) != 1 || got[0].Observation.TestedSHA != nil || got[0].Observation.JobID == nil || got[0].Observation.ExclusionReason != "provenance_unverified" || got[0].Observation.Entries[0].Cases[0].Case.Status != "fail" || outcome.State != "complete" {
					t.Fatal("unsupported trace erased factual report or invented provenance/completeness gap")
				}
				return
			}
			if len(gaps) == 0 || outcome.State == "complete" || len(got) != 0 {
				t.Fatal("unavailable/mismatched job facts fabricated a report")
			}
			if mode == "finished_at" || mode == "empty_bytes" || mode == "zero_time" {
				if gaps[len(gaps)-1].Raw == nil || !bytes.Equal(gaps[len(gaps)-1].Raw, c.Raw) || c.TraceCalls != 0 {
					t.Fatal("source-time gap lost available nil-versus-empty bytes")
				}
			} else if c.DirectCalls != 0 {
				t.Fatal("erased/nonterminal/foreign job acquired bytes")
			}
		})
	}
}

func TestGitLabCallbacksAndSourceFailures(t *testing.T) {
	for _, mode := range []string{"artifact", "gap_ordinary", "gap_canceled", "gap_deadline", "gap_auth", "gap_rate", "source401", "source429", "cancel_after_artifact"} {
		t.Run(mode, func(t *testing.T) {
			c := &gitlabAcquisitionFixture{Version: "19.4.0", Jobs: 2}
			sentinel := errors.New("fixture writer failure")
			if strings.HasPrefix(mode, "gap_") {
				c.TraceStatus = 404
			}
			switch mode {
			case "gap_canceled":
				sentinel = context.Canceled
			case "gap_deadline":
				sentinel = context.DeadlineExceeded
			case "gap_auth":
				sentinel = &httpError{Reason: "auth"}
			case "gap_rate":
				sentinel = &httpError{Reason: "rate_limited"}
			case "source401":
				c.TraceStatus = 401
			case "source429":
				c.TraceStatus = 429
			}
			p := gitlabAcquisitionProvider(t, c)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			artifacts, gaps := 0, 0
			sink := acquisitionSink{Gap: func(acquisitionGap) error {
				gaps++
				if strings.HasPrefix(mode, "gap_") {
					return sentinel
				}
				return nil
			}, Artifact: func(acquiredArtifact) error {
				artifacts++
				if mode == "artifact" {
					return sentinel
				}
				if mode == "cancel_after_artifact" {
					cancel()
				}
				return nil
			}}
			outcome, err := p.acquireRun(ctx, gitlabSourceRun(), sink)
			wantArtifacts := 1
			if strings.HasPrefix(mode, "gap_") {
				wantArtifacts = 0
			}
			if err == nil || outcome.State == "complete" || artifacts != wantArtifacts || c.DirectCalls != 1 || c.TraceCalls != 1 {
				t.Fatal("callback/context/source failure continued to another job or lost acquired bytes")
			}
			if mode == "artifact" || strings.HasPrefix(mode, "gap_") {
				if err != sentinel {
					t.Fatal("callback error identity changed")
				}
			}
			if strings.HasPrefix(mode, "source") && !gitlabFatal(err) {
				t.Fatal("source auth/rate error did not propagate")
			}
			if mode == "cancel_after_artifact" && !errors.Is(err, context.Canceled) {
				t.Fatal("final cancellation identity lost")
			}
		})
	}
}

func TestGitLabJobIDsAndRequirements(t *testing.T) {
	c := &gitlabAcquisitionFixture{Version: "19.4.0", Jobs: 2}
	p := gitlabAcquisitionProvider(t, c)
	var gaps []acquisitionGap
	var got []acquiredArtifact
	outcome, err := p.acquireRun(context.Background(), gitlabSourceRun(), gitlabCapture(&gaps, &got))
	if err != nil || outcome.State != "complete" || len(got) != 2 || *got[0].Observation.JobID != "201" || *got[1].Observation.JobID != "202" || got[0].Observation.Attempt != nil || got[1].Observation.Attempt != nil || len(outcome.Attempts) != 0 {
		t.Fatal("same-named retry jobs lost exact execution identities")
	}
	for _, mode := range []string{"artifact", "gap", "context"} {
		t.Run(mode, func(t *testing.T) {
			f := &gitlabAcquisitionFixture{Version: "19.4.0"}
			provider := gitlabAcquisitionProvider(t, f)
			sink := gitlabCapture(&gaps, &got)
			if mode == "artifact" {
				sink.Artifact = nil
			}
			if mode == "gap" {
				sink.Gap = nil
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if mode == "context" {
				cancel()
			}
			result, err := provider.acquireRun(ctx, gitlabSourceRun(), sink)
			if err == nil || result.State == "complete" || f.JobCalls != 0 || f.DirectCalls != 0 {
				t.Fatal("invalid/canceled acquisition performed source work")
			}
		})
	}
}

func TestGitLabTestedCommitMetadata(t *testing.T) {
	for _, mode := range []string{"complete", "root", "id", "parents", "parent_value", "additions", "deletions", "stats", "authored", "auth", "rate", "server", "unsupported", "archive", "language_conflict", "callback"} {
		t.Run(mode, func(t *testing.T) {
			c := &gitlabAcquisitionFixture{Version: "19.4.0"}
			c.CommitChange = func(row map[string]any) {
				switch mode {
				case "root":
					row["parent_ids"] = []string{}
				case "id":
					row["id"] = strings.Repeat("a", 40)
				case "parents":
					delete(row, "parent_ids")
				case "parent_value":
					row["parent_ids"] = []string{"short"}
				case "additions":
					row["stats"] = map[string]int{"additions": -1, "deletions": 1, "total": 9000}
				case "deletions":
					row["stats"] = map[string]int{"additions": 2, "deletions": -1, "total": 9000}
				case "stats":
					delete(row, "stats")
				case "authored":
					delete(row, "authored_date")
				}
			}
			switch mode {
			case "auth":
				c.CommitStatus = 401
			case "rate":
				c.CommitStatus = 429
			case "server":
				c.CommitStatus = 500
			case "unsupported":
				c.TraceChange = func(string) string { return "unknown producer\n" }
			case "archive":
				c.Version = "19.3.0"
				c.ReportStatus = 404
			case "callback":
				c.CommitStatus = 404
			}
			p := gitlabAcquisitionProvider(t, c)
			if mode == "language_conflict" {
				p.request.Language = "go"
			}
			var gaps []acquisitionGap
			var got []acquiredArtifact
			sink := gitlabCapture(&gaps, &got)
			sentinel := errors.New("commit gap writer rejected")
			if mode == "callback" {
				sink.Gap = func(acquisitionGap) error { return sentinel }
			}
			outcome, err := p.acquireRun(context.Background(), gitlabSourceRun(), sink)
			if mode == "callback" {
				if err != sentinel || len(got) != 0 || outcome.State == "complete" {
					t.Fatal("commit gap callback lost origin or emitted later artifact")
				}
				return
			}
			if (err != nil) != (mode == "auth" || mode == "rate") || len(got) != 1 {
				t.Fatal("metadata failure lost acquired report or source fatality")
			}
			o := got[0].Observation
			if mode == "unsupported" || mode == "archive" {
				if c.CommitCalls != 0 || o.Commit != nil || o.TestedSHA != nil || outcome.State != "complete" {
					t.Fatal("unknown tested revision acquired guessed commit metadata")
				}
				return
			}
			wantCalls := 1
			if mode == "server" {
				wantCalls = 3
			}
			if c.CommitCalls != wantCalls || c.ModuleCalls != 0 {
				t.Fatal("wrong tested commit request count or unsupported module acquisition")
			}
			if mode == "complete" || mode == "root" || mode == "language_conflict" {
				m := o.Commit
				if outcome.State != "complete" || len(gaps) != 0 || o.ExclusionReason != "commit_file_count_unverified" || o.ExclusionDetail == "" || m == nil || m.AuthoredAt == nil || !m.AuthoredAt.Equal(gitlabRequest().Since) || m.ParentSHAs == nil || m.FilesChanged != nil || m.Kinds != nil || m.LinesAdded == nil || *m.LinesAdded != 2 || m.LinesRemoved == nil || *m.LinesRemoved != 1 {
					t.Fatal("known metadata limit invented file count or made factual source incomplete")
				}
				if mode == "root" && len(m.ParentSHAs) != 0 || mode != "root" && (len(m.ParentSHAs) != 2 || m.ParentSHAs[0] != strings.Repeat("b", 40) || m.ParentSHAs[1] != strings.Repeat("c", 40)) {
					t.Fatal("all actual parents/root evidence not retained")
				}
				if mode == "language_conflict" && o.Entries[0].Cases[0].ExclusionReason != "language_conflict" {
					t.Fatal("producer/operator conflict invented Go identity")
				}
			} else {
				if outcome.State == "complete" || len(gaps) != 1 || gaps[0].Reason != "commit_unavailable" || gaps[0].UnitKey != "commit:"+gitLabFixtureSHA || gaps[0].Raw != nil {
					t.Fatal("failed required commit metadata did not create bounded acquisition gap")
				}
				if mode == "id" && o.Commit != nil {
					t.Fatal("wrong revision commit supplied facts")
				}
				if mode == "authored" && (o.Commit == nil || o.Commit.AuthoredAt != nil || o.Commit.ParentSHAs == nil || o.Commit.LinesAdded == nil) {
					t.Fatal("partial commit facts were discarded or missing fact fabricated")
				}
				if mode == "additions" && (o.Commit == nil || o.Commit.LinesAdded != nil || o.Commit.LinesRemoved == nil) {
					t.Fatal("negative actual stats became valid metadata")
				}
			}
		})
	}
}

func TestGitLabJobsPagination(t *testing.T) {
	for _, mode := range []string{"pages", "loop", "filter", "cap", "null", "oversized", "auth", "rate", "server", "callback"} {
		t.Run(mode, func(t *testing.T) {
			calls := 0
			p := gitlabDiscoveryFixture(t, gitlabRequest(), "19.4.0", func(w http.ResponseWriter, r *http.Request) {
				if !strings.HasSuffix(r.URL.Path, "/pipelines/17/jobs") || r.URL.Query().Get("include_retried") != "true" || r.URL.Query().Get("per_page") != "100" {
					t.Error("job paging lost exact route/retry filter")
				}
				calls++
				switch mode {
				case "auth":
					w.WriteHeader(401)
					return
				case "rate":
					w.WriteHeader(429)
					return
				case "server":
					w.WriteHeader(500)
					return
				case "callback":
					w.WriteHeader(404)
					return
				case "null":
					gitlabJSON(t, w, nil)
					return
				case "filter":
					w.Header().Set("Link", "<?page=2>; rel=\"next\"")
				case "loop":
					w.Header().Set("X-Next-Page", strconv.Itoa(3-calls))
				case "cap":
					w.Header().Set("X-Next-Page", strconv.Itoa(calls+1))
				case "pages":
					if calls == 1 {
						w.Header().Set("X-Next-Page", "2")
					}
				}
				rows := []any{map[string]any{"id": 201}}
				if mode == "pages" && calls == 2 {
					rows = append(rows, map[string]any{"id": 202})
				}
				if mode == "cap" || mode == "oversized" {
					rows = nil
					for i := 0; i < 100; i++ {
						rows = append(rows, map[string]int{"id": calls*100 + i + 1})
					}
					if mode == "oversized" {
						rows = append(rows, map[string]int{"id": 99999})
					}
				}
				gitlabJSON(t, w, rows)
			})
			if err := p.prepare(context.Background()); err != nil {
				t.Fatal(err)
			}
			var gaps []acquisitionGap
			var got []acquiredArtifact
			sink := gitlabCapture(&gaps, &got)
			sentinel := errors.New("jobs gap writer rejected")
			if mode == "callback" {
				sink.Gap = func(acquisitionGap) error { return sentinel }
			}
			outcome := runOutcome{State: "complete"}
			jobs, err := p.listJobs(context.Background(), gitlabSourceRun(), sink, &outcome)
			if mode == "pages" {
				if err != nil || len(gaps) != 0 || len(jobs) != 2 || jobs[0].ID != 201 || jobs[1].ID != 202 || calls != 2 {
					t.Fatal("job pagination lost actual IDs or duplicated execution")
				}
				return
			}
			if mode == "callback" {
				if err != sentinel || outcome.State == "complete" || calls != 1 {
					t.Fatal("job metadata gap callback origin lost")
				}
				return
			}
			if (err != nil) != (mode == "auth" || mode == "rate") || outcome.State == "complete" || len(gaps) != 1 || calls > 10 || mode == "loop" && calls != 2 || mode == "filter" && calls != 1 || mode == "server" && calls != 3 || mode == "cap" && (len(jobs) != 1000 || outcome.State != "capped") {
				t.Fatal("partial jobs claimed complete or exceeded bounded paging")
			}
		})
	}
}

func TestGitLabByteBudgets(t *testing.T) {
	for _, mode := range []string{"job_trace", "pipeline", "retry", "probe", "artifact"} {
		t.Run(mode, func(t *testing.T) {
			c := &gitlabAcquisitionFixture{Version: "19.4.0", Jobs: 5}
			if mode == "job_trace" || mode == "artifact" {
				c.Jobs = 1
			}
			if mode == "retry" {
				c.Jobs = 3
			}
			p := gitlabAcquisitionProvider(t, c)
			base := p.client.api.Transport
			if base == nil {
				base = http.DefaultTransport
			}
			attempts := map[string]int{}
			var downloaded int64
			p.client.api.Transport = httpRoundTripFunc(func(r *http.Request) (*http.Response, error) {
				response, err := base.RoundTrip(r)
				if err != nil {
					return response, err
				}
				trace := strings.HasSuffix(r.URL.Path, "/trace")
				artifact := mode == "artifact" && strings.HasSuffix(r.URL.Path, "/artifacts")
				if !trace && !artifact {
					return response, nil
				}
				_ = response.Body.Close()
				attempts[r.URL.Path]++
				size, fail := int64(16<<20), false
				switch mode {
				case "job_trace":
					size++
				case "artifact":
					size = (64 << 20) + 2
				case "retry":
					size = 1 << 20
					if attempts[r.URL.Path] <= 2 {
						size, fail = 16<<20, true
					}
				case "probe":
					if strings.Contains(r.URL.Path, "/203/") {
						size--
					}
					if strings.Contains(r.URL.Path, "/204/") {
						size += 2
					}
				}
				response.Body = io.NopCloser(&githubRetryReadBody{remaining: size, fail: fail, downloaded: &downloaded})
				response.ContentLength = -1
				response.Header.Del("Content-Length")
				return response, nil
			})
			var gaps []acquisitionGap
			var got []acquiredArtifact
			outcome, err := p.acquireRun(context.Background(), gitlabSourceRun(), gitlabCapture(&gaps, &got))
			wantArtifacts, wantCalls, wantBytes := 5, 4, int64(64<<20)
			switch mode {
			case "job_trace":
				wantArtifacts, wantCalls, wantBytes = 1, 1, (16<<20)+1
			case "artifact":
				wantArtifacts, wantCalls, wantBytes = 0, 0, (64<<20)+1
			case "retry":
				wantArtifacts, wantCalls = 3, 5
			}
			if err != nil || outcome.State != "capped" || len(got) != wantArtifacts || c.TraceCalls != wantCalls || downloaded != wantBytes || len(gaps) == 0 {
				t.Fatalf("provider byte bounds lost: artifacts=%d traces=%d bytes=%d gaps=%d state=%s", len(got), c.TraceCalls, downloaded, len(gaps), outcome.State)
			}
			for _, g := range gaps {
				if g.Raw != nil || g.Reason != "acquisition_capped" && g.Reason != "trace_unavailable" {
					t.Fatal("partial/oversized trace entered raw evidence")
				}
			}
			for _, a := range got {
				if a.Observation.TestedSHA != nil || !bytes.Equal(a.Raw, c.Raw) || a.Observation.JobID == nil {
					t.Fatal("capped trace erased acquired job report or invented tested revision")
				}
			}
			t.Logf("actual successful-status body reads=%d trace HTTP requests=%d retained reports=%d", downloaded, c.TraceCalls, len(got))
		})
	}
}

func TestGitLabReportIdentityAndGaps(t *testing.T) {
	for _, mode := range []string{"archive_glob", "direct_zip", "explicit_python", "unidentified", "empty_case", "non_junit", "incomplete_empty", "suite_incomplete", "source_time_callback", "parse_callback", "report_callback", "pipeline_running", "optional_job_metadata"} {
		t.Run(mode, func(t *testing.T) {
			c := &gitlabAcquisitionFixture{Version: "19.4.0"}
			switch mode {
			case "archive_glob":
				c.Version = "19.3.0"
				c.ReportStatus = 404
			case "direct_zip":
				c.Raw = githubReportZIP(t, "reports/junit.xml", "<testsuite><testcase classname=\"tests.test_value\" name=\"test_value[param]\"/></testsuite>")
			case "explicit_python", "unidentified":
				c.TraceChange = func(string) string { return "unknown producer\n" }
			case "empty_case":
				c.Raw = []byte("<testsuite></testsuite>")
			case "non_junit":
				c.Raw = []byte("<html></html>")
			case "incomplete_empty":
				c.Raw = []byte("<testsuite tests=\"1\"></testsuite>")
			case "suite_incomplete":
				c.Raw = []byte("<testsuite tests=\"2\"><testcase classname=\"tests.test_value\" name=\"test_value[param]\"/></testsuite>")
			case "source_time_callback":
				c.JobChange = func(v map[string]any) { delete(v, "finished_at") }
			case "parse_callback":
				c.Raw = []byte("wrong successful body")
			case "report_callback":
				c.ReportStatus = 404
			case "optional_job_metadata":
				c.JobChange = func(v map[string]any) {
					delete(v, "project")
					delete(v, "pipeline")
					delete(v, "commit")
					delete(v, "ref")
				}
			}
			p := gitlabAcquisitionProvider(t, c)
			if mode == "archive_glob" {
				p.request.JUnitGlob = "reports/not-present.xml"
			}
			if mode == "explicit_python" {
				p.request.Language = "python"
			}
			run := gitlabSourceRun()
			if mode == "pipeline_running" {
				run.Status = "running"
			}
			var gaps []acquisitionGap
			var got []acquiredArtifact
			sink := gitlabCapture(&gaps, &got)
			sentinel := errors.New("report gap writer rejected")
			if strings.HasSuffix(mode, "callback") {
				sink.Gap = func(acquisitionGap) error { return sentinel }
			}
			outcome, err := p.acquireRun(context.Background(), run, sink)
			if strings.HasSuffix(mode, "callback") {
				if err != sentinel || len(got) != 0 || outcome.State == "complete" || c.TraceCalls != 0 {
					t.Fatal("pre-observation callback error permitted later source/callback work")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if mode == "archive_glob" {
				if len(got) != 0 || len(gaps) != 1 || gaps[0].Raw == nil || outcome.State == "complete" {
					t.Fatal("archive glob mismatch fabricated observation")
				}
				return
			}
			if mode == "non_junit" || mode == "incomplete_empty" {
				if len(got) != 0 || len(gaps) != 1 || !bytes.Equal(gaps[0].Raw, c.Raw) || outcome.State == "complete" || c.TraceCalls != 0 || c.CommitCalls != 0 {
					t.Fatal("non-JUnit or incomplete empty body invented report/provenance")
				}
				return
			}
			if mode == "empty_case" {
				if len(got) != 1 || got[0].Observation.TestedSHA == nil || *got[0].Observation.TestedSHA != gitLabFixtureSHA || got[0].Observation.Commit == nil || c.TraceCalls != 1 || c.CommitCalls != 1 || outcome.State != "complete" || len(gaps) != 0 || len(got[0].Observation.Entries) != 1 || len(got[0].Observation.Entries[0].Cases) != 0 || !bytes.Equal(got[0].Raw, c.Raw) {
					t.Fatal("valid complete zero-case report lost actual producer facts or invented testcases")
				}
				return
			}
			if len(got) != 1 {
				t.Fatal("factual report was lost")
			}
			o := got[0].Observation
			switch mode {
			case "direct_zip":
				if o.TestedSHA != nil || c.TraceCalls != 0 || o.Entries[0].Name != "reports/junit.xml" {
					t.Fatal("direct ZIP guessed report-path provenance")
				}
			case "explicit_python":
				if o.Entries[0].Cases[0].Identity.ID != "py:tests.test_value:test_value[param]" || o.TestedSHA != nil || c.CommitCalls != 0 {
					t.Fatal("explicit identity was lost or promoted into tested revision")
				}
			case "unidentified":
				if o.Entries[0].Cases[0].ExclusionReason != "language_unavailable" || outcome.State != "complete" {
					t.Fatal("unknown language fabricated identity or source gap")
				}
			case "suite_incomplete":
				if !o.SuiteIncomplete || o.State == "complete" || outcome.State == "complete" || len(gaps) == 0 {
					t.Fatal("omitted testcase count became complete source")
				}
			case "pipeline_running":
				if outcome.State == "complete" || len(gaps) != 1 || gaps[0].Reason != "run_incomplete" {
					t.Fatal("running pipeline became complete")
				}
			case "optional_job_metadata":
				if o.TestedSHA == nil || o.JobID == nil || outcome.State != "complete" {
					t.Fatal("exact endpoint association rejected absent redundant metadata")
				}
			}
		})
	}
}

func TestGitLabCancellationBeforeArtifact(t *testing.T) {
	c := &gitlabAcquisitionFixture{Version: "19.4.0", TraceStatus: 404, Jobs: 2}
	p := gitlabAcquisitionProvider(t, c)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	artifacts, gaps := 0, 0
	sink := acquisitionSink{Gap: func(acquisitionGap) error { gaps++; cancel(); return nil }, Artifact: func(acquiredArtifact) error { artifacts++; return nil }}
	outcome, err := p.acquireRun(ctx, gitlabSourceRun(), sink)
	if !errors.Is(err, context.Canceled) || outcome.State == "complete" || gaps != 1 || artifacts != 0 || c.DirectCalls != 1 || c.TraceCalls != 1 {
		t.Fatal("canceled gap continuation emitted a later artifact or performed another source request")
	}
}

func TestGitLabRunGapCancellationStops(t *testing.T) {
	for _, location := range []string{"run_gap", "trace_gap"} {
		t.Run(location, func(t *testing.T) {
			c := &gitlabAcquisitionFixture{Version: "19.4.0"}
			run := gitlabSourceRun()
			wantReason := "trace_unavailable"
			if location == "run_gap" {
				run.Status = "running"
				wantReason = "run_incomplete"
			} else {
				c.TraceStatus = http.StatusNotFound
			}
			p := gitlabAcquisitionProvider(t, c)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var reasons []string
			artifacts := 0
			sink := acquisitionSink{Gap: func(g acquisitionGap) error {
				reasons = append(reasons, g.Reason)
				cancel()
				return nil
			}, Artifact: func(acquiredArtifact) error { artifacts++; return nil }}
			outcome, err := p.acquireRun(ctx, run, sink)
			t.Logf("reasons=%v artifacts=%d jobs=%d reports=%d traces=%d state=%s original_cancellation=%t", reasons, artifacts, c.JobCalls, c.DirectCalls, c.TraceCalls, outcome.State, err == context.Canceled)
			if err != context.Canceled || len(reasons) != 1 || reasons[0] != wantReason || artifacts != 0 || outcome.State != "incomplete" {
				t.Fatal("cancellation after a gap acknowledgment allowed another callback or lost original cancellation")
			}
			if location == "run_gap" && (c.JobCalls != 0 || c.DirectCalls != 0 || c.TraceCalls != 0 || c.CommitCalls != 0 || c.ArchiveCalls != 0 || c.ModuleCalls != 0) {
				t.Fatal("canceled run gap allowed later source requests")
			}
			if location == "trace_gap" && (c.JobCalls != 1 || c.DirectCalls != 1 || c.TraceCalls != 1 || c.CommitCalls != 0) {
				t.Fatal("trace-gap cancellation changed its existing stop boundary")
			}
		})
	}
}
