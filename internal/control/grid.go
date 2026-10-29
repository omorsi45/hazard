package control

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

const (
	maxGridPoints = 10000
	gridRounding  = 1e9   // values are rounded to 1e-9
	gridEndSlack  = 1e-12 // the end value is inclusive within this distance
)

// ParseGrid parses "start:end:step" into the ascending lambda grid
// start, start+step, ... up to end (inclusive within 1e-12), each value
// rounded to 1e-9. All three numbers must be finite with 0 < start <= end < 1
// and step > 0, the grid may hold at most 10000 points, and rounding must keep
// every point inside (0, 1) and strictly ascending.
func ParseGrid(s string) ([]float64, error) {
	parts := strings.Split(s, ":")
	if len(parts) != 3 {
		return nil, fmt.Errorf("grid %q: want start:end:step", s)
	}
	var v [3]float64
	for i, name := range [3]string{"start", "end", "step"} {
		f, err := strconv.ParseFloat(parts[i], 64)
		if err != nil {
			return nil, fmt.Errorf("grid %q: %s: %w", s, name, err)
		}
		if math.IsNaN(f) || math.IsInf(f, 0) {
			return nil, fmt.Errorf("grid %q: %s must be finite", s, name)
		}
		v[i] = f
	}
	start, end, step := v[0], v[1], v[2]
	if !(start > 0 && start <= end && end < 1) {
		return nil, fmt.Errorf("grid %q: need 0 < start <= end < 1", s)
	}
	if !(step > 0) {
		return nil, fmt.Errorf("grid %q: step must be > 0", s)
	}

	// Count first so a tiny step cannot trigger a huge allocation.
	count := math.Floor((end-start+gridEndSlack)/step) + 1
	if count > maxGridPoints {
		return nil, fmt.Errorf("grid %q: more than %d points", s, maxGridPoints)
	}
	out := make([]float64, 0, int(count))
	for i := 0; i < int(count); i++ {
		x := math.Round((start+float64(i)*step)*gridRounding) / gridRounding
		if !(x > 0 && x < 1) {
			return nil, fmt.Errorf("grid %q: point %d (%v) leaves (0, 1) after rounding to 1e-9", s, i, x)
		}
		if i > 0 && !(x > out[i-1]) {
			return nil, fmt.Errorf("grid %q: points %d and %d collapse to %v after rounding to 1e-9", s, i-1, i, x)
		}
		out = append(out, x)
	}
	return out, nil
}
