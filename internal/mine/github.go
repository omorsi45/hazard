package mine

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
)

type githubProvider struct {
	request Request
	client  *httpClient
}

func newGitHubProvider(request Request, client *httpClient) (*githubProvider, error) {
	if client == nil || client.provider != "github" || request.Provider != "github" || !validGoReportPath(request.Project) || len(strings.Split(request.Project, "/")) != 2 || request.Since.IsZero() || request.Until.IsZero() || request.Since.After(request.Until) || request.MaxPipelines <= 0 || (request.Language != "" && request.Language != "go" && request.Language != "python") {
		return nil, &httpError{Reason: "invalid_request"}
	}
	request.Since, request.Until = request.Since.UTC(), request.Until.UTC()
	return &githubProvider{request: request, client: client}, nil
}

func (p *githubProvider) endpoint(endpoint string, query url.Values) string {
	u := *p.client.origin
	project := strings.Split(p.request.Project, "/")
	u.Path = strings.TrimSuffix(u.Path, "/") + "/repos/" + url.PathEscape(project[0]) + "/" + url.PathEscape(project[1]) + "/" + endpoint
	u.RawPath = ""
	u.RawQuery = query.Encode()
	return u.String()
}

func (p *githubProvider) nextPage(current string, headers http.Header) (string, error) {
	next, err := p.client.nextPage(current, headers)
	if err != nil || next == "" {
		return next, err
	}
	before, _ := url.Parse(current)
	after, _ := url.Parse(next)
	a, b := before.Query(), after.Query()
	a.Del("page")
	b.Del("page")
	n, parseErr := strconv.Atoi(after.Query().Get("page"))
	if parseErr != nil || n <= 0 || a.Encode() != b.Encode() {
		return "", &httpError{Reason: "invalid_pagination"}
	}
	return next, nil
}

type githubRun struct {
	ID            int64      `json:"id"`
	SourceHeadSHA string     `json:"head_sha"`
	Branch        string     `json:"head_branch"`
	Status        string     `json:"status"`
	CreatedAt     *time.Time `json:"created_at"`
	RunAttempt    *int       `json:"run_attempt"`
	Repository    *struct {
		FullName string `json:"full_name"`
	} `json:"repository"`
}

func (p *githubProvider) listRuns(ctx context.Context) ([]sourceRun, []acquisitionGap, error) {
	var runs []sourceRun
	var gaps []acquisitionGap
	seen := make(map[int64]int)
	stopped := false
	failure := func(reason string, err error) error {
		gaps = append(gaps, acquisitionGap{UnitKey: "runs", Reason: reason})
		return err
	}
	capGap := func(detail string) {
		gaps = append(gaps, acquisitionGap{UnitKey: "runs", Reason: "discovery_capped", Detail: detail})
	}
	var scan func(time.Time, time.Time) error
	scan = func(since, until time.Time) error {
		if !since.Before(until) || stopped {
			return nil
		}
		query := url.Values{"branch": {p.request.Branch}, "created": {since.Format(time.RFC3339Nano) + ".." + until.Format(time.RFC3339Nano)}, "per_page": {"100"}, "page": {"1"}}
		current := p.endpoint("actions/runs", query)
		pages := make(map[string]bool)
		capped, count, total := false, 0, -1
		for pageNumber := 0; current != ""; pageNumber++ {
			if pages[current] {
				return failure("discovery_pagination", &httpError{Reason: "invalid_pagination"})
			}
			if pageNumber >= 10 {
				capGap("page_limit")
				return nil
			}
			pages[current] = true
			var page struct {
				Total *int        `json:"total_count"`
				Runs  []githubRun `json:"workflow_runs"`
			}
			headers, err := p.client.getJSON(ctx, current, &page)
			if err != nil {
				return failure("discovery_unavailable", err)
			}
			if page.Total == nil || *page.Total < 0 || len(page.Runs) > 100 || (total >= 0 && total != *page.Total) {
				return failure("discovery_metadata", &httpError{Reason: "invalid_response"})
			}
			total = *page.Total
			count += len(page.Runs)
			for _, r := range page.Runs {
				if r.ID <= 0 || r.CreatedAt == nil || r.CreatedAt.IsZero() || (r.SourceHeadSHA != "" && !hexadecimal(r.SourceHeadSHA, 40)) {
					return failure("discovery_metadata", &httpError{Reason: "invalid_response"})
				}
				if r.Repository != nil && r.Repository.FullName != "" && r.Repository.FullName != p.request.Project {
					return failure("discovery_source", &httpError{Reason: "invalid_response"})
				}
				if r.CreatedAt.Before(since) || !r.CreatedAt.Before(until) || r.Branch != p.request.Branch {
					continue
				}
				if index, exists := seen[r.ID]; exists {
					prior := &runs[index]
					if prior.SourceHeadSHA != r.SourceHeadSHA || !prior.CreatedAt.Equal(*r.CreatedAt) || prior.Branch != r.Branch {
						return failure("discovery_source", &httpError{Reason: "invalid_response"})
					}
					if r.RunAttempt != nil && *r.RunAttempt > 0 && (prior.RunAttempt == nil || *r.RunAttempt > *prior.RunAttempt) {
						prior.RunAttempt = r.RunAttempt
					}
					continue
				}
				if len(runs) >= p.request.MaxPipelines {
					capGap("request_limit")
					stopped = true
					return nil
				}
				attempt := r.RunAttempt
				if attempt != nil && *attempt <= 0 {
					attempt = nil
				}
				seen[r.ID] = len(runs)
				runs = append(runs, sourceRun{ID: strconv.FormatInt(r.ID, 10), SourceHeadSHA: r.SourceHeadSHA, Branch: r.Branch, Status: r.Status, CreatedAt: r.CreatedAt, RunAttempt: attempt})
			}
			next, err := p.nextPage(current, headers)
			if err != nil {
				return failure("discovery_pagination", err)
			}
			if total >= 1000 {
				capped = true
				break
			}
			if next == "" && count < total {
				return failure("discovery_metadata", &httpError{Reason: "invalid_response"})
			}
			if next != "" && len(runs) >= p.request.MaxPipelines {
				capGap("request_limit")
				stopped = true
				return nil
			}
			current = next
		}
		if capped {
			if until.Sub(since) <= time.Second {
				capGap("one_second_window")
				return nil
			}
			middle := since.Add(until.Sub(since) / 2).Truncate(time.Second)
			if !middle.After(since) {
				middle = since.Add(time.Second)
			}
			if !middle.Before(until) {
				capGap("one_second_window")
				return nil
			}
			if len(runs) >= p.request.MaxPipelines {
				capGap("request_limit")
				stopped = true
				return nil
			}
			if err := scan(since, middle); err != nil {
				return err
			}
			return scan(middle, until)
		}
		return nil
	}
	err := scan(p.request.Since, p.request.Until)
	return runs, gaps, err
}

type githubJob struct {
	ID         int64  `json:"id"`
	RunID      int64  `json:"run_id"`
	RunAttempt *int   `json:"run_attempt"`
	Status     string `json:"status"`
	Attempt    int
}

type githubArtifact struct {
	ID          int64      `json:"id"`
	Size        *int64     `json:"size_in_bytes"`
	Digest      string     `json:"digest"`
	Expired     bool       `json:"expired"`
	CreatedAt   *time.Time `json:"created_at"`
	WorkflowRun *struct {
		ID      int64  `json:"id"`
		HeadSHA string `json:"head_sha"`
		Branch  string `json:"head_branch"`
	} `json:"workflow_run"`
}

func githubGap(sink acquisitionSink, outcome *runOutcome, gap acquisitionGap) error {
	if gap.Reason == "acquisition_capped" {
		outcome.State = "capped"
	} else if outcome.State != "capped" {
		outcome.State = "incomplete"
	}
	return sink.Gap(gap)
}

func githubFatal(err error) bool {
	var acquisition *httpError
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &acquisition) && (acquisition.Reason == "auth" || acquisition.Reason == "rate_limited"))
}

func (p *githubProvider) listJobs(ctx context.Context, run sourceRun, sink acquisitionSink, outcome *runOutcome) ([]githubJob, error) {
	var jobs []githubJob
	runID, err := strconv.ParseInt(run.ID, 10, 64)
	if err != nil || runID <= 0 {
		return nil, &httpError{Reason: "invalid_request"}
	}
	gap := func(attempt int, reason, detail string) error {
		ordinal := attempt
		g := acquisitionGap{RunID: run.ID, UnitKey: "jobs", SourceTime: run.CreatedAt, Reason: reason, Detail: detail}
		if attempt > 0 {
			g.Attempt = &ordinal
			g.UnitKey = "attempt:" + strconv.Itoa(attempt)
		}
		return githubGap(sink, outcome, g)
	}
	if run.RunAttempt == nil || *run.RunAttempt <= 0 {
		return nil, gap(0, "attempt_unavailable", "")
	}
	limit := *run.RunAttempt
	if limit > 100 {
		if err := gap(0, "acquisition_capped", "attempt_limit"); err != nil {
			return jobs, err
		}
		limit = 100
	}
	type execution struct {
		ID      int64
		Attempt int
	}
	seen := make(map[execution]bool)
	inspected := 0
	for attempt := 1; attempt <= limit; attempt++ {
		current := p.endpoint("actions/runs/"+run.ID+"/attempts/"+strconv.Itoa(attempt)+"/jobs", url.Values{"per_page": {"100"}, "page": {"1"}})
		pages := make(map[string]bool)
		count, total := 0, -1
		for pageNumber := 0; current != ""; pageNumber++ {
			if pages[current] {
				if err := gap(attempt, "jobs_pagination", ""); err != nil {
					return jobs, err
				}
				break
			}
			if pageNumber >= 10 {
				return jobs, gap(attempt, "acquisition_capped", "job_page_limit")
			}
			pages[current] = true
			var page struct {
				Total *int        `json:"total_count"`
				Jobs  []githubJob `json:"jobs"`
			}
			headers, fetchErr := p.client.getJSON(ctx, current, &page)
			if fetchErr != nil {
				if err := gap(attempt, "jobs_unavailable", ""); err != nil {
					return jobs, err
				}
				if githubFatal(fetchErr) {
					return jobs, fetchErr
				}
				break
			}
			if page.Total == nil || *page.Total < 0 || len(page.Jobs) > 100 || (total >= 0 && total != *page.Total) {
				if err := gap(attempt, "job_metadata", ""); err != nil {
					return jobs, err
				}
				break
			}
			if pageNumber == 0 {
				outcome.Attempts = append(outcome.Attempts, attempt)
			}
			total = *page.Total
			count += len(page.Jobs)
			for _, job := range page.Jobs {
				inspected++
				if job.ID <= 0 || job.RunID != runID || (job.RunAttempt != nil && *job.RunAttempt != attempt) {
					if err := gap(attempt, "job_metadata", ""); err != nil {
						return jobs, err
					}
				} else if key := (execution{ID: job.ID, Attempt: attempt}); !seen[key] {
					job.Attempt = attempt
					seen[key] = true
					jobs = append(jobs, job)
				}
				if inspected >= 1000 {
					return jobs, gap(attempt, "acquisition_capped", "job_limit")
				}
			}
			next, nextErr := p.nextPage(current, headers)
			if nextErr != nil {
				if err := gap(attempt, "jobs_pagination", ""); err != nil {
					return jobs, err
				}
				break
			}
			if next == "" && count < total {
				if err := gap(attempt, "jobs_unavailable", ""); err != nil {
					return jobs, err
				}
			}
			current = next
		}
	}
	return jobs, nil
}

func (p *githubProvider) listArtifacts(ctx context.Context, run sourceRun, sink acquisitionSink, outcome *runOutcome) ([]githubArtifact, error) {
	var artifacts []githubArtifact
	current := p.endpoint("actions/runs/"+run.ID+"/artifacts", url.Values{"per_page": {"100"}, "page": {"1"}})
	pages := make(map[string]bool)
	seen := make(map[int64]bool)
	count, total := 0, -1
	gap := func(reason, detail string) error {
		return githubGap(sink, outcome, acquisitionGap{RunID: run.ID, UnitKey: "artifacts", SourceTime: run.CreatedAt, Reason: reason, Detail: detail})
	}
	for pageNumber := 0; current != ""; pageNumber++ {
		if pages[current] {
			return artifacts, gap("artifacts_pagination", "")
		}
		if pageNumber >= 10 {
			return artifacts, gap("acquisition_capped", "artifact_page_limit")
		}
		pages[current] = true
		var page struct {
			Total     *int             `json:"total_count"`
			Artifacts []githubArtifact `json:"artifacts"`
		}
		headers, fetchErr := p.client.getJSON(ctx, current, &page)
		if fetchErr != nil {
			if err := gap("artifacts_unavailable", ""); err != nil {
				return artifacts, err
			}
			if githubFatal(fetchErr) {
				return artifacts, fetchErr
			}
			return artifacts, nil
		}
		if page.Total == nil || *page.Total < 0 || len(page.Artifacts) > 100 || (total >= 0 && total != *page.Total) {
			return artifacts, gap("artifact_metadata", "")
		}
		total = *page.Total
		for _, artifact := range page.Artifacts {
			count++
			if artifact.ID <= 0 {
				if err := gap("artifact_metadata", ""); err != nil {
					return artifacts, err
				}
			} else if !seen[artifact.ID] {
				seen[artifact.ID] = true
				artifacts = append(artifacts, artifact)
			}
			if count >= 1000 {
				return artifacts, gap("acquisition_capped", "artifact_limit")
			}
		}
		next, nextErr := p.nextPage(current, headers)
		if nextErr != nil {
			return artifacts, gap("artifacts_pagination", "")
		}
		if next == "" && count < total {
			return artifacts, gap("artifacts_unavailable", "")
		}
		current = next
	}
	return artifacts, nil
}

func (p *githubProvider) acquireRun(ctx context.Context, run sourceRun, sink acquisitionSink) (runOutcome, error) {
	outcome := runOutcome{State: "incomplete"}
	runID, parseErr := strconv.ParseInt(run.ID, 10, 64)
	if sink.Artifact == nil || sink.Gap == nil || parseErr != nil || runID <= 0 {
		return outcome, &httpError{Reason: "invalid_request"}
	}
	if err := ctx.Err(); err != nil {
		return outcome, err
	}
	outcome.State = "complete"
	if run.Status != "completed" {
		if err := githubGap(sink, &outcome, acquisitionGap{RunID: run.ID, UnitKey: "run", SourceTime: run.CreatedAt, Reason: "run_incomplete"}); err != nil {
			return outcome, err
		}
	}
	jobs, err := p.listJobs(ctx, run, sink, &outcome)
	if err != nil {
		return outcome, err
	}
	artifacts, err := p.listArtifacts(ctx, run, sink, &outcome)
	if err != nil {
		return outcome, err
	}
	var logs []githubLog
	logsLoaded := false
	var logErr error
	modules := make(map[string]struct {
		module string
		err    error
	})
	commits := make(map[string]struct {
		metadata *CommitMetadata
		err      error
	})
	for _, artifact := range artifacts {
		if err := ctx.Err(); err != nil {
			outcome.State = "incomplete"
			return outcome, err
		}
		artifactID := strconv.FormatInt(artifact.ID, 10)
		gap := acquisitionGap{RunID: run.ID, UnitKey: "artifact:" + artifactID, ArtifactID: &artifactID, SourceTime: artifact.CreatedAt}
		reject := func(reason string, raw []byte) error {
			g := gap
			g.Reason = reason
			g.Raw = raw
			return githubGap(sink, &outcome, g)
		}
		if artifact.Expired {
			if err := reject("artifact_expired", nil); err != nil {
				return outcome, err
			}
			continue
		}
		if artifact.WorkflowRun != nil && (artifact.WorkflowRun.ID != runID || (artifact.WorkflowRun.HeadSHA != "" && artifact.WorkflowRun.HeadSHA != run.SourceHeadSHA) || (artifact.WorkflowRun.Branch != "" && artifact.WorkflowRun.Branch != run.Branch)) {
			if err := reject("artifact_source", nil); err != nil {
				return outcome, err
			}
			continue
		}
		if artifact.Size != nil && (*artifact.Size < 0 || *artifact.Size > maxArtifactInputBytes) {
			g := gap
			g.Reason = "acquisition_capped"
			g.Detail = "artifact_size_limit"
			if err := githubGap(sink, &outcome, g); err != nil {
				return outcome, err
			}
			continue
		}
		raw, fetchErr := p.client.getBytes(ctx, p.endpoint("actions/artifacts/"+artifactID+"/zip", nil), maxArtifactInputBytes)
		if fetchErr != nil {
			if err := reject("artifact_unavailable", nil); err != nil {
				return outcome, err
			}
			if githubFatal(fetchErr) {
				return outcome, fetchErr
			}
			continue
		}
		digest := sha256.Sum256(raw)
		hash := hex.EncodeToString(digest[:])
		if artifact.Digest != "" && artifact.Digest != "sha256:"+hash {
			if err := reject("artifact_digest_mismatch", raw); err != nil {
				return outcome, err
			}
			continue
		}
		if artifact.Size != nil && *artifact.Size != int64(len(raw)) {
			if err := reject("artifact_size_mismatch", raw); err != nil {
				return outcome, err
			}
			continue
		}
		if artifact.CreatedAt == nil || artifact.CreatedAt.IsZero() {
			if err := reject("source_time_missing", raw); err != nil {
				return outcome, err
			}
			continue
		}
		entries, decodeErr := DecodeArtifact(bytes.NewReader(raw), "zip", p.request.JUnitGlob)
		if decodeErr != nil {
			if err := reject("artifact_parse", raw); err != nil {
				return outcome, err
			}
			continue
		}
		observation := ProviderObservation{Provider: "github", Project: p.request.Project, RunID: run.ID, UnitKey: gap.UnitKey, ArtifactID: &artifactID, ContentSHA256: hash, SourceHeadSHA: run.SourceHeadSHA, Branch: run.Branch, ObservedAt: artifact.CreatedAt.UTC(), RunCreatedAt: run.CreatedAt, RunAttempt: run.RunAttempt, RequestedSince: &p.request.Since, RequestedUntil: &p.request.Until, State: "complete"}
		if !logsLoaded && len(entries) > 0 {
			logsLoaded = true
			logs, logErr = p.loadLogs(ctx, run, jobs, sink, &outcome)
			if logErr != nil && !githubFatal(logErr) {
				return outcome, logErr
			}
		}
		if err := ctx.Err(); err != nil {
			outcome.State = "incomplete"
			return outcome, err
		}
		var verified githubProvenance
		var producer githubJob
		producers := 0
		apiSize, artifactRunID := int64(0), int64(0)
		if artifact.Size != nil {
			apiSize = *artifact.Size
		}
		if artifact.WorkflowRun != nil {
			artifactRunID = artifact.WorkflowRun.ID
		}
		for _, log := range logs {
			context := githubProvenanceContext{Repository: p.request.Project, RunID: runID, JobRunID: log.Job.RunID, JobID: log.Job.ID, LogJobID: log.Job.ID, ArtifactID: artifact.ID, ArtifactRunID: artifactRunID, Attempt: log.Job.Attempt, JobAttempt: log.Job.Attempt, ArchiveSHA256: hash, ArtifactDigest: artifact.Digest, ArchiveSize: int64(len(raw)), ArtifactSize: apiSize, LogComplete: log.Job.Status == "completed"}
			evidence, verifyErr := verifyGitHubProvenance(log.Raw, context)
			if verifyErr == nil {
				producers++
				verified = evidence
				producer = log.Job
			}
		}
		if producers > 1 {
			g := gap
			g.Reason = "acquisition_conflict"
			g.Detail = "multiple_verified_producers"
			if err := githubGap(sink, &outcome, g); err != nil {
				return outcome, err
			}
		}
		var fatalErr error
		if logErr != nil {
			fatalErr = logErr
		}
		if producers == 1 {
			jobID := strconv.FormatInt(producer.ID, 10)
			attempt := producer.Attempt
			sha := verified.TestedSHA
			revisionEvidence := "checkout_ref=" + verified.CheckoutRef + ";log_sha256=" + verified.LogSHA256
			executionEvidence := verified.ProducerEvidence + ";log_sha256=" + verified.LogSHA256
			observation.JobID = &jobID
			observation.Attempt = &attempt
			observation.TestedSHA = &sha
			observation.TestedRevisionEvidence = &revisionEvidence
			observation.ExecutionEvidence = &executionEvidence
			if fatalErr == nil {
				metadata, known := commits[sha]
				if !known {
					metadata.metadata, metadata.err = p.commitMetadata(ctx, sha)
					commits[sha] = metadata
				}
				observation.Commit = metadata.metadata
				if metadata.err != nil {
					if err := githubGap(sink, &outcome, acquisitionGap{RunID: run.ID, UnitKey: "commit:" + sha, Reason: "commit_unavailable"}); err != nil {
						return outcome, err
					}
					if githubFatal(metadata.err) {
						fatalErr = metadata.err
					}
				}
			}
		} else {
			observation.ExclusionReason = "provenance_unverified"
			if producers > 1 {
				observation.ExclusionReason = "ambiguous_producer"
			}
		}
		if len(entries) == 0 {
			observation.State = "incomplete"
			observation.ExclusionReason = "no_matching_report"
			if err := reject("no_matching_report", nil); err != nil {
				return outcome, err
			}
		}
		for _, entry := range entries {
			result := ObservationEntry{Name: entry.Name, SHA256: entry.SHA256}
			language := ""
			if producers == 1 && verified.ReportPath == entry.Name {
				language = verified.Language
			}
			for _, rawCase := range entry.Cases {
				mapped := mineIdentity(p.request.Language, language, "", rawCase)
				if mapped.IdentityError == "go_package_unavailable" && producers == 1 && fatalErr == nil {
					module, known := modules[verified.TestedSHA]
					if !known {
						module.module, module.err = p.readTestedModule(ctx, verified.TestedSHA)
						modules[verified.TestedSHA] = module
					}
					if module.err != nil {
						if err := githubGap(sink, &outcome, acquisitionGap{RunID: run.ID, UnitKey: "module:" + verified.TestedSHA, Reason: "module_unavailable"}); err != nil {
							return outcome, err
						}
						if githubFatal(module.err) {
							fatalErr = module.err
						}
					} else {
						mapped = mineIdentity(p.request.Language, language, module.module, rawCase)
					}
				}
				if mapped.IdentityError != "" {
					mapped.ExclusionReason = mapped.IdentityError
				}
				result.Cases = append(result.Cases, mapped)
				observation.SuiteIncomplete = observation.SuiteIncomplete || rawCase.SuiteIncomplete
				observation.DiagnosticsTruncated = observation.DiagnosticsTruncated || rawCase.DiagnosticsTruncated
			}
			observation.Entries = append(observation.Entries, result)
		}
		if observation.SuiteIncomplete || observation.DiagnosticsTruncated {
			observation.State = "incomplete"
			if err := reject("report_incomplete", nil); err != nil {
				return outcome, err
			}
		}
		if err := ctx.Err(); err != nil {
			outcome.State = "incomplete"
			return outcome, err
		}
		if err := sink.Artifact(acquiredArtifact{Observation: observation, Raw: raw}); err != nil {
			outcome.State = "incomplete"
			return outcome, err
		}
		if err := ctx.Err(); err != nil {
			outcome.State = "incomplete"
			return outcome, err
		}
		if fatalErr != nil {
			outcome.State = "incomplete"
			return outcome, fatalErr
		}
	}
	return outcome, nil
}

var _ acquisitionProvider = (*githubProvider)(nil)

type githubLog struct {
	Job githubJob
	Raw []byte
}

func (p *githubProvider) loadLogs(ctx context.Context, run sourceRun, jobs []githubJob, sink acquisitionSink, outcome *runOutcome) ([]githubLog, error) {
	var logs []githubLog
	remaining := int64(64 << 20)
	for _, job := range jobs {
		jobID := strconv.FormatInt(job.ID, 10)
		attempt := job.Attempt
		gap := acquisitionGap{RunID: run.ID, UnitKey: "job:" + jobID, JobID: &jobID, Attempt: &attempt, SourceTime: run.CreatedAt}
		if job.Status != "completed" {
			gap.Reason = "job_incomplete"
			if err := githubGap(sink, outcome, gap); err != nil {
				return logs, err
			}
			continue
		}
		raw, fetchErr := p.client.getBytesBudgeted(ctx, p.endpoint("actions/jobs/"+jobID+"/logs", nil), maxGitHubLogBytes, &remaining)
		if fetchErr != nil {
			gap.Reason = "log_unavailable"
			var failure *httpError
			if errors.As(fetchErr, &failure) && failure.Reason == "budget_exhausted" {
				gap.Reason = "acquisition_capped"
				gap.Detail = "run_log_budget"
				return logs, githubGap(sink, outcome, gap)
			}
			if err := githubGap(sink, outcome, gap); err != nil {
				return logs, err
			}
			if githubFatal(fetchErr) {
				return logs, fetchErr
			}
			continue
		}
		logs = append(logs, githubLog{Job: job, Raw: raw})
	}
	return logs, nil
}

func (p *githubProvider) readTestedModule(ctx context.Context, sha string) (string, error) {
	var response struct {
		Type     string `json:"type"`
		Path     string `json:"path"`
		Encoding string `json:"encoding"`
		Content  string `json:"content"`
	}
	_, err := p.client.getJSON(ctx, p.endpoint("contents/go.mod", url.Values{"ref": {sha}}), &response)
	if err != nil {
		return "", err
	}
	if response.Type != "file" || response.Path != "go.mod" || response.Encoding != "base64" {
		return "", &httpError{Reason: "invalid_response"}
	}
	reader := base64.NewDecoder(base64.StdEncoding, strings.NewReader(response.Content))
	raw, err := io.ReadAll(io.LimitReader(reader, maxGoModuleBytes+1))
	if err != nil || len(raw) > maxGoModuleBytes {
		return "", &httpError{Reason: "invalid_response"}
	}
	return readGoModule(raw)
}

func (p *githubProvider) commitMetadata(ctx context.Context, sha string) (*CommitMetadata, error) {
	metadata := &CommitMetadata{}
	current := p.endpoint("commits/"+sha, url.Values{"per_page": {"100"}, "page": {"1"}})
	pages := make(map[string]bool)
	files := make(map[string]bool)
	complete, filesComplete := true, true
	invalid := &httpError{Reason: "metadata_incomplete"}
	sameInt := func(a, b *int) bool { return a == nil && b == nil || a != nil && b != nil && *a == *b }
	for pageNumber := 0; current != ""; pageNumber++ {
		if pages[current] || pageNumber >= 30 {
			return metadata, invalid
		}
		pages[current] = true
		var response struct {
			SHA    string `json:"sha"`
			Commit *struct {
				Author *struct {
					Date *time.Time `json:"date"`
				} `json:"author"`
			} `json:"commit"`
			Parents *[]struct {
				SHA string `json:"sha"`
			} `json:"parents"`
			Stats *struct {
				Additions *int `json:"additions"`
				Deletions *int `json:"deletions"`
			} `json:"stats"`
			Files *[]struct {
				Filename string `json:"filename"`
			} `json:"files"`
		}
		headers, err := p.client.getJSON(ctx, current, &response)
		if err != nil {
			return metadata, err
		}
		if response.SHA != sha {
			return metadata, invalid
		}
		candidate := CommitMetadata{}
		if response.Commit != nil && response.Commit.Author != nil && response.Commit.Author.Date != nil && !response.Commit.Author.Date.IsZero() {
			authored := response.Commit.Author.Date.UTC()
			candidate.AuthoredAt = &authored
		}
		if response.Parents != nil {
			candidate.ParentSHAs = make([]string, 0, len(*response.Parents))
			for _, parent := range *response.Parents {
				if !hexadecimal(parent.SHA, 40) {
					candidate.ParentSHAs = nil
					break
				}
				candidate.ParentSHAs = append(candidate.ParentSHAs, parent.SHA)
			}
		}
		if response.Stats != nil {
			if response.Stats.Additions != nil && *response.Stats.Additions >= 0 {
				candidate.LinesAdded = response.Stats.Additions
			}
			if response.Stats.Deletions != nil && *response.Stats.Deletions >= 0 {
				candidate.LinesRemoved = response.Stats.Deletions
			}
		}
		if candidate.AuthoredAt == nil || candidate.ParentSHAs == nil || candidate.LinesAdded == nil || candidate.LinesRemoved == nil {
			complete = false
		}
		if pageNumber == 0 {
			*metadata = candidate
		} else {
			if (metadata.AuthoredAt == nil) != (candidate.AuthoredAt == nil) || (metadata.AuthoredAt != nil && !metadata.AuthoredAt.Equal(*candidate.AuthoredAt)) {
				metadata.AuthoredAt = nil
				complete, filesComplete = false, false
			}
			if (metadata.ParentSHAs == nil) != (candidate.ParentSHAs == nil) || !slices.Equal(metadata.ParentSHAs, candidate.ParentSHAs) {
				metadata.ParentSHAs = nil
				complete, filesComplete = false, false
			}
			if !sameInt(metadata.LinesAdded, candidate.LinesAdded) {
				metadata.LinesAdded = nil
				complete, filesComplete = false, false
			}
			if !sameInt(metadata.LinesRemoved, candidate.LinesRemoved) {
				metadata.LinesRemoved = nil
				complete, filesComplete = false, false
			}
		}
		if response.Files == nil || len(*response.Files) > 100 {
			return metadata, invalid
		}
		for _, file := range *response.Files {
			if file.Filename == "" || files[file.Filename] {
				return metadata, invalid
			}
			files[file.Filename] = true
			if len(files) >= 3000 {
				return metadata, invalid
			}
		}
		next, err := p.nextPage(current, headers)
		if err != nil {
			return metadata, err
		}
		current = next
	}
	if filesComplete {
		count := len(files)
		metadata.FilesChanged = &count
	}
	if !complete {
		return metadata, invalid
	}
	return metadata, nil
}
