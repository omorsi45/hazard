# Runner adapters

Hazard narrows a test run by driving the project's own runner. It has two adapters, `go test` and pytest. This page describes how each one turns a selection into native commands, what it does when the selection cannot be expressed, and which arguments you may pass through. It is written for people who run `hazard run` in CI or who write a new adapter.

`hazard run --runner auto` runs every enabled language, Go first and then Python. `--runner gotest` and `--runner pytest` restrict the run to one adapter. A narrowed run executes only the adapters that have selected tests, and a full-suite decision executes every enabled adapter's full suite (pytest is left out when the repository holds no Python file, see [Configuring a single-language repository](#configuring-a-single-language-repository)).

## Versions

CI runs Go 1.26.6, Python 3.11, pytest 8.4.2 and coverage 7.10.7.

| Component | Version |
|---|---|
| Go, in CI and in `go.mod` | 1.26.6 |
| Python in CI | 3.11 |
| pytest in CI | 8.4.2 |
| coverage.py in CI | 7.10.7 |

Other versions are not tested.

## Behavior common to both adapters

- **The exit code is the runner's.** `hazard run` exits with the first nonzero exit code of the processes it started. It never exits 0 after an error, and an internal failure of an adapter exits 70.
- **A JUnit report is written** to `--junit` (default `.hazard/junit.xml`). A report left by an earlier run is removed first, so a failed run cannot be read as the previous run's result. When both adapters run, their reports are merged into one `<testsuites>` file, and nothing is published unless both adapters wrote theirs.
- **An adapter that cannot start, or that ends in an internal error, widens its job to its full suite once.** The telemetry records the reason as `internal_error`. With `--no-fallback`, the error ends the command instead.
- **A selected test that cannot be expressed to its runner** (a missing package or runner ID, an unknown language, a disabled language) runs the full suite for the whole command, with the reason `runner_limit`.
- **Selection never runs Python.** The index, built by `hazard index`, holds everything selection needs. See [DESIGN.md](DESIGN.md).

## The go test adapter

### Identity and discovery

A Go test is identified as `go:<import path>:<TestName>`. Tests are discovered statically from the files that `go list` reports for the configured build tags, and the inventory is rebuilt by `hazard index`. A test that is defined in a changed Go test file is rediscovered at the selection head, so a new Go test is never dropped.

### Anchored, escaped `-run` patterns

A narrowed run starts one `go test -json` process per package, in import-path order. Each process gets an anchored alternation of the selected top-level test names:

```
go test -json -run '^(TestRefreshNearExpiry|TestTokenGrace)$' example.com/m/internal/auth
```

- Each name is escaped with `regexp.QuoteMeta`, then sorted and deduplicated.
- The pattern is anchored with `^(...)$`. Without the anchors, `TestFoo` also matches `TestFooBar`.
- `languages.go.tags` become `-tags=<tag,tag>`, and the arguments after `--` follow, both before the package argument.
- A full run is one process over `languages.go.packages` (`./...` by default).

### Subtests widen to their top-level test

`-run` addresses a subtest as `Parent/Sub`, and the separator requires the parent in the pattern. Hazard selects at top-level granularity. A selected subtest ID such as `TestAdd/adds_small_numbers` becomes the pattern for `TestAdd`, so the whole top-level test and all its subtests run. That is a superset of the selection, which is the safe direction.

### Package widening under the Windows command-length budget

Windows limits a command line to about 32,000 characters. Hazard keeps the pattern of one process at or below 8,000 characters. When a package's anchored pattern is longer, the package runs whole, with no `-run` argument, and its import path is reported as widened.

| Pattern length | Command |
|---|---|
| 8,000 characters | stays anchored |
| 8,001 characters | runs the whole package |

The tests in `internal/runner` cover this at three levels:

- `TestGoInvocationsPatternLengthBoundary` checks the exact boundary above.
- `TestRunGoFiveHundredTestsWidenToTheWholePackage` generates a package of 500 tests with 40-character names, selects all of them, and checks first that the plan has no `-run` argument and lists the package as widened. It then runs the package natively and requires 500 passing cases.
- `TestRunGoThreeOfFiveHundredTestsStayAnchored` selects 3 of those 500 tests and requires one anchored process that executes exactly those 3.

These tests pass in the Windows and the Linux gates of this branch.

### Exit passthrough and the JUnit report

- Every process runs, even after one fails. The result's exit code is the first nonzero code, and a package that does not build is nonzero, never green.
- The JUnit report is built from the `go test -json` stream. An output tail of at most 8 KiB is kept per test and per package. A test that never reported a result is an error, a suite with no package result is incomplete, and a package whose build failed cannot parse as green.
- The report is written atomically, and only when every process ran to completion.

### A selected test that did not run

`go test -run` prints `ok ... [no tests to run]` and exits 0 when the pattern matches nothing, so a selected test that was renamed or removed since the index was built would pass unnoticed. After a narrowed run, `hazard` compares the selected top-level tests with the tests that have a `run` event in the `go test -json` stream. If any selected test never started, the adapter reports an abstention with the reason `go_missing_tests` and the IDs of the missing tests, `hazard run` warns and runs the full Go suite once, and the telemetry records `runner_limit`. A package that does not build, a test binary that panics and a `-failfast` stop before the selected test also leave tests unstarted, so they take the same path. The report of the narrowed attempt is written first and the full run replaces it.

## The pytest adapter

### Identity

A pytest test is identified in history as `py:<classname>:<name>`. The runner ID is the pytest node ID, for example `tests/test_x.py::Class::test_a[p]`. A name that contains an object representation or a hexadecimal address is marked unstable, which keeps it in the always-run set.

### An embedded selection plugin and node IDs in a file

Node IDs never travel on the command line. Parametrized IDs contain characters that break quoting in different shells, and large selections exceed command-length limits. Hazard instead writes the plugin and the selection to disk:

```
python -m pytest -p hazard_pytest --junitxml=<report> <pytest_args> <testpaths> <passthrough>
```

- The plugin is embedded in the binary (`internal/runner/plugin/hazard_pytest.py`) and written to `.hazard/hazard_pytest.py` when the file on disk is not current. `PYTHONPATH` gets `.hazard` in front.
- `HAZARD_SELECTION` names a UTF-8 file with one node ID per line, sorted and deduplicated. An ID with a line break, a NUL byte or invalid UTF-8 is rejected.
- A full run sets `HAZARD_SELECTION` to the empty string, so a value inherited from the environment cannot narrow it.
- `languages.python.pytest_args` (default `-q`) and `languages.python.testpaths` (default `tests`) come from `hazard.yml`.

### Missing IDs abstain before any test runs

If a selected node ID matches no collected item, the suite has changed since the index was built (a rename or a removal), and the selection cannot be trusted. The plugin writes the unmatched IDs to the file named by `HAZARD_MISSING_OUT` and stops with exit code 4 before a test executes. The adapter reports the abstention with the reason `pytest_missing_ids` and the sorted list of missing IDs, whatever the exit code. `hazard run` warns and runs the full pytest suite once, and the telemetry records `runner_limit`.

### Deselection reporting

Tests that are not selected are reported to pytest through the `pytest_deselected` hook, so the pytest summary shows them as deselected and not as missing.

### Python analysis

Python runs only inside `hazard index`, for pytest collection and for coverage.py per-test contexts. At selection time, imports come from a constrained parser written in Go. These cases send the whole suite:

- a changed Python test module whose test names are not all in the inventory (`python_inventory_changed`), because parametrized node IDs cannot be derived without collection;
- a changed Python file in a package with a dynamic or unresolvable import (`python_dynamic_import:<package>`).

## Passthrough rules

Arguments after `--` go to every adapter that runs, and none of them may narrow the tests. A violating argument ends the command with a usage error before anything runs. Hazard checks this because a passthrough filter would silently turn a full run into a partial one, and a selection into a smaller one.

| Adapter | Rejected |
|---|---|
| go test | `-run`, `-skip`, `-list`, `-bench`, `-c`, `-n`, `-o`; any positional argument such as a package pattern; the same narrowing flags after `-args` |
| pytest | `-k`, `-m`, `--deselect`, `--lf`, `--last-failed`, `--ff`, `--failed-first`; collection-only and listing options such as `--collect-only`, `--setup-only` and `--fixtures`; `-h`, `-V`; any positional argument; anything after `--` |

Other flags pass through unchanged. `-race`, `-count=1` and `-vet=off` are accepted for Go, and `-x`, `-vv` and `--maxfail=1` for pytest.

## Configuring a single-language repository

Both languages are enabled by default, and a Go-only repository needs no configuration. Under `--runner auto`, a full-suite run leaves the pytest job out when Go still runs and git lists no Python file in the repository (`git ls-files -co --exclude-standard -- '*.py'`, ignoring `.hazard/`: tracked files and untracked files that are not ignored). `hazard run` then prints the warning `python_absent` and runs Go alone. If git cannot be asked, or a single Python file exists, pytest runs as before, so a Python project whose `languages.python.testpaths` is wrong stays red with `file or directory not found`. `--runner pytest` always runs pytest, and a narrowed run is not affected: it runs only the adapters that have selected tests.

A Python-only repository should disable Go in `hazard.yml`, with `languages.go.enabled: false`. A Go-only repository can still disable Python explicitly to skip the check:

```yaml
version: 1
languages:
  python:
    enabled: false
```
