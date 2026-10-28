-- Label cohort ledger. The legacy regressions and excluded_labels tables stay
-- untouched: they collapse environment and version, so they are not populated.
-- Rows of the three child tables and the identity columns of label_builds are
-- immutable. Only the current-build marker (is_current) may change.

CREATE TABLE label_builds (
  build_id TEXT PRIMARY KEY CHECK (length(build_id) = 64),
  provider TEXT NOT NULL,
  project TEXT NOT NULL,
  cutoff TEXT NOT NULL,
  algorithm_version TEXT NOT NULL,
  input_digest TEXT NOT NULL CHECK (length(input_digest) = 64),
  decision_digest TEXT NOT NULL CHECK (length(decision_digest) = 64),
  always_run_json TEXT NOT NULL,
  acquisition_exclusions_json TEXT NOT NULL,
  active_gaps INTEGER NOT NULL CHECK (active_gaps >= 0),
  created_at TEXT NOT NULL,
  is_current INTEGER NOT NULL DEFAULT 0 CHECK (is_current IN (0, 1)),
  UNIQUE (algorithm_version, provider, project, cutoff, input_digest)
);
CREATE UNIQUE INDEX idx_label_builds_current ON label_builds(is_current) WHERE is_current = 1;

CREATE TABLE label_candidates (
  build_id TEXT NOT NULL REFERENCES label_builds(build_id),
  candidate_key TEXT NOT NULL,
  kind TEXT NOT NULL CHECK (kind IN ('candidate', 'unresolved')),
  state TEXT NOT NULL CHECK (state IN ('pending', 'excluded', 'weak', 'strong')),
  tested_sha TEXT,
  test_id TEXT,
  environment_id TEXT,
  branch TEXT,
  member_count INTEGER NOT NULL CHECK (member_count >= 1),
  evidence_json TEXT NOT NULL,
  PRIMARY KEY (build_id, candidate_key),
  CHECK (kind = 'candidate' OR state IN ('pending', 'excluded')),
  CHECK (kind = 'candidate' OR member_count = 1),
  CHECK (kind = 'unresolved' OR (tested_sha IS NOT NULL AND test_id IS NOT NULL AND environment_id IS NOT NULL))
);

CREATE TABLE label_candidate_members (
  build_id TEXT NOT NULL,
  provider TEXT NOT NULL,
  project TEXT NOT NULL,
  unit_key TEXT NOT NULL,
  content_sha256 TEXT NOT NULL,
  artifact_entry TEXT NOT NULL,
  case_ordinal INTEGER NOT NULL CHECK (case_ordinal >= 0),
  candidate_key TEXT NOT NULL,
  status TEXT NOT NULL CHECK (status IN ('fail', 'error')),
  observed_at TEXT NOT NULL,
  snapshot_json TEXT NOT NULL,
  PRIMARY KEY (build_id, provider, project, unit_key, content_sha256, artifact_entry, case_ordinal),
  FOREIGN KEY (build_id, candidate_key) REFERENCES label_candidates(build_id, candidate_key)
);
CREATE INDEX idx_label_candidate_members_candidate ON label_candidate_members(build_id, candidate_key);

CREATE TABLE label_candidate_exclusions (
  build_id TEXT NOT NULL,
  candidate_key TEXT NOT NULL,
  reason TEXT NOT NULL CHECK (length(reason) > 0),
  effect TEXT NOT NULL CHECK (effect IN ('exclude', 'pending', 'weak')),
  evidence_json TEXT NOT NULL,
  detail TEXT NOT NULL DEFAULT '',
  PRIMARY KEY (build_id, candidate_key, reason),
  FOREIGN KEY (build_id, candidate_key) REFERENCES label_candidates(build_id, candidate_key)
);

CREATE TRIGGER label_candidates_no_update BEFORE UPDATE ON label_candidates
BEGIN SELECT RAISE(ABORT, 'label_candidates rows are immutable'); END;
CREATE TRIGGER label_candidates_no_delete BEFORE DELETE ON label_candidates
BEGIN SELECT RAISE(ABORT, 'label_candidates rows are immutable'); END;
CREATE TRIGGER label_candidate_members_no_update BEFORE UPDATE ON label_candidate_members
BEGIN SELECT RAISE(ABORT, 'label_candidate_members rows are immutable'); END;
CREATE TRIGGER label_candidate_members_no_delete BEFORE DELETE ON label_candidate_members
BEGIN SELECT RAISE(ABORT, 'label_candidate_members rows are immutable'); END;
CREATE TRIGGER label_candidate_exclusions_no_update BEFORE UPDATE ON label_candidate_exclusions
BEGIN SELECT RAISE(ABORT, 'label_candidate_exclusions rows are immutable'); END;
CREATE TRIGGER label_candidate_exclusions_no_delete BEFORE DELETE ON label_candidate_exclusions
BEGIN SELECT RAISE(ABORT, 'label_candidate_exclusions rows are immutable'); END;
CREATE TRIGGER label_builds_no_identity_update BEFORE UPDATE OF
  build_id, provider, project, cutoff, algorithm_version, input_digest, decision_digest,
  always_run_json, acquisition_exclusions_json, active_gaps, created_at ON label_builds
BEGIN SELECT RAISE(ABORT, 'label_builds identity is immutable'); END;
CREATE TRIGGER label_builds_no_delete BEFORE DELETE ON label_builds
BEGIN SELECT RAISE(ABORT, 'label_builds rows are immutable'); END;
