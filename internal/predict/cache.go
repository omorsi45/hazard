package predict

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/omorsi45/hazard/internal/impact"
)

// Cache is the judgment cache over the judgments table. A judgment is keyed by
// everything that determines the question: the model release, the rubric
// version, the feature version, the change block as sent and the descriptor of
// the test as sent. There is no expiry and no eviction: an input that is
// missing from the key is a bug, not a reason for a TTL.
type Cache struct{ db *sql.DB }

// NewCache returns a cache over db, which must have the store migrations
// applied.
func NewCache(db *sql.DB) *Cache { return &Cache{db: db} }

var errNoCacheDatabase = errors.New("the judgment cache has no database")

// cachedJudgment is a judgment read back from the cache.
type cachedJudgment struct {
	p     float64
	model string // the dated snapshot that produced it
}

// judgmentRow is a judgment to store.
type judgmentRow struct {
	key, testID, model, rubric string
	p                          float64
	latencyMS                  int
	costUSD                    float64
}

// cacheKey is sha256(model release, rubric version, feature version, change
// fingerprint, descriptor hash). The parts are separated by a zero byte so no
// two different tuples share an input.
func cacheKey(release, rubric, fingerprint, descriptorHash string) string {
	return hashHex([]byte(strings.Join([]string{release, rubric, impact.FeatureVersion, fingerprint, descriptorHash}, "\x00")))
}

// cacheKey returns the key of one descriptor of the batch.
func (b *batch) cacheKey(d *descriptor) string {
	return cacheKey(b.j.cfg.Model, b.j.rubric.version, b.fingerprint, d.hash)
}

func (c *Cache) get(ctx context.Context, key string) (cachedJudgment, bool, error) {
	var out cachedJudgment
	if c == nil || c.db == nil {
		return out, false, errNoCacheDatabase
	}
	err := c.db.QueryRowContext(ctx,
		`SELECT p_break, model_version FROM judgments WHERE cache_key = ? AND predictor = 'jev'`, key,
	).Scan(&out.p, &out.model)
	if errors.Is(err, sql.ErrNoRows) {
		return out, false, nil
	}
	if err != nil {
		return out, false, fmt.Errorf("read a judgment: %w", err)
	}
	if math.IsNaN(out.p) || out.p < 0 || out.p > 1 {
		return out, false, fmt.Errorf("judgment %s holds the probability %v", key, out.p)
	}
	return out, true, nil
}

func (c *Cache) put(ctx context.Context, row judgmentRow, at time.Time) error {
	if c == nil || c.db == nil {
		return errNoCacheDatabase
	}
	_, err := c.db.ExecContext(ctx,
		`INSERT OR REPLACE INTO judgments
		   (cache_key, test_id, p_break, predictor, model_version, rubric_version, latency_ms, cost_usd, created_at)
		 VALUES (?, ?, ?, 'jev', ?, ?, ?, ?, ?)`,
		row.key, row.testID, row.p, row.model, row.rubric, row.latencyMS, row.costUSD, at.UTC().Format(time.RFC3339),
	)
	if err != nil {
		return fmt.Errorf("store a judgment: %w", err)
	}
	return nil
}
