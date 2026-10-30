CREATE TABLE idempotency (
    account      TEXT    NOT NULL,
    key          TEXT    NOT NULL,
    request_hash BLOB    NOT NULL,
    state        TEXT    NOT NULL CHECK (state IN ('in_progress', 'done', 'failed', 'retryable', 'unknown')),
    result       BLOB    NOT NULL DEFAULT x'',
    created_at   TEXT    NOT NULL,
    updated_at   TEXT    NOT NULL,
    PRIMARY KEY (account, key)
);
CREATE INDEX idempotency_created ON idempotency (created_at);

CREATE TABLE account_limits (
    account   TEXT    PRIMARY KEY,
    imap_cap  INTEGER NOT NULL CHECK (imap_cap >= 1),
    probed_at TEXT    NOT NULL
);
