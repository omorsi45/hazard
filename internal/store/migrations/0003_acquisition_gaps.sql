CREATE TABLE acquisition_gaps (
  id INTEGER PRIMARY KEY,
  provider TEXT NOT NULL CHECK (provider IN ('github', 'gitlab')),
  project TEXT NOT NULL,
  run_id TEXT NOT NULL DEFAULT '',
  unit_key TEXT NOT NULL,
  reason TEXT NOT NULL,
  detail TEXT NOT NULL DEFAULT '',
  job_id TEXT,
  artifact_id TEXT,
  attempt INTEGER CHECK (attempt >= 1),
  source_time TEXT,
  first_acquired_at TEXT NOT NULL,
  last_acquired_at TEXT NOT NULL,
  resolved_at TEXT,
  content_sha256 TEXT CHECK (content_sha256 IS NULL OR length(content_sha256) = 64),
  raw_blob_path TEXT,
  CHECK ((content_sha256 IS NULL AND raw_blob_path IS NULL)
    OR (content_sha256 IS NOT NULL AND raw_blob_path IS NOT NULL))
);

CREATE UNIQUE INDEX idx_acquisition_gaps_active_episode
  ON acquisition_gaps(provider, project, run_id, unit_key, reason, COALESCE(content_sha256, ''))
  WHERE resolved_at IS NULL;

CREATE INDEX idx_acquisition_gaps_active_conflict
  ON acquisition_gaps(provider, project, unit_key)
  WHERE resolved_at IS NULL AND reason = 'acquisition_conflict';
