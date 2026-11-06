-- Work items: a tree from objectives down to tasks, with acceptance
-- criteria, blockers, the mail threads linked to each item, the item_mail
-- events already reported, and the requests that made items, for
-- idempotent creates. item_revision holds the one global revision every
-- item write bumps. Times are text in the fixed-width UTC layout the event
-- log uses, dates are YYYY-MM-DD.
CREATE TABLE items (
    id          TEXT    PRIMARY KEY,
    type        TEXT    NOT NULL CHECK (type IN ('objective', 'key-result', 'initiative', 'epic', 'feature', 'story', 'bug', 'support-ticket', 'task')),
    title       TEXT    NOT NULL,
    description TEXT    NOT NULL DEFAULT '',
    state       TEXT    NOT NULL DEFAULT 'backlog' CHECK (state IN ('backlog', 'in-progress', 'in-review', 'done')),
    area        TEXT    CHECK (area IN ('work', 'personal')),
    priority    TEXT    CHECK (priority IN ('p0', 'p1', 'p2', 'p3')),
    start_date  TEXT,
    due_date    TEXT,
    estimate    REAL    CHECK (estimate IS NULL OR estimate >= 0),
    parent_id   TEXT    REFERENCES items (id),
    ongoing     INTEGER NOT NULL DEFAULT 0,
    deleted_at  TEXT,
    revision    INTEGER NOT NULL,
    created_at  TEXT    NOT NULL,
    updated_at  TEXT    NOT NULL,
    created_by  TEXT    NOT NULL,
    updated_by  TEXT    NOT NULL
);
CREATE INDEX items_by_parent ON items (parent_id);
CREATE INDEX items_by_revision ON items (revision, id);

CREATE TABLE item_revision (
    id    INTEGER PRIMARY KEY CHECK (id = 1),
    value INTEGER NOT NULL
);
INSERT INTO item_revision (id, value) VALUES (1, 0);

CREATE TABLE item_criteria (
    id        TEXT    PRIMARY KEY,
    item_id   TEXT    NOT NULL REFERENCES items (id),
    position  INTEGER NOT NULL,
    text      TEXT    NOT NULL,
    ticked    INTEGER NOT NULL DEFAULT 0,
    evidence  TEXT    NOT NULL DEFAULT '',
    ticked_at TEXT,
    ticked_by TEXT
);
CREATE INDEX item_criteria_by_item ON item_criteria (item_id, position);

CREATE TABLE item_blockers (
    item_id       TEXT NOT NULL REFERENCES items (id),
    blocked_by_id TEXT NOT NULL REFERENCES items (id),
    PRIMARY KEY (item_id, blocked_by_id),
    CHECK (item_id <> blocked_by_id)
);
CREATE INDEX item_blockers_by_blocker ON item_blockers (blocked_by_id);

CREATE TABLE item_links (
    item_id      TEXT NOT NULL REFERENCES items (id),
    account      TEXT NOT NULL,
    thread_key   TEXT NOT NULL,
    root_msgid   TEXT NOT NULL DEFAULT '',
    anchor_msgid TEXT NOT NULL DEFAULT '',
    note         TEXT NOT NULL DEFAULT '',
    created_at   TEXT NOT NULL,
    created_by   TEXT NOT NULL,
    PRIMARY KEY (item_id, account, thread_key)
);
CREATE INDEX item_links_by_thread ON item_links (account, thread_key);

CREATE TABLE item_mail_seen (
    item_id     TEXT NOT NULL,
    account     TEXT NOT NULL,
    message_key TEXT NOT NULL,
    at          TEXT NOT NULL,
    PRIMARY KEY (item_id, account, message_key)
);
CREATE INDEX item_mail_seen_at ON item_mail_seen (at);

CREATE TABLE item_requests (
    key          TEXT PRIMARY KEY,
    request_hash TEXT NOT NULL,
    item_id      TEXT NOT NULL REFERENCES items (id),
    at           TEXT NOT NULL
);
CREATE INDEX item_requests_at ON item_requests (at);

-- The event log gets the item and item_mail kinds. As in 0004, SQLite
-- cannot change a CHECK constraint in place, so the table is rebuilt. Every
-- event keeps its ID, and the AUTOINCREMENT sequence carries over, so the
-- log's tail and the groups' positions stay valid even when every event was
-- trimmed.
CREATE TABLE events_next (
    id      INTEGER PRIMARY KEY AUTOINCREMENT,
    at      TEXT    NOT NULL,
    account TEXT    NOT NULL,
    folder  TEXT    NOT NULL DEFAULT '',
    kind    TEXT    NOT NULL CHECK (kind IN ('new', 'flags', 'expunged', 'status', 'gap', 'unreadable', 'item', 'item_mail')),
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
