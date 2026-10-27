package store

// migrations are applied in order; PRAGMA user_version holds how many ran.
// They are forward-only: a released migration is never edited, a change
// is a new migration appended here.
var migrations = []string{
	// 1: the schema of the first release with a console (0.2.0).
	`
CREATE TABLE meta (
	name  TEXT PRIMARY KEY,
	value BLOB NOT NULL
) WITHOUT ROWID;

CREATE TABLE daemons (
	id   INTEGER PRIMARY KEY,
	name TEXT NOT NULL UNIQUE
);

-- last_at: the project's latest build, for the project menu.
CREATE TABLE projects (
	instance   INTEGER NOT NULL,
	project_id INTEGER NOT NULL,
	path       TEXT NOT NULL DEFAULT '',
	last_at    INTEGER,
	PRIMARY KEY (instance, project_id)
) WITHOUT ROWID;

-- From the job pods' annotations; status, web_url and api_at from GitLab's
-- API when the console read it; seen_at: when the console first heard of it.
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
	status        TEXT NOT NULL DEFAULT '',
	web_url       TEXT NOT NULL DEFAULT '',
	api_at        INTEGER,
	seen_at       INTEGER,
	PRIMARY KEY (instance, pipeline_id)
) WITHOUT ROWID;
CREATE INDEX pipelines_project ON pipelines (instance, project_id);

-- From the job pods; status, queued_at, finished_at, duration_ms, queued_ms
-- (the wait for a runner), runner, allow_failure and started_by from
-- GitLab's API.
CREATE TABLE jobs (
	instance      INTEGER NOT NULL,
	job_id        INTEGER NOT NULL,
	pipeline_id   INTEGER,
	project_id    INTEGER,
	name          TEXT NOT NULL DEFAULT '',
	stage         TEXT NOT NULL DEFAULT '',
	url           TEXT NOT NULL DEFAULT '',
	started_at    INTEGER,
	node          TEXT NOT NULL DEFAULT '',
	status        TEXT NOT NULL DEFAULT '',
	queued_at     INTEGER,
	finished_at   INTEGER,
	duration_ms   INTEGER,
	queued_ms     INTEGER,
	runner        TEXT NOT NULL DEFAULT '',
	seen_at       INTEGER,
	allow_failure INTEGER NOT NULL DEFAULT 0,
	started_by    TEXT NOT NULL DEFAULT '',
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

-- built, built_ms: the steps a build executed and their time (resolving a
-- base image aside), so lists need not read the steps. reason_via: who
-- decided the reason after the build was stored (GitLab's comparison of
-- two commits, "gitlab <from>..<to>", or why nothing could:
-- "unresolved: ..."); empty: BuildKit's records alone.
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
	reason_via    TEXT NOT NULL DEFAULT '',
	saved_ms      INTEGER NOT NULL DEFAULT 0,
	output_day    INTEGER,
	built         INTEGER NOT NULL DEFAULT 0,
	built_ms      INTEGER NOT NULL DEFAULT 0,
	UNIQUE (ref, daemon_id)
);
CREATE INDEX builds_job ON builds (instance, job_id);
CREATE INDEX builds_pipeline ON builds (instance, pipeline_id);
CREATE INDEX builds_target ON builds (instance, project_id, target, started_at);
CREATE INDEX builds_project_time ON builds (instance, project_id, started_at);
CREATE INDEX builds_completed ON builds (completed_at);
CREATE INDEX builds_dockerfile ON builds (dockerfile_id);
CREATE INDEX builds_reason_by ON builds (reason_by) WHERE reason_by IS NOT NULL;
CREATE INDEX builds_elsewhere ON builds (instance, project_id, started_at)
	WHERE reason_kind = 'cache-elsewhere' AND reason_via = '';

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

-- One row per image and UTC day, kept a year after the builds themselves
-- are pruned: counts by outcome and build times.
CREATE TABLE image_days (
	instance       INTEGER NOT NULL,
	project_id     INTEGER NOT NULL,
	target         TEXT NOT NULL,
	day            INTEGER NOT NULL, -- days since 1970-01-01, UTC
	builds         INTEGER NOT NULL,
	cached         INTEGER NOT NULL,
	change         INTEGER NOT NULL,
	farm           INTEGER NOT NULL,
	unclear        INTEGER NOT NULL,
	failed         INTEGER NOT NULL,
	built_ms       INTEGER NOT NULL, -- executed steps' time, all builds
	cached_p50_ms  INTEGER NOT NULL,
	rebuilt_p50_ms INTEGER NOT NULL,
	rebuilt_p95_ms INTEGER NOT NULL,
	PRIMARY KEY (instance, project_id, target, day)
) WITHOUT ROWID;

-- Sign-in through GitLab: tokens sealed with the console's key.
CREATE TABLE sessions (
	id         BLOB PRIMARY KEY,
	instance   TEXT NOT NULL,
	user_id    INTEGER NOT NULL,
	username   TEXT NOT NULL,
	name       TEXT NOT NULL DEFAULT '',
	token      BLOB NOT NULL,
	created_at INTEGER NOT NULL,
	seen_at    INTEGER NOT NULL
) WITHOUT ROWID;
CREATE INDEX sessions_seen ON sessions (seen_at);

-- The runners' S3 cache folders as last listed.
CREATE TABLE cache_objects (
	store       TEXT NOT NULL,
	instance    INTEGER,
	project_id  INTEGER NOT NULL,
	key         TEXT NOT NULL,
	bytes       INTEGER NOT NULL,
	modified_at INTEGER NOT NULL,
	PRIMARY KEY (store, project_id, key)
) WITHOUT ROWID;
CREATE INDEX cache_objects_project ON cache_objects (instance, project_id);
CREATE TABLE cache_stores (
	store      TEXT PRIMARY KEY,
	scanned_at INTEGER,
	lifecycle  INTEGER,
	error      TEXT NOT NULL DEFAULT ''
) WITHOUT ROWID;
`,
	// 2: 0.3.0, the build router: the node each daemon runs on (a
	// project's cache is warm on a node, whatever the daemon pod is
	// called after a restart) and the routers' decisions.
	`
ALTER TABLE daemons ADD COLUMN node TEXT NOT NULL DEFAULT '';
-- persists: the daemon's cache survives its restart (hostPath), so its
-- builds keep warming the node after the pod is replaced.
ALTER TABLE daemons ADD COLUMN persists INTEGER NOT NULL DEFAULT 0;

CREATE TABLE route_decisions (
	id         INTEGER PRIMARY KEY,
	instance   INTEGER,
	job_id     INTEGER,
	project_id INTEGER,
	node       TEXT NOT NULL,
	daemon     TEXT NOT NULL,
	reason     TEXT NOT NULL,
	candidates INTEGER NOT NULL,
	job_node   TEXT NOT NULL DEFAULT '',
	at         INTEGER NOT NULL
);
CREATE INDEX route_decisions_job ON route_decisions (instance, job_id);
CREATE INDEX route_decisions_at ON route_decisions (at);
CREATE INDEX builds_started ON builds (started_at);

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
