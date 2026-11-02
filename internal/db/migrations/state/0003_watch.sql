CREATE TABLE sync_folders (
    account        TEXT    NOT NULL,
    folder         TEXT    NOT NULL,
    uidvalidity    INTEGER NOT NULL DEFAULT 0,
    uidnext        INTEGER NOT NULL DEFAULT 0,
    highest_modseq INTEGER NOT NULL DEFAULT 0,
    condstore      INTEGER NOT NULL DEFAULT 0 CHECK (condstore IN (0, 1)),
    mode           TEXT    NOT NULL DEFAULT '' CHECK (mode IN ('', 'idle', 'poll')),
    state          TEXT    NOT NULL DEFAULT '',
    last_sync_at   TEXT    NOT NULL DEFAULT '',
    last_sweep_at  TEXT    NOT NULL DEFAULT '',
    last_error     TEXT    NOT NULL DEFAULT '',
    PRIMARY KEY (account, folder)
);

CREATE TABLE sync_uids (
    account     TEXT    NOT NULL,
    folder      TEXT    NOT NULL,
    uid         INTEGER NOT NULL,
    gmail_msgid INTEGER NOT NULL DEFAULT 0,
    flags_hash  TEXT    NOT NULL DEFAULT '',
    PRIMARY KEY (account, folder, uid)
) WITHOUT ROWID;

CREATE TABLE events (
    id      INTEGER PRIMARY KEY AUTOINCREMENT,
    at      TEXT    NOT NULL,
    account TEXT    NOT NULL,
    folder  TEXT    NOT NULL DEFAULT '',
    kind    TEXT    NOT NULL CHECK (kind IN ('new', 'flags', 'expunged', 'status', 'gap')),
    ref     TEXT    NOT NULL DEFAULT '',
    payload TEXT    NOT NULL DEFAULT '{}'
);
CREATE INDEX events_at ON events (at);

CREATE TABLE event_groups (
    name         TEXT    PRIMARY KEY,
    acked_id     INTEGER NOT NULL DEFAULT 0,
    created_at   TEXT    NOT NULL,
    last_seen_at TEXT    NOT NULL
);
