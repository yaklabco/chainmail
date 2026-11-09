# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- A documentation site at https://chainmail.sh, with a user guide, an operations guide, a configuration and command reference, and a developer guide.
- Rules, kept in `state.db`, that act on mail as it arrives, on a schedule and on demand. A rule is a search query matched against the local cache, and a list of actions: label, unlabel, copy, flag, unflag, read, unread, link to a work item, forward, move, archive and trash. `chainmail rule` creates, dry-runs, runs and inspects rules, and eight MCP tools do the same for agents.
- Guardrails for rules. A rule acts on each message at most once, and a run stops at its cap. Forwards go through the send policy, every action is audited under client kind `rule`, and a rule that keeps failing turns itself off.
- Due events for work items: `item_due_soon`, `item_due` and `item_overdue`. The new `[rules]` config table sets their time with `timezone`, `due_time` and `due_soon_days`.
- The opt-in `rule` event kind, which reports every change to a rule. `chainmail watch` and `read_events` accept it and the three due kinds.
- `chainmail audit --client`, which keeps the rows of one client kind, such as `rule` for the actions rules take.
- A landing page at https://chainmail.sh, with the manual under https://chainmail.sh/docs/.
- Calendars on Google accounts, through the Google Calendar API, and on CalDAV servers such as Mailwright. The daemon syncs them into `calendar.db`, next to `cache.db`, and expands recurring events when they are read. The new `[calendar]` table sets `timezone`, `poll`, `window_past` and `window_future`, and an `[accounts.<name>.calendar]` table sets an account's `store`, `url`, `include`, `exclude` and `default`.
- `chainmail calendars`, `agenda`, `event`, `invites`, `invite`, `rsvp` and `freebusy` read calendars, show free/busy time and answer invites. `chainmail event create`, `event edit` and `event cancel` change events. Ten MCP tools do the same for agents: `list_calendars`, `agenda`, `get_event`, `list_invites`, `get_invite`, `free_busy`, `rsvp`, `create_event`, `update_event` and `cancel_event`.
- Invites from calendars and from mail. The cache finds `text/calendar` invites in the new mail of watched accounts, and an account with store `none` answers them with an iCalendar REPLY email to the organizer.
- The send policy covers calendars. It checks the organizer of every answer and the attendees of every change that notifies them. A held answer by email is saved to Drafts, a held new event is created as a draft that invites no one, and a held change or cancel is not applied.
- The opt-in event kinds `invite`, `event_changed`, `event_cancelled` and `invite_reply`. `chainmail watch --kind` and `read_events` accept them.
- `chainmail cache status` shows each account's calendar sync: its state (`ready`, `syncing`, `auth_required` or `error`), its last sync, and how many calendars and events it holds.
- OAuth clients with an `issuer`, such as a Keycloak realm, take their endpoints from OIDC discovery and need no secret. Their `scopes` add to what their accounts ask for. A `custom` account can log in with `auth = "oauth2"` through such a client, which is how chainmail reaches Mailwright.
- `tls_server_name` on an account's `imap` and `smtp` tables, for a server reached through a host name its certificate does not hold, such as an internal load balancer.
- The error reasons `NOT_ORGANIZER` and `CALENDAR_READ_ONLY` (exit 3), `NO_CALENDAR_STORE` (exit 2) and `EVENT_CONFLICT` (exit 7).
- `stave test:caldav`, which runs the CalDAV store tests against the Mailwright image `CHAINMAIL_MAILWRIGHT_IMAGE` names. CI does not run it.

### Changed

- The README is a short introduction, and the quick start and runbook moved to chainmail.sh.
- Watch group names that start with `rules.` are refused, as names that start with `cache.` are. The upgrade deletes any such group.
- A client that names itself `rule` or `daemon` is recorded in the audit log as `(invalid)`, so those client kinds mark only the daemon's own work.
- The upgrade applies `state.db` migration 6, and an older binary refuses the database afterwards. Take a snapshot before you upgrade.
- The manual has the landing page's look.
- The manual's tables and diagrams are restyled to match, and a diagram too wide for the page opens at full size.
- The landing page leads with what an agent does with your mail: a morning brief, threads filed under tasks, follow-ups before a due date, rules that file receipts, held replies and search across accounts. The two stories that need unreleased features say so.
- https://chainmail.sh/search is a new page on search speed, with the benchmark figures for the local index against the mail servers. It replaces the redirect from `/search` to the manual's search page.
- The logo is now a wordmark: two linked gold rings, the name in DM Mono, typed in on load, and a blinking underscore cursor. The favicons and the link preview image use it too, and the gopher mascot is retired.
- A Google account that logs in with OAuth2 now asks for three calendar scopes, unless its calendar table sets `store = "none"`. A refresh token from before lacks them, so mail keeps working and the account's calendar state is `auth_required` until you run `chainmail auth login <account>`.
- A `gmail` account with `auth = "password"` has calendar store `none`, and `store = "google"` on it is a config error, since the Calendar API takes no password.
- `CACHE_NOT_READY` also answers a calendar read before the account's first calendar sync, and `HELD` a calendar answer or change the policy held.
- The upgrade applies `state.db` migration 7, and an older binary refuses the database afterwards. Take a snapshot before you upgrade.
- The README, the quick start and the runbook describe calendars.

### Fixed

- Tab completion of message refs no longer offers nothing when the inbox is empty. Commands that read or act on any message, such as `get`, `thread`, `reply` and `move`, offer inbox mail first and then the newest mail from any folder. `archive` still offers inbox mail only.

## [0.1.0] - 2026-11-06

### Added

- The daemon, which owns every credential and mail connection, and its Unix socket with a peer check that allows only the daemon's user and the client group.
- An age-encrypted vault for passwords, OAuth client secrets and refresh tokens, with `secret set`, `auth login` for Google's OAuth2 consent flow, and `accounts test`.
- Accounts on Google Workspace and Gmail over OAuth2, Gmail with an app password, iCloud, Hostinger and any other IMAP and SMTP server.
- Reading with `folders`, `search`, `get`, `thread` and `attachment`, Gmail-style search operators, refs, and an untrusted envelope around everything taken from a message.
- Writing with `send`, `send-raw`, `reply`, `forward` and `draft`, with Markdown bodies, attachments, inline images, signatures, dry runs and idempotency keys.
- Organizing with `move`, `copy`, `archive`, `trash`, `expunge`, flags, Gmail labels and folder create, rename and delete, plus `accounts probe` to size each account's connection pool.
- The send policy: a send to anyone off the allowlist is held as a draft and never transmitted, and `expunge` and `folder delete` are refused unless the config allows them.
- An append-only audit log in `state.db` and the `audit` command to read it.
- A NixOS module that runs the daemon as its own system user with a TPM2-sealed vault key.
- `chainmail mcp`, an MCP server over stdio that gives AI agents the same calls through the same policy and audit.
- Watching with IMAP IDLE and polling, a durable event log with named groups that resume across restarts, `chainmail watch` and the `read_events` MCP tool.
- A local cache in `cache.db` with SQLite FTS5 full-text search, which answers searches and reads when it can answer completely, and the `cache status`, `cache rebuild` and `cache evict` commands.
- Work items in `state.db`, from objectives down to tasks, with acceptance criteria, blockers, links to mail threads, `item:` searches and `item_mail` events.
- The CLI's styled help, a `cli.toml` settings file, shell completion for bash, zsh, fish and PowerShell, and a man page.

### Changed

- The Go module moved to `github.com/yaklabco/chainmail`.

[Unreleased]: https://github.com/yaklabco/chainmail/compare/v0.1.0...HEAD
[0.1.0]: https://github.com/yaklabco/chainmail/releases/tag/v0.1.0
