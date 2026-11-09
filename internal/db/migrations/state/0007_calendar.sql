-- The event log gets the calendar kinds: invite, event_changed,
-- event_cancelled and invite_reply. As in 0004 to 0006, SQLite cannot
-- change a CHECK constraint in place, so the table is rebuilt. Every event
-- keeps its ID, and the AUTOINCREMENT sequence carries over, so the log's
-- tail and the groups' positions stay valid even when every event was
-- trimmed. events_at stays the only index on events.
CREATE TABLE events_next (
    id      INTEGER PRIMARY KEY AUTOINCREMENT,
    at      TEXT    NOT NULL,
    account TEXT    NOT NULL,
    folder  TEXT    NOT NULL DEFAULT '',
    kind    TEXT    NOT NULL CHECK (kind IN ('new', 'flags', 'expunged', 'status', 'gap', 'unreadable', 'item', 'item_mail', 'rule', 'item_due_soon', 'item_due', 'item_overdue', 'invite', 'event_changed', 'event_cancelled', 'invite_reply')),
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
