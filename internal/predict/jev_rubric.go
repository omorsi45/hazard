package predict

import "fmt"

// Rubric versions. They are part of the cache key: a new wording is a new
// version, never an edit of an old one.
const (
	// RubricR1 is the long form of the question, as in the design.
	RubricR1 = "r1"
	// RubricR1S is the short form, which repeats less text per question.
	RubricR1S = "r1s"
)

// rubricText is the shared rubric placed once in the state of a request.
const rubricText = "A test BREAKS when the change alters any behavior the test observes, directly or indirectly. " +
	"Indirect paths count: configuration defaults, environment variables, shared fixtures or testdata, " +
	"serialized formats, error message text, exported constants, timing and ordering, database schema, " +
	"and generated code. A test does NOT break merely because it lives near the change or imports the same package. " +
	"Answer about a pass-to-fail transition, not about whether the change is correct."

// Tier question, asked at most once per call in the first shard.
const (
	tierKey          = "tier"
	tierInstructions = "Applying state.rubric to state.change, which scope of tests is enough to catch every test the change breaks?"
)

var tierCriteria = map[string]string{
	"targeted": "Only tests that exercise the changed code directly can break.",
	"module":   "Tests across the packages or modules that contain the change can break.",
	"suite":    "Tests anywhere in the suite can break, or the reach of the change is unclear.",
}

// questionCriteria are the answers of a noul question.
var questionCriteria = map[string]string{
	"true":  "There is a plausible causal path from the change to behavior this test observes.",
	"false": "This test cannot observe any behavior the change alters.",
}

// rubric is one version of the question wording.
type rubric struct {
	version      string
	instructions func(key string) string
}

func rubricFor(version string) (rubric, error) {
	switch version {
	case RubricR1:
		return rubric{version, func(key string) string {
			return fmt.Sprintf("Applying state.rubric to state.change, could the change cause state.tests.%s to go from passing to failing?", key)
		}}, nil
	case RubricR1S:
		return rubric{version, func(key string) string {
			return fmt.Sprintf("Break state.tests.%s? Use state.rubric.", key)
		}}, nil
	default:
		return rubric{}, fmt.Errorf("unknown rubric version %q (want %q or %q)", version, RubricR1, RubricR1S)
	}
}
