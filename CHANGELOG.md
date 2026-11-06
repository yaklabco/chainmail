# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

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
