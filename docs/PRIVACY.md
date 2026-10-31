# Privacy: what leaves the machine

This page is for people who must decide whether Hazard may run on a repository. It lists everything that can leave the machine, at each redaction level, with the real bytes. It also lists what Hazard keeps on disk.

## Summary

- The deterministic selector (L0 and L1), the local predictors, the controller, the runners and `hazard audit` make no network request. No source, path or test name leaves the machine through them.
- The only code that sends source-derived text is the Jev client, reached through the endpoint in `predictor.jev.endpoint`. `select`, `run`, `calibrate` and `eval` use it only when the predictor in force is `jev` or `ensemble` and a key is present in the environment variable named by `predictor.jev.api_key_env` (`OPENROUTER_API_KEY` by default). Without the key, `eval` reports the rows `needs OPENROUTER_API_KEY` and makes no request.
- `hazard ci-mine` is the only other command that uses the network. It calls the GitHub or GitLab API with the token in the variable named by `--token-env`, to download CI reports.
- `hazard select --dry-run-payload` prints the exact bytes the external predictor would receive and sends nothing. It needs no key.
- `privacy.allow_external_predictor: false` disables the external predictor completely. That is the supported air-gapped configuration.

Today no `hazard.lock` ships, so `hazard select` and `hazard run` decide with the full suite before any predictor is consulted. Nothing is sent in that state.

## The redaction levels

`privacy.redact` (or `--redact`) takes `none`, `structure` or `hash`. Any other value is rejected, and the redaction code treats an unknown level as `hash`. The default is `structure`.

| What | `none` | `structure` (default) | `hash` |
|---|---|---|---|
| Sent to the external predictor | yes | yes | nothing: the external predictor refuses to start |
| Changed file path and package | yes | yes | not applicable |
| Change kind, status, lines added and removed, 7-character base and head commit | yes | yes | not applicable |
| Declared symbol names the hunks touch | yes | yes | not applicable |
| Hunk summary | yes | yes | not applicable |
| Hunk text (the diff lines) | only with `privacy.send_hunk_text: true` | never | not applicable |
| Test name | yes, complete | yes, cut at the first `[` or `/` | not applicable |
| Test purpose | the first doc-comment sentence, or the name split into words | the test name split into lower-case words | not applicable |
| Test assertion targets (`asserts`) | yes, up to 5 | never | not applicable |
| Hops from the change and covered changed lines (counts) | yes | yes | not applicable |

Two flags narrow `none` and `structure` further. With `privacy.send_paths: false`, every path and package is replaced by a token `h_` followed by 16 hex characters (a truncated SHA-256). With `privacy.send_symbol_names: false`, declared symbols and test names are tokenized the same way, and the test purpose and assertion targets are omitted, because they name symbols.

### Why `structure` is narrower than it first looked

The first design sent the test purpose and assertion targets at `structure` as well. Assertion targets come from the test source and can contain literals, so `structure` now sends only the test name split into words and omits the assertions. Full purposes and assertions are sent only at `none`.

### The local hunk summary

The `hunk_summary` is generated on the machine from counts, the names of declared symbols, and fixed phrases for the kinds of token that changed (comparison operator, logical operator, arithmetic operator, literal, identifier, control flow, comment, whitespace, reordering, punctuation). It never carries source text, a literal value, a comment, or an identifier other than a declared symbol name. In the payloads below it reads `1 line changed in Port: literal changed`.

## Real payloads

These come from `hazard select --dry-run-payload` on a copy of the Go fixture repository in `testdata/repos/go`, committed to a temporary git repository and indexed with `hazard index --coverage`. The change is one line in `server/server.go`, in the working tree:

```diff
-const Port = 8080
+const Port = 9090
```

The copy's `hazard.yml` is:

```yaml
version: 1
privacy:
  redact: none
  send_hunk_text: true
  send_symbol_names: true
  send_paths: true
  allow_external_predictor: true
```

`--predictor jev` makes the Jev client the active predictor, which a dry run needs when no lock exists. `--max-candidates 2` keeps the payload short: without it, the same change sends ten test descriptors and ten questions. The abbreviated commit `94e5355` is the fixture copy's commit and differs on every run.

### Level `structure`

```
hazard select --base main --dry-run-payload --predictor jev --redact structure --max-candidates 2
```

```
{"requests":[{"model":"typesafe/jev-1.13","state":{"rubric":"A test BREAKS when the change alters any behavior the test observes, directly or indirectly. Indirect paths count: configuration defaults, environment variables, shared fixtures or testdata, serialized formats, error message text, exported constants, timing and ordering, database schema, and generated code. A test does NOT break merely because it lives near the change or imports the same package. Answer about a pass-to-fail transition, not about whether the change is correct.","change":{"base":"94e5355","head":"94e5355","lines_added":1,"lines_removed":1,"files":[{"path":"server/server.go","kind":"source","status":"M","package":"example.com/hazardfixture/server","hunk_summary":"1 line changed in Port: literal changed","symbols":["Port"]}]},"tests":{"t_001":{"name":"TestAddSubtestsWithSpaces","package":"example.com/hazardfixture/core","purpose":"add subtests with spaces","hops_from_change":99,"covers_changed_lines":0},"t_002":{"name":"TestFoo","package":"example.com/hazardfixture/core","purpose":"foo","hops_from_change":99,"covers_changed_lines":0}}},"questions":{"t_001":{"type":"noul","instructions":"Applying state.rubric to state.change, could the change cause state.tests.t_001 to go from passing to failing?","criteria":{"false":"This test cannot observe any behavior the change alters.","true":"There is a plausible causal path from the change to behavior this test observes."}},"t_002":{"type":"noul","instructions":"Applying state.rubric to state.change, could the change cause state.tests.t_002 to go from passing to failing?","criteria":{"false":"This test cannot observe any behavior the change alters.","true":"There is a plausible causal path from the change to behavior this test observes."}}}}]}
```

The output is 1,786 bytes. Even though this copy sets `send_hunk_text: true`, there is no `hunk_text` and no `asserts`. The test `purpose` is the test name as words. A `hops_from_change` of 99 means the test is not reachable from the changed package.

### Level `none`

```
hazard select --base main --dry-run-payload --predictor jev --redact none --max-candidates 2
```

```
{"requests":[{"model":"typesafe/jev-1.13","state":{"rubric":"A test BREAKS when the change alters any behavior the test observes, directly or indirectly. Indirect paths count: configuration defaults, environment variables, shared fixtures or testdata, serialized formats, error message text, exported constants, timing and ordering, database schema, and generated code. A test does NOT break merely because it lives near the change or imports the same package. Answer about a pass-to-fail transition, not about whether the change is correct.","change":{"base":"94e5355","head":"94e5355","lines_added":1,"lines_removed":1,"files":[{"path":"server/server.go","kind":"source","status":"M","package":"example.com/hazardfixture/server","hunk_summary":"1 line changed in Port: literal changed","symbols":["Port"],"hunk_text":"@@ -6,1 +6,1 @@\n-const Port = 8080\n+const Port = 9090\n"}]},"tests":{"t_001":{"name":"TestAddSubtestsWithSpaces","package":"example.com/hazardfixture/core","purpose":"Test Add Subtests With Spaces","asserts":["got != tc.want"],"hops_from_change":99,"covers_changed_lines":0},"t_002":{"name":"TestFoo","package":"example.com/hazardfixture/core","purpose":"Test Foo","asserts":["got != 3"],"hops_from_change":99,"covers_changed_lines":0}}},"questions":{"t_001":{"type":"noul","instructions":"Applying state.rubric to state.change, could the change cause state.tests.t_001 to go from passing to failing?","criteria":{"false":"This test cannot observe any behavior the change alters.","true":"There is a plausible causal path from the change to behavior this test observes."}},"t_002":{"type":"noul","instructions":"Applying state.rubric to state.change, could the change cause state.tests.t_002 to go from passing to failing?","criteria":{"false":"This test cannot observe any behavior the change alters.","true":"There is a plausible causal path from the change to behavior this test observes."}}}}]}
```

The output is 1,920 bytes. Compared with `structure`, it adds the diff lines as `hunk_text` and the assertion targets as `asserts`, and the `purpose` is the complete test name. With `send_hunk_text: false`, the default, `hunk_text` is absent.

### Level `hash`

```
hazard select --base main --dry-run-payload --predictor jev --redact hash --max-candidates 2
```

Standard output:

```
{"requests":[]}
```

Standard error:

```
warning: dry run: no payload can be built: privacy.redact is "hash": the external predictor needs "none" or "structure"
warning: no external predictor in the active policy; nothing would be sent
```

At `hash`, the external predictor is not built, so no request exists to print. The level is for runs that must stay local: the deterministic selector still works, and nothing is sent.

### With the external predictor disabled

With `privacy.allow_external_predictor: false` in `hazard.yml` (and an index built under that configuration), the same command prints:

```
{"requests":[]}
```

```
warning: dry run: no payload can be built: privacy.allow_external_predictor is false: the external predictor is disabled
warning: no external predictor in the active policy; nothing would be sent
```

## What happens when the predictor cannot be used

Hazard does not swap in another predictor silently. If a lock names `jev` or `ensemble` and the key is missing, the redaction level is `hash`, or `allow_external_predictor` is false, the locked predictor is unavailable and the decision is the full suite with the abstain reason `predictor_unavailable`. A different predictor would need its own lock, because a lock binds a threshold to one predictor and one policy. An offline lock from a local predictor (`static` or `logistic`) works with no network.

## The key

The key is read from the environment variable named in `predictor.jev.api_key_env`. It is sent only in the request header. It is never written to telemetry or to the judgment cache, it is scrubbed from warnings, and the client prints it as `[redacted]`. The client does not follow redirects, so the key cannot travel to another host. `--dry-run-payload` uses a placeholder and never reads the key.

## Cost control

Each request shard is sent only if its conservative cost estimate still fits in the remaining budget (`--budget-usd`, or `predictor.jev.budget_usd_per_run`, 0.01 by default). When it does not, the shard is skipped, and all of its tests are selected (`budget_exhausted`). The vendor reports the actual cost after the request completes, so the estimate cannot be an exact ceiling on billing for requests already in flight.

## What stays on disk

Everything below is under `.hazard/` in the repository, which is Hazard's own state and is excluded from change sets.

- **Judgment cache** (`.hazard/hazard.db`, table `judgments`). One row per answered question: `cache_key`, `test_id`, `p_break`, `predictor`, `model_version`, `rubric_version`, `latency_ms`, `cost_usd`, `created_at`. The key is a SHA-256 over the model release, the rubric version, the feature version, a hash of the change block as sent and a hash of the test descriptor as sent. Neither payload text nor hunk text is stored.
- **Telemetry** (`.hazard/runs.jsonl`, one JSON line per selection or run). It holds test IDs, selection sources, probabilities, counts, timings, cost and the abstain reason. It never holds payload bytes, hunk text, test purposes, assertion targets, warnings or keys. A real record from the fixture above:

  ```
  {"id":"6549b9f81ce8b505ae6cd8e313f3b4f5","time":"2026-10-31T03:27:26Z","commit":"94e5355f5838ee7b31ff68df03f43a0964b3eaaf","mode":"select","full_suite":true,"abstain_reason":"no_lock","lambda":0,"lock_digest":"","n_total":11,"n_certain":1,"n_added":0,"n_always_run":0,"n_tier":0,"members":[{"test_id":"go:example.com/hazardfixture/server:TestDefaultPortMatchesConfig","source":"certain"}],"est_ms_selected":0,"est_ms_full":0,"overhead_ms":248,"cost_usd":0,"hazard_version":"0.1.0-dev"}
  ```

  Set `telemetry.enabled: false` to write none.
- **Audit records**: `.hazard/audits.jsonl` and the `field_misses` table in `.hazard/hazard.db`, written by `hazard audit`.
- **Index**: `.hazard/index/manifest.json` holds the file list with content hashes, the dependency graph, the test inventory (including each test's purpose and assertion targets, read from your source) and per-test coverage blocks. It contains source-derived text. Treat it like the working tree.

`hazard select --json` prints the candidate tests with their purposes and assertion targets to standard output on your machine. That output is local. It is not what a predictor receives, and the level above decides what the predictor receives.
