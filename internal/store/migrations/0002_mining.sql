CREATE TABLE mining_sources (
  singleton INTEGER PRIMARY KEY CHECK (singleton = 1),
  provider TEXT NOT NULL CHECK (provider IN ('github', 'gitlab')),
  project TEXT NOT NULL,
  observed_at TEXT NOT NULL,
  UNIQUE (provider, project)
);

CREATE TABLE mining_runs (
  provider TEXT NOT NULL,
  project TEXT NOT NULL,
  run_id TEXT NOT NULL,
  attempt_key TEXT NOT NULL,
  attempt INTEGER CHECK (attempt >= 1),
  source_head_sha TEXT NOT NULL,
  branch TEXT NOT NULL,
  created_at TEXT,
  requested_since TEXT,
  requested_until TEXT,
  state TEXT NOT NULL CHECK (state IN ('complete', 'incomplete', 'capped')),
  observed_at TEXT NOT NULL,
  PRIMARY KEY (provider, project, run_id, attempt_key)
);

CREATE TABLE ci_artifacts (
  provider TEXT NOT NULL,
  project TEXT NOT NULL,
  unit_key TEXT NOT NULL,
  content_sha256 TEXT NOT NULL,
  run_id TEXT NOT NULL,
  artifact_id TEXT,
  job_id TEXT,
  attempt INTEGER CHECK (attempt >= 1),
  environment_id TEXT,
  tested_sha TEXT,
  tested_revision_evidence TEXT,
  execution_evidence TEXT,
  environment_evidence TEXT,
  raw_blob_path TEXT NOT NULL,
  source_head_sha TEXT NOT NULL,
  branch TEXT NOT NULL,
  tested_authored_at TEXT,
  tested_parent_shas_json TEXT,
  tested_files_changed INTEGER,
  tested_lines_added INTEGER,
  tested_lines_removed INTEGER,
  tested_kinds_json TEXT NOT NULL,
  suite_incomplete INTEGER NOT NULL CHECK (suite_incomplete IN (0, 1)),
  diagnostics_truncated INTEGER NOT NULL CHECK (diagnostics_truncated IN (0, 1)),
  state TEXT NOT NULL CHECK (state IN ('complete', 'incomplete', 'capped')),
  observed_at TEXT NOT NULL,
  replay_cases INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (provider, project, unit_key, content_sha256)
);

CREATE TABLE ci_observations (
  provider TEXT NOT NULL,
  project TEXT NOT NULL,
  unit_key TEXT NOT NULL,
  content_sha256 TEXT NOT NULL,
  artifact_entry TEXT NOT NULL,
  entry_sha256 TEXT NOT NULL,
  case_ordinal INTEGER NOT NULL CHECK (case_ordinal >= 0),
  raw_suite TEXT NOT NULL,
  raw_name TEXT NOT NULL,
  raw_classname TEXT NOT NULL,
  raw_file TEXT NOT NULL,
  status TEXT NOT NULL,
  duration_ms INTEGER,
  raw_message TEXT NOT NULL,
  raw_type TEXT NOT NULL,
  raw_properties_json TEXT NOT NULL,
  suite_incomplete INTEGER NOT NULL CHECK (suite_incomplete IN (0, 1)),
  diagnostics_truncated INTEGER NOT NULL CHECK (diagnostics_truncated IN (0, 1)),
  test_id TEXT NOT NULL,
  identity_language TEXT NOT NULL,
  identity_package TEXT NOT NULL,
  identity_name TEXT NOT NULL,
  identity_file TEXT NOT NULL,
  identity_runner_id TEXT NOT NULL,
  unstable_id INTEGER NOT NULL CHECK (unstable_id IN (0, 1)),
  identity_reason TEXT NOT NULL,
  identity_error TEXT NOT NULL,
  identity_evidence TEXT,
  infra_reason TEXT NOT NULL,
  result_key TEXT,
  observed_at TEXT NOT NULL,
  PRIMARY KEY (provider, project, unit_key, content_sha256, artifact_entry, case_ordinal),
  FOREIGN KEY (provider, project, unit_key, content_sha256)
    REFERENCES ci_artifacts(provider, project, unit_key, content_sha256)
);
CREATE INDEX idx_ci_observations_result ON ci_observations(result_key);

CREATE TABLE acquisition_exclusions (
  provider TEXT NOT NULL,
  project TEXT NOT NULL,
  unit_key TEXT NOT NULL,
  content_sha256 TEXT NOT NULL,
  artifact_entry TEXT NOT NULL DEFAULT '',
  case_ordinal INTEGER NOT NULL DEFAULT -1,
  reason TEXT NOT NULL,
  detail TEXT NOT NULL,
  observed_at TEXT NOT NULL,
  PRIMARY KEY (provider, project, unit_key, content_sha256, artifact_entry, case_ordinal, reason),
  FOREIGN KEY (provider, project, unit_key, content_sha256)
    REFERENCES ci_artifacts(provider, project, unit_key, content_sha256)
);
