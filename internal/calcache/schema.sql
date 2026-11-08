CREATE TABLE accounts (
  account TEXT PRIMARY KEY,
  ready_at TEXT NOT NULL
);
CREATE TABLE calendars (
  account TEXT NOT NULL,
  calendar_id TEXT NOT NULL,
  name TEXT NOT NULL,
  access TEXT NOT NULL CHECK (access IN ('read', 'write')),
  is_primary INTEGER NOT NULL,
  time_zone TEXT NOT NULL,
  is_default INTEGER NOT NULL,
  sync_token TEXT NOT NULL DEFAULT '',
  last_sync TEXT NOT NULL DEFAULT '',
  last_error TEXT NOT NULL DEFAULT '',
  PRIMARY KEY (account, calendar_id)
);
CREATE TABLE cal_events (
  account TEXT NOT NULL,
  calendar_id TEXT NOT NULL,
  provider_id TEXT NOT NULL,
  recurrence_id TEXT NOT NULL,
  ref TEXT NOT NULL,
  is_master INTEGER NOT NULL,
  etag TEXT NOT NULL, uid TEXT NOT NULL,
  start_utc TEXT NOT NULL, end_utc TEXT NOT NULL,
  tzid TEXT NOT NULL, all_day INTEGER NOT NULL,
  status TEXT NOT NULL, sequence INTEGER NOT NULL, transparent INTEGER NOT NULL,
  organizer_json TEXT NOT NULL, attendees_json TEXT NOT NULL, attendees_truncated INTEGER NOT NULL,
  rrule TEXT NOT NULL, rdates_json TEXT NOT NULL, exdates_json TEXT NOT NULL,
  summary TEXT NOT NULL, location TEXT NOT NULL, description TEXT NOT NULL,
  updated TEXT NOT NULL,
  raw BLOB,
  my_partstat TEXT NOT NULL, is_organizer INTEGER NOT NULL,
  PRIMARY KEY (account, calendar_id, provider_id, recurrence_id),
  FOREIGN KEY (account, calendar_id) REFERENCES calendars (account, calendar_id) ON DELETE CASCADE
);
CREATE INDEX cal_events_by_ref ON cal_events (ref);
CREATE INDEX cal_events_by_uid ON cal_events (account, uid);
CREATE INDEX cal_events_by_end ON cal_events (is_master, end_utc);
CREATE INDEX cal_events_by_start ON cal_events (account, calendar_id, start_utc);
CREATE TABLE mail_invites (
  account TEXT NOT NULL,
  message_ref TEXT NOT NULL,
  uid TEXT NOT NULL,
  ref TEXT NOT NULL,
  method TEXT NOT NULL,
  sequence INTEGER NOT NULL,
  dtstamp TEXT NOT NULL,
  raw BLOB NOT NULL,
  charset TEXT NOT NULL DEFAULT '',
  found_at TEXT NOT NULL,
  announced INTEGER NOT NULL DEFAULT 0,
  settled INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (account, message_ref, uid)
);
CREATE INDEX mail_invites_by_ref ON mail_invites (ref);
CREATE INDEX mail_invites_by_uid ON mail_invites (account, uid);
CREATE TABLE invite_fetches (
  account TEXT NOT NULL,
  message_ref TEXT NOT NULL,
  part TEXT NOT NULL,
  method TEXT NOT NULL,
  charset TEXT NOT NULL,
  size INTEGER NOT NULL,
  attempts INTEGER NOT NULL DEFAULT 0,
  next_at TEXT NOT NULL,
  found_at TEXT NOT NULL,
  PRIMARY KEY (account, message_ref, part)
);
CREATE INDEX invite_fetches_by_next ON invite_fetches (next_at);
