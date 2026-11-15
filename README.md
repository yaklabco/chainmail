<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="docs/img/wordmark-dark.svg" />
    <img src="docs/img/wordmark-light.svg" alt="chainmail" width="360" />
  </picture>
</p>

<p align="center">
  <a href="https://github.com/yaklabco/chainmail/releases/latest"><img src="https://img.shields.io/github/v/release/yaklabco/chainmail" alt="Latest release" /></a>
  <a href="https://github.com/yaklabco/chainmail/actions/workflows/ci.yml"><img src="https://github.com/yaklabco/chainmail/actions/workflows/ci.yml/badge.svg" alt="CI" /></a>
  <a href="LICENSE"><img src="https://img.shields.io/github/license/yaklabco/chainmail" alt="MIT license" /></a>
  <a href="https://chainmail.sh"><img src="https://img.shields.io/badge/docs-chainmail.sh-1e3a6e" alt="Docs" /></a>
</p>

**chainmail** is email for AI agents, behind a policy daemon.

## Why

- The daemon owns every password and token, in an age-encrypted vault, and no call ever returns one.
- A send to anyone off the allowlist is saved as a draft for you to review, never sent.
- Nearly every call is recorded in an append-only audit log.
- Agents reach the daemon over MCP, behind the same policy and audit log as the CLI.

```mermaid
flowchart LR
  A[AI agent] -- MCP over stdio --> M[chainmail mcp]
  C[chainmail CLI] -- ConnectRPC --> S((Unix socket))
  M -- ConnectRPC --> S
  S --> D[chainmail daemon]
  D --- V[(vault)]
  D --- P{policy}
  D --- L[(audit log)]
  D --- K[(cache)]
  D -- IMAP --> I[mail servers]
  D -- SMTP --> I
```

## Install

```sh
brew install yaklabco/tap/chainmail
chainmail version
```

Homebrew installs the binary, its shell completions and its man page. It does not start the daemon; `chainmail bootstrap` does. Release archives, Nix and building from source are on the [install page](https://chainmail.sh/docs/getting-started/install).

## Set up

`chainmail bootstrap` sets up a production daemon that runs as its own user, with one account. Run it as yourself, not as root. It asks for sudo once.

```sh
chainmail bootstrap you@example.com
```

Bootstrap finds the provider from the address. It supports Gmail, Google Workspace, iCloud and Hostinger. `--provider` names one when the address does not show it. Bootstrap prints its plan, asks for the account's app password, and asks you to confirm. A Google Workspace account needs `--oauth-client-id`, and bootstrap asks for the OAuth client secret instead. Bootstrap then creates the `chainmail` user (`_chainmail` on macOS) and the `chainmail-clients` group. It installs the binary, the config, the vault key and the service. It starts the daemon, stores the password and tests the login. `--dry-run` prints the plan and changes nothing. A rerun adds another account or repairs the install.

When bootstrap makes a new vault key, it asks whether to back it up. The default is no backup. The key then stays on this machine, and only the daemon's user can read it. If the key is lost, you run bootstrap again and enter each account's password again. No mail is lost. The other choice shows the key once on the terminal, for you to save in a password manager. Anything that can see the terminal at that moment sees the key too, including an agent watching the session. `--key-backup none` or `--key-backup show` answers the question in advance.

Bootstrap adds you to `chainmail-clients`. Log out and back in, or run `newgrp chainmail-clients`, before you use chainmail. The account is named after the address's local part, `you` here, unless `--name` picks another:

```sh
chainmail accounts list
chainmail folders --account you
chainmail search is:unread in:inbox
```

On macOS, bootstrap also writes your `cli.toml` so commands find the daemon's socket. Bootstrap supports Linux with systemd and macOS. On NixOS, use the [NixOS module](https://chainmail.sh/docs/operations/deploy#nixos) instead.

An agent that can run sudo without a password can get around all of this. Run agents from an account that cannot.

The [quick start](https://chainmail.sh/docs/getting-started/quick-start) goes on to Google accounts, sends and the policy, watching and items.

### Developing chainmail

`chainmail daemon --dev` runs a daemon as your own user, for work on chainmail itself:

> dev mode: this daemon runs as you, so any program running as you, including agents, can read the vault key and every stored password. Use it only to develop chainmail, never with real accounts.

```sh
chainmail daemon --dev --config ./config.toml --state-dir ./state \
    --vault-key-file ./vault-key --socket ./state/chainmail.sock
```

The [developer guide](https://chainmail.sh/docs/developers/architecture) covers building and testing.

## Features

- [Reading and search](https://chainmail.sh/docs/guide/reading-and-searching): `folders`, `search`, `get`, `thread` and `attachment` across every account, with Gmail-style operators. Reading never marks mail read.
- [Sending and drafts](https://chainmail.sh/docs/guide/sending): `send`, `send-raw`, `reply`, `forward` and `draft`, with Markdown bodies, attachments, dry runs and idempotency keys.
- [Organizing](https://chainmail.sh/docs/guide/organizing): move, copy, archive, trash, flags, Gmail labels and folders, with refs piped in from a search.
- [The send policy](https://chainmail.sh/docs/concepts/policy): sends off the allowlist are held as drafts, and `expunge` and `folder delete` are refused unless the config allows them.
- [The audit log](https://chainmail.sh/docs/guide/audit-log): an append-only table in `state.db`, read with `chainmail audit`, that never holds message content or secrets.
- [MCP](https://chainmail.sh/docs/agents/setup): `chainmail mcp` serves the tools to an AI agent over stdio.
- [Watching and events](https://chainmail.sh/docs/guide/watching): IMAP IDLE and polling feed a durable event log, and a named group resumes where it stopped.
- [The local cache](https://chainmail.sh/docs/concepts/cache): `cache.db`, indexed with SQLite FTS5, answers searches and reads when it is complete and fresh.
- [Work items](https://chainmail.sh/docs/guide/items): a tree of items from objectives down to tasks, with mail threads linked to them and events when they come due.
- [Rules](https://chainmail.sh/docs/guide/rules): rules in `state.db` label, file, forward and archive mail as it arrives, on a schedule or on demand. They match with the search language against the cache, and each action is audited.
- [Calendars](https://chainmail.sh/docs/guide/calendars): `calendars`, `agenda`, `event`, `invites`, `invite`, `rsvp` and `freebusy` read calendars and answer invites on Google accounts and CalDAV servers. `event create`, `event edit` and `event cancel` change events under the send policy.
- [NixOS module](https://chainmail.sh/docs/operations/deploy): runs the daemon as its own system user, with the vault key passed as an encrypted systemd credential.

## Calendars

chainmail reads and answers calendars on Google accounts through the Google Calendar API, and on CalDAV servers such as Mailwright. The daemon syncs them into `calendar.db`, next to `cache.db`, every 5 minutes by default. Ten MCP tools give agents the same calls, such as `agenda`, `rsvp` and `create_event`.

- Each account's `[accounts.<name>.calendar]` table sets `store` to `google`, `caldav` or `none`. A Google account that logs in with OAuth2 defaults to `google`, and every other account defaults to `none`.
- The cache finds invites in the new mail of every watched account. On an account with store `none`, `chainmail rsvp` answers them with an iCalendar REPLY email to the organizer.
- A Google account with store `google` asks for three calendar scopes. A token from before calendars lacks them, so mail keeps working and calendar calls fail with `AUTH_REQUIRED` until you run `chainmail auth login <account>`.
- A Mailwright account uses the `custom` provider with `auth = "oauth2"` and an OAuth client that has an `issuer`, such as a Keycloak realm. Its calendar table sets `store = "caldav"` and the server's `url`.
- The send policy checks every answer and every change that notifies anyone else. A held answer through the calendar changes nothing, and a held answer by email is saved to Drafts. A new event with an attendee off the allowlist becomes a draft event that invites no one, and you send its invitations from your calendar app.
- `chainmail watch --kind invite,event_changed,event_cancelled,invite_reply` reports new invites and changes to your events.

## Documentation

The manual is at [chainmail.sh](https://chainmail.sh).

- [Getting started](https://chainmail.sh/docs/getting-started/install): install, a first daemon, accounts and connecting an agent.
- [User guide](https://chainmail.sh/docs/guide/reading-and-searching): reading, sending, organizing, items, rules, calendars, watching, the audit log and CLI settings.
- [Agents](https://chainmail.sh/docs/agents/setup): setting up an MCP client and working with mail as an agent.
- [Operations](https://chainmail.sh/docs/operations/deploy): deploying, health checks, auth, upgrades, backups and troubleshooting.
- [Reference](https://chainmail.sh/docs/reference/configuration): configuration, commands, MCP tools and exit codes.
- [Developer guide](https://chainmail.sh/docs/developers/architecture): architecture, building, testing, the API and releasing.

## Status

chainmail is in active development. Releases are published on [GitHub](https://github.com/yaklabco/chainmail/releases) and in the [yaklabco/homebrew-tap](https://github.com/yaklabco/homebrew-tap) tap, and the [changelog](https://chainmail.sh/docs/reference/changelog) lists what each one changed.

## License

chainmail is released under the MIT license. See [LICENSE](LICENSE).
