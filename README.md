# Hazard

Hazard selects the tests a diff could break, runs only those, and falls back to the full suite, with a recorded reason, whenever it cannot trust its own selection.

It is a command line tool and Go library for Go and Python repositories, released under the Apache License 2.0.

## Status

Hazard ships as a conservative deterministic selector with explicit abstention to the full suite:

- L0 classifies the change. L1 combines the import graph with fresh per-test coverage. A changed line that lies outside every coverage block is treated as unknown and widens the selection.
- The calibrated predictor layers (a static ranking, a logistic model, the Jev model, and an ensemble) are implemented and tested. They have no certified lock, and `hazard select` and `hazard run` do not consult a predictor until a lock exists.
- `hazard calibrate` is implemented and tested. On the one real corpus that qualified for measurement it refuses: `calibrate: refused: need n >= 299, have 0`. No recall bound is certified.
- Without a valid `hazard.lock`, the decision is the full suite with the abstain reason `no_lock`. `--shadow` shows what narrowing would select, and records it.

The numbers below come from one real corpus (no usable labels) and one synthetic population (mutants of a single Go repository). Read [What is and is not claimed](#what-is-and-is-not-claimed) before relying on either.

## How it works

1. **L0, change classification.** `git diff` becomes a change set: files with a kind (source, test, config, fixture, generated, docs, lock, build), hunks with the declarations they touch, and a list of full-suite triggers. A changed file that matches a trigger (`go.mod`, `go.sum`, `pyproject.toml`, `uv.lock`, `Dockerfile*`, `.gitlab-ci.yml`, `.github/workflows/**`, `Makefile`, `**/conftest.py`, `**/testdata/**` by default) sends the whole suite.
2. **L1, the deterministic floor.** The index (`hazard index`) holds the test inventory, the Go package and Python module graph, and with `--coverage` one coverage record per test. The CERTAIN set is the union of these rules:
   - every test of a package whose test file changed, and every test whose own file changed;
   - in graph mode, every test within `impact.max_hops` (3 by default) of a changed package in the reverse graph;
   - in coverage mode, every test whose coverage overlaps a changed line;
   - every test of a package that holds a changed fixture;
   - always-run tests (unstable identities).

   In coverage mode, the unbounded reverse-graph rule replaces the coverage rule for any test without usable coverage and for any changed file whose coverage does not apply. That covers stale or missing coverage, a base file that no longer matches the coverage digest, and a changed line in no coverage block of any test. These cases widen the selection and never narrow it.
3. **L2, optional predictors.** A predictor scores the tests L1 did not select. A score at or above the threshold adds a test. Nothing a predictor says can remove a test the floor selected.
4. **L3, the controller.** The controller verifies the lock (digest, model release, rubric, feature version, selection policy), forms the union, and abstains to the full suite when a rule fires: a full-suite trigger, stale coverage when coverage is required, predictor failure on more than 25 percent of candidates, truncation of more than 10 percent of candidates, more than 40 percent of the packages changed, a drift alarm, or an estimated saving below `control.min_saving_ratio` (0.15 by default).

Every failure or unknown degrades toward running more tests: a missing index, a stale index, a shallow clone without a merge base, a missing lock, a corrupt lock, an unavailable predictor, a runner that cannot express the selection. The reasons are listed in [docs/DESIGN.md](docs/DESIGN.md).

## Install

Build from a clone (Go 1.26.6 is the version CI uses):

```
go build -trimpath -o hazard ./cmd/hazard
```

Or install the command:

```
go install github.com/omorsi45/hazard/cmd/hazard@latest
```

Python repositories need Python 3.11 or newer with pytest and coverage.py. CI pins pytest 8.4.2 and coverage 7.10.7.

## Quick start

A Go-only repository needs one line of configuration. By default both languages are enabled, and a Go-only repository would otherwise get a pytest run that fails with `file or directory not found: tests`.

```yaml
# hazard.yml
version: 1
languages:
  python:
    enabled: false
```

Build the index. `--coverage` runs the test suite: one baseline `go test`, then one `go test` per top-level Go test (and one pytest run under coverage.py for Python).

```
hazard index --coverage
```

See what narrowing would select for your change, without changing what runs:

```
hazard select --shadow --explain --base origin/main
```

On the fixture repository in `testdata/repos/go`, after changing one constant in `server/server.go`, standard output is the two lines below, and standard error adds `warning: shadow: the run set is the full suite; the decision already was one (no_lock)`:

```
full suite: shadow
hypothetical go:example.com/hazardfixture/server:TestDefaultPortMatchesConfig	certain	graph
```

Without `--shadow`, the first line reads `full suite: no_lock`.

Run the full suite and record the hypothetical selection next to it:

```
hazard run --shadow --base origin/main
```

Audit the recorded selections against a full run. Do this after every full run:

```
hazard audit --full-run .hazard/junit.xml --since-run <selection id or timestamp> --bisect
```

Narrowing starts only when `hazard calibrate` writes a `hazard.lock`. At alpha 0.01 and delta 0.05 it refuses until it has at least 299 calibration units, where a unit is a commit that a strong regression label blames ([docs/GUARANTEE.md](docs/GUARANTEE.md)).

## Commands

| Command | What it does |
|---|---|
| `hazard index` | Build the test index (collection and `--coverage` execute project code) |
| `hazard select` | Select tests without running them |
| `hazard run` | Select and run tests; exit with the native runner's exit code |
| `hazard explain` | Explain one test decision |
| `hazard audit` | Audit a selection against a full run |
| `hazard calibrate` | Fit the risk threshold from the strong CI labels and write `hazard.lock` |
| `hazard train` | Fit the static and logistic predictors |
| `hazard eval` | Evaluate the selection configurations (the ablation) |
| `hazard mutate` | Generate mutation labels (builds and runs the test suite many times) |
| `hazard labels` | Build and inspect regression labels (`build`, `flaky`, `stats`) |
| `hazard ci-mine` | Mine CI history |

Every command accepts `--json`. `hazard run --help` lists the runner flags; arguments after `--` go to the native runner, and none may narrow the tests.

## Deployment shape

Narrowing is one part of a loop. The loop is what makes a miss a delay and not an escape.

1. Start in `--shadow` mode. The decision is the full suite and Hazard records what it would have selected.
2. Once a lock exists, select on feature branches only. The full suite still gates the default branch.
3. Run the full suite on every merge to the default branch and on a schedule (`audit.full_suite_on_merge_to_default` and `audit.full_suite_schedule`, `0 3 * * *` by default).
4. Run `hazard audit` after every full run. It writes missed tests to `.hazard/hazard.db` and one line per audited selection to `.hazard/audits.jsonl`, and the drift monitor reads them.

CI needs the full history. A shallow clone has no merge base, and Hazard then runs the full suite on every pull request. Working templates: [docs/ci/github-actions.yml](docs/ci/github-actions.yml) and [docs/ci/gitlab-ci.yml](docs/ci/gitlab-ci.yml).

## Measured results

### Real CI corpus: label yield

Corpus: Azure/azure-iot-sdk-python, mined with `hazard ci-mine --provider github` over the window 2025-10-13 to 2026-10-12.

- 499 runs (500 attempts) and 537 artifacts inspected. 2,880,686 raw test cases, 2,401,373 factual results on 91 result-bearing commits, 5,935 distinct tests.
- The results that survive provenance checks span 2026-08-25 to 2026-09-22, about one month of the twelve requested. The target was 300 result-bearing commits.
- 14 raw failing cases (9 `fail`, 5 `error`) and no grouped candidate. 0 strong regression labels. The 9 failures are timing and handler-count assertions that recur across unrelated runs, and the 5 errors are collection failures. None has a verifiable tested revision.

Calibration on that store, `hazard calibrate --alpha 0.01 --delta 0.05 --split temporal --split-at 2026-10-13 --predictor logistic --unit commit --out hazard.lock`, exit 1, no lock written:

```
calibrate: refused: need n >= 299, have 0
```

The full accounting is in [docs/CORPUS.md](docs/CORPUS.md).

### Synthetic ablation

The real corpus has no labels to evaluate selection quality, so the ablation below is synthetic (mutation labels on prometheus/common v0.73.0), a Go repository with 201 tests in 9 packages. These labels come from mutants, never from CI history, and never enter calibration. Each event is one mutant.

Ablation, synthetic (mutation labels on prometheus/common v0.73.0): 262 held-out test events, 188 with a failing test. Fractions of 1 with 95% bootstrap intervals (2000 resamples, seed 1). Time ratio is a diagnostic, not a saving.

| configuration | R_c | R_f | selection ratio | time ratio | abstention | effective saving |
|---|---|---|---|---|---|---|
| B0 run everything | 1.000 [1.000, 1.000] | 1.000 [1.000, 1.000] | 1.000 [1.000, 1.000] | 1.000 [1.000, 1.000] | N/A | 0.000 [0.000, 0.000] |
| B1 L1 graph only | 1.000 [1.000, 1.000] | 1.000 [1.000, 1.000] | 0.377 [0.355, 0.399] | 0.232 [0.204, 0.262] | 0.008 [0.000, 0.019] | 0.738 [0.708, 0.767] |
| B2 L1 + coverage | 1.000 [1.000, 1.000] | 1.000 [1.000, 1.000] | 0.110 [0.089, 0.133] | 0.148 [0.110, 0.187] | 0.008 [0.000, 0.019] | 0.814 [0.776, 0.851] |
| P0 + static rank | not trained: 0 residual positives in 99,252 training examples | | | | | |
| P1 + logistic | not trained: 0 residual positives in 99,252 training examples | | | | | |
| P2 + Jev | needs OPENROUTER_API_KEY | | | | | |
| P3 + ensemble | needs OPENROUTER_API_KEY | | | | | |

In the table, which is synthetic (mutation labels on prometheus/common v0.73.0), R_c is the fraction of events with a failing test whose failing tests were all selected. R_f is the fraction of failing tests that were selected. Effective saving is `1 - (E[cost(S)] + E[overhead]) / E[cost(T)]` over all 262 events, abstentions and overhead included. It is the only savings figure Hazard reports.

On the synthetic (mutation labels on prometheus/common v0.73.0) population, the deterministic floor already selected every failing test of every training event, so P0 and P1 had no residual positives to learn from and were not trained. P2 and P3 need a key that was not available. This population says nothing about whether the predictors improve recall or savings.

For synthetic (mutation labels on prometheus/common v0.73.0), R_c is 1.000 on 188 events, which is 0 misses. The exact one-sided 95% Clopper-Pearson upper bound on the miss rate with 0 misses in 188 is 0.01581, so the data are consistent with a recall as low as about 0.984. That is an estimate on one repository with synthetic labels, not a guarantee.

## What is and is not claimed

- No recall bound is certified. Calibration refuses on the only real corpus, and no `hazard.lock` ships.
- The synthetic results come from first-order mutants of one Go repository. Mutants are not regressions people wrote, and one repository says nothing about another.
- Nothing is claimed for Python selection quality on any real repository. The Python adapter is tested on fixture repositories.
- The value of the Jev model is unmeasured, because no key was available. It is positioned for configuration-style edges that coverage cannot see.
- Effective saving, computed over all events with abstentions and overhead, is the only savings figure. Time ratio is shown as a diagnostic.
- A selection is never a substitute for the scheduled full run. See [docs/GUARANTEE.md](docs/GUARANTEE.md) for what the full run and the audit protect.

## How it differs

These tools solve the same problem in different ways. This section describes what each documents about itself, not how well it works. Hazard has no numbers to compare against them and makes no claim of being better.

- **Bazel** selects tests through the build graph: a change maps to the targets that depend on it, and only those are rebuilt and tested. It relies on dependencies being declared in build rules, so a project has to be built with Bazel. Hazard reads Go imports, Python imports and per-test coverage from projects that run `go test` and pytest as they are.
- **Launchable** is a hosted service. Its predictive test selection trains a machine learning model on a team's historical test results and uses it to pick tests for a change. Hazard is a local program with no hosted component. Its learned layers are optional, and they can only add tests to a deterministic floor.
- **Datadog Test Optimization** includes Test Impact Analysis, a hosted service that uses per-test code coverage from past runs, collected by Datadog's instrumentation, together with commit information to skip tests. Hazard collects coverage locally with `hazard index --coverage` and keeps it in the repository's `.hazard` directory.
- **Google TAP** is the test automation platform Google runs for its monorepo. It selects the tests affected by a change from the dependency graph and runs on Google's infrastructure. It is internal to Google.

What Hazard does differently:

- It is local and open source, and works on ordinary Go and Python repositories without changing their build.
- It has a deterministic floor that probabilities can only add to.
- Its recall bound is either certified on real labels or explicitly refused. Today it is refused.
- It abstains to the full suite and records why (`hazard select --json` and `.hazard/runs.jsonl`).
- It has privacy levels and a payload dry run that prints, without sending, exactly what would leave the machine ([docs/PRIVACY.md](docs/PRIVACY.md)).

## Documentation

- [docs/GUARANTEE.md](docs/GUARANTEE.md): the per-commit loss, the calibration procedure, and why nothing is certified yet
- [docs/PRIVACY.md](docs/PRIVACY.md): what leaves the machine at each redaction level, with real payloads
- [docs/ADAPTERS.md](docs/ADAPTERS.md): the `go test` and pytest adapters
- [docs/CORPUS.md](docs/CORPUS.md): the preregistered candidates, the real corpus, and the synthetic target
- [docs/DESIGN.md](docs/DESIGN.md): architecture, the degradation matrix, and corrections to the original design

## Development

```
go build ./...
go test ./... -count=1
golangci-lint run
```

CI builds with Go 1.26.6 and runs the tests with Python 3.11, pytest 8.4.2 and coverage 7.10.7. Set `HAZARD_REQUIRE_PYTEST=1` and `HAZARD_TEST_PYTHON` to the interpreter path to run the pytest tests instead of skipping them.

## License

Apache License 2.0. See [LICENSE](LICENSE).
