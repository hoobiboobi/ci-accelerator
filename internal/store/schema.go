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
	// 3: when the console first heard of a job or pipeline: retention for
	// those it never learned a time of (rows stored before count from now).
	`
ALTER TABLE jobs ADD COLUMN seen_at INTEGER;
ALTER TABLE pipelines ADD COLUMN seen_at INTEGER;
UPDATE jobs SET seen_at = CAST(strftime('%s', 'now') AS INTEGER) * 1000;
UPDATE pipelines SET seen_at = CAST(strftime('%s', 'now') AS INTEGER) * 1000;
CREATE INDEX jobs_name ON jobs (instance, project_id, name);
`,
	// 4: a job allowed to fail (GitLab's allow_failure).
	`
ALTER TABLE jobs ADD COLUMN allow_failure INTEGER NOT NULL DEFAULT 0;
`,
	// 5: sessions of people signed in through GitLab. The id is a hash of
	// the cookie, so the database holds no usable session; the token is
	// sealed with a key the database does not hold.
	`
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
`,
	// 6: the runners' S3 caches (what each bucket folder holds, from the
	// last listing) and job logs read once (what a finished job did, as a
	// versioned JSON digest: its cache use, later more).
	`
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
CREATE TABLE job_logs (
	instance INTEGER NOT NULL,
	job_id   INTEGER NOT NULL,
	read_at  INTEGER NOT NULL,
	version  INTEGER NOT NULL,
	digest   TEXT NOT NULL,
	PRIMARY KEY (instance, job_id)
) WITHOUT ROWID;
`,
	// 7: who started a job, as GitLab says (for a manual job: who ran it).
	`
ALTER TABLE jobs ADD COLUMN started_by TEXT NOT NULL DEFAULT '';
`,
	// 8: what lists need without reading steps: how many steps a build
	// executed and their time (resolving a base image does not count), the
	// builds of a project by time, and when a project last built.
	`
ALTER TABLE builds ADD COLUMN built INTEGER NOT NULL DEFAULT 0;
ALTER TABLE builds ADD COLUMN built_ms INTEGER NOT NULL DEFAULT 0;
UPDATE builds SET
	built = (SELECT count(*) FROM steps s LEFT JOIN step_defs d ON d.identity = s.identity
		WHERE s.build_id = builds.id AND s.state = 1 AND COALESCE(s.name, d.name, '') NOT LIKE 'FROM %'),
	built_ms = (SELECT COALESCE(sum(s.dur_ms), 0) FROM steps s LEFT JOIN step_defs d ON d.identity = s.identity
		WHERE s.build_id = builds.id AND s.state = 1 AND COALESCE(s.name, d.name, '') NOT LIKE 'FROM %');
CREATE INDEX builds_project_time ON builds (instance, project_id, started_at);
ALTER TABLE projects ADD COLUMN last_at INTEGER;
UPDATE projects SET last_at = (SELECT max(b.started_at) FROM builds b
	WHERE b.instance = projects.instance AND b.project_id = projects.project_id);
CREATE INDEX pipelines_project ON pipelines (instance, project_id);
`,
	// 9: one row per image and UTC day, kept a year after the builds
	// themselves are pruned: counts by outcome and build times.
	`
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
`,
	// 10: who decided a build's reason after it was stored: GitLab's
	// comparison of two commits ("gitlab <from>..<to>"), or why nothing
	// could ("unresolved: ..."); empty: BuildKit's records alone.
	`
ALTER TABLE builds ADD COLUMN reason_via TEXT NOT NULL DEFAULT '';
CREATE INDEX builds_elsewhere ON builds (instance, project_id, started_at)
	WHERE reason_kind = 'cache-elsewhere' AND reason_via = '';
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
