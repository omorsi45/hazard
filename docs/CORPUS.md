# Corpus: candidates, measurements and the synthetic target

This page records which repositories were considered as evaluation corpora, what was measured on them, and what the measurements allow Hazard to claim. It is written for people who want to reproduce a result or judge how far it generalizes.

A calibrated threshold needs real regression labels: commits that CI history shows breaking a test the commit's change caused, with the tested revision and the test identity verified. Mutation labels, which are abundant, are synthetic and never enter calibration. So there are two separate bodies of evidence here, and they answer different questions.

## The preregistered candidates

The evaluation cohort was fixed before any selection result existed, and it was not changed afterwards. The candidate list was first written on 2026-09-29, and the cohort was fixed on 2026-10-04. Each candidate was chosen for its language, the project coverage it adds, and the observable availability of its CI sources, not for how Hazard performs on it. An alternate, scikit-learn/scikit-learn, was named at the same time as a separate entry, not as a replacement for any candidate.

Eligibility was measured by a bounded, read-only pass over the GitHub API on 2026-10-04 for prometheus/prometheus, grafana/loki, pandas-dev/pandas, scipy/scipy and the alternate: repository metadata, the first page of completed runs on the default branch, one workflow, its artifact list and its jobs. No repository code was executed and no artifact was downloaded. The pass shows availability at one point in time, not repository-wide absence. Azure/azure-iot-sdk-python was measured earlier, on 2026-09-29, by downloading one real report, and then in full by the mining run below.

| Candidate | Language | Measured eligibility |
|---|---|---|
| prometheus/prometheus | Go | The inspected workflow runs Go tests and build actions and has no JUnit generation or publishing directive. The 13 artifacts of the selected run have opaque names and were not assumed to be reports. The repository-wide artifact listing failed to decode. No retained JUnit archive was established. |
| grafana/loki | Go | The reusable workflow runs `gotestsum` without a JUnit output option. The selected run returned 0 artifacts. |
| pandas-dev/pandas | Python | Tests run through a composite action and a pixi task. No JUnit upload directive was visible. The selected run returned 0 artifacts. |
| scipy/scipy | Python | `pytest` and `spin test` run in several matrix jobs with no JUnit or upload directive in the inspected file. The selected run returned 0 artifacts. |
| Azure/azure-iot-sdk-python | Python | The workflow runs pytest with `--junitxml` and uploads the report and coverage under `always()`. A real report was downloaded and parsed (5,454 test cases in one matrix leg). Measured by `hazard ci-mine` in the real-corpus run below. |
| scikit-learn/scikit-learn (alternate) | Python | The workflow sets `JUNITXML` and consumes it for tracking issues, but no raw-report artifact came back for the sampled push run. |

For four of the five, no retained JUnit artifact was found in the observation pass. None of the five is certified eligible. Source-level counts of 500 test functions, a twelve-month per-test archive, 300 distinct report-bearing commits and strong-label yield were not established by the pass.

There is a further limit that applies to every candidate. Public GitHub Actions artifacts and logs are retained for at most 90 days under the documented settings, so a twelve-month CI window does not hold twelve months of outcomes.

## The real corpus: Azure/azure-iot-sdk-python

### Acquisition

```
hazard --json ci-mine --provider github --project Azure/azure-iot-sdk-python \
  --since 2025-10-13 --until 2026-10-12 --branch "" --language python \
  --junit-glob "**/azure-iot-device-test-results.xml" --max-pipelines 5000 \
  --concurrency 8 --token-env HAZARD_CI_TOKEN
```

The first run exited 0. The identical run with `--resume` also exited 0, added no raw cases, results or artifacts, and recorded 13 new transient source failures (`jobs_unavailable` and `artifact_unavailable`) as active gaps, so a later `--resume` can retry them. The measured results:

| Measure | Value |
|---|---|
| Runs inspected | 499 (500 attempts) |
| Artifacts inspected | 537 |
| Raw test cases | 2,880,686 |
| Factual results | 2,401,373 |
| Result-bearing commits | 91 |
| Distinct tests | 5,935 |
| Requested window | 2025-10-13 to 2026-10-13 |
| Observed result window | 2026-08-25 to 2026-09-22 |
| Active acquisition gaps after the resume | 21 |

The shortfalls stay visible:

- 91 result-bearing commits is below the target of 300.
- The results that survive provenance checks cover about one month of the twelve requested. Only runs whose retained artifact contains the named report, and whose provenance verifies, produce results.
- 1,295,955 raw cases are excluded because their tested revision could not be verified.

A replay of the same window is idempotent: the resume added 0 raw cases, 0 factual results, 0 artifacts, 0 commits and 0 tests.

### Label yield and the exclusion accounting

`hazard labels build --before 2026-10-13`, `hazard labels flaky --before 2026-10-13` and a repeated build return the same build ID, so the commands converge. `hazard labels stats`, verbatim:

```
labels stats: build_id=336aa176cff9c2cdc6eeb888c6c275a0947a23b17c7a7aac4c9ebcae1cfdc705 freshness=current
cutoff: 2026-10-13T00:00:00Z
algorithm_version: labels/1
input_digest: 532e1aef23ab56a378beceb940370c0c333b6f8eb767fa55b370034ae8c412e0
decision_digest: 1b3adf4e68d11df8275e451d469bf7e8fe73792b562d8703d70cf6d619e3aef3
raw_failing_cases=14 grouped_member_count=0 unresolved_raw_members=14 unresolved_pending=9 unresolved_excluded=5
raw_candidates=0 excluded_candidates=0 pending_candidates=0 weak_candidates=0 strong_candidates=0
candidate_reason_counts (overlapping, not additive): none
unresolved_reason_counts (overlapping, not additive): execution_error=5 missing_environment=14 missing_identity=5 missing_tested_revision=14
strong_commits_by_repository: none
always_run_tests (0):
acquisition_exclusions_by_reason: ambiguous_producer=4 no_matching_report=3 provenance_unverified=251 unsupported_test_identity=5 unverified_tested_revision=1295955
active_gaps=21
```

The accounting conserves:

- 14 raw failing cases are 0 grouped plus 14 unresolved, and the 14 unresolved are 9 pending plus 5 excluded.
- 0 candidates are 0 raw, 0 excluded, 0 pending, 0 weak and 0 strong.
- The acquisition exclusions are 4 + 3 + 251 + 5 + 1,295,955, which is 1,296,218 rows.

All 14 failing cases were read by hand, since no strong label exists to audit:

- The 9 `fail` cases are in `test_async_handler_manager` and `test_sync_handler_manager`. They are handler-invocation count assertions (`assert 1 == 5`, `assert 99 == 100`) and a timeout (`Timed out waiting for condition`) that recur across unrelated runs, which is the signature of timing flakiness, not of a regression. Every one comes from an artifact whose tested revision could not be verified, so it cannot be compared with a parent run.
- The 5 `error` cases are collection failures (`ImportError while importing test module`) in `tests.unit.test_service_helper`, five artifacts of one run. They are execution errors with no test identity.

No case is a plausible regression with verifiable parent evidence. The zero is consistent with the raw data.

### Calibration on these labels

```
hazard calibrate --alpha 0.01 --delta 0.05 --split temporal --split-at 2026-10-13 \
  --predictor logistic --grid 0.005:0.500:0.005 --unit commit --out hazard.lock
```

Exit 1, empty standard error, no lock before or after:

```
calibrate: refused: need n >= 299, have 0
label_build: build_id=336aa176cff9c2cdc6eeb888c6c275a0947a23b17c7a7aac4c9ebcae1cfdc705 cutoff=2026-10-13T00:00:00Z
excluded_units: none
```

The refusal is the recorded calibration outcome. [GUARANTEE.md](GUARANTEE.md) explains the requirement of 299.

## The synthetic target: prometheus/common v0.73.0

The real corpus has no labels to evaluate selection with, and its language is Python while the deterministic selector was built and measured for Go. So the evaluation target is separate from the preregistered cohort, and it is not one of the five candidates: prometheus/common at commit `983d50981d14d136bf5770ede264415d6d56316b` (tag v0.73.0).

It was chosen because it is public, has several tested packages (`config`, `expfmt`, `model`, `route`, `server`, `version`, `promslog` and helpers), so the reverse import graph has real edges, and its test suite is fast enough for a mutation campaign. Its config tests read YAML fixtures under `config/testdata`, so configuration mutants have killers that the import graph cannot see. BurntSushi/toml was rejected because it has one tested package, so every Go killer would be reachable at hop 0 and the graph comparison would be empty.

Python is disabled for the target through `.hazard/hazard.yml` (`languages: python: enabled: false`), since the repository has no Python.

### Index

With `hazard index --coverage` at the base commit, the index for synthetic (mutation labels on prometheus/common v0.73.0) holds 201 tests in 9 packages, and all 201 have a coverage record (0 failed, 0 not run).

### Mutation campaign

The campaign for synthetic (mutation labels on prometheus/common v0.73.0), run at the base commit:

```
hazard -v mutate --base 983d50981d14d136bf5770ede264415d6d56316b \
  --files '**/*.go,config/testdata/**' --n 1200 \
  --operators cmp,logic,const,ret,stmt,cfg --timeout 120s --concurrency 4 \
  --out .hazard/mutants.jsonl
```

The campaign on synthetic (mutation labels on prometheus/common v0.73.0) found 6,917 candidate mutants and executed 1,200 of them. The baseline was 201 tests and two passing baseline runs, and 713 mutants were usable (killed by at least one test). The statuses were:

```
build_error=84 killed=713 survived=394 timeout=7 unconfirmed=2
```

Statuses by operator for synthetic (mutation labels on prometheus/common v0.73.0), computed from the mutant records:

| Operator | build_error | killed | survived | timeout | unconfirmed | total |
|---|---|---|---|---|---|---|
| cfg | 0 | 9 | 2 | 0 | 0 | 11 |
| cmp | 0 | 151 | 40 | 2 | 0 | 193 |
| const | 51 | 152 | 148 | 1 | 0 | 352 |
| logic | 2 | 201 | 51 | 1 | 0 | 255 |
| ret | 8 | 64 | 118 | 1 | 0 | 191 |
| stmt | 23 | 136 | 35 | 2 | 2 | 198 |

Nine killed mutants have killers that are unreachable in the L1 import graph. All nine are `cfg` mutants of YAML fixtures under `config/testdata`, whose single killer test is unreachable because a fixture file belongs to no Go package. The default `**/testdata/**` full-suite trigger covers exactly this class.

### Partitions

Each mutant is assigned to train, tune or test by a hash of the mutated file, so no file appears in two partitions. The partitions of synthetic (mutation labels on prometheus/common v0.73.0), after `hazard train --before 2026-10-28`:

| Partition | build_error | killed | survived | timeout |
|---|---|---|---|---|
| train | 53 | 340 | 212 | 1 |
| tune | 11 | 185 | 108 | 4 |
| test | 20 | 188 | 74 | 2 |

The 262 events of the test partition of synthetic (mutation labels on prometheus/common v0.73.0), which are the 188 killed and the 74 surviving mutants, are the population of the ablation in the [README](../README.md#synthetic-ablation). Of those events, 188 have a failing test, and 2 are configuration edits.

### Training found nothing for the predictors to learn

`hazard train` built 540 training events and 99,252 examples from synthetic (mutation labels on prometheus/common v0.73.0), with 0 positives. Every one of the 1,084 failing tests of those events was already a member of the CERTAIN set, so no residual candidate was ever a killer. Both the static and the logistic fit were refused (`fit set has no positive examples`), and nothing was written. With the variant configuration below, the same holds: 100,300 examples, 0 positives and 1,091 failing tests, all already selected.

### Variant without the full-suite trigger on test data

For synthetic (mutation labels on prometheus/common v0.73.0), a second evaluation removes `**/testdata/**` from `repo.full_suite_triggers`, with its own index, to check how much the trigger contributes. Fractions of 1 with 95% bootstrap intervals (2000 resamples):

| configuration | R_c | R_f | selection ratio | time ratio | abstention | effective saving |
|---|---|---|---|---|---|---|
| B1 L1 graph only | 1.000 [1.000, 1.000] | 1.000 [1.000, 1.000] | 0.372 [0.351, 0.393] | 0.231 [0.203, 0.260] | 0.000 [0.000, 0.000] | 0.739 [0.710, 0.768] |
| B2 L1 + coverage | 1.000 [1.000, 1.000] | 1.000 [1.000, 1.000] | 0.105 [0.085, 0.126] | 0.147 [0.109, 0.186] | 0.000 [0.000, 0.000] | 0.815 [0.777, 0.852] |

The two configuration events of the test partition are still caught without the trigger, because a fixture under `config/testdata` maps to the tests of the config package (the fixture rule). Two events say little about configuration-only edits in general.

### How the first measurement was corrected

An earlier measurement of synthetic (mutation labels on prometheus/common v0.73.0) put B2 at a per-commit recall of 0.963 [0.934, 0.989] on the same 262 test events, while B1 was 1.000. Of the 188 killed mutants of the test partition, 8 had no killer whose coverage blocks contain the mutated line, and in all 8 the line is in no coverage block of any test. Seven were constant mutants of a package-level initializer, which Go coverage does not instrument, and one was a configuration mutant already sent to the full suite by the trigger. Coverage mode had read "no block" as "no dependent test", which treats an unknown as zero.

The fix treats a changed line outside every test's coverage blocks as unknown, so the unbounded reverse-graph rule applies to that file. After the fix, B2 reaches 1.000. The blind-spot accounting after the fix for synthetic (mutation labels on prometheus/common v0.73.0), killed mutants whose line is in no coverage block of any test, is 14 of 340 in train, 2 of 185 in tune and 8 of 188 in test. The outcome therefore depends on that rule, and the rule is covered by tests that fail when it is removed.

## What this corpus does and does not support

- It supports a statement about label yield: on one real repository, with 91 result-bearing commits, there were 0 strong labels, so nothing can be calibrated.
- It supports a statement about the deterministic selector on first-order mutants of one Go repository, with the intervals above.
- It does not support any statement about Python repositories, about other Go repositories, about real regressions, or about the predictors, which found nothing to learn.
- The real-CI rows of the ablation read N/A, because there are no strong units. They are not zero.
