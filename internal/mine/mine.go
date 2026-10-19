package mine

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/omorsi45/hazard/internal/store"
)

// Options supplies transport credentials directly; it is never serialized.
type Options struct {
	HTTPClient *http.Client `json:"-"`
	APIBase    string       `json:"-"`
	Token      string       `json:"-"`
}

func validateMineRequest(req Request) (Request, error) {
	validProject := validGoReportPath(req.Project)
	switch req.Provider {
	case "github":
		validProject = validProject && len(strings.Split(req.Project, "/")) == 2
	case "gitlab":
		n, err := strconv.ParseInt(req.Project, 10, 64)
		validProject = (err == nil && n > 0) || (validProject && strings.Contains(req.Project, "/"))
	default:
		validProject = false
	}
	if !validProject || (req.Language != "" && req.Language != "go" && req.Language != "python") || req.MaxPipelines <= 0 || req.Concurrency <= 0 || strings.TrimSpace(req.DBPath) == "" || strings.TrimSpace(req.RawDir) == "" || req.Since.IsZero() || req.Until.IsZero() || !req.Since.Before(req.Until) {
		return req, errors.New("invalid mining request")
	}
	if req.JUnitGlob == "" {
		req.JUnitGlob = "**/junit*.xml"
	}
	if !artifactValidPath(req.JUnitGlob) {
		return req, errors.New("invalid report glob")
	}
	for _, part := range strings.Split(req.JUnitGlob, "/") {
		if _, err := path.Match(part, ""); err != nil {
			return req, errors.New("invalid report glob")
		}
	}
	req.Since, req.Until = req.Since.UTC(), req.Until.UTC()
	return req, nil
}

// Mine refreshes source evidence even when Resume is set.
func Mine(ctx context.Context, req Request, opts Options) (Stats, error) {
	req, err := validateMineRequest(req)
	if err != nil {
		return Stats{}, err
	}
	base := opts.APIBase
	if base == "" {
		if req.Provider == "github" {
			base = "https://api.github.com"
		} else {
			base = "https://gitlab.com/api/v4"
		}
	}
	client, err := newHTTPClient(opts.HTTPClient, base, req.Provider, opts.Token)
	if err != nil {
		return Stats{}, err
	}
	var provider acquisitionProvider
	if req.Provider == "github" {
		provider, err = newGitHubProvider(req, client)
	} else {
		provider, err = newGitLabProvider(req, client)
	}
	if err != nil {
		return Stats{}, err
	}
	return mineWithProvider(ctx, req, provider)
}

func bindMine(ctx context.Context, tx *sql.Tx, req Request) error {
	if _, err := tx.ExecContext(ctx, `INSERT INTO mining_sources(singleton,provider,project,observed_at) VALUES(1,?,?,?) ON CONFLICT(singleton) DO NOTHING`, req.Provider, req.Project, time.Now().UTC().Format(observationTimeFormat)); err != nil {
		return err
	}
	var provider, project string
	if err := tx.QueryRowContext(ctx, `SELECT provider,project FROM mining_sources WHERE singleton=1`).Scan(&provider, &project); err != nil {
		return err
	}
	if provider != req.Provider || project != req.Project {
		return errors.New("mining database is bound to a different provider/project")
	}
	return nil
}

func mineWithProvider(ctx context.Context, req Request, provider acquisitionProvider) (stats Stats, err error) {
	req, err = validateMineRequest(req)
	if err != nil {
		return stats, err
	}
	s, err := store.Open(req.DBPath)
	if err != nil {
		return stats, err
	}
	defer func() {
		if closeErr := s.Close(); closeErr != nil {
			err = errors.Join(err, closeErr)
		}
	}()
	tx, err := s.DB().BeginTx(ctx, nil)
	if err != nil {
		return stats, err
	}
	if err = bindMine(ctx, tx, req); err != nil {
		_ = tx.Rollback()
		return stats, err
	}
	if err = tx.Commit(); err != nil {
		return stats, err
	}
	baseline, err := persistenceStats(ctx, s.DB())
	if err != nil {
		return stats, err
	}
	w := mineWriter{db: s.DB(), req: req, runs: map[string]*mineRun{}, activity: Activity{GapsByReason: map[string]int{}}}
	defer func() {
		// All workers are joined before the single detached cleanup budget starts.
		var detached context.Context
		var stop context.CancelFunc
		defer func() {
			if stop != nil {
				stop()
			}
		}()
		bookkeeping := func() context.Context {
			if ctx.Err() != nil || w.canceled {
				if detached == nil {
					detached, stop = context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
				}
				return detached
			}
			return ctx
		}
		addError := func(e error) {
			if e != nil {
				if err == nil {
					err = e
				} else {
					err = errors.Join(err, e)
				}
			}
		}
		if err == nil && ctx.Err() != nil {
			err = ctx.Err()
		}
		finalized := map[string]bool{}
		for _, m := range w.finished {
			finalized[m.run.source.ID] = true
			state := m.outcome.State
			if state != "complete" && state != "capped" {
				state = "incomplete"
			}
			if state != "capped" && (m.sourceErr != nil || m.callbackErr != nil || m.run.gap || w.canceled || ctx.Err() != nil) {
				state = "incomplete"
			}
			addError(w.checkpoint(bookkeeping(), m.run, state))
		}
		for id, record := range w.runs {
			if !finalized[id] && record.state == "" {
				addError(w.checkpoint(bookkeeping(), record, "incomplete"))
			}
		}
		snapshot, snapshotErr := persistenceStats(bookkeeping(), w.db)
		addError(snapshotErr)
		stats = snapshot
		if snapshotErr == nil {
			w.activity.NewRawCases = stats.RawCases - baseline.RawCases
			w.activity.ReplayCases = stats.ReplayDuplicates - baseline.ReplayDuplicates
			w.activity.NetFactualResults = stats.FactualResults - baseline.FactualResults
			addError(w.db.QueryRowContext(bookkeeping(), `SELECT count(*) FROM acquisition_gaps WHERE resolved_at IS NULL`).Scan(&stats.ActiveGaps))
			addError(w.db.QueryRowContext(bookkeeping(), `SELECT count(*) FROM acquisition_gaps WHERE resolved_at IS NOT NULL`).Scan(&stats.ResolvedGaps))
		}
		for _, m := range w.finished {
			w.activity.InspectedRuns++
			inspected := map[int]bool{}
			for _, n := range m.outcome.Attempts {
				if n > 0 {
					inspected[n] = true
				}
			}
			w.activity.InspectedAttempts += len(inspected)
			if m.run.state == "capped" {
				w.activity.CappedRuns++
			} else if m.run.state != "complete" {
				w.activity.IncompleteRuns++
			}
		}
		stats.Activity = w.activity
		stats.RequestedSince, stats.RequestedUntil = req.Since, req.Until
	}()
	runs, gaps, sourceErr := provider.listRuns(ctx)
	for _, run := range runs {
		record := w.knownRun(run.ID)
		record.source = run
		record.know(run.RunAttempt)
	}
	for _, gap := range gaps {
		if err = w.writeGap(ctx, gap); err != nil {
			w.canceled = true
			return stats, err
		}
	}
	if len(gaps) == 0 && sourceErr == nil {
		if _, err = w.db.ExecContext(ctx, `UPDATE acquisition_gaps SET resolved_at=? WHERE provider=? AND project=? AND run_id='' AND unit_key LIKE ? AND resolved_at IS NULL AND reason!='acquisition_conflict'`, time.Now().UTC().Format(observationTimeFormat), req.Provider, req.Project, w.discoveryPrefix()+"%"); err != nil {
			w.canceled = true
			return stats, err
		}
	}
	for _, record := range w.runs {
		if err = w.checkpoint(ctx, record, "incomplete"); err != nil {
			w.canceled = true
			return stats, err
		}
	}
	if sourceErr != nil && mineFatalSource(sourceErr) {
		w.canceled = true
		return stats, sourceErr
	}
	err = w.acquire(ctx, provider, runs)
	if err == nil {
		err = sourceErr
	}
	return stats, err
}

type mineRun struct {
	source   sourceRun
	attempts map[int]bool
	gap      bool
	state    string
}

func (r *mineRun) know(attempt *int) {
	if attempt != nil && *attempt > 0 {
		r.attempts[*attempt] = true
	}
}

type mineWriter struct {
	db       *sql.DB
	req      Request
	runs     map[string]*mineRun
	finished []mineMessage
	canceled bool
	activity Activity
}

func (w *mineWriter) knownRun(id string) *mineRun {
	r := w.runs[id]
	if r == nil {
		r = &mineRun{source: sourceRun{ID: id}, attempts: map[int]bool{}}
		w.runs[id] = r
	}
	return r
}

type mineMessage struct {
	run                    *mineRun
	start                  bool
	artifact               *acquiredArtifact
	gap                    *acquisitionGap
	outcome                runOutcome
	sourceErr, callbackErr error
	done                   bool
	ack                    chan error
}

func mineFatalSource(err error) bool {
	var httpErr *httpError
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &httpErr) && (httpErr.Reason == "auth" || httpErr.Reason == "rate_limited"))
}

func (w *mineWriter) acquire(ctx context.Context, provider acquisitionProvider, runs []sourceRun) error {
	workCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	jobs := make(chan *mineRun)
	messages := make(chan mineMessage)
	send := func(m mineMessage) error {
		m.ack = make(chan error, 1)
		select {
		case messages <- m:
		case <-workCtx.Done():
			return workCtx.Err()
		}
		// Once accepted, the writer always acknowledges, even during cancellation.
		return <-m.ack
	}
	var wg sync.WaitGroup
	for i := 0; i < min(w.req.Concurrency, len(runs)); i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				var r *mineRun
				select {
				case <-workCtx.Done():
					return
				case next, ok := <-jobs:
					if !ok {
						return
					}
					r = next
				}
				if workCtx.Err() != nil {
					return
				}
				if err := send(mineMessage{run: r, start: true}); err != nil {
					return
				}
				var callbackErr error
				emit := func(m mineMessage) error {
					e := send(m)
					if callbackErr == nil {
						callbackErr = e
					}
					return e
				}
				outcome, sourceErr := provider.acquireRun(workCtx, r.source, acquisitionSink{
					Artifact: func(a acquiredArtifact) error { return emit(mineMessage{run: r, artifact: &a}) },
					Gap:      func(g acquisitionGap) error { return emit(mineMessage{run: r, gap: &g}) },
				})
				// Final partial attempt facts must reach the writer after cancellation too.
				messages <- mineMessage{run: r, outcome: outcome, sourceErr: sourceErr, callbackErr: callbackErr, done: true}
			}
		}()
	}
	dispatchDone := make(chan struct{})
	go func() {
		defer close(dispatchDone)
		defer close(jobs)
		for _, run := range runs {
			select {
			case jobs <- w.runs[run.ID]:
			case <-workCtx.Done():
				return
			}
		}
	}()
	go func() { wg.Wait(); close(messages) }()
	var firstErr error
	for m := range messages {
		var err error
		switch {
		case m.start:
			err = workCtx.Err()
		case m.artifact != nil:
			err = w.writeArtifact(workCtx, m.run, *m.artifact)
		case m.gap != nil:
			if m.gap.RunID != m.run.source.ID {
				err = errors.New("gap run identity mismatch")
			} else {
				err = w.writeGap(workCtx, *m.gap)
			}
		case m.done:
			for _, attempt := range m.outcome.Attempts {
				m.run.know(&attempt)
			}
			w.finished = append(w.finished, m)
			if m.callbackErr != nil {
				err = m.callbackErr
			} else {
				err = m.sourceErr
			}

		}
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			if !m.done || m.callbackErr != nil || mineFatalSource(err) {
				cancel()
			}
		}
		if m.ack != nil {
			m.ack <- err
		}
	}
	<-dispatchDone
	w.canceled = workCtx.Err() != nil
	if firstErr == nil {
		firstErr = ctx.Err()
	}
	return firstErr
}

func (w *mineWriter) writeArtifact(ctx context.Context, r *mineRun, a acquiredArtifact) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	obs := a.Observation
	if obs.Provider != w.req.Provider || obs.Project != w.req.Project || obs.RunID != r.source.ID {
		return errors.New("artifact source identity mismatch")
	}
	digest := fmt.Sprintf("%x", sha256.Sum256(a.Raw))
	if obs.ContentSHA256 != digest {
		return errors.New("artifact content digest mismatch")
	}
	_, rawPath, err := storeRawBlob(w.req.RawDir, a.Raw)
	if err != nil {
		return err
	}
	obs.RawBlobPath = rawPath
	if err = persistObservation(ctx, w.db, obs); err != nil {
		var conflict *AcquisitionConflictError
		if !errors.As(err, &conflict) {
			return err
		}
		gap := acquisitionGap{RunID: obs.RunID, UnitKey: obs.UnitKey, JobID: obs.JobID, ArtifactID: obs.ArtifactID, Attempt: obs.Attempt, Reason: "acquisition_conflict", Detail: conflict.Reason, SourceTime: &obs.ObservedAt}
		r.gap = true
		w.activity.GapsByReason["acquisition_conflict"]++
		r.know(obs.RunAttempt)
		return w.persistGap(ctx, gap, digest, rawPath)
	}
	r.know(obs.RunAttempt)
	w.activity.PersistedArtifacts++
	return nil
}

func (w *mineWriter) checkpoint(ctx context.Context, r *mineRun, state string) error {
	tx, err := w.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err = bindMine(ctx, tx, w.req); err != nil {
		return err
	}
	if state == "complete" {
		if _, err = tx.ExecContext(ctx, `UPDATE acquisition_gaps SET resolved_at=? WHERE provider=? AND project=? AND run_id=? AND resolved_at IS NULL AND reason!='acquisition_conflict'`, time.Now().UTC().Format(observationTimeFormat), w.req.Provider, w.req.Project, r.source.ID); err != nil {
			return err
		}
		var blocked bool
		if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM acquisition_gaps WHERE provider=? AND project=? AND resolved_at IS NULL AND (run_id=? OR (reason='acquisition_conflict' AND unit_key IN (SELECT unit_key FROM ci_artifacts WHERE provider=? AND project=? AND run_id=?))))`, w.req.Provider, w.req.Project, r.source.ID, w.req.Provider, w.req.Project, r.source.ID).Scan(&blocked); err != nil {
			return err
		}
		if blocked {
			state = "incomplete"
		}
	}
	if _, err = tx.ExecContext(ctx, `UPDATE mining_runs SET state=? WHERE provider=? AND project=? AND run_id=?`, state, w.req.Provider, w.req.Project, r.source.ID); err != nil {
		return err
	}
	attempts := []int{0}
	if len(r.attempts) > 0 {
		attempts = nil
		for n := range r.attempts {
			attempts = append(attempts, n)
		}
	}
	for _, n := range attempts {
		key := "unknown"
		var attempt, created any
		if n > 0 {
			key = strconv.Itoa(n)
			attempt = n
		}
		if r.source.CreatedAt != nil {
			created = r.source.CreatedAt.UTC().Format(observationTimeFormat)
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO mining_runs(provider,project,run_id,attempt_key,attempt,source_head_sha,branch,created_at,requested_since,requested_until,state,observed_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)
   ON CONFLICT(provider,project,run_id,attempt_key) DO UPDATE SET state=excluded.state,source_head_sha=CASE WHEN mining_runs.source_head_sha='' THEN excluded.source_head_sha ELSE mining_runs.source_head_sha END,branch=CASE WHEN mining_runs.branch='' THEN excluded.branch ELSE mining_runs.branch END,created_at=coalesce(mining_runs.created_at,excluded.created_at),requested_since=excluded.requested_since,requested_until=excluded.requested_until,observed_at=excluded.observed_at`,
			w.req.Provider, w.req.Project, r.source.ID, key, attempt, r.source.SourceHeadSHA, r.source.Branch, created, w.req.Since.Format(observationTimeFormat), w.req.Until.Format(observationTimeFormat), state, time.Now().UTC().Format(observationTimeFormat))
		if err != nil {
			return err
		}
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	r.state = state
	return nil
}

func (w *mineWriter) discoveryPrefix() string {
	data, _ := json.Marshal([]string{w.req.Provider, w.req.Project, w.req.Branch, w.req.Since.Format(time.RFC3339Nano), w.req.Until.Format(time.RFC3339Nano)})
	return fmt.Sprintf("discovery:%x:", sha256.Sum256(data))
}

func mineCode(value string, limit int) bool {
	if value == "" || len(value) > limit {
		return false
	}
	for _, r := range value {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '_' {
			return false
		}
	}
	return true
}

func (w *mineWriter) writeGap(ctx context.Context, gap acquisitionGap) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if gap.UnitKey == "" || len(gap.UnitKey) > 1024 || !mineCode(gap.Reason, 80) || (gap.Attempt != nil && *gap.Attempt < 1) {
		return errors.New("invalid acquisition gap")
	}
	w.activity.GapsByReason[gap.Reason]++
	// Only selected provider codes are durable detail, never a response or log.
	if !mineCode(gap.Detail, 160) {
		gap.Detail = ""
	}
	if gap.RunID != "" {
		r := w.knownRun(gap.RunID)
		r.gap = true
		r.know(gap.Attempt)
	}
	if gap.RunID == "" {
		gap.UnitKey = w.discoveryPrefix() + gap.UnitKey
	}
	var digest, rawPath any
	if gap.Raw != nil {
		hash, p, err := storeRawBlob(w.req.RawDir, gap.Raw)
		if err != nil {
			return err
		}
		digest, rawPath = hash, p
	}
	return w.persistGap(ctx, gap, digest, rawPath)
}

func (w *mineWriter) persistGap(ctx context.Context, gap acquisitionGap, digest, rawPath any) error {
	tx, err := w.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err = bindMine(ctx, tx, w.req); err != nil {
		return err
	}
	if err = w.saveGap(ctx, tx, gap, digest, rawPath); err != nil {
		return err
	}
	if gap.Reason == "acquisition_conflict" {
		if err = w.withdrawConflict(ctx, tx, gap); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (w *mineWriter) saveGap(ctx context.Context, tx *sql.Tx, gap acquisitionGap, digest, rawPath any) error {
	var source any
	if gap.SourceTime != nil && !gap.SourceTime.IsZero() {
		source = gap.SourceTime.UTC().Format(observationTimeFormat)
	}
	now := time.Now().UTC().Format(observationTimeFormat)
	_, err := tx.ExecContext(ctx, `INSERT INTO acquisition_gaps(provider,project,run_id,unit_key,reason,detail,job_id,artifact_id,attempt,source_time,first_acquired_at,last_acquired_at,content_sha256,raw_blob_path)
 VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT DO UPDATE SET last_acquired_at=excluded.last_acquired_at,
 job_id=coalesce(acquisition_gaps.job_id,excluded.job_id),artifact_id=coalesce(acquisition_gaps.artifact_id,excluded.artifact_id),attempt=coalesce(acquisition_gaps.attempt,excluded.attempt),source_time=coalesce(acquisition_gaps.source_time,excluded.source_time),detail=CASE WHEN acquisition_gaps.detail='' THEN excluded.detail ELSE acquisition_gaps.detail END`,
		w.req.Provider, w.req.Project, gap.RunID, gap.UnitKey, gap.Reason, gap.Detail, gap.JobID, gap.ArtifactID, gap.Attempt, source, now, now, digest, rawPath)
	return err
}

func (w *mineWriter) withdrawConflict(ctx context.Context, tx *sql.Tx, gap acquisitionGap) error {
	rows, err := tx.QueryContext(ctx, `SELECT DISTINCT result_key FROM ci_observations WHERE provider=? AND project=? AND unit_key=? AND result_key IS NOT NULL`, w.req.Provider, w.req.Project, gap.UnitKey)
	if err != nil {
		return err
	}
	var keys []resultKey
	for rows.Next() {
		var encoded string
		var key resultKey
		if err = rows.Scan(&encoded); err != nil {
			_ = rows.Close()
			return err
		}
		if err = json.Unmarshal([]byte(encoded), &key); err != nil {
			_ = rows.Close()
			return err
		}
		keys = append(keys, key)
	}
	if err = errors.Join(rows.Err(), rows.Close()); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE mining_runs SET state='incomplete' WHERE provider=? AND project=? AND (run_id=? OR run_id IN (SELECT run_id FROM ci_artifacts WHERE provider=? AND project=? AND unit_key=?))`, w.req.Provider, w.req.Project, gap.RunID, w.req.Provider, w.req.Project, gap.UnitKey); err != nil {
		return err
	}
	for _, key := range keys {
		if _, err = tx.ExecContext(ctx, "DELETE FROM test_results WHERE "+resultWhere, key.args()...); err != nil {
			return err
		}
	}
	return nil
}
