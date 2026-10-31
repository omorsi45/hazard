package eval

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
)

// Durations are the per-test baseline durations a mutation run publishes next
// to its records (<out>.baseline.json).
type Durations struct {
	Path    string
	BaseSHA string
	MS      map[string]int // test ID -> median duration in ms, measurements only
	Ignored int            // entries that were not measurements (negative)
}

type durationsFile struct {
	BaseSHA string           `json:"base_sha"`
	Tests   map[string]int64 `json:"tests"`
}

// ReadDurations reads a baseline file. A file that cannot be read, is not one
// JSON document of the expected shape, or holds no test at all is an error: a
// cost model built from it would be a guess.
func ReadDurations(path string) (Durations, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Durations{}, fmt.Errorf("eval: read the test durations: %w", err)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	var f durationsFile
	if err := dec.Decode(&f); err != nil {
		return Durations{}, fmt.Errorf("eval: %s is not a baseline durations file: %w", path, err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return Durations{}, fmt.Errorf("eval: %s has data after the baseline durations document", path)
	}
	d := Durations{Path: path, BaseSHA: f.BaseSHA, MS: make(map[string]int, len(f.Tests))}
	for id, ms := range f.Tests {
		if ms < 0 {
			d.Ignored++
			continue
		}
		d.MS[id] = int(ms)
	}
	if len(d.MS) == 0 {
		return Durations{}, fmt.Errorf("eval: %s holds no test duration", path)
	}
	return d, nil
}

// CostModel prices a set of tests by their measured durations. A test with no
// measurement costs the median of the known ones (the mean of the middle pair,
// rounded up, for an even count: it can only make a test look dearer), and is
// counted.
type CostModel struct {
	ms      map[string]int
	median  int
	imputed map[string]bool
}

// NewCostModel returns nil when there is no measurement at all: nothing can be
// priced, and a unit cost would be a number nobody measured.
func NewCostModel(ms map[string]int) *CostModel {
	known := make([]int, 0, len(ms))
	for _, v := range ms {
		if v >= 0 {
			known = append(known, v)
		}
	}
	if len(known) == 0 {
		return nil
	}
	slices.Sort(known)
	mid := len(known) / 2
	median := known[mid]
	if len(known)%2 == 0 {
		median = known[mid-1] + (known[mid]-known[mid-1]+1)/2
	}
	return &CostModel{ms: ms, median: median, imputed: map[string]bool{}}
}

// Cost is the price of one test in ms.
func (c *CostModel) Cost(id string) float64 {
	if v, ok := c.ms[id]; ok && v >= 0 {
		return float64(v)
	}
	c.imputed[id] = true
	return float64(c.median)
}

// Sum is the price of a set of tests.
func (c *CostModel) Sum(ids []string) float64 {
	total := 0.0
	for _, id := range ids {
		total += c.Cost(id)
	}
	return total
}

// Imputed is the number of distinct tests priced at the median so far.
func (c *CostModel) Imputed() int { return len(c.imputed) }

// Known is the number of measured tests.
func (c *CostModel) Known() int {
	n := 0
	for _, v := range c.ms {
		if v >= 0 {
			n++
		}
	}
	return n
}
