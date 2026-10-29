package predict

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
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

	mu   sync.Mutex
	last JevStats
}

var _ Predictor = (*Jev)(nil)

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
		now: opts.Now, askTier: opts.AskTier,
	}
	if j.client == nil {
		j.client = &http.Client{}
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
	return nil
}

// Name returns the predictor name.
func (j *Jev) Name() string { return "jev" }

// Version returns the configured model release.
func (j *Jev) Version() string { return j.cfg.Model }

// Predict asks the model about every candidate.
func (j *Jev) Predict(ctx context.Context, ch *change.ChangeSet, cands []impact.Candidate) ([]Prob, error) {
	return nil, errors.New("jev: Predict is not implemented yet")
}

// LastStats returns the statistics of the last call.
func (j *Jev) LastStats() JevStats {
	j.mu.Lock()
	defer j.mu.Unlock()
	s := j.last
	s.Warnings = append([]string(nil), s.Warnings...)
	return s
}
