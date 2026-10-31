# What Hazard guarantees, and what it does not

This page states the claim Hazard is built to make, the procedure that would back it, why no such claim is certified today, and the limits that would apply if one were. It is written for people deciding whether to trust a narrowed test run.

## Where things stand

Two different kinds of guarantee exist, and only one is in force.

**Structural guarantee, in force.** The selection is a set union: the deterministic floor, the always-run tests, any predictor additions, and any widening tier. No predictor answer removes a test the floor selected. Every unknown, failure or malformed value degrades to the full suite, never to a smaller selection. This is checked by tests that remove each guard and require the test to fail.

**Statistical guarantee, not certified.** The design calls for a per-commit bound on missing a genuine regression, fitted offline on real CI labels. The fitting command, `hazard calibrate`, is implemented and tested. On the one real corpus that qualified for measurement, Azure/azure-iot-sdk-python, it refuses:

```
calibrate: refused: need n >= 299, have 0
```

It exits 1 and writes no `hazard.lock`. Without a valid lock the default decision is the full suite, with the abstain reason `no_lock`, and `--shadow` shows what narrowing would select. Hazard therefore claims no recall bound.

## The quantity being controlled

The unit is the commit, not the (commit, test) pair. One commit's tests are not independent of each other, and "did this commit's regression reach the default branch" is the operational question.

For a commit `c`, let `F(c)` be its genuine regressions and `S(c, lambda)` the selected set at threshold `lambda`:

```
L(c, lambda) = 1  if F(c) is not empty and F(c) is not a subset of S(c, lambda)
             = 0  otherwise
```

The target is `R(lambda) = E[L(c, lambda)] <= alpha`, with alpha 0.01 by default. The selection at threshold `lambda` is the CERTAIN set plus every residual candidate whose predicted probability is at least `lambda`. Lowering `lambda` only adds tests, so the loss is monotone in `lambda`, and that monotonicity is what allows fixed-sequence testing.

The loss is conditional on regression-bearing commits. Only commits that a strong CI regression label blames enter the denominator, per repository. Labels from mutation never enter calibration. A recall estimate that mixes in commits without a regression would measure a different quantity.

## The procedure: Learn-then-Test

`hazard calibrate` follows the Learn-then-Test framework (Angelopoulos, Bates, Candes, Jordan and Lei, arXiv:2110.01052) with a Hoeffding-Bentkus p-value:

1. For each `lambda` in a grid (default `0.005:0.500:0.005`), compute the empirical miss rate `Rhat(lambda)` over the calibration commits. A commit's loss is computed by running the same controller that `hazard select` uses, abstentions included. An abstention runs the full suite and cannot miss, so it has loss 0. A regression that is neither certain nor a scored candidate counts as a miss.
2. For each `lambda`, compute the p-value for the hypothesis `R(lambda) > alpha`:

   ```
   p = 1                                                     if Rhat >= alpha
   p = min( exp(-n * KL(Rhat || alpha)),
            e * P(Bin(n, alpha) <= ceil(n * Rhat)) )         otherwise
   ```

   The branch `p = 1` at `Rhat >= alpha` matters. The Kullback-Leibler term is positive on both sides of alpha, so the un-clipped expression becomes small for large losses and would certify a threshold whose empirical loss is above the target.
3. Walk the grid from the smallest `lambda` (the most conservative, since it selects the most tests). Reject each hypothesis while `p <= delta`, and stop at the first `lambda` that is not rejected. The threshold is the largest `lambda` in the rejected prefix.
4. If the rejected prefix is empty, write no lock and exit 1. A tool that always produces a threshold is a tool whose threshold means nothing.
5. Otherwise write the threshold, `n`, the empirical miss rate, the full p-value map, and every identity the threshold depends on into `hazard.lock`.

### The minimum sample size

With zero misses, the p-value is `0.99^n` at alpha 0.01, and the smallest `n` with `0.99^n <= 0.05` is 299. `MinCalibrationN(0.01, 0.05)` is 299 in `internal/control`, and the refusal prints it:

```
calibrate: refused: need n >= <need>, have <have>
```

A smaller alpha or delta needs more units, and a larger alpha needs fewer: at alpha 0.05 and delta 0.05 the requirement is still 59 commits. No alpha certifies with zero labels.

### What a lock binds

A lock is valid only for the system that produced it. The digest covers every field, and the engine refuses the lock, and runs the full suite, when any of these differ from the running system:

| Lock field | Refusal reason |
|---|---|
| digest does not match the content | `lock_corrupt` |
| model release (`model_version`) | `model_drift` |
| rubric version | `rubric_mismatch` |
| feature version or weights digest | `feature_mismatch` |
| selection policy digest (candidate cap, hops, triggers, redaction level, minimum saving, predictor kind) | `policy_mismatch` |

The policy is part of the calibration input, so changing the candidate cap or the redaction level voids a lock calibrated without it.

## Why nothing is certified today

The measured inputs (details in [docs/CORPUS.md](CORPUS.md)):

- Azure/azure-iot-sdk-python yielded 2,401,373 factual results on 91 result-bearing commits, with 14 raw failing cases out of 2,880,686 raw test cases.
- None of the 14 became a strong label: 0 grouped candidates and 0 strong regression-bearing commits.
- The corrected Hoeffding-Bentkus test needs `n >= 299` at alpha 0.01 and delta 0.05, so with 0 units the lock cannot be written.

The options that were considered:

- A weaker alpha does not help. At alpha 0.05 and delta 0.05 the requirement is 59 commits, and no alpha certifies with zero labels.
- A larger corpus is not available. The other four preregistered candidates showed no retained JUnit artifacts in a bounded observation pass, and the one repository that has them yields no strong labels over the retained month.
- Dropping the formal guarantee as the headline is what was decided. Hazard ships as a conservative deterministic selector with explicit abstention.

## Scope of the claim

If a lock is ever written, this is its scope, verbatim:

> the guarantee is over genuine regressions as labeled by the mining pipeline, on the repository it was calibrated against, under the model and rubric versions in the lock, and assuming commits behave like the calibration window. It does not cover renamed tests, tests that have never run, regressions invisible to the suite, or the period after a model version change.

Several consequences follow.

- **Labels define the claim.** A regression the mining pipeline did not label is not in the denominator. Flaky tests are excluded from eligible recall, and the raw accounting of excluded candidates is published next to it.
- **One repository.** Counts are kept per repository, and labels pooled across repositories do not certify any one of them.
- **Identity.** A renamed test, or a test that has never run, has no history to select it on.
- **Invisible regressions.** A regression the suite cannot observe is invisible to selection and to the full run alike.

## Exchangeability and the temporal backtest

The finite-sample guarantee needs calibration and future commits to be independent draws from the same population. Commit history is not. It has trend, refactors that reshape the dependency graph, and a model version that moves. A random split of dependent history does not create independent data.

Two numbers must therefore be read side by side:

| Number | How it is produced | What it means |
|---|---|---|
| Learn-then-Test bound | random split of the labeled commits (`--split random`) | valid under independence, optimistic to the extent history is non-stationary |
| temporal backtest | calibrate on commits before `--split-at`, evaluate on later ones (`--split temporal`, the default) | the realistic estimate, and the number to believe |
| the gap | the difference | a measurement of what drift costs |

The random split exists to produce the formal bound. The temporal split sets production policy. Today neither exists for real labels, because there are no units to split.

Predictors are fitted on training data that is kept apart from the risk-calibration window, and the units a predictor was trained on are dropped from calibration. The threshold grid and the selection policy are frozen before calibration, and nothing is retuned on the held-out evaluation set.

## Drift monitoring

After a lock is in use, a monitor watches the field miss rate. Observations come only from `hazard audit`: an audited, regression-bearing selection made under the current lock, and whether it missed a failure that the full run found. Unaudited runs are never counted as successes.

- The monitor keeps the last `control.drift.window_runs` observations under the current lock (500 by default). A new lock starts from nothing.
- Below the minimum sample size (299 at the default alpha and delta) the state is `warming`. A Clopper-Pearson upper bound with few observations is high even when nothing drifted, so warming does not by itself invalidate a valid lock. A confirmed miss alarms immediately, because a miss is evidence on its own.
- From the minimum sample size on, the monitor alarms when the one-sided Clopper-Pearson upper confidence bound on the field miss rate exceeds alpha.
- An alarm makes every later decision the full suite, with the abstain reason `drift`, until a recalibration writes a new lock.

This is an operational alarm. Repeated rolling checks are not a time-uniform certification.

## The nightly full suite is part of the claim

A narrowed run is only safe in a deployment where the full suite still runs:

1. Run in `--shadow` mode first. The decision is the full suite and the hypothetical selection is recorded.
2. Select on feature branches only. The full suite still gates the default branch.
3. Run the full suite on every merge to the default branch and on a schedule (`audit.full_suite_on_merge_to_default`, and `audit.full_suite_schedule` with `0 3 * * *` by default). This bounds how long a miss can stay undetected, so a miss is a delay and not an escape.
4. Run `hazard audit` after every full run. It compares each recorded selection with the full run at the same commit, and with `--bisect` it can attribute a later failure to the commit where it first fails.

Hazard does not schedule anything itself. The templates in [docs/ci](ci/github-actions.yml) show the loop for GitHub Actions and GitLab CI. Any statement about recall without "and the full suite still runs on merge and nightly" is an overclaim.

## What the synthetic evaluation does and does not show

The evaluation of the deterministic selector is on synthetic (mutation labels on prometheus/common v0.73.0): 188 held-out events with a failing test, and 0 misses for the coverage-driven configuration. The exact one-sided 95% Clopper-Pearson upper bound on the miss rate is 0.01581.

That is an estimate with a bound on one repository's synthetic population, not a guarantee. Mutation labels never enter calibration, and a population of first-order mutants of one Go repository does not describe how commits behave elsewhere. The table is in the [README](../README.md#synthetic-ablation).
