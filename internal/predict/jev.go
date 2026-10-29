package predict

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/omorsi45/hazard/internal/change"
	"github.com/omorsi45/hazard/internal/config"
	"github.com/omorsi45/hazard/internal/impact"
)

// DefaultPricePerMTok is the observed price in USD of one million input
// tokens, used to reserve the cost of a request before it is sent.
const DefaultPricePerMTok = 0.042

const (
	// failedShardP is the probability of a test whose shard gave no usable
	// answer. A failure selects the test, so it must stay at the top of the
	// range.
	failedShardP = 1.0

	// Sources of a Prob.
	sourceJev     = "jev"
	sourceCache   = "cache"
	sourceFailed  = "jev_failed"
	sourceInvalid = "jev_invalid"
	sourceBudget  = "budget_exhausted"

	// reservationMargin is the safety factor on the estimated cost of a
	// request that is reserved before the request is sent.
	reservationMargin = 1.5
	// maxResponseBytes bounds what is read from a response.
	maxResponseBytes = 8 << 20
	// maxBackoffShift bounds the doubling of the retry backoff.
	maxBackoffShift = 16
	// snapshotDigits is the shortest date suffix of a served snapshot.
	snapshotDigits = 6
)

// JevOptions configures the Jev client.
type JevOptions struct {
	Config        config.Jev
	Privacy       config.Privacy
	RubricVersion string // "r1" or "r1s"; empty means "r1"
	APIKey        string
	HTTPClient    *http.Client
	Cache         *Cache // nil means no cache
	// BudgetUSD caps the spend of one call. A value that is not positive
	// means the cap of the configuration.
	BudgetUSD float64
	Now       func() time.Time
	// AskTier adds the optional tier question to the first request of a call.
	AskTier bool
}

// String describes the options without the API key, so that printing them
// with any verb cannot leak it.
func (o JevOptions) String() string {
	return fmt.Sprintf("JevOptions{model: %s, redact: %s, rubric: %s, key: [redacted]}", o.Config.Model, o.Privacy.Redact, o.RubricVersion)
}

// GoString is String for the %#v verb.
func (o JevOptions) GoString() string { return o.String() }

// JevStats describes the last call of Predict.
type JevStats struct {
	// Requests counts the HTTP requests sent, retries included.
	Requests int
	// CacheHits counts the candidates answered from the cache.
	CacheHits int
	// FailedShards counts the shards whose candidates were set to 1.0 because
	// no usable answer came back.
	FailedShards int
	// BudgetSkipped counts the shards that were not sent because the budget
	// could not cover them.
	BudgetSkipped int
	// CostUSD is the sum of usage.cost over the answered shards.
	CostUSD float64
	// ServedModel is the dated snapshot the first answered shard reported.
	ServedModel string
	// Tier is the answer to the tier question, empty when it was not asked or
	// not answered.
	Tier     string
	Warnings []string
}

// Jev is the client of the Decisions API.
type Jev struct {
	cfg     config.Jev
	privacy config.Privacy
	rubric  rubric
	key     string
	client  *http.Client
	cache   *Cache
	budget  float64
	now     func() time.Time
	askTier bool
	sleep   func(ctx context.Context, d time.Duration) error

	mu   sync.Mutex
	last JevStats
}

var _ Predictor = (*Jev)(nil)

// String names the client without its API key.
func (j *Jev) String() string { return "Jev{model: " + j.cfg.Model + ", key: [redacted]}" }

// GoString is String for the %#v verb.
func (j *Jev) GoString() string { return j.String() }

// NewJev validates opts and returns a client. It refuses when the external
// predictor is disabled, when the redaction level cannot be sent (hash and
// anything unknown) and when there is no API key.
func NewJev(opts JevOptions) (*Jev, error) {
	p := opts.Privacy
	if !p.AllowExternalPredictor {
		return nil, errors.New("privacy.allow_external_predictor is false: the external predictor is disabled")
	}
	if p.Redact != "none" && p.Redact != "structure" {
		return nil, fmt.Errorf("privacy.redact is %q: the external predictor needs \"none\" or \"structure\"", p.Redact)
	}
	if strings.TrimSpace(opts.APIKey) == "" {
		return nil, errors.New("the Jev API key is empty")
	}
	if err := validateJevConfig(opts.Config); err != nil {
		return nil, err
	}
	if opts.AskTier && opts.Config.QuestionsPerRequest < 2 {
		return nil, errors.New("predictor.jev.questions_per_request must be at least 2 when the tier question is asked")
	}
	version := opts.RubricVersion
	if version == "" {
		version = RubricR1
	}
	r, err := rubricFor(version)
	if err != nil {
		return nil, err
	}
	budget := opts.BudgetUSD
	if budget <= 0 {
		budget = opts.Config.BudgetUSDPerRun
	}
	if math.IsNaN(budget) || math.IsInf(budget, 0) || budget < 0 {
		return nil, fmt.Errorf("the budget %v is not a valid amount", budget)
	}
	j := &Jev{
		cfg: opts.Config, privacy: p, rubric: r, key: opts.APIKey,
		client: opts.HTTPClient, cache: opts.Cache, budget: budget,
		now: opts.Now, askTier: opts.AskTier, sleep: sleepContext,
	}
	if j.client == nil {
		// The key travels in a header: never follow a redirect with it.
		j.client = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		}}
	}
	if j.now == nil {
		j.now = time.Now
	}
	return j, nil
}

func validateJevConfig(c config.Jev) error {
	switch {
	case c.Endpoint == "":
		return errors.New("predictor.jev.endpoint is empty")
	case c.Model == "":
		return errors.New("predictor.jev.model is empty")
	case c.QuestionsPerRequest < 1:
		return errors.New("predictor.jev.questions_per_request must be positive")
	case c.MaxStateTokens < 1:
		return errors.New("predictor.jev.max_state_tokens must be positive")
	case c.MaxRequestTokens < 1:
		return errors.New("predictor.jev.max_request_tokens must be positive")
	case c.Concurrency < 1:
		return errors.New("predictor.jev.concurrency must be positive")
	case c.TimeoutMS < 1:
		return errors.New("predictor.jev.timeout_ms must be positive")
	case c.Retries < 0:
		return errors.New("predictor.jev.retries must not be negative")
	case c.RetryBackoffMS < 0:
		return errors.New("predictor.jev.retry_backoff_ms must not be negative")
	}
	u, err := url.Parse(c.Endpoint)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return errors.New("predictor.jev.endpoint is not an http or https URL")
	}
	return nil
}

// sleepContext waits for d or until ctx is done.
func sleepContext(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// Name returns the predictor name.
func (j *Jev) Name() string { return "jev" }

// Version returns the configured model release.
func (j *Jev) Version() string { return j.cfg.Model }

// LastStats returns the statistics of the last call.
func (j *Jev) LastStats() JevStats {
	j.mu.Lock()
	defer j.mu.Unlock()
	s := j.last
	s.Warnings = append([]string(nil), s.Warnings...)
	return s
}

// warn records a warning, with the key scrubbed out of it.
func (j *Jev) warn(stats *JevStats, format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	stats.Warnings = append(stats.Warnings, strings.ReplaceAll(msg, j.key, "[redacted]"))
}

// Predict asks the model about every candidate. The answer holds one Prob per
// candidate in the order of cands. A shard that cannot be answered, a shard
// that the budget cannot cover and an answer that is missing or malformed give
// P=1.0 for the tests concerned: the failure selects the test. A served model
// of another release is an error, ErrModelDrift, and so is a cancelled
// context.
func (j *Jev) Predict(ctx context.Context, ch *change.ChangeSet, cands []impact.Candidate) ([]Prob, error) {
	var stats JevStats
	probs, err := j.predict(ctx, ch, cands, &stats)
	sort.Strings(stats.Warnings)
	j.mu.Lock()
	j.last = stats
	j.mu.Unlock()
	return probs, err
}

func (j *Jev) predict(ctx context.Context, ch *change.ChangeSet, cands []impact.Candidate, stats *JevStats) ([]Prob, error) {
	if ch == nil {
		return nil, errors.New("jev: no change set")
	}
	probs := make([]Prob, len(cands))
	if len(cands) == 0 {
		return probs, nil
	}
	b, err := j.prepare(ch, cands)
	if err != nil {
		return nil, err
	}
	pending := make([]*descriptor, 0, len(b.descs))
	cacheModel := ""
	cacheReadable := j.cache != nil
	for _, d := range b.descs {
		if cacheReadable {
			hit, found, err := j.cache.get(ctx, b.cacheKey(d))
			if err != nil {
				j.warn(stats, "the judgment cache cannot be read and every test is asked: %v", err)
				cacheReadable = false
			} else if found {
				probs[d.index] = Prob{TestID: d.testID, P: hit.p, Source: sourceCache, Shard: -1, Truncated: d.truncated}
				stats.CacheHits++
				if cacheModel == "" {
					cacheModel = hit.model
				}
				continue
			}
		}
		pending = append(pending, d)
	}

	shards, err := b.shards(pending)
	if err != nil {
		j.warn(stats, "no request was sent: %v", err)
		stats.FailedShards++
		for _, d := range pending {
			probs[d.index] = failedProb(d, sourceFailed, -1)
		}
		return j.finish(cands, probs)
	}

	var rows []judgmentRow
	var drift, canceled error
	for _, out := range j.dispatch(ctx, shards) {
		stats.Requests += out.requests
		switch {
		case out.drift != nil:
			if drift == nil {
				drift = out.drift
				stats.ServedModel = out.served
			}
		case out.ctxErr != nil:
			if canceled == nil {
				canceled = out.ctxErr
			}
		case out.skipped:
			stats.BudgetSkipped++
			for _, d := range out.shard.members {
				probs[d.index] = failedProb(d, sourceBudget, -1)
			}
		case out.failure != "":
			stats.FailedShards++
			j.warn(stats, "shard %d: %s", out.shard.index, out.failure)
			for _, d := range out.shard.members {
				probs[d.index] = failedProb(d, sourceFailed, -1)
			}
		default:
			rows = j.answered(stats, probs, out, b, rows)
		}
	}
	if stats.ServedModel == "" {
		stats.ServedModel = cacheModel
	}
	if stats.BudgetSkipped > 0 {
		j.warn(stats, "the budget of $%.6f is reached: %d shard(s) were not sent and their tests are selected", j.budget, stats.BudgetSkipped)
	}
	if drift != nil {
		return nil, drift
	}
	if canceled != nil {
		return nil, fmt.Errorf("jev: %w", canceled)
	}
	if cacheReadable && len(rows) > 0 {
		at := j.now()
		for _, row := range rows {
			if err := j.cache.put(ctx, row, at); err != nil {
				j.warn(stats, "judgments could not be cached: %v", err)
				break
			}
		}
	}
	return j.finish(cands, probs)
}

// finish checks the answer before it leaves.
func (j *Jev) finish(cands []impact.Candidate, probs []Prob) ([]Prob, error) {
	if err := ValidateProbs(cands, probs); err != nil {
		return nil, fmt.Errorf("jev: %w", err)
	}
	return probs, nil
}

func failedProb(d *descriptor, source string, shard int) Prob {
	return Prob{TestID: d.testID, P: failedShardP, Source: source, Shard: shard, Truncated: d.truncated}
}

// answered records the answers of a shard that came back and returns rows,
// extended by the judgments worth caching.
func (j *Jev) answered(stats *JevStats, probs []Prob, out *outcome, b *batch, rows []judgmentRow) []judgmentRow {
	s := out.shard
	stats.CostUSD += out.cost
	switch {
	case stats.ServedModel == "":
		stats.ServedModel = out.served
	case stats.ServedModel != out.served:
		j.warn(stats, "the served model changed during the call: %s and %s", stats.ServedModel, out.served)
	}
	if out.costNote != "" {
		j.warn(stats, "shard %d: %s", s.index, out.costNote)
	}
	if s.tier {
		stats.Tier = out.tier
		if out.tierNote != "" {
			j.warn(stats, "shard %d: %s", s.index, out.tierNote)
		}
	}
	share := out.cost / float64(len(s.members))
	latency := int(out.latency.Milliseconds())
	invalid := 0
	for i, d := range s.members {
		p := Prob{TestID: d.testID, Source: sourceJev, LatencyMS: latency, CostUSD: share, Shard: s.index, Truncated: d.truncated}
		if a := out.answers[i]; a.ok {
			p.P = a.p
			rows = append(rows, judgmentRow{
				key: b.cacheKey(d), testID: d.testID, model: out.served, rubric: j.rubric.version,
				p: a.p, latencyMS: latency, costUSD: share,
			})
		} else {
			p.P, p.Source = failedShardP, sourceInvalid
			invalid++
		}
		probs[d.index] = p
	}
	if invalid > 0 {
		j.warn(stats, "shard %d: %d of %d answers are missing or invalid and those tests are selected", s.index, invalid, len(s.members))
	}
	return rows
}

// answer is the probability read from one noul answer.
type answer struct {
	p  float64
	ok bool
}

// outcome is what became of one shard.
type outcome struct {
	shard    *shard
	reserved float64 // reservation held while the shard was in flight
	charged  float64 // amount that replaces the reservation

	requests int
	skipped  bool   // not sent: the budget could not cover it
	failure  string // no usable answer, and why
	drift    error  // the served model is of another release
	ctxErr   error  // the caller's context ended

	served   string
	cost     float64 // usage.cost, or the reservation when it was unusable
	costNote string
	answers  []answer // parallel to shard.members
	tier     string
	tierNote string
	latency  time.Duration
}

// dispatch sends the shards, at most Concurrency at a time. A shard is sent
// only after its estimated cost is reserved; once the budget cannot cover a
// shard, no further shard is sent. A model drift cancels the shards in flight.
func (j *Jev) dispatch(ctx context.Context, shards []*shard) []*outcome {
	outcomes := make([]*outcome, len(shards))
	budget := &budgetTracker{limit: j.budget}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	slots := make(chan struct{}, j.cfg.Concurrency)
	var wg sync.WaitGroup
	for i, s := range shards {
		out := &outcome{shard: s, reserved: reservation(s)}
		outcomes[i] = out
		select {
		case slots <- struct{}{}:
		case <-runCtx.Done():
			out.ctxErr = runCtx.Err()
			continue
		}
		if !budget.reserve(out.reserved) {
			<-slots
			out.skipped = true
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-slots }()
			j.send(runCtx, out)
			budget.settle(out.reserved, out.charged)
			if out.drift != nil {
				cancel()
			}
		}()
	}
	wg.Wait()
	return outcomes
}

// reservation is the cost held back for a shard before it is sent: the
// estimated input tokens at the observed price, with a margin.
func reservation(s *shard) float64 {
	return float64(estimateTokens(len(s.body))) / 1e6 * DefaultPricePerMTok * reservationMargin
}

// budgetTracker enforces the per-call cap on the spend.
type budgetTracker struct {
	mu        sync.Mutex
	limit     float64
	spent     float64
	reserved  float64
	exhausted bool
}

// reserve holds amount back and reports whether the cap allows it.
func (b *budgetTracker) reserve(amount float64) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.exhausted {
		return false
	}
	if b.spent+b.reserved+amount > b.limit {
		b.exhausted = true
		return false
	}
	b.reserved += amount
	return true
}

// settle replaces a reservation by what was charged.
func (b *budgetTracker) settle(reserved, charged float64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.reserved = math.Max(0, b.reserved-reserved)
	b.spent += charged
}

// send posts one shard, retrying what may succeed on a second try, and fills
// out. The same bytes go out on every attempt.
func (j *Jev) send(ctx context.Context, out *outcome) {
	start := j.now()
	defer func() { out.latency = j.now().Sub(start) }()
	attempts := j.cfg.Retries + 1
	for attempt := 0; attempt < attempts; attempt++ {
		if attempt > 0 {
			if err := j.sleep(ctx, j.backoff(attempt-1)); err != nil {
				out.ctxErr = err
				return
			}
		}
		status, body, err := j.post(ctx, out.shard.body)
		out.requests++
		switch {
		case err != nil:
			if ctx.Err() != nil {
				out.ctxErr = ctx.Err()
				return
			}
			out.failure = "request failed: " + j.scrub(err.Error())
		case status == http.StatusOK:
			out.failure = ""
			j.parse(out, body)
			return
		case status == http.StatusTooManyRequests || status >= http.StatusInternalServerError:
			out.failure = fmt.Sprintf("status %d", status)
		default:
			out.failure = fmt.Sprintf("status %d, not retried", status)
			return
		}
	}
	out.failure = fmt.Sprintf("%s after %d attempts", out.failure, attempts)
}

func (j *Jev) scrub(s string) string { return strings.ReplaceAll(s, j.key, "[redacted]") }

// backoff is retry_backoff_ms * 2^attempt, attempt counted from 0.
func (j *Jev) backoff(attempt int) time.Duration {
	return time.Duration(j.cfg.RetryBackoffMS) * time.Millisecond << min(attempt, maxBackoffShift)
}

// post makes one attempt, bounded by timeout_ms.
func (j *Jev) post(ctx context.Context, body []byte) (int, []byte, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Duration(j.cfg.TimeoutMS)*time.Millisecond)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, j.cfg.Endpoint, bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+j.key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := j.client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return 0, nil, err
	}
	if len(data) > maxResponseBytes {
		return 0, nil, errors.New("the response is too large")
	}
	return resp.StatusCode, data, nil
}

// parse reads a 200 response into out.
func (j *Jev) parse(out *outcome, body []byte) {
	var resp struct {
		Model   string                     `json:"model"`
		Answers map[string]json.RawMessage `json:"answers"`
		Usage   json.RawMessage            `json:"usage"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		// What was billed is unknown: charge the reservation.
		out.failure = "the response is not valid JSON"
		out.charged = out.reserved
		return
	}
	out.served = resp.Model
	out.cost, out.costNote = costOf(resp.Usage, out.reserved)
	out.charged = out.cost
	if !releaseMatches(j.cfg.Model, resp.Model) {
		out.drift = fmt.Errorf("%w: configured %q, served %q", ErrModelDrift, j.cfg.Model, resp.Model)
		return
	}
	s := out.shard
	out.answers = make([]answer, len(s.keys))
	for i, key := range s.keys {
		out.answers[i] = parseNoul(resp.Answers[key])
	}
	if s.tier {
		out.tier, out.tierNote = parseTier(resp.Answers[tierKey])
	}
}

// costOf reads usage.cost. A missing or invalid value counts as the
// reservation.
func costOf(raw json.RawMessage, reservation float64) (float64, string) {
	var u struct {
		Cost *float64 `json:"cost"`
	}
	if len(raw) == 0 || json.Unmarshal(raw, &u) != nil || u.Cost == nil {
		return reservation, "usage.cost is missing or invalid: the reservation is counted as spent"
	}
	if c := *u.Cost; !math.IsNaN(c) && !math.IsInf(c, 0) && c >= 0 {
		return c, ""
	}
	return reservation, "usage.cost is missing or invalid: the reservation is counted as spent"
}

// parseNoul reads a noul answer. Anything but a number in [0, 1] is no
// answer.
func parseNoul(raw json.RawMessage) answer {
	var a struct {
		Type string   `json:"type"`
		Noul *float64 `json:"noul"`
	}
	if len(raw) == 0 || json.Unmarshal(raw, &a) != nil || a.Noul == nil {
		return answer{}
	}
	if a.Type != "" && a.Type != "noul" {
		return answer{}
	}
	if v := *a.Noul; !math.IsNaN(v) && !math.IsInf(v, 0) && v >= 0 && v <= 1 {
		return answer{p: v, ok: true}
	}
	return answer{}
}

// parseTier reads the answer to the tier question.
func parseTier(raw json.RawMessage) (string, string) {
	var a struct {
		Type   string `json:"type"`
		Choice string `json:"choice"`
	}
	if len(raw) == 0 {
		return "", "the tier question was not answered"
	}
	if json.Unmarshal(raw, &a) != nil || (a.Type != "" && a.Type != "choice") {
		return "", "the tier answer is malformed"
	}
	if _, known := tierCriteria[a.Choice]; known {
		return a.Choice, ""
	}
	return "", "the tier answer is not a known tier"
}

// releaseMatches reports whether served is the configured release itself or a
// dated snapshot of it: the release, a dash and a run of digits.
func releaseMatches(release, served string) bool {
	if served == release {
		return true
	}
	date, ok := strings.CutPrefix(served, release+"-")
	if !ok || len(date) < snapshotDigits {
		return false
	}
	for _, r := range date {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
