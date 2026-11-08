-- Rules: definitions with revisions and soft delete, run history, the
-- act-once table, and the due events already sent for work items. Times are
-- text in the fixed-width UTC layout the event log uses, dates YYYY-MM-DD.
CREATE TABLE rules (
    id              TEXT    PRIMARY KEY,
    name            TEXT    NOT NULL,
    description     TEXT    NOT NULL DEFAULT '',
    accounts        TEXT    NOT NULL DEFAULT '[]',
    query           TEXT    NOT NULL,
    on_new          INTEGER NOT NULL DEFAULT 0,
    schedule        TEXT    NOT NULL DEFAULT '',
    priority        INTEGER NOT NULL DEFAULT 100,
    stop            INTEGER NOT NULL DEFAULT 0,
    actions         TEXT    NOT NULL,
    max_per_run     INTEGER NOT NULL DEFAULT 500 CHECK (max_per_run BETWEEN 1 AND 5000),
    enabled         INTEGER NOT NULL DEFAULT 1,
    disabled_reason TEXT    NOT NULL DEFAULT '',
    revision        INTEGER NOT NULL,
    created_at      TEXT    NOT NULL,
    updated_at      TEXT    NOT NULL,
    deleted_at      TEXT,
    created_by      TEXT    NOT NULL,
    updated_by      TEXT    NOT NULL,
    next_run_at     TEXT,
    last_run_at     TEXT,
    failed_runs     INTEGER NOT NULL DEFAULT 0
);
CREATE UNIQUE INDEX rules_live_name ON rules (name) WHERE deleted_at IS NULL;
CREATE INDEX rules_by_priority ON rules (priority, id);
CREATE INDEX rules_by_next_run ON rules (next_run_at) WHERE next_run_at IS NOT NULL;

CREATE TABLE rule_revisions (
    rule_id  TEXT    NOT NULL REFERENCES rules (id),
    revision INTEGER NOT NULL,
    change   TEXT    NOT NULL CHECK (change IN ('created', 'updated', 'deleted', 'restored', 'disabled', 'enabled')),
    rule     TEXT    NOT NULL,
    actor    TEXT    NOT NULL,
    at       TEXT    NOT NULL,
    PRIMARY KEY (rule_id, revision)
);

CREATE TABLE rule_runs (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    rule_id      TEXT    NOT NULL,
    account      TEXT    NOT NULL,
    trigger_kind TEXT    NOT NULL CHECK (trigger_kind IN ('new', 'schedule', 'manual')),
    started_at   TEXT    NOT NULL,
    finished_at  TEXT    NOT NULL,
    matched      INTEGER NOT NULL DEFAULT 0,
    applied      INTEGER NOT NULL DEFAULT 0,
    held         INTEGER NOT NULL DEFAULT 0,
    skipped      INTEGER NOT NULL DEFAULT 0,
    failed       INTEGER NOT NULL DEFAULT 0,
    outcome      TEXT    NOT NULL CHECK (outcome IN ('ok', 'capped', 'failed', 'cache_not_ready', 'interrupted')),
    reason       TEXT    NOT NULL DEFAULT '',
    hint         TEXT    NOT NULL DEFAULT ''
);
CREATE INDEX rule_runs_by_rule ON rule_runs (rule_id, id);
CREATE INDEX rule_runs_started ON rule_runs (started_at);

CREATE TABLE rule_applied (
    rule_id     TEXT    NOT NULL,
    account     TEXT    NOT NULL,
    message_key TEXT    NOT NULL,
    run_id      INTEGER NOT NULL,
    outcome     TEXT    NOT NULL CHECK (outcome IN ('pending', 'applied', 'held')),
    note        TEXT    NOT NULL DEFAULT '',
    at          TEXT    NOT NULL,
    PRIMARY KEY (rule_id, account, message_key)
);
CREATE INDEX rule_applied_by_key ON rule_applied (account, message_key);

CREATE TABLE item_due_sent (
    item_id  TEXT NOT NULL,
    due_date TEXT NOT NULL,
    stage    TEXT NOT NULL CHECK (stage IN ('item_due_soon', 'item_due', 'item_overdue')),
    at       TEXT NOT NULL,
    PRIMARY KEY (item_id, due_date, stage)
);

CREATE INDEX items_by_due ON items (due_date) WHERE due_date IS NOT NULL;

-- A client group named like a rules group would be adopted as a runner's
-- position. Drop any, so every rules group opens at the tail.
DELETE FROM event_groups WHERE name LIKE 'rules.%';

-- The event log gets the rule, item_due_soon, item_due and item_overdue
-- kinds. As in 0004 and 0005, SQLite cannot change a CHECK constraint in
-- place, so the table is rebuilt. Every event keeps its ID, and the
-- AUTOINCREMENT sequence carries over, so the log's tail and the groups'
-- positions stay valid even when every event was trimmed. events_at stays
-- the only index on events.
CREATE TABLE events_next (
    id      INTEGER PRIMARY KEY AUTOINCREMENT,
    at      TEXT    NOT NULL,
    account TEXT    NOT NULL,
    folder  TEXT    NOT NULL DEFAULT '',
    kind    TEXT    NOT NULL CHECK (kind IN ('new', 'flags', 'expunged', 'status', 'gap', 'unreadable', 'item', 'item_mail', 'rule', 'item_due_soon', 'item_due', 'item_overdue')),
    ref     TEXT    NOT NULL DEFAULT '',
    payload TEXT    NOT NULL DEFAULT '{}'
);
INSERT INTO events_next (id, at, account, folder, kind, ref, payload)
    SELECT id, at, account, folder, kind, ref, payload FROM events;
DELETE FROM sqlite_sequence WHERE name = 'events_next';
INSERT INTO sqlite_sequence (name, seq) SELECT 'events_next', seq FROM sqlite_sequence WHERE name = 'events';
DROP TABLE events;
ALTER TABLE events_next RENAME TO events;
CREATE INDEX events_at ON events (at);
