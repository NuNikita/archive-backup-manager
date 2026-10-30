CREATE TABLE IF NOT EXISTS users (
 id BIGSERIAL PRIMARY KEY,
 username TEXT NOT NULL UNIQUE,
 password_hash TEXT NOT NULL,
 role TEXT NOT NULL CHECK (role IN ('admin', 'viewer'))
);
CREATE TABLE IF NOT EXISTS sessions (
 token_hash TEXT PRIMARY KEY,
 user_id BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
 csrf_token TEXT NOT NULL,
 expires_at TIMESTAMPTZ NOT NULL
);
CREATE TABLE IF NOT EXISTS plans (
 id BIGSERIAL PRIMARY KEY,
 name TEXT NOT NULL CHECK (length(name) BETWEEN 1 AND 100),
 source TEXT NOT NULL,
 interval_minutes INTEGER NOT NULL CHECK (interval_minutes BETWEEN 1 AND 43200),
 keep_last INTEGER NOT NULL CHECK (keep_last BETWEEN 1 AND 100),
 enabled BOOLEAN NOT NULL DEFAULT false,
 next_run TIMESTAMPTZ NOT NULL DEFAULT now(),
 created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE TABLE IF NOT EXISTS tasks (
 id BIGSERIAL PRIMARY KEY,
 plan_id BIGINT REFERENCES plans(id) ON DELETE SET NULL,
 plan_name TEXT NOT NULL,
 source TEXT NOT NULL,
 keep_last INTEGER NOT NULL,
 kind TEXT NOT NULL CHECK (kind IN ('backup', 'restore', 'check')),
 status TEXT NOT NULL DEFAULT 'queued' CHECK (status IN ('queued','running','success','failed')),
 snapshot_id TEXT NOT NULL DEFAULT '',
 restore_path TEXT NOT NULL DEFAULT '',
 log TEXT NOT NULL DEFAULT '',
 created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
 started_at TIMESTAMPTZ,
 finished_at TIMESTAMPTZ
);
CREATE UNIQUE INDEX IF NOT EXISTS one_active_task_per_plan ON tasks(plan_id)
 WHERE status IN ('queued','running') AND plan_id IS NOT NULL;
CREATE TABLE IF NOT EXISTS snapshots (
 id TEXT PRIMARY KEY,
 plan_id BIGINT NOT NULL REFERENCES plans(id) ON DELETE CASCADE,
 created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE TABLE IF NOT EXISTS audit (
 id BIGSERIAL PRIMARY KEY,
 username TEXT NOT NULL,
 action TEXT NOT NULL,
 created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
