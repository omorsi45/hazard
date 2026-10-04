CREATE TABLE tests (
  id TEXT PRIMARY KEY,
  lang TEXT NOT NULL CHECK (lang IN ('go', 'python')),
  package TEXT NOT NULL,
  name TEXT NOT NULL,
  file TEXT NOT NULL,
  first_seen TEXT NOT NULL,
  last_seen TEXT NOT NULL,
  median_ms INTEGER,
  runs INTEGER NOT NULL DEFAULT 0,
  fails INTEGER NOT NULL DEFAULT 0,
  flaky_score REAL NOT NULL DEFAULT 0.0,
  unstable_id INTEGER NOT NULL DEFAULT 0,
  quarantined INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE commits (
  sha TEXT PRIMARY KEY,
  parent_sha TEXT,
  authored_at TEXT NOT NULL,
  branch TEXT,
  files_changed INTEGER NOT NULL,
  lines_added INTEGER NOT NULL,
  lines_removed INTEGER NOT NULL,
  kinds TEXT NOT NULL
);

CREATE TABLE test_results (
  commit_sha TEXT NOT NULL,
  test_id TEXT NOT NULL,
  status TEXT NOT NULL CHECK (status IN ('pass', 'fail', 'error', 'skip')),
  duration_ms INTEGER,
  pipeline_id TEXT NOT NULL DEFAULT '',
  job_id TEXT NOT NULL DEFAULT '',
  attempt INTEGER NOT NULL DEFAULT 1 CHECK (attempt >= 1),
  observed_at TEXT NOT NULL,
  source TEXT NOT NULL DEFAULT '',
  project TEXT NOT NULL DEFAULT '',
  environment_id TEXT NOT NULL DEFAULT '',
  artifact_id TEXT NOT NULL DEFAULT '',
  artifact_path TEXT,
  raw_name TEXT,
  raw_classname TEXT,
  raw_message TEXT,
  infra_reason TEXT,
  PRIMARY KEY (source, project, commit_sha, test_id, pipeline_id, job_id, environment_id, attempt, artifact_id)
);
CREATE INDEX idx_tr_test ON test_results(test_id, observed_at);
CREATE INDEX idx_tr_commit ON test_results(commit_sha);

CREATE TABLE regressions (
  commit_sha TEXT NOT NULL,
  test_id TEXT NOT NULL,
  confidence TEXT NOT NULL CHECK (confidence IN ('strong', 'weak')),
  evidence TEXT NOT NULL,
  PRIMARY KEY (commit_sha, test_id)
);

CREATE TABLE excluded_labels (
  commit_sha TEXT NOT NULL,
  test_id TEXT NOT NULL,
  reason TEXT NOT NULL CHECK (reason IN ('flaky', 'infra', 'no_parent_run', 'skipped', 'env')),
  detail TEXT,
  PRIMARY KEY (commit_sha, test_id, reason)
);

CREATE TABLE mutants (
  id TEXT PRIMARY KEY,
  file TEXT NOT NULL,
  line INTEGER NOT NULL,
  operator TEXT NOT NULL,
  original TEXT NOT NULL,
  mutated TEXT NOT NULL,
  base_sha TEXT NOT NULL,
  status TEXT NOT NULL CHECK (status IN ('killed', 'survived', 'build_error', 'timeout')),
  created_at TEXT NOT NULL
);

CREATE TABLE mutant_kills (
  mutant_id TEXT NOT NULL,
  test_id TEXT NOT NULL,
  PRIMARY KEY (mutant_id, test_id)
);

CREATE TABLE judgments (
  cache_key TEXT PRIMARY KEY,
  test_id TEXT NOT NULL,
  p_break REAL NOT NULL CHECK (p_break >= 0 AND p_break <= 1),
  predictor TEXT NOT NULL CHECK (predictor IN ('static', 'logistic', 'jev', 'ensemble')),
  model_version TEXT NOT NULL,
  rubric_version TEXT NOT NULL,
  latency_ms INTEGER,
  cost_usd REAL,
  created_at TEXT NOT NULL
);

CREATE TABLE selections (
  id TEXT PRIMARY KEY,
  commit_sha TEXT NOT NULL,
  created_at TEXT NOT NULL,
  mode TEXT NOT NULL CHECK (mode IN ('select', 'run', 'shadow')),
  tier TEXT NOT NULL CHECK (tier IN ('targeted', 'module', 'suite')),
  abstained INTEGER NOT NULL CHECK (abstained IN (0, 1)),
  abstain_reason TEXT,
  lambda REAL NOT NULL,
  lock_digest TEXT NOT NULL,
  n_total INTEGER NOT NULL,
  n_certain INTEGER NOT NULL,
  n_added INTEGER NOT NULL,
  est_ms_sel INTEGER NOT NULL,
  est_ms_full INTEGER NOT NULL,
  overhead_ms INTEGER NOT NULL,
  jev_cost_usd REAL NOT NULL DEFAULT 0
);

CREATE TABLE selection_members (
  selection_id TEXT NOT NULL,
  test_id TEXT NOT NULL,
  source TEXT NOT NULL CHECK (source IN ('certain', 'added', 'tier', 'full')),
  p_break REAL,
  PRIMARY KEY (selection_id, test_id)
);

CREATE TABLE field_misses (
  id TEXT PRIMARY KEY,
  selection_id TEXT NOT NULL,
  test_id TEXT NOT NULL,
  detected_at TEXT NOT NULL,
  full_run_id TEXT NOT NULL,
  blame_window TEXT NOT NULL,
  attributed INTEGER NOT NULL
);

CREATE TABLE calibrations (
  id TEXT PRIMARY KEY,
  created_at TEXT NOT NULL,
  alpha REAL NOT NULL,
  delta REAL NOT NULL,
  lambda REAL NOT NULL,
  n_cal INTEGER NOT NULL,
  emp_miss_rate REAL NOT NULL,
  split TEXT NOT NULL CHECK (split IN ('random', 'temporal')),
  predictor TEXT NOT NULL,
  model_version TEXT NOT NULL,
  rubric_version TEXT NOT NULL,
  grid TEXT NOT NULL,
  pvalues TEXT NOT NULL
);



