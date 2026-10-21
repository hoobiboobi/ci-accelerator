package store

// migrations are applied in order; PRAGMA user_version holds how many ran.
// They are forward-only: a released migration is never edited.
var migrations = []string{
	// 1: the first schema.
	`
CREATE TABLE meta (
	name  TEXT PRIMARY KEY,
	value BLOB NOT NULL
) WITHOUT ROWID;

CREATE TABLE daemons (
	id   INTEGER PRIMARY KEY,
	name TEXT NOT NULL UNIQUE
);

CREATE TABLE projects (
	instance   INTEGER NOT NULL,
	project_id INTEGER NOT NULL,
	path       TEXT NOT NULL DEFAULT '',
	PRIMARY KEY (instance, project_id)
) WITHOUT ROWID;

CREATE TABLE pipelines (
	instance      INTEGER NOT NULL,
	pipeline_id   INTEGER NOT NULL,
	project_id    INTEGER,
	ref           TEXT NOT NULL DEFAULT '',
	sha           TEXT NOT NULL DEFAULT '',
	source        TEXT NOT NULL DEFAULT '',
	created_at    INTEGER,
	commit_title  TEXT NOT NULL DEFAULT '',
	commit_author TEXT NOT NULL DEFAULT '',
	mr_iid        INTEGER,
	PRIMARY KEY (instance, pipeline_id)
) WITHOUT ROWID;

CREATE TABLE jobs (
	instance    INTEGER NOT NULL,
	job_id      INTEGER NOT NULL,
	pipeline_id INTEGER,
	project_id  INTEGER,
	name        TEXT NOT NULL DEFAULT '',
	stage       TEXT NOT NULL DEFAULT '',
	url         TEXT NOT NULL DEFAULT '',
	started_at  INTEGER,
	node        TEXT NOT NULL DEFAULT '',
	PRIMARY KEY (instance, job_id)
) WITHOUT ROWID;
CREATE INDEX jobs_pipeline ON jobs (instance, pipeline_id);

CREATE TABLE dockerfiles (
	id        INTEGER PRIMARY KEY,
	name      TEXT NOT NULL,
	text      TEXT NOT NULL,
	last_seen INTEGER NOT NULL
);

CREATE TABLE step_defs (
	identity  INTEGER PRIMARY KEY,
	op        BLOB NOT NULL,
	inputs    BLOB NOT NULL,
	name      TEXT NOT NULL DEFAULT '',
	last_seen INTEGER NOT NULL
);

CREATE TABLE builds (
	id            INTEGER PRIMARY KEY,
	daemon_id     INTEGER NOT NULL REFERENCES daemons (id),
	ref           TEXT NOT NULL,
	generation    INTEGER NOT NULL,
	attribution   INTEGER NOT NULL,
	instance      INTEGER,
	pipeline_id   INTEGER,
	job_id        INTEGER,
	project_id    INTEGER,
	target        TEXT NOT NULL DEFAULT '',
	dockerfile_id INTEGER REFERENCES dockerfiles (id),
	build_args    TEXT NOT NULL DEFAULT '{}',
	started_at    INTEGER NOT NULL,
	completed_at  INTEGER,
	error         TEXT NOT NULL DEFAULT '',
	context_bytes INTEGER NOT NULL DEFAULT 0,
	images        TEXT NOT NULL DEFAULT '[]',
	reason_kind   TEXT NOT NULL DEFAULT '',
	reason_step   INTEGER,
	reason_old    TEXT NOT NULL DEFAULT '',
	reason_new    TEXT NOT NULL DEFAULT '',
	reason_arg    TEXT NOT NULL DEFAULT '',
	reason_paths  TEXT NOT NULL DEFAULT '[]',
	reason_by     INTEGER REFERENCES builds (id) ON DELETE SET NULL,
	saved_ms      INTEGER NOT NULL DEFAULT 0,
	output_day    INTEGER,
	UNIQUE (ref, daemon_id)
);
CREATE INDEX builds_job ON builds (instance, job_id);
CREATE INDEX builds_pipeline ON builds (instance, pipeline_id);
CREATE INDEX builds_target ON builds (instance, project_id, target, started_at);
CREATE INDEX builds_completed ON builds (completed_at);
CREATE INDEX builds_dockerfile ON builds (dockerfile_id);
CREATE INDEX builds_reason_by ON builds (reason_by) WHERE reason_by IS NOT NULL;

CREATE TABLE steps (
	build_id      INTEGER NOT NULL REFERENCES builds (id) ON DELETE CASCADE,
	idx           INTEGER NOT NULL,
	key           TEXT NOT NULL,
	name          TEXT,
	identity      INTEGER,
	state         INTEGER NOT NULL,
	start_ms      INTEGER NOT NULL,
	dur_ms        INTEGER NOT NULL,
	lines         TEXT NOT NULL DEFAULT '',
	received_from INTEGER REFERENCES builds (id) ON DELETE SET NULL,
	error         TEXT NOT NULL DEFAULT '',
	output_len    INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY (build_id, idx)
) WITHOUT ROWID;
CREATE INDEX steps_identity ON steps (identity, build_id);
CREATE INDEX steps_received ON steps (received_from) WHERE received_from IS NOT NULL;
`,
	// 2: what GitLab's API says about jobs and pipelines.
	`
ALTER TABLE jobs ADD COLUMN status TEXT NOT NULL DEFAULT '';
ALTER TABLE jobs ADD COLUMN queued_at INTEGER;
ALTER TABLE jobs ADD COLUMN finished_at INTEGER;
ALTER TABLE jobs ADD COLUMN duration_ms INTEGER;
ALTER TABLE jobs ADD COLUMN queued_ms INTEGER;
ALTER TABLE jobs ADD COLUMN runner TEXT NOT NULL DEFAULT '';
ALTER TABLE pipelines ADD COLUMN status TEXT NOT NULL DEFAULT '';
ALTER TABLE pipelines ADD COLUMN web_url TEXT NOT NULL DEFAULT '';
ALTER TABLE pipelines ADD COLUMN api_at INTEGER;
`,
}

// outputSchema is the schema of a daily output file. Output files are
// written for one day and deleted whole, so they are never migrated: a
// new layout gets a new file name.
const outputSchema = `
CREATE TABLE IF NOT EXISTS dicts (
	id      INTEGER PRIMARY KEY,
	project TEXT NOT NULL,
	data    BLOB NOT NULL
);
CREATE TABLE IF NOT EXISTS outputs (
	key     INTEGER PRIMARY KEY,
	dict_id INTEGER,
	data    BLOB NOT NULL
);
`
