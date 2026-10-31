# Design

This page describes how Hazard is built: the packages, the path a change takes from `git diff` to a test run, the files Hazard keeps, how every failure degrades, and where the implementation departs from the original specification. It is written for contributors and for anyone reviewing the safety argument.

## Principles

- **A set union with one-sided authority.** The selection is the union of the deterministic floor, the always-run tests, the predictor additions and any widening tier. A predictor or a tier can enlarge it. Nothing can shrink what the floor selected.
- **Unknown is never zero.** A missing, malformed or unverifiable input is treated as the worst case: the test is selected, or the full suite runs.
- **Every failure degrades toward more testing.** A change that degrades toward running fewer tests is a defect.
- **Abstention is a result.** The full-suite fallback carries a reason that is recorded with the decision.
- **Explicit preparation.** Expensive work (collection, coverage, training, mining) runs in named commands. Selection reads files and runs `git`.

## Packages

| Package | Role |
|---|---|
| `cmd/hazard` | The `hazard` binary. |
| `pkg/hazard` | The public library entry point, `Select(ctx, Options) (Decision, error)`, backed by the same engine as the command line. |
| `internal/cli` | The command tree (Cobra): `index`, `select`, `run`, `explain`, `audit`, `calibrate`, `train`, `eval`, `mutate`, `labels`, `ci-mine`, `version`. |
| `internal/config` | Strict YAML configuration with defaults. It rejects aliases, merge keys, unknown fields and non-integer values for integer fields. |
| `internal/change` | L0. Turns a git diff into a change set: files, kinds, hunks with the declarations they touch, and the full-suite triggers they match. |
| `internal/graph` | The Go package graph and the Python module graph, with reverse-reachability by hop count. |
| `internal/index` | Builds and publishes the manifest: test inventory, graph, optional per-test coverage, staleness checks. |
| `internal/impact` | L1. Computes the CERTAIN set and the ranked residual candidates with their features. |
| `internal/predict` | L2. The static ranking, the logistic model, the Jev client with its judgment cache, the ensemble, and training. |
| `internal/redact` | Builds the change block and test descriptors that may leave the machine, from the privacy configuration. |
| `internal/control` | L3. The Hoeffding-Bentkus p-value, Learn-then-Test, the lock and its identity checks, the controller, the drift monitor. |
| `internal/engine` | The pipeline shared by `select`, `run` and `explain`: configuration, change extraction, index loading, L1, the lock, the predictor, the controller. |
| `internal/runner` | The `go test` and pytest adapters, the passthrough rules, JUnit parsing and building. |
| `internal/telemetry` | `runs.jsonl` records and `hazard audit`. |
| `internal/store` | The SQLite store (`.hazard/hazard.db`, pure-Go driver). |
| `internal/mine` | CI mining for GitHub and GitLab: provenance, raw storage, acquisition gaps and exclusions. |
| `internal/labels` | Freezes a regression-label cohort from acquired CI history, with flaky hygiene. |
| `internal/mutate` | Generates mutants and records which tests kill them. |
| `internal/events` | Turns mutation records and CI labels into change events for training and evaluation. |
| `internal/eval` | The ablation, the bootstrap intervals and the report. |
| `research/` | A Python cross-check of the Learn-then-Test and reliability code. |
| `testdata/` | Fixture repositories for Go and Python, and JUnit and Jev samples. |

## Data flow

```
git diff ──► L0 change ──► change set
                              │
.hazard/index/manifest.json ──┤
                              ▼
                       L1 impact ──► CERTAIN set + ranked candidates
                              │
hazard.lock ──► verify ───────┤        (no lock: stop here, full suite, no_lock)
                              ▼
                  L2 predict (candidates only) ──► probabilities
                              │
                              ▼
                  L3 control.Decide ──► selection, or full suite + reason
                              │
                              ▼
                  runner adapters ──► native runner, JUnit report, exit code
                              │
                              ▼
                  .hazard/runs.jsonl ──► hazard audit ──► field misses ──► drift monitor
```

1. **L0, `internal/change`.** The base defaults to the merge base with the default branch. The head defaults to the working tree, including uncommitted changes, and an explicit `--head` is an immutable view. Added and current lines are attributed from the head contents, and removed lines from the base contents, so a deleted declaration still counts as affected. Renames keep both paths for trigger checks. A match with `repo.full_suite_triggers` demands the full suite.
2. **L1, `internal/impact`.** The CERTAIN set is built from direct test-file changes, graph reachability, per-test coverage and fixture rules. See the README for the rules. Candidates are every other test, ranked by `impact.candidate_ranking`. The cap (`impact.max_candidates`, 400) applies to candidates only and never removes a CERTAIN test.
3. **L2, `internal/predict`.** Predictors score candidates. A score at or above the lock's threshold adds the test. A failed, missing or malformed score counts as 1.
4. **L3, `internal/control`.** `Decide` returns the union and applies the abstention rules in a fixed order.
5. **Runners.** The selection becomes native commands, and Hazard exits with the runner's code. See [ADAPTERS.md](ADAPTERS.md).
6. **Telemetry and audit.** Each decision appends a record. After a full run, `hazard audit` compares recorded selections with the full result.

The time budget of one selection is 5,000 ms by default (`--budget-ms`). A budget that is spent before the predictor runs is a full-suite decision (`predictor_error`).

### The controller's rules

Rule 1 comes first and demands the full suite before anything is selected, in this order: a full-suite trigger, a reason from L1 or an adapter, a lock problem. Otherwise the selection is the union of CERTAIN, always-run, and every candidate that is failed or has a score not below the threshold. The module tier then adds every test of each package that has a selected test, and the suite tier abstains. Last, the first matching rule turns the decision into the full suite, and the narrowed selection stays visible as the hypothetical selection:

| Order | Condition | Reason |
|---|---|---|
| 1 | coverage is stale and `impact.coverage_required` is true | `coverage_stale` |
| 2 | the predictor failed on more than 25 percent of candidates | `predictor_failure` |
| 3 | more than 10 percent of the candidates were cut by the cap | `truncated` |
| 4 | more than 40 percent of the packages changed | `too_broad` |
| 5 | the drift monitor is in alarm | `drift` |
| 6 | the estimated saving is below `control.min_saving_ratio` (0.15) | `not_worth_it` |

The thresholds are strict: exactly 25, 10 and 40 percent do not abstain. Test cost uses the median duration of each test, and a test without a known duration costs the median of the known ones, rounded up.

## The index manifest

`hazard index` publishes one JSON file, `.hazard/index/manifest.json`, by atomic rename. A failed or canceled build never replaces the previous manifest. It holds:

- the schema version, the repository root, the head commit, whether the tree was dirty, a content digest and the configuration digest;
- every considered source, test and configuration file with its SHA-256;
- the Go package and Python module graph;
- the test inventory (ID, language, package, name, file, runner ID, purpose, up to 5 assertion targets, an unstable flag);
- with `--coverage`, per-test coverage blocks, the collection head, and the digest of each instrumented file;
- the versions of the tools used, the commands the build ran, diagnostics, and a `Complete` flag. An incomplete index means the full suite.

Coverage granularity is a top-level Go test, with its subtests treated as one group, and a pytest test context for Python.

**Staleness.** An index is usable for a change set when the configuration digest matches, every recorded file outside the change set is byte-identical at the selection head, and no relevant file exists at the head outside the change set without being recorded. Files inside the change set are exempt, because a graph edge out of a changed file starts at a package the change already selects. The configuration file itself is compared by digest only. A new or renamed directory maps to no package and sends the full suite. A changed Go test file is rediscovered at the head, so new Go tests are never dropped.

## The lock

`hazard.lock` is JSON with a digest over every other field. It holds the schema version, creation time, alpha, delta, unit, the threshold, the predictor kind, the model release, the rubric version, the feature version, the weights digest, the policy digest, the L1 mode (`graph` or `coverage`) the threshold was calibrated in, the calibration sample size and empirical miss rate, the split, the grid with the p-value of every evaluated threshold, and the sorted IDs of the calibration commits. The engine verifies it on every decision, and a selection under a lock runs L1 in the lock's mode. A mismatch is a full-suite decision, never a silent fallback. See [GUARANTEE.md](GUARANTEE.md).

## Telemetry and audit

Each `select` and `run` appends one line to `.hazard/runs.jsonl`: decision ID, time, commit, mode (`select`, `run` or `shadow`), whether the full suite ran and why, the lock digest, counts by source, the members of the selection (the hypothetical members for a full-suite decision), estimated costs, overhead, and the runner's exit code. It never holds payloads, hunk text, keys, purposes or assertions ([PRIVACY.md](PRIVACY.md)).

`hazard audit --full-run <junit> --since-run <id or timestamp>` compares each recorded selection at the full run's commit with the full result. A test that failed in the full run and was not selected is a field miss. A failure that no selection at that commit explains is reported as unattributed, unless `--bisect` finds the commit where the test first fails, running the test alone in a temporary worktree at each step, and a selection made there left it out. Misses go to `.hazard/hazard.db`, and one line per audited selection goes to `.hazard/audits.jsonl`. Only narrowed selections are audited. Full-suite and shadow records are not audited, and they are never counted as successes.

## Failure and degradation matrix

Every row degrades toward running more tests. The reason is what telemetry and `--json` output record.

| Condition | Behavior | Recorded reason |
|---|---|---|
| shallow clone or unrelated histories, no merge base | full suite | `no_merge_base` |
| the change set cannot be extracted | full suite | `change_error` |
| no index | full suite | `no_index` |
| the index file cannot be read or parsed | full suite | `index_corrupt` |
| the index is incomplete | full suite | `index_incomplete` |
| the index is stale: configuration changed, a recorded file changed outside the change set, a new relevant file appeared | full suite | `index_stale` |
| the history database cannot be read | full suite | `history_error` |
| a changed file matches a full-suite trigger | full suite | `trigger:<pattern>:<path>` |
| a changed Go or Python file maps to no package (new or renamed directory) | full suite | `unmapped_change:<path>` |
| a changed Python file in a package with a dynamic import | full suite | `python_dynamic_import:<package>` |
| a changed Python test module has test names missing from the inventory | full suite | `python_inventory_changed` |
| the test inventory cannot be derived | full suite | `inventory_error` |
| coverage is missing or stale and `impact.coverage_required` is false | wider selection: the unbounded reverse-graph rule for every reachable test | `coverage_state` in the L1 output |
| coverage is missing or stale and `impact.coverage_required` is true | full suite | `coverage_required:<state>` or `coverage_stale` |
| a changed line is outside every coverage block, or the base digest differs from the coverage digest | the unbounded reverse-graph rule for that file | none (a wider selection) |
| no `hazard.lock` | full suite | `no_lock` |
| the lock cannot be read | full suite | `lock_error` |
| the lock cannot be parsed, has an unknown version, a threshold outside (0, 1), or a digest that does not match | full suite | `lock_corrupt` |
| the model release differs from the lock | full suite | `model_drift` |
| the rubric version, feature version or weights, or the selection policy differs from the lock | full suite | `rubric_mismatch`, `feature_mismatch`, `policy_mismatch` |
| the predictor artifact is missing, or, for Jev and the ensemble, the key variable is unset, the redaction level is `hash`, or `allow_external_predictor` is false | full suite; no other predictor is substituted | `predictor_unavailable` |
| the predictor errors or the time budget is spent | full suite | `predictor_error` |
| the served model release differs from the lock | full suite | `model_drift` |
| one request shard fails, times out or answers malformed | all tests of that shard are selected | failure source on each test, for example `jev_failed` |
| the spend budget cannot cover a shard | all tests of that shard are selected | `budget_exhausted` on each test |
| the predictor failed on more than 25 percent of candidates | full suite | `predictor_failure` |
| more than 10 percent of candidates were cut by the cap | full suite | `truncated` |
| more than 40 percent of packages changed | full suite | `too_broad` |
| the drift monitor alarms | full suite until a new lock is written | `drift` |
| the estimated saving is below `control.min_saving_ratio` | full suite | `not_worth_it` |
| the advisory tier answers `suite` | full suite | `tier_suite` |
| a selected test cannot be expressed to its runner | full suite | `runner_limit` |
| selected pytest node IDs are not collected | the full pytest suite | `runner_limit` |
| an adapter or the selection fails internally | the full suite of that adapter, or of the command | `internal_error` |
| `--full` | full suite | `forced_full` |
| `--shadow` | the full suite runs, and the hypothetical selection is recorded | `shadow`, or the reason that already applied |
| a test has an unstable identity or is quarantined | the test is always selected | `unstable_id`, `quarantined` |

With `--no-fallback`, a missing or unusable lock, a lock that no longer matches the running system, or a locked predictor that is unavailable or fails (`predictor_unavailable`, `predictor_error`, such as an unset API key or a spent budget) ends `hazard run` with exit code 3 and nothing runs.

### How this matrix differs from the original

The original matrix had a missing API key fall back to the local logistic predictor. That substitution can drop tests the remote predictor would have added, so it is now `predictor_unavailable` and a full suite, unless a lock was calibrated for the local predictor itself. The original also read missing or stale coverage as graph-only selection in all cases. A coverage-driven policy is wider than a bounded graph, so degrading from coverage to a bounded graph could remove tests. Missing or stale coverage now selects every reachable test, whatever the hop bound.

## Corrections to the original specification

The specification this project started from was corrected as the work met evidence. Each line names the decision and the reason. The numbers such as D056 are the project's internal decision identifiers, and the reasons are repeated here in full.

**Statistics and the guarantee**

- D001, conditional recall: calibrate on regression-bearing commits per repository, because the unconditional loss and the recall denominator were different quantities.
- D002, lower tail: the Hoeffding-Bentkus p-value is 1 when the empirical loss is at or above alpha, because the un-clipped divergence is small for large losses. Predictor scores are trained apart from the risk-calibration data.
- D011 and D020, drift: the default window is 500, and too few audited observations is a reported warming state, not an abstention. A confirmed miss alarms at once.
- D012, independence: the finite-sample bound assumes independent calibration units, so a random split of dependent history does not make it valid. The temporal backtest is the number to believe.
- D005, denominators: raw candidate accounting is kept, and excluded (flaky) labels are removed from eligible recall.
- D007, audits: compare a selection with the full run at the same commit and environment first. A later failure is not proof of a miss, and repeated rolling bounds are alarms, not certification.
- D014, scores: a remote yes-score answers the prompt. It is not a validated regression probability.
- D056, the headline: zero strong labels were found, so the formal guarantee is dropped as the headline. Hazard ships as a conservative deterministic selector with explicit abstention.
- D067, the product: the deterministic floor is the product. The predictor layers stay implemented, tested and unconsulted until a lock exists.

**Selection**

- D003 and D019, coverage: coverage-driven narrowing needs per-test attribution, because a Go coverage profile of the whole suite is an aggregate. A policy calibrated for graph-only operation stays allowed.
- D006, the candidate cap: exceeding the safe candidate capacity abstains, because a rank cap can evict a test that an insertion previously selected.
- D004, policy identity: the whole policy and the exact predictor snapshot are bound to the lock, and a missing key or a privacy setting never substitutes a different predictor.
- D047, removed source: attribute removed lines from the base contents, because a deleted declaration has no new-side range at the head.
- D055, staleness: an index may differ from the head inside the change set, because requiring a byte-identical tree sent every working-tree edit to the full suite.
- D064, configuration: the configuration is compared by digest and never as head content, because an ignored configuration file is absent from every commit tree.
- D066, unknown coverage: a changed line outside every coverage block is unknown and takes the reverse-graph rule. Before this fix, the same measurement of synthetic (mutation labels on prometheus/common v0.73.0) gave B2 R_c 0.963 [0.934, 0.989], because coverage mode had read "no block" as "no dependent test".
- D065, the logistic fit: Newton steps replace fixed-step gradient descent, which could not reach the stopping tolerance. Platt scaling uses smoothed targets, because an unpenalized fit has no finite optimum on separable data.

**Privacy and cost**

- D008, cost and redaction: reserve the estimated cost before dispatch, because the vendor bills before it reports usage. Redact test descriptors and assertions as well as hunks.
- D057, the Jev wire format: re-verified against the vendor's documentation. At `structure`, the test name is sent as words with no assertions. The tier answer can only widen.

**Mining and labels**

- D013 and D052, the corpus: public artifact retention is limited, so the corpus window is measured and not promised. The real repository was accepted with its shortfall visible.
- D015, deletion: a failing test that is deleted is a weak label unless an independent fix is shown, because deletion hides a bug as readily as it repairs one.
- D016, D017 and D027, provenance: a GitHub artifact record has no job ID or run attempt, and a source head is not the tested revision. Such observations stay raw until provenance is verified.
- D029 and D031, identity: factual results are separate from comparable labels, and a report's language and package come from verified evidence, not from the repository's primary language.
- D048, labels: label evidence is versioned and the cutoff is an explicit exclusive UTC date.
- D033 to D046, providers: pagination, capability checks, log framing and gap handling were made explicit rules after mining real histories.

**Interface and configuration**

- D018, YAML: aliases and merge keys are rejected, because they silently bypass integer checks.
- D021 and D022, commands: `hazard index` and `hazard train` exist because the specification needed per-test coverage, Python analysis and trained predictors without an entry point that prepares them. Python analysis happens in explicit preparation, never at selection.
- D023, the library: `Select(ctx, Options)` is the public API, with a fixed chronological 80/20 split for fitting versus score scaling.
- D054, the index: one atomically published manifest replaces content-addressed objects, and Python imports are read by a constrained parser in Go.
- D060, the target: the mutation target is prometheus/common v0.73.0 because it has real graph edges and fixtures that only coverage and fixture rules can see.
