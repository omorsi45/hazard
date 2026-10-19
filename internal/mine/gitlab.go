package mine

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

type gitlabProvider struct {
	request      Request
	client       *httpClient
	once         sync.Once
	prepareErr   error
	projectID    int64
	versionKnown bool
	major, minor int
}

func newGitLabProvider(request Request, client *httpClient) (*gitlabProvider, error) {
	id, idErr := strconv.ParseInt(request.Project, 10, 64)
	if client == nil || client.provider != "gitlab" || request.Provider != "gitlab" || !validGoReportPath(request.Project) || (idErr == nil && id <= 0) || (idErr != nil && !strings.Contains(request.Project, "/")) || request.Since.IsZero() || request.Until.IsZero() || request.Since.After(request.Until) || request.MaxPipelines <= 0 || (request.Language != "" && request.Language != "go" && request.Language != "python") {
		return nil, &httpError{Reason: "invalid_request"}
	}
	request.Since, request.Until = request.Since.UTC(), request.Until.UTC()
	return &gitlabProvider{request: request, client: client}, nil
}

func (p *gitlabProvider) endpoint(path string, query url.Values) string {
	result := strings.TrimSuffix(p.client.origin.String(), "/") + "/" + path
	if len(query) != 0 {
		result += "?" + query.Encode()
	}
	return result
}

func (p *gitlabProvider) projectEndpoint(path string, query url.Values) string {
	return p.endpoint("projects/"+strconv.FormatInt(p.projectID, 10)+"/"+path, query)
}

func gitlabFatal(err error) bool {
	var failure *httpError
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &failure) && (failure.Reason == "auth" || failure.Reason == "rate_limited"))
}

var gitlabServerVersion = regexp.MustCompile(`^([0-9]+)\.([0-9]+)(\.[0-9]+)?([-+][A-Za-z0-9.-]+)?$`)

func (p *gitlabProvider) prepare(ctx context.Context) error {
	p.once.Do(func() {
		var project struct {
			ID   int64
			Path string `json:"path_with_namespace"`
		}
		_, err := p.client.getJSON(ctx, p.endpoint("projects/"+url.PathEscape(p.request.Project), nil), &project)
		if err != nil {
			p.prepareErr = err
			return
		}
		configured, numeric := strconv.ParseInt(p.request.Project, 10, 64)
		if project.ID <= 0 || (numeric == nil && project.ID != configured) || (numeric != nil && project.Path != p.request.Project) {
			p.prepareErr = &httpError{Reason: "invalid_response"}
			return
		}
		p.projectID = project.ID
		var metadata struct{ Version string }
		_, err = p.client.getJSON(ctx, p.endpoint("version", nil), &metadata)
		if err != nil {
			var failure *httpError
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &failure) && failure.Reason == "rate_limited") {
				p.prepareErr = err
			}
			return
		}
		parts := gitlabServerVersion.FindStringSubmatch(metadata.Version)
		if len(parts) == 0 {
			return
		}
		major, majorErr := strconv.Atoi(parts[1])
		minor, minorErr := strconv.Atoi(parts[2])
		if majorErr != nil || minorErr != nil {
			return
		}
		p.major, p.minor, p.versionKnown = major, minor, true
	})
	return p.prepareErr
}

type gitlabPipeline struct {
	ID               int64
	ProjectID        *int64 `json:"project_id"`
	SHA, Ref, Status string
	CreatedAt        *time.Time `json:"created_at"`
}

type gitlabDownstream struct {
	ID        int64
	ProjectID *int64 `json:"project_id"`
}

func (p *gitlabProvider) nextPage(current string, headers http.Header) (string, error) {
	next, err := p.client.nextPage(current, headers)
	if err != nil || next == "" {
		return next, err
	}
	before, _ := url.Parse(current)
	after, _ := url.Parse(next)
	a, b := before.Query(), after.Query()
	a.Del("page")
	b.Del("page")
	page, parseErr := strconv.Atoi(after.Query().Get("page"))
	if parseErr != nil || page <= 0 || a.Encode() != b.Encode() {
		return "", &httpError{Reason: "invalid_pagination"}
	}
	return next, nil
}

func (p *gitlabProvider) legacyBefore(minor int) bool {
	return p.versionKnown && (p.major < 19 || p.major == 19 && p.minor < minor)
}

func (p *gitlabProvider) pipelineRun(row gitlabPipeline) (sourceRun, bool, error) {
	if row.ID <= 0 || row.CreatedAt == nil || row.CreatedAt.IsZero() || (row.SHA != "" && !hexadecimal(row.SHA, 40)) || (row.ProjectID != nil && *row.ProjectID != p.projectID) {
		return sourceRun{}, false, &httpError{Reason: "invalid_response"}
	}
	if row.Ref != p.request.Branch || row.CreatedAt.Before(p.request.Since) || !row.CreatedAt.Before(p.request.Until) {
		return sourceRun{}, false, nil
	}
	return sourceRun{ID: strconv.FormatInt(row.ID, 10), SourceHeadSHA: row.SHA, Branch: row.Ref, Status: row.Status, CreatedAt: row.CreatedAt}, true, nil
}

func (p *gitlabProvider) downstream(ctx context.Context, run sourceRun) ([]gitlabDownstream, []acquisitionGap, error) {
	var links []gitlabDownstream
	gap := func(reason, detail string) ([]gitlabDownstream, []acquisitionGap, error) {
		return links, []acquisitionGap{{RunID: run.ID, UnitKey: "pipeline:" + run.ID + ":triggers", SourceTime: run.CreatedAt, Reason: reason, Detail: detail}}, nil
	}
	query := url.Values{"per_page": {"100"}, "page": {"1"}}
	current := p.projectEndpoint("pipelines/"+run.ID+"/trigger_jobs", query)
	pages := map[string]bool{}
	count := 0
	for pageNumber := 0; current != ""; pageNumber++ {
		if pages[current] {
			return gap("triggers_pagination", "")
		}
		if pageNumber >= 10 {
			return gap("discovery_capped", "trigger_page_limit")
		}
		pages[current] = true
		var rows []struct {
			ID         int64
			Downstream *gitlabDownstream `json:"downstream_pipeline"`
		}
		headers, err := p.client.getJSON(ctx, current, &rows)
		var failure *httpError
		if err != nil && pageNumber == 0 && p.legacyBefore(2) && errors.As(err, &failure) && (failure.StatusCode == 404 || failure.StatusCode == 405) {
			current = p.projectEndpoint("pipelines/"+run.ID+"/bridges", query)
			pages[current] = true
			headers, err = p.client.getJSON(ctx, current, &rows)
		}
		if err != nil {
			detail := ""
			if !p.versionKnown {
				detail = "capability_unknown"
			}
			_, gaps, _ := gap("triggers_unavailable", detail)
			if gitlabFatal(err) {
				return links, gaps, err
			}
			return links, gaps, nil
		}
		if rows == nil || len(rows) > 100 {
			return gap("triggers_metadata", "")
		}
		for _, row := range rows {
			count++
			if row.ID <= 0 {
				return gap("triggers_metadata", "")
			}
			if row.Downstream != nil {
				links = append(links, *row.Downstream)
			}
			if count >= 1000 {
				return gap("discovery_capped", "trigger_limit")
			}
		}
		current, err = p.nextPage(current, headers)
		if err != nil {
			return gap("triggers_pagination", "")
		}
	}
	return links, nil, nil
}

func (p *gitlabProvider) listRuns(ctx context.Context) ([]sourceRun, []acquisitionGap, error) {
	if err := p.prepare(ctx); err != nil {
		return nil, []acquisitionGap{{UnitKey: "project", Reason: "discovery_unavailable"}}, err
	}
	var runs []sourceRun
	var gaps []acquisitionGap
	seen := map[int64]bool{}
	inspected := 0
	stopped := false
	claim := func(id int64) bool {
		if id <= 0 {
			gaps = append(gaps, acquisitionGap{UnitKey: "pipelines", Reason: "discovery_metadata"})
			return false
		}
		if seen[id] {
			return false
		}
		if inspected >= p.request.MaxPipelines {
			gaps = append(gaps, acquisitionGap{UnitKey: "pipelines", Reason: "discovery_capped", Detail: "request_limit"})
			stopped = true
			return false
		}
		seen[id] = true
		inspected++
		return true
	}
	query := url.Values{"ref": {p.request.Branch}, "created_after": {p.request.Since.Format(time.RFC3339Nano)}, "created_before": {p.request.Until.Format(time.RFC3339Nano)}, "per_page": {"100"}, "page": {"1"}}
	current := p.projectEndpoint("pipelines", query)
	pages := map[string]bool{}
	for pageNumber := 0; current != ""; pageNumber++ {
		if pages[current] {
			gaps = append(gaps, acquisitionGap{UnitKey: "pipelines", Reason: "discovery_pagination"})
			break
		}
		if pageNumber >= 10 {
			gaps = append(gaps, acquisitionGap{UnitKey: "pipelines", Reason: "discovery_capped", Detail: "pipeline_page_limit"})
			break
		}
		pages[current] = true
		var rows []gitlabPipeline
		headers, err := p.client.getJSON(ctx, current, &rows)
		if err != nil {
			gaps = append(gaps, acquisitionGap{UnitKey: "pipelines", Reason: "discovery_unavailable"})
			if gitlabFatal(err) {
				return runs, gaps, err
			}
			break
		}
		if rows == nil || len(rows) > 100 {
			gaps = append(gaps, acquisitionGap{UnitKey: "pipelines", Reason: "discovery_metadata"})
			break
		}
		for _, row := range rows {
			if !claim(row.ID) {
				if stopped {
					return runs, gaps, nil
				}
				continue
			}
			run, inScope, err := p.pipelineRun(row)
			if err != nil {
				gaps = append(gaps, acquisitionGap{UnitKey: "pipeline:" + strconv.FormatInt(row.ID, 10), Reason: "discovery_metadata"})
				continue
			}
			if inScope {
				runs = append(runs, run)
			}
		}
		current, err = p.nextPage(current, headers)
		if err != nil {
			gaps = append(gaps, acquisitionGap{UnitKey: "pipelines", Reason: "discovery_pagination"})
			break
		}
	}
	for index := 0; index < len(runs); index++ {
		run := runs[index]
		links, triggerGaps, err := p.downstream(ctx, run)
		gaps = append(gaps, triggerGaps...)
		if err != nil {
			return runs, gaps, err
		}
		for _, link := range links {
			if link.ProjectID != nil && *link.ProjectID != p.projectID {
				gaps = append(gaps, acquisitionGap{RunID: run.ID, UnitKey: "pipeline:" + run.ID + ":downstream", Reason: "downstream_outside_scope", Detail: "cross_project"})
				continue
			}
			if !claim(link.ID) {
				if stopped {
					return runs, gaps, nil
				}
				continue
			}
			var child gitlabPipeline
			_, err := p.client.getJSON(ctx, p.projectEndpoint("pipelines/"+strconv.FormatInt(link.ID, 10), nil), &child)
			if err != nil {
				gaps = append(gaps, acquisitionGap{RunID: run.ID, UnitKey: "pipeline:" + strconv.FormatInt(link.ID, 10), Reason: "downstream_unavailable"})
				if gitlabFatal(err) {
					return runs, gaps, err
				}
				continue
			}
			if child.ID != link.ID {
				gaps = append(gaps, acquisitionGap{RunID: run.ID, UnitKey: "pipeline:" + strconv.FormatInt(link.ID, 10), Reason: "downstream_metadata"})
				continue
			}
			nested, inScope, validationErr := p.pipelineRun(child)
			if validationErr != nil {
				gaps = append(gaps, acquisitionGap{RunID: run.ID, UnitKey: "pipeline:" + strconv.FormatInt(link.ID, 10), Reason: "downstream_metadata"})
				continue
			}
			if !inScope {
				gaps = append(gaps, acquisitionGap{RunID: run.ID, UnitKey: "pipeline:" + strconv.FormatInt(link.ID, 10), Reason: "downstream_outside_scope", Detail: "ref_or_window"})
				continue
			}
			runs = append(runs, nested)
		}
	}
	return runs, gaps, nil
}

type gitlabJob struct {
	ID            int64
	Status, Ref   string
	FailureReason string     `json:"failure_reason"`
	FinishedAt    *time.Time `json:"finished_at"`
	ErasedAt      *time.Time `json:"erased_at"`
	Project       *struct{ ID int64 }
	Pipeline      *struct {
		ID        int64
		ProjectID *int64 `json:"project_id"`
		SHA, Ref  string
	}
	Commit        *struct{ ID string }
	ArtifactsFile *struct {
		Filename string
		Size     int64
	} `json:"artifacts_file"`
}

func gitlabGap(sink acquisitionSink, outcome *runOutcome, gap acquisitionGap) error {
	if gap.Reason == "acquisition_capped" {
		outcome.State = "capped"
	} else if outcome.State != "capped" {
		outcome.State = "incomplete"
	}
	return sink.Gap(gap)
}

func gitlabTerminal(status string) bool {
	return status == "success" || status == "failed" || status == "canceled" || status == "skipped"
}

func (p *gitlabProvider) listJobs(ctx context.Context, run sourceRun, sink acquisitionSink, outcome *runOutcome) ([]gitlabJob, error) {
	var jobs []gitlabJob
	seen := map[int64]bool{}
	pages := map[string]bool{}
	current := p.projectEndpoint("pipelines/"+run.ID+"/jobs", url.Values{"include_retried": {"true"}, "per_page": {"100"}, "page": {"1"}})
	fail := func(reason, detail string) ([]gitlabJob, error) {
		return jobs, gitlabGap(sink, outcome, acquisitionGap{RunID: run.ID, UnitKey: "pipeline:" + run.ID + ":jobs", SourceTime: run.CreatedAt, Reason: reason, Detail: detail})
	}
	count := 0
	for pageNumber := 0; current != ""; pageNumber++ {
		if pages[current] {
			return fail("jobs_pagination", "")
		}
		if pageNumber >= 10 {
			return fail("acquisition_capped", "job_page_limit")
		}
		pages[current] = true
		var rows []gitlabJob
		headers, sourceErr := p.client.getJSON(ctx, current, &rows)
		if sourceErr != nil {
			_, callbackErr := fail("jobs_unavailable", "")
			if callbackErr != nil {
				return jobs, callbackErr
			}
			if gitlabFatal(sourceErr) {
				return jobs, sourceErr
			}
			return jobs, nil
		}
		if rows == nil || len(rows) > 100 {
			return fail("jobs_metadata", "")
		}
		for _, row := range rows {
			count++
			if row.ID <= 0 {
				return fail("jobs_metadata", "")
			}
			if !seen[row.ID] {
				jobs = append(jobs, row)
				seen[row.ID] = true
			}
			if count >= 1000 {
				return fail("acquisition_capped", "job_limit")
			}
		}
		var err error
		current, err = p.nextPage(current, headers)
		if err != nil {
			return fail("jobs_pagination", "")
		}
	}
	return jobs, nil
}

func (p *gitlabProvider) jobMatches(job gitlabJob, run sourceRun, runID int64) bool {
	return (job.Project == nil || job.Project.ID == p.projectID) &&
		(job.Pipeline == nil || job.Pipeline.ID == runID &&
			(job.Pipeline.ProjectID == nil || *job.Pipeline.ProjectID == p.projectID) &&
			(job.Pipeline.SHA == "" || job.Pipeline.SHA == run.SourceHeadSHA) &&
			(job.Pipeline.Ref == "" || job.Pipeline.Ref == run.Branch)) &&
		(job.Commit == nil || job.Commit.ID == run.SourceHeadSHA) &&
		(job.Ref == "" || job.Ref == run.Branch)
}

func (p *gitlabProvider) acquireJob(ctx context.Context, run sourceRun, runID int64, job gitlabJob, sink acquisitionSink, outcome *runOutcome, remaining *int64) error {
	jobID := strconv.FormatInt(job.ID, 10)
	gap := acquisitionGap{RunID: run.ID, UnitKey: "job:" + jobID + ":junit", JobID: &jobID, SourceTime: job.FinishedAt}
	reject := func(reason, detail string, raw []byte) error {
		g := gap
		g.Reason, g.Detail, g.Raw = reason, detail, raw
		return gitlabGap(sink, outcome, g)
	}
	if !p.jobMatches(job, run, runID) {
		return reject("job_source", "", nil)
	}
	if !gitlabTerminal(job.Status) {
		return reject("job_incomplete", "", nil)
	}
	if job.ErasedAt != nil {
		return reject("artifact_erased", "", nil)
	}
	base := "jobs/" + jobID + "/"
	raw, sourceErr := p.client.getBytes(ctx, p.projectEndpoint(base+"artifacts", url.Values{"file_type": {"junit"}}), maxArtifactInputBytes)
	archive := false
	var failure *httpError
	if sourceErr != nil && errors.As(sourceErr, &failure) &&
		(p.legacyBefore(4) && (failure.StatusCode == 400 || failure.StatusCode == 404 || failure.StatusCode == 405) ||
			failure.StatusCode == 404 && job.ArtifactsFile != nil && job.ArtifactsFile.Filename != "" && job.ArtifactsFile.Size > 0) {
		archive = true
		gap.UnitKey = "job:" + jobID + ":archive"
		raw, sourceErr = p.client.getBytes(ctx, p.projectEndpoint(base+"artifacts", nil), maxArtifactInputBytes)
	}
	if sourceErr != nil {
		reason, detail := "artifact_unavailable", ""
		if errors.As(sourceErr, &failure) && failure.Reason == "too_large" {
			reason, detail = "acquisition_capped", "artifact_byte_limit"
		}
		if !p.versionKnown && !archive && detail == "" {
			detail = "capability_unknown"
		}
		if err := reject(reason, detail, nil); err != nil {
			return err
		}
		if gitlabFatal(sourceErr) {
			return sourceErr
		}
		return nil
	}
	if job.FinishedAt == nil || job.FinishedAt.IsZero() {
		return reject("source_time_missing", "", raw)
	}
	entries, err := DecodeArtifact(bytes.NewReader(raw), "", p.request.JUnitGlob)
	if err != nil {
		return reject("artifact_parse", "", raw)
	}
	if len(entries) == 0 {
		return reject("no_matching_report", "", raw)
	}
	digest := sha256.Sum256(raw)
	hash := hex.EncodeToString(digest[:])
	execution := "project_id=" + strconv.FormatInt(p.projectID, 10) + ";pipeline_id=" + run.ID + ";job_id=" + jobID + ";observed_at=job_finished_at"
	observation := ProviderObservation{Provider: "gitlab", Project: p.request.Project, RunID: run.ID, UnitKey: gap.UnitKey, JobID: &jobID, ContentSHA256: hash, SourceHeadSHA: run.SourceHeadSHA, Branch: run.Branch, ObservedAt: job.FinishedAt.UTC(), RunCreatedAt: run.CreatedAt, RequestedSince: &p.request.Since, RequestedUntil: &p.request.Until, State: "complete", ExecutionEvidence: &execution, ExclusionReason: "provenance_unverified"}
	language := ""
	var fatalErr error
	// Only a single XML/gzip job report has the accepted direct-source mapping.
	if !archive && len(entries) == 1 && !bytes.HasPrefix(raw, []byte("PK")) {
		trace, traceErr := p.client.getBytesBudgeted(ctx, p.projectEndpoint(base+"trace", nil), maxGitHubLogBytes, remaining)
		if traceErr != nil {
			reason, detail := "trace_unavailable", ""
			if errors.As(traceErr, &failure) && (failure.Reason == "budget_exhausted" || failure.Reason == "too_large") {
				reason, detail = "acquisition_capped", "pipeline_trace_budget"
				if failure.Reason == "too_large" {
					detail = "job_trace_byte_limit"
				}
			}
			if err := reject(reason, detail, nil); err != nil {
				return err
			}
			if gitlabFatal(traceErr) {
				fatalErr = traceErr
			}
		} else {
			verified, verifyErr := verifyGitLabProvenance(trace, gitlabProvenanceContext{ProjectID: p.projectID, PipelineID: runID, JobID: job.ID, JobProjectID: p.projectID, JobPipelineID: runID, TraceJobID: job.ID, JobStatus: job.Status, FailureReason: job.FailureReason, ReportSHA256: hash, ReportValidated: true, TraceComplete: true})
			if verifyErr == nil {
				sha := verified.TestedSHA
				revision := "measured_head;log_sha256=" + verified.LogSHA256
				execution += ";" + verified.ProducerEvidence + ";report_path=" + verified.ReportPath + ";log_sha256=" + verified.LogSHA256
				observation.TestedSHA, observation.TestedRevisionEvidence = &sha, &revision
				observation.ExclusionReason = ""
				language = verified.Language
			}
		}
	}
	if observation.TestedSHA != nil && fatalErr == nil {
		metadata, sourceErr := p.commitMetadata(ctx, *observation.TestedSHA)
		observation.Commit = metadata
		if sourceErr != nil {
			if err := gitlabGap(sink, outcome, acquisitionGap{RunID: run.ID, UnitKey: "commit:" + *observation.TestedSHA, Reason: "commit_unavailable"}); err != nil {
				return err
			}
			if gitlabFatal(sourceErr) {
				fatalErr = sourceErr
			}
		} else {
			// The commit endpoint cannot certify an exact changed-file count.
			observation.ExclusionReason = "commit_file_count_unverified"
			observation.ExclusionDetail = "commit_endpoint_file_count_unknown"
		}
	}

	for _, entry := range entries {
		result := ObservationEntry{Name: entry.Name, SHA256: entry.SHA256}
		for _, rawCase := range entry.Cases {
			mapped := mineIdentity(p.request.Language, language, "", rawCase)
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
		if err := reject("report_incomplete", "", nil); err != nil {
			return err
		}
	}
	if err := ctx.Err(); err != nil {
		outcome.State = "incomplete"
		return err
	}
	if err := sink.Artifact(acquiredArtifact{Observation: observation, Raw: raw}); err != nil {
		outcome.State = "incomplete"
		return err
	}
	if err := ctx.Err(); err != nil {
		outcome.State = "incomplete"
		return err
	}
	return fatalErr
}

func (p *gitlabProvider) acquireRun(ctx context.Context, run sourceRun, sink acquisitionSink) (runOutcome, error) {
	outcome := runOutcome{State: "incomplete"}
	runID, err := strconv.ParseInt(run.ID, 10, 64)
	if sink.Artifact == nil || sink.Gap == nil || err != nil || runID <= 0 {
		return outcome, &httpError{Reason: "invalid_request"}
	}
	if err := ctx.Err(); err != nil {
		return outcome, err
	}
	if err := p.prepare(ctx); err != nil {
		return outcome, err
	}
	outcome.State = "complete"
	if !gitlabTerminal(run.Status) {
		if err := gitlabGap(sink, &outcome, acquisitionGap{RunID: run.ID, UnitKey: "pipeline:" + run.ID, SourceTime: run.CreatedAt, Reason: "run_incomplete"}); err != nil {
			return outcome, err
		}
	}
	jobs, err := p.listJobs(ctx, run, sink, &outcome)
	if err != nil {
		return outcome, err
	}
	remaining := int64(64 << 20)
	for _, job := range jobs {
		if err := ctx.Err(); err != nil {
			outcome.State = "incomplete"
			return outcome, err
		}
		if err := p.acquireJob(ctx, run, runID, job, sink, &outcome, &remaining); err != nil {
			return outcome, err
		}
	}
	if err := ctx.Err(); err != nil {
		outcome.State = "incomplete"
		return outcome, err
	}
	return outcome, nil
}

var _ acquisitionProvider = (*gitlabProvider)(nil)

func (p *gitlabProvider) commitMetadata(ctx context.Context, sha string) (*CommitMetadata, error) {
	var response struct {
		ID         string
		AuthoredAt *time.Time `json:"authored_date"`
		Parents    *[]string  `json:"parent_ids"`
		Stats      *struct{ Additions, Deletions *int }
	}
	_, err := p.client.getJSON(ctx, p.projectEndpoint("repository/commits/"+sha, nil), &response)
	if err != nil {
		return nil, err
	}
	invalid := &httpError{Reason: "metadata_incomplete"}
	if response.ID != sha {
		return nil, invalid
	}
	metadata := &CommitMetadata{}
	if response.AuthoredAt != nil && !response.AuthoredAt.IsZero() {
		authored := response.AuthoredAt.UTC()
		metadata.AuthoredAt = &authored
	}
	if response.Parents != nil {
		metadata.ParentSHAs = make([]string, 0, len(*response.Parents))
		for _, parent := range *response.Parents {
			if !hexadecimal(parent, 40) {
				metadata.ParentSHAs = nil
				break
			}
			metadata.ParentSHAs = append(metadata.ParentSHAs, parent)
		}
	}
	if response.Stats != nil {
		if response.Stats.Additions != nil && *response.Stats.Additions >= 0 {
			metadata.LinesAdded = response.Stats.Additions
		}
		if response.Stats.Deletions != nil && *response.Stats.Deletions >= 0 {
			metadata.LinesRemoved = response.Stats.Deletions
		}
	}
	if metadata.AuthoredAt == nil || metadata.ParentSHAs == nil || metadata.LinesAdded == nil || metadata.LinesRemoved == nil {
		return metadata, invalid
	}
	return metadata, nil
}
