CREATE TABLE messages (
  id INTEGER PRIMARY KEY,
  account TEXT NOT NULL, folder TEXT NOT NULL,
  uidvalidity INTEGER NOT NULL, uid INTEGER NOT NULL,
  gmail_msgid INTEGER NOT NULL DEFAULT 0, gmail_thrid INTEGER NOT NULL DEFAULT 0,
  internal_date INTEGER NOT NULL, date INTEGER NOT NULL, size INTEGER NOT NULL,
  subject TEXT NOT NULL,
  from_json TEXT NOT NULL, to_json TEXT NOT NULL, cc_json TEXT NOT NULL, bcc_json TEXT NOT NULL,
  message_id TEXT NOT NULL, in_reply_to_json TEXT NOT NULL, references_json TEXT NOT NULL,
  thread_id TEXT NOT NULL,
  flags_json TEXT NOT NULL, labels_json TEXT NOT NULL,
  has_attachment INTEGER NOT NULL, attachments_json TEXT NOT NULL,
  attachments_unknown INTEGER NOT NULL DEFAULT 0,
  subject_norm TEXT NOT NULL,
  from_norm TEXT NOT NULL, to_norm TEXT NOT NULL, cc_norm TEXT NOT NULL, bcc_norm TEXT NOT NULL,
  flags_norm TEXT NOT NULL, labels_norm TEXT NOT NULL, attachments_norm TEXT NOT NULL,
  cached_at INTEGER NOT NULL,
  UNIQUE (account, folder, uidvalidity, uid)
);
CREATE INDEX messages_by_uid ON messages (account, folder, uidvalidity, uid DESC);
CREATE INDEX messages_by_arrival ON messages (account, folder, internal_date DESC);
CREATE INDEX messages_by_msgid ON messages (account, message_id);
CREATE INDEX messages_by_thread ON messages (account, thread_id);
CREATE TABLE bodies (
  msg INTEGER PRIMARY KEY REFERENCES messages(id) ON DELETE CASCADE,
  raw BLOB NOT NULL, text_clipped INTEGER NOT NULL, size_bytes INTEGER NOT NULL,
  fetched_at INTEGER NOT NULL, last_read_at INTEGER
);
CREATE TABLE body_failures (
  msg INTEGER PRIMARY KEY REFERENCES messages(id) ON DELETE CASCADE,
  attempts INTEGER NOT NULL, first_attempt_at INTEGER NOT NULL, last_attempt_at INTEGER NOT NULL,
  reason TEXT NOT NULL
);
CREATE VIRTUAL TABLE messages_fts USING fts5(
  subject, names, addresses, body,
  content='', contentless_delete=1, tokenize='unicode61 remove_diacritics 2'
);
CREATE TABLE folders (
  account TEXT NOT NULL, folder TEXT NOT NULL,
  uidvalidity INTEGER NOT NULL, baseline_uidnext INTEGER NOT NULL,
  backfill_low_uid INTEGER NOT NULL, backfill_done INTEGER NOT NULL,
  window_done INTEGER NOT NULL, last_applied_at INTEGER,
  PRIMARY KEY (account, folder)
);
CREATE TABLE ref_recovery (
  account TEXT NOT NULL, folder TEXT NOT NULL, uidvalidity INTEGER NOT NULL, uid INTEGER NOT NULL,
  message_id TEXT NOT NULL, removed_at INTEGER NOT NULL,
  PRIMARY KEY (account, folder, uidvalidity, uid)
);
CREATE TABLE budget (account TEXT NOT NULL, day TEXT NOT NULL, bytes INTEGER NOT NULL, PRIMARY KEY (account, day));
CREATE TABLE unreadable (
  account TEXT NOT NULL, folder TEXT NOT NULL, uidvalidity INTEGER NOT NULL, uid INTEGER NOT NULL,
  first_at INTEGER NOT NULL, last_at INTEGER NOT NULL, attempts INTEGER NOT NULL,
  PRIMARY KEY (account, folder, uidvalidity, uid)
);
