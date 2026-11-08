---
title: The audit log
description: Read the daemon's record of every call, filter it by account and action, and page through it.
sidebar_position: 7
---

# The audit log

The daemon records every call it serves in the `audit` table of `state.db`. `chainmail audit` lists those rows, newest first.

Rows hold no message content, recipients or secrets, so any user allowed on the socket may read them. Reading the log is not itself audited.

```sh
chainmail audit
chainmail audit --since 1h --output table
chainmail audit --account work --action send --limit 20
chainmail audit --client rule --since 1h --output table
```

## Flags

| Flag | Default | Meaning |
|---|---|---|
| `--since` | `24h` | How far back to look, as a Go duration such as `90m` or `168h`. |
| `--account` | all accounts | Only rows of this account. An unknown account fails with `INVALID_ARGUMENT`. |
| `--action` | all actions | Only rows of this action, such as `send`, `expunge` or `folders.delete`. See [actions](#actions). |
| `--client` | all clients | Only rows of this client kind, such as `cli`, `mcp` or `rule`. A value that is not a well-formed client kind fails with `INVALID_ARGUMENT`. |
| `--limit` | `100` | Rows per page, up to 1000. |
| `--before-id` | none | Only rows older than this id. Pass the previous page's `next_before_id`. |

`--action` completes from the actions in the newest rows, when [shell completion](./cli-settings.md#shell-completion) is set up.

## Output

With JSON output the command prints `{"entries":[...],"next_before_id":...}` and exits 0.

Each entry has these fields:

| Field | Holds |
|---|---|
| `id` | The row's number. |
| `at` | When the call was made. |
| `caller_uid` | The caller's user ID, read from the socket. This is the authoritative identity. |
| `client` | The client kind: `cli`, `mcp`, `live-test`, `daemon` for work the daemon starts itself, such as a background probe, or `rule` for an action a rule took. The client reports it itself, so any local caller can claim any kind except `daemon` and `rule`, which are recorded as `(invalid)` from a socket client. |
| `account` | The account the call was for. |
| `action` | What was done. See [actions](#actions). |
| `targets` | Refs, Message-IDs and counts the call touched, such as `source=cache` on a search. |
| `decision` | What the policy or the daemon decided. See [decisions](#decisions). |
| `result` | `ok` or `error`. |
| `reason` | The error reason, such as `POLICY_DENIED`. |

Like other 64-bit fields, `id` and `next_before_id` are JSON strings. `next_before_id` is `"0"` on the last page.

On a terminal the command prints one line per row. When there are more rows, it prints the `--before-id` to pass for the next page:

```sh
chainmail audit --since 168h --before-id 4182
```

## Actions

The audited reads are `accounts.list`, `account.test`, `secret.set`, `auth.login.begin`, `auth.login.finish`, `folders.list`, `search`, `get`, `get.raw`, `thread`, `attachment`, `watch.subscribe`, `watch.read`, `watch.groups.list`, `item.get`, `item.list`, `rule.get`, `rule.list`, `rule.dry_run`, `rule.runs` and `rule.status`.

The audited writes are `send`, `send_raw`, `reply`, `forward`, `upload`, `draft.create`, `draft.update`, `draft.send`, `draft.delete`, `move`, `copy`, `archive`, `trash`, `expunge`, `flags`, `labels`, `folders.create`, `folders.rename`, `folders.delete`, `limits.probe`, `watch.groups.delete`, `cache.rebuild`, `cache.evict`, `item.create`, `item.update`, `item.delete`, `item.restore`, `item.criteria`, `item.tick`, `item.block`, `item.link`, `item.unlink`, `rule.create`, `rule.update`, `rule.delete`, `rule.restore` and `rule.run`.

Every call is recorded except `admin status`, `audit`, `watch status`, `cache status` and a watch's acknowledgements. A batch that spans accounts records one row per account. Over MCP, each check of which policy-gated tools to list is one `accounts.list` row (see [which tools are listed](../agents/setup.md#which-tools-are-listed)).

An action a rule takes is recorded under its own action, such as `labels`, `move`, `forward` or `item.link`, with client kind `rule` and the daemon's user. Its targets add `rule=<id>` and `run=<id>`, so `chainmail audit --client rule` shows what each rule did and in which run (see [Rules](./rules.md#the-audit-log)).

## Decisions

| Decision | Means |
|---|---|
| `allowed` | A send the policy passed. |
| `held` | A send the policy held as a draft, with result `ok` and reason `HELD`. Its targets are the draft's ref, its Message-ID, `blocked:<n>` with the number of blocked recipients, and `too_many` when that applied. |
| `denied` | A refused `expunge` or `folder delete`, with result `error` and reason `POLICY_DENIED`. |
| `duplicate` | A repeated send with the same idempotency key, or a retried `item.create` with the same key. |
| `dry_run` | A dry run. |

[The security model](../concepts/security-model.md) describes what each row records and what it never holds, and [the send policy](../concepts/policy.md) what each decision means for a send.

Every flag is in the [audit command reference](../reference/commands/chainmail-audit.md).
