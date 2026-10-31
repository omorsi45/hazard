package eval

import (
	"os"
	"strings"

	"github.com/omorsi45/hazard/internal/config"
	"github.com/omorsi45/hazard/internal/control"
	"github.com/omorsi45/hazard/internal/engine"
	"github.com/omorsi45/hazard/internal/predict"
)

// Test seams.
var (
	loadPredictor = engine.LoadPredictor
	lookupEnv     = os.LookupEnv
)

// Predictor kinds of --predictors, in the order of the rows P0 to P3.
const (
	PredictorStatic   = "static"
	PredictorLogistic = "logistic"
	PredictorJev      = "jev"
	PredictorEnsemble = "ensemble"
)

var predictorRow = map[string]string{
	PredictorStatic: RowP0, PredictorLogistic: RowP1, PredictorJev: RowP2, PredictorEnsemble: RowP3,
}

// PredictorInfo records the artifact a predictor row was scored with.
type PredictorInfo struct {
	ID            string `json:"id"`
	Kind          string `json:"kind"`
	Status        string `json:"status"`
	ModelRelease  string `json:"model_release,omitempty"`
	WeightsDigest string `json:"weights_digest,omitempty"`
}

// loadedPredictors are the predictors a run asked for.
type loadedPredictors struct {
	States []PredictorState
	Info   []PredictorInfo
	Usable map[string]predict.Predictor
}

// oneLine collapses all white space, newlines included, to single spaces.
func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

// loadPredictors builds the predictor of each kind. One that cannot be built
// (a missing or refused artifact, a privacy setting) reads unavailable with the
// reason; a remote one without its key reads needs <variable> and is not built
// at all, so nothing is sent anywhere without it.
func loadPredictors(repoDir string, cfg config.Config, kinds []string) loadedPredictors {
	out := loadedPredictors{Usable: map[string]predict.Predictor{}}
	for _, kind := range kinds {
		st := PredictorState{Kind: kind, ID: predictorRow[kind]}
		info := PredictorInfo{ID: st.ID, Kind: kind}
		var id control.Identity
		switch {
		case (kind == PredictorJev || kind == PredictorEnsemble) && !hasKey(cfg.Predictor.Jev.APIKeyEnv):
			st.Status = "needs " + cfg.Predictor.Jev.APIKeyEnv
		default:
			p, ident, err := loadPredictor(repoDir, cfg, kind)
			if err != nil {
				st.Status = "unavailable: " + oneLine(err.Error())
				break
			}
			out.Usable[kind], id = p, ident
		}
		info.Status = st.Status
		if st.Status == "" {
			info.Status = StatusMeasured
			info.ModelRelease, info.WeightsDigest = id.ModelRelease, id.WeightsDigest
		}
		out.States = append(out.States, st)
		out.Info = append(out.Info, info)
	}
	return out
}

// hasKey reports whether the environment variable holds a non-blank key.
func hasKey(name string) bool {
	v, ok := lookupEnv(name)
	return ok && strings.TrimSpace(v) != ""
}
