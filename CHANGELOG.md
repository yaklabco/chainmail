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

### Changed

- The README is a short introduction, and the quick start and runbook moved to chainmail.sh.
- Watch group names that start with `rules.` are refused, as names that start with `cache.` are. The upgrade deletes any such group.
- A client that names itself `rule` or `daemon` is recorded in the audit log as `(invalid)`, so those client kinds mark only the daemon's own work.
- The upgrade applies `state.db` migration 6, and an older binary refuses the database afterwards. Take a snapshot before you upgrade.
- The manual has the landing page's look.
- The manual's tables and diagrams are restyled to match, and a diagram too wide for the page opens at full size.
- The logo is now a wordmark: two linked gold rings, the name in DM Mono, typed in on load, and a blinking underscore cursor. The favicons and the link preview image use it too, and the gopher mascot is retired.

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
