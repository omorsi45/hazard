package impact

// Features is the f2 feature record for one residual candidate.
type Features struct {
	Hops              int // reverse graph distance, 99 if unreachable
	Reachable         bool
	CoverageOverlap   float64 // covered changed lines / changed lines
	CoverageKnown     bool
	CoverageStale     bool
	CofailureRate     float64
	CofailureSupport  int
	TestFailRate      float64
	DaysSinceLastFail float64 // 365 when unknown
	SharedSymbolHits  int
	SameFile          bool
	SamePackage       bool
	ChangedFileKinds  [8]int // counts by change.FileKind over the change set
	LinesAdded        int
	LinesRemoved      int
	FilesChanged      int
	TestMedianMS      int // 0 when unknown
	TestIsIntegration bool
}

// Vector returns the 25 feature values in the documented order: Hops,
// Reachable, CoverageOverlap, CoverageKnown, CoverageStale, CofailureRate,
// CofailureSupport, TestFailRate, DaysSinceLastFail, SharedSymbolHits,
// SameFile, SamePackage, ChangedFileKinds[0..7], LinesAdded, LinesRemoved,
// FilesChanged, TestMedianMS, TestIsIntegration. Booleans are 0 or 1 and Hops
// is 99 when the test is unreachable.
func (f Features) Vector() []float64 {
	hops := float64(f.Hops)
	if !f.Reachable {
		hops = unreachableHops
	}
	v := make([]float64, 0, vectorLen)
	v = append(v,
		hops,
		b2f(f.Reachable),
		f.CoverageOverlap,
		b2f(f.CoverageKnown),
		b2f(f.CoverageStale),
		f.CofailureRate,
		float64(f.CofailureSupport),
		f.TestFailRate,
		f.DaysSinceLastFail,
		float64(f.SharedSymbolHits),
		b2f(f.SameFile),
		b2f(f.SamePackage),
	)
	for _, n := range f.ChangedFileKinds {
		v = append(v, float64(n))
	}
	return append(v,
		float64(f.LinesAdded),
		float64(f.LinesRemoved),
		float64(f.FilesChanged),
		float64(f.TestMedianMS),
		b2f(f.TestIsIntegration),
	)
}

// vectorLen is the number of values Vector returns.
const vectorLen = 25

// unreachableHops is the Hops value of a test no changed package reaches.
const unreachableHops = 99

func b2f(b bool) float64 {
	if b {
		return 1
	}
	return 0
}
