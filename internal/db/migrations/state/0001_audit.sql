CREATE TABLE audit (
    id          INTEGER PRIMARY KEY,
    at          TEXT    NOT NULL,
    caller_uid  INTEGER NOT NULL,
    client      TEXT    NOT NULL,
    account     TEXT    NOT NULL DEFAULT '',
    action      TEXT    NOT NULL,
    targets     TEXT    NOT NULL DEFAULT '[]',
    decision    TEXT    NOT NULL DEFAULT '',
    approval_id TEXT    NOT NULL DEFAULT '',
    result      TEXT    NOT NULL CHECK (result IN ('ok', 'error')),
    reason      TEXT    NOT NULL DEFAULT ''
);

CREATE INDEX audit_at ON audit (at);

CREATE TRIGGER audit_no_update BEFORE UPDATE ON audit
BEGIN
    SELECT RAISE(ABORT, 'audit is append-only');
END;

CREATE TRIGGER audit_no_delete BEFORE DELETE ON audit
BEGIN
    SELECT RAISE(ABORT, 'audit is append-only');
END;
