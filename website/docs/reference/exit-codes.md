---
title: Exit codes and error reasons
description: Every exit code and error reason chainmail reports, what each means, and what to check first.
sidebar_position: 4
---

# Exit codes and error reasons

Every failure carries a reason, a stable upper-case code such as `NOT_FOUND`. The CLI maps each reason to an exit code. Reasons and exit codes are a contract with agents and never change. Both come from [`internal/api`](https://github.com/yaklabco/chainmail/blob/main/internal/api/api.go).

## Output

On success a command exits 0. With JSON output it prints one line of JSON on stdout. Field names are snake_case, and unset fields are included.

On failure a command exits non-zero. When stderr is not a terminal, it prints one line of JSON there, and leaves out `hint` when there is none:

```json
{"error":{"reason":"NOT_FOUND","message":"unknown account: \"nosuch\"","hint":"run: chainmail accounts list"}}
```

Read the hint first. It often names the command to run.

On a terminal the same failure is styled for a person instead: the reason as a header, then the message, then the hint. Text from the daemon is cleaned of control and invisible characters first. The reason and the exit code are the same in both modes.

An agent that runs chainmail under a pseudo-terminal should redirect stderr to get the JSON line:

```sh
chainmail accounts list 2>err.json
```

## The table

| Exit | Reason | Meaning | What to check first |
|---|---|---|---|
| 0 | | Success. | |
| 1 | `INTERNAL` | A bug or an unexpected failure. A daemon that fails to start, such as on a config error, reports it this way. Any reason not in this table also exits 1. | The message, then the daemon's log, such as `sudo journalctl -u chainmail` around that time. |
| 1 | `MALFORMED_RESPONSE` | The mail server sent a response chainmail cannot read. Trying again gets the same answer, so it is not exit 7. | `chainmail cache status` (the `UNREADABLE` column) and the daemon's WARN log line naming the account, folder and UID. See [The cache in operation](../operations/cache.md). |
| 2 | `USAGE` | A bad invocation. See [Usage errors](#usage-errors). | `chainmail <command> --help`, and `cli.toml` when the message names it. |
| 2 | `INVALID_ARGUMENT` | A bad value, or something the server or provider refuses, such as `label` on an account that is not Google. A name that breaks the naming rules, such as `Work`, is this reason, not `NOT_FOUND`. | The message. Account and client names match `^[a-z0-9][a-z0-9-]{0,62}$`. |
| 3 | `POLICY_DENIED` | The policy forbids the action, such as `expunge` with `allow_expunge` off. | The capabilities in `chainmail accounts list --output json`, and [Policy in operation](../operations/policy.md). |
| 3 | `FORBIDDEN` | Your user may not use the daemon's socket. | `id -nG` and `/etc/group`. See [Health checks](../operations/health.md). Do not use `sudo` for the CLI. |
| 4 | `HELD` | The send policy held a send. It was not sent. It was saved to Drafts for James to review and send from his own mail client. The result is still printed on stdout. | Do not retry. See [The send policy](../concepts/policy.md) and [Policy in operation](../operations/policy.md). |
| 5 | `AUTH_REQUIRED` | A secret is missing, or the account already needs a human to log in. | The `DETAIL` column of `chainmail accounts list --output table`. The hint says what to run. |
| 5 | `AUTH_REJECTED` | A mail server or Google rejected the credentials. | `chainmail accounts test <account>`, then [Auth](../operations/auth.md). |
| 6 | `NOT_FOUND` | No such account, pass entry, folder, message, attachment part, item, criterion or link. | `chainmail accounts list`, `chainmail folders` or `chainmail item list`. |
| 6 | `STALE_REF` | The ref went stale: the folder was renumbered or the message moved. | Search again for a fresh ref. See [Refs and cursors](../concepts/refs-and-cursors.md). |
| 7 | `SERVER_UNAVAILABLE` | A mail server or Google could not be reached, or refused for now. A call with several steps can fail after an earlier step took effect. An updated draft, for one, can be saved while removing its old version fails, with the message `the new version was saved, but removing the old one failed`. | The message and hint before any retry. Then `chainmail accounts test <account>` and `chainmail watch status`. |
| 7 | `TIMEOUT` | A mail server or the daemon did not answer in time, or the client gave up on the call. | The same as `SERVER_UNAVAILABLE`, then the daemon's log. |
| 7 | `DAEMON_UNREACHABLE` | The CLI could not reach the daemon. | Whether the daemon runs, such as `systemctl status chainmail`, and which socket the CLI used (`--socket`, `CHAINMAIL_SOCKET`, `cli.toml`). See [Health checks](../operations/health.md). |
| 7 | `CACHE_NOT_READY` | The cache cannot answer an `item:` search yet, such as while it backfills or trails the event log. Trying again later can work. | `chainmail cache status`. See [Items in operation](../operations/items.md). |
| 8 | `SEND_OUTCOME_UNKNOWN` | A send may or may not have gone out. | The Sent folder, before you send again. See [Sending](../guide/sending.md). |
| 9 | `TOTP_INVALID`, `TOTP_LOCKED` | Approval codes, reserved for a later sub-project. Nothing returns them yet. | |

## Usage errors

Usage errors always exit 2 with `USAGE`. These are usage errors:

- A missing or extra argument, or an unknown flag.
- A group command run on its own, such as `chainmail accounts`.
- An unknown subcommand, and `chainmail help <unknown>`.
- A bad output format from `--output`, `CHAINMAIL_OUTPUT` or `cli.toml`.
- A `cli.toml` that cannot be read or parsed, is not a regular file, is larger than 64 KiB, holds an unknown key, or holds a value that is not a string. See [CLI settings](../guide/cli-settings.md).
- A `CHAINMAIL_DEV` that is not a boolean (`true`, `false`, `1`, `0`, `t` or `f`) when `--dev` is not given.
- `chainmail completion` without a shell, or with one it does not know.

## Partial failures

Some commands act on several things and report one exit code for all of them.

- The batch commands (`move`, `copy`, `archive`, `trash`, `expunge`, `flags`, `label` and the flag shortcuts) print an outcome per ref. When any ref fails, the command exits with the first failure's reason and the message `<n> of <m> refs failed`.
- `get` exits non-zero only when every message failed, with the first one's reason.
- `accounts test` exits with `AUTH_REJECTED` when either check was rejected, else with the reason of the first failed check.
- `accounts probe` exits with the first failed probe's reason and the message `<n> of <m> probes failed`.

## Interrupts

Interrupting a prompt with Ctrl-C, such as the `secret set` prompt or the `auth login` paste, exits 1 with `INTERNAL` and a message containing `interrupted`. So does an interrupt while a command reads refs from stdin.

## Reasons without a daemon answer

When a call fails before the daemon can answer, the CLI derives the reason from the transport:

| Failure | Reason | Hint |
|---|---|---|
| The daemon refused the socket peer | `FORBIDDEN` | `your user may not use this daemon socket; join its client group` |
| The socket could not be reached | `DAEMON_UNREACHABLE` | `is the chainmail daemon running? check --socket or CHAINMAIL_SOCKET` |
| The call ran out of time | `TIMEOUT` | none |
| Anything else | `INTERNAL` | none |

## In MCP

`chainmail mcp` reports the same reasons in its error results, without exit codes. A failed call is a tool result with `isError: true`. Its text is `REASON: message`, followed by `hint: ...` when there is a hint, and its structured content is `{"error":{"reason":...,"message":...,"hint":...}}`.

A held send is a successful result with `status` `held`, so `HELD` never appears in MCP. Arguments that fail a tool's input schema come back as `INVALID_ARGUMENT` results. A call to an unknown tool is a JSON-RPC error.

Error text there is deny by default. A call that was cancelled or not answered in time gets `TIMEOUT`, a refusal gets `FORBIDDEN`, and any other error the server did not word itself gets `INTERNAL` with the message `internal error in chainmail mcp; see its stderr log`. See [MCP tools](./mcp-tools.md) and [Working with mail](../agents/working-with-mail.md).
