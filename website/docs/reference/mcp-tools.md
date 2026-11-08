---
title: MCP tools
description: Every tool chainmail mcp serves, with its arguments, annotations, results and audit action.
sidebar_position: 3
---

# MCP tools

`chainmail mcp` serves 46 tools to an AI agent over the Model Context Protocol on stdio. 44 are always listed. `expunge` and `delete_folder` are listed only while the policy allows them (see [Gated tools](#gated-tools)). Every tool calls the daemon over its socket, behind the same policy and audit log as the CLI.

[Agents setup](../agents/setup.md) registers the server with a client. [Working with mail](../agents/working-with-mail.md) covers how an agent should treat untrusted content, held sends, derived idempotency keys, files and errors.

The tool list on this page matches [`tools_list.golden`](https://github.com/yaklabco/chainmail/blob/main/internal/mcpserver/testdata/tools_list.golden), the test fixture that pins every tool's schema. The Meaning column of each argument table is the description the tool's input schema carries, which is the text an agent reads.

## Conventions

- Inputs take refs, addresses and folder names as plain strings. Refs come from `search`, `get_message`, `get_thread` and `read_events`. Pass them unchanged. See [Refs and cursors](../concepts/refs-and-cursors.md).
- Outputs give dates in RFC 3339.
- A message's `flags` are the system flags by short name: `seen`, `flagged`, `answered`, `draft` and `deleted`. Every other flag, such as `$Forwarded`, is in `keywords`, as untrusted.
- Text that comes from a message or a mail server is wrapped as `{"untrusted": "..."}`. So is work item text, whoever wrote it, and a rule's description, query and forward note. Refs are the exception. See [Untrusted content](../concepts/untrusted-content.md).
- The batch tools (`move`, `copy`, `archive`, `trash`, `expunge`, `set_flags`, `set_labels`) take 1 to 500 refs, from any accounts. Each acts on each message's own account.
- The send and draft tools take `attachments` and `inline[].path` as absolute paths of regular files. Each file is uploaded to the daemon first, and each upload is audited as `upload`.
- `body_format` is `markdown` (the default), `text` or `html`, as `--text` and `--html-file` choose on the CLI. With `html`, `body` holds the HTML and the optional `html_text` gives the plain-text part, which is otherwise converted from the HTML.
- `headers` adds extra headers to a message: `Reply-To` and headers starting with `X-` only. Any other name, a value with a line break, or the same name twice in different case fails with `INVALID_ARGUMENT`. The CLI has no flag for extra headers.
- `inline` is an array of objects, each with `name` (how the body refers to the image, as in `![chart](chart.png)`) and `path` (the image's absolute path). Both are required.
- Every successful result carries structured content and a text block: a short summary, then the result as JSON. A failed call is a result with `isError: true` and the structured content `{"error":{"reason":...,"message":...,"hint":...}}`. The reasons are those on [Exit codes](./exit-codes.md).

## Annotations

Every tool sets all four MCP annotations explicitly, because some clients read a missing hint as its spec default.

| Tool | Title | Read-only | Destructive | Idempotent | Open world |
|---|---|---|---|---|---|
| `archive` | Archive messages | no | no | yes | yes |
| `cache_status` | Show the mail cache | yes | no | yes | no |
| `copy` | Copy messages | no | no | no | yes |
| `create_draft` | Create a draft | no | no | no | yes |
| `create_folder` | Create a folder | no | no | yes | yes |
| `create_item` | Create a work item | no | no | no | no |
| `create_rule` | Create a rule | no | no | no | no |
| `delete_draft` | Delete a draft | no | with expunge allowed | yes | yes |
| `delete_folder` | Delete a folder | no | yes | yes | yes |
| `delete_item` | Delete a work item | no | no | yes | no |
| `delete_rule` | Delete a rule | no | no | yes | no |
| `download_attachment` | Download an attachment | yes | no | yes | yes |
| `dry_run_rule` | Dry-run a rule | yes | no | yes | no |
| `edit_criteria` | Edit acceptance criteria | no | no | no | no |
| `expunge` | Expunge messages | no | yes | yes | yes |
| `forward` | Forward a message | no | no | no | yes |
| `get_item` | Show a work item | yes | no | yes | no |
| `get_message` | Read messages | yes | no | yes | yes |
| `get_rule` | Show a rule | yes | no | yes | no |
| `get_thread` | Read a thread | yes | no | yes | yes |
| `link_thread` | Link a thread to an item | no | no | yes | no |
| `list_accounts` | List accounts | yes | no | yes | no |
| `list_folders` | List folders | yes | no | yes | yes |
| `list_items` | List work items | yes | no | yes | no |
| `list_rules` | List rules | yes | no | yes | no |
| `move` | Move messages | no | no | yes | yes |
| `read_events` | Read mail events | no | no | no | yes |
| `rename_folder` | Rename a folder | no | no | no | yes |
| `reply` | Reply to a message | no | no | no | yes |
| `restore_item` | Restore a work item | no | no | yes | no |
| `restore_rule` | Restore a rule | no | no | yes | no |
| `run_rule` | Run a rule now | no | yes | no | yes |
| `save_raw_message` | Save the raw message | yes | no | yes | yes |
| `search` | Search mail | yes | no | yes | yes |
| `send_draft` | Send a draft | no | with expunge allowed | no | yes |
| `send_email` | Send an email | no | no | no | yes |
| `set_blocker` | Set a blocker | no | no | yes | no |
| `set_flags` | Set flags | no | no | yes | yes |
| `set_labels` | Set Gmail labels | no | no | yes | yes |
| `test_account` | Test an account | yes | no | yes | yes |
| `tick_criterion` | Tick a criterion | no | no | yes | no |
| `trash` | Move messages to Trash | no | no | yes | yes |
| `unlink_thread` | Unlink a thread from an item | no | no | yes | no |
| `update_draft` | Update a draft | no | with expunge allowed | no | yes |
| `update_item` | Update a work item | no | no | yes | no |
| `update_rule` | Update a rule | no | no | yes | no |

`update_draft`, `send_draft` and `delete_draft` are destructive only while the policy allows expunge on at least one account. The daemon then expunges the old draft instead of moving it to Trash. Otherwise they are not destructive. No other annotation depends on the policy.

`read_events` changes no mail, but it is neither read-only nor idempotent, since a read with a group moves the group's acknowledged position. `create_item` and `edit_criteria` are not idempotent, since a retry without a key makes a second item and a retried add appends again. The other item tools are idempotent and not destructive, since deletes are soft. No item tool is open world.

`run_rule` is destructive, open world and not idempotent, since a rule can move, trash and forward mail on the servers, and a second run acts on mail the first did not reach. `create_rule` is not idempotent. `update_rule`, `delete_rule` and `restore_rule` are idempotent and not destructive, since deletes are soft. `get_rule`, `list_rules` and `dry_run_rule` are read-only and idempotent. No rule tool but `run_rule` is open world.

## Gated tools

`expunge` and `delete_folder` are listed only while the policy allows them on at least one account, as `list_accounts` reports. The server asks the daemon when it starts, and again before a `tools/list` once its last check is 60 seconds old. It runs no timer, so an idle session writes no audit rows. Each check is one `accounts.list` row.

The same check sets the draft tools' `destructiveHint`. When the answer changes the list or that hint, the server sends `notifications/tools/list_changed`. While the daemon cannot be reached, the two tools are not listed and the draft tools are not marked destructive.

The daemon still checks the policy on every call, so a stale list leads at worst to a `POLICY_DENIED` result. See [Policy in operation](../operations/policy.md).

## Audit actions

The daemon audits each call with the client kind `mcp`. Each tool below names its action, which `chainmail audit --action` filters on. See [The audit log](../guide/audit-log.md).

## Accounts and folders

### `list_accounts`

Lists the configured accounts: name, provider, address, auth state, and whether the policy lets `expunge` and `delete_folder` act on each.

No arguments.

Returns `accounts`. Read-only, idempotent. Audit action `accounts.list`.

### `test_account`

Logs in to an account's IMAP and SMTP servers and reports each check. Use it when calls to an account fail with `AUTH_REQUIRED`, `AUTH_REJECTED` or `SERVER_UNAVAILABLE`. The IMAP capabilities and a server's error text are untrusted.

| Argument | Type | Required | Meaning |
|---|---|---|---|
| `account` | string | yes | the account to test, as list_accounts names it |

Returns `account`, `imap` and `smtp`. Read-only, idempotent, open world. Audit action `account.test`.

### `list_folders`

Lists folders with their role, message count and unseen count, for every account or those named. Each folder's untrusted name is the name `move`, `copy`, `rename_folder` and search's `in:` take.

| Argument | Type | Required | Meaning |
|---|---|---|---|
| `accounts` | array of strings | no | accounts to list; empty means every account |

Returns `accounts` and `partial`, the accounts or folders that could not be read. Read-only, idempotent, open world. Audit action `folders.list`.

### `create_folder`

Creates a folder and any missing parents. Returns the name the server uses, as untrusted, since a server's namespace prefix can be part of it.

| Argument | Type | Required | Meaning |
|---|---|---|---|
| `account` | string | yes | the account to create it on |
| `name` | string | yes | the full folder name with / between levels; a personal namespace prefix may be left out |

Returns `account` and `name`. Idempotent, open world. Audit action `folders.create`.

### `rename_folder`

Renames a folder and returns the new name the server uses, as untrusted.

| Argument | Type | Required | Meaning |
|---|---|---|---|
| `account` | string | yes | the account the folder is on |
| `from` | string | yes | the folder's current name, as list_folders names it |
| `to` | string | yes | the new name, written as create_folder takes it |

Returns `account` and `name`. Open world. Audit action `folders.rename`.

### `delete_folder`

Deletes a folder, and with `force` the mail in it. Listed only while the policy allows folder deletion on some account. INBOX, role folders and folders with subfolders are refused.

| Argument | Type | Required | Meaning |
|---|---|---|---|
| `account` | string | yes | the account the folder is on |
| `force` | boolean | no | delete it even when it holds mail, which is deleted with it (on Google the messages stay in All Mail) |
| `name` | string | yes | the folder to delete; INBOX, role folders and folders with subfolders are refused |

Returns `account`, `name` and `deleted`. Destructive, idempotent, open world. Audit action `folders.delete`, with the decision `denied` when the policy refuses it.

## Reading

### `search`

Searches with the [query language](../guide/reading-and-searching.md), newest first. Pass `next_cursor` as `cursor` for the next page. Each account is answered from the local cache when the cache can answer the whole query and is fresh, and by its mail server otherwise. `source` and `account_counts` say which. `item:` searches need the cache, so `source: "server"` refuses them.

| Argument | Type | Required | Meaning |
|---|---|---|---|
| `accounts` | array of strings | no | accounts to search, added to any account: terms; empty means every account |
| `count_only` | boolean | no | count matches without listing them |
| `cursor` | string | no | next_cursor from the previous page of the same search |
| `limit` | integer | no | rows per page, 1 to 200; default 25 |
| `query` | string | no | a Gmail-style query such as invoice from:acme newer_than:30d; empty matches every message |
| `snippets` | boolean | no | add a snippet of up to 200 characters to each row |
| `source` | string | no | where answers come from: auto (default) uses the local cache for each account it can answer completely and freshly and the mail server otherwise; cache uses the cache even when it is incomplete or stale; server skips the cache |
| `threads` | boolean | no | collapse rows to the newest message of each thread |

Returns `rows`, `count`, `next_cursor`, `source`, `account_counts` and `partial`. Subjects, names and snippets are untrusted. Read-only, idempotent, open world. Audit action `search`.

### `get_message`

Reads 1 to 50 messages by ref: headers, body text and the attachment list. Reading never marks mail read. Each message says whether it came from the cache or the server. The call succeeds when any ref succeeds, with each failed ref in `errors`.

| Argument | Type | Required | Meaning |
|---|---|---|---|
| `full` | boolean | no | keep quoted text and signatures, and do not cut the body at the daemon's body limit |
| `refs` | array of strings | yes | 1 to 50 refs from search or get_thread |

Returns `messages`, `errors` and `source`. Read-only, idempotent, open world. Audit action `get`.

### `get_thread`

Reads every message in a ref's thread, oldest first. Each message says whether it came from the cache or the server.

| Argument | Type | Required | Meaning |
|---|---|---|---|
| `full` | boolean | no | keep quoted text and signatures, and do not cut bodies at the daemon's body limit |
| `ref` | string | yes | any message of the thread |

Returns `thread_id`, `messages`, `truncated`, `omitted_refs` and `source`. When `truncated` is true, pass `omitted_refs` to `get_message`. Read-only, idempotent, open world. Audit action `thread`.

## Files

`download_attachment` and `save_raw_message` write a new file with mode `0600` and never replace an existing one. Without `dir` they write to `$XDG_CACHE_HOME/chainmail/attachments`, or `~/.cache/chainmail/attachments` when `XDG_CACHE_HOME` is unset or relative. [Working with mail](../agents/working-with-mail.md) gives the full rules for `dir` and file names.

### `download_attachment`

Saves one attachment to a new file and returns its path. The file name comes from the message and is sanitized. A clash gets ` (2)`, ` (3)` before the extension.

| Argument | Type | Required | Meaning |
|---|---|---|---|
| `dir` | string | no | an existing directory as an absolute path, owned by this user and writable by no other user; default ~/.cache/chainmail/attachments |
| `part` | string | yes | an attachment part from get_message, such as 2 or 1.2 |
| `ref` | string | yes | the message, from search or get_message |

Returns `path`, `size`, `content_type` and `filename`. `path`, `content_type` and `filename` are untrusted. Read-only, idempotent, open world. Audit action `attachment`.

### `save_raw_message`

Saves a message's original RFC 5322 bytes to a new `message.eml` file and returns its path.

| Argument | Type | Required | Meaning |
|---|---|---|---|
| `dir` | string | no | an existing directory as an absolute path, owned by this user and writable by no other user; default ~/.cache/chainmail/attachments |
| `ref` | string | yes | the message, from search or get_message |

Returns `path`, `size` and `content_type`, which is always `message/rfc822`. Read-only, idempotent, open world. Audit action `get.raw`.

## Sending and drafts

### Compose results

`send_email`, `reply`, `forward`, `create_draft`, `update_draft` and `send_draft` return one result shape. `status` is always present.

| `status` | Meaning | Fields |
|---|---|---|
| `sent` | The server accepted the message. | `message_id`, as untrusted, `sent_ref`, `sent_copy_error` when the Sent copy failed, `idempotency_key` |
| `draft` | Saved to Drafts, not sent. | `draft_ref` |
| `held` | Held by the policy. The message was not sent. It is in the account's Drafts for James to send, edit or delete. Do not retry it. | `draft_ref`, empty when the server did not report it, `blocked`, as untrusted values, `too_many_recipients`, `hint`, `message_id`, `idempotency_key` |
| `duplicate` | A replay of an earlier send with the same key. Nothing was sent again. | `original_status`, which is `sent`, `idempotency_key` |

A hold is a result, not an error, so `HELD` never appears as an error reason in MCP. See [The send policy](../concepts/policy.md).

`send_email`, `reply`, `forward` and `send_draft` take an optional `idempotency_key`. Without one, the server derives a key from the tool name, the input and the files, so an identical call within the daemon's 7 days replays the first result. `create_draft`, `update_draft`, and `reply` or `forward` with `draft: true` derive no key, so each call saves a new draft. [Working with mail](../agents/working-with-mail.md) explains how the key is derived.

### `send_email`

Sends a new message from an account.

| Argument | Type | Required | Meaning |
|---|---|---|---|
| `account` | string | yes | the account to send from, as list_accounts names it |
| `attachments` | array of strings | no | absolute paths of files to attach, in order |
| `bcc` | array of strings | no | blind-copied recipients |
| `body` | string | no | the new text, Markdown unless body_format says otherwise |
| `body_format` | string | no | markdown (the default), text or html |
| `cc` | array of strings | no | copied recipients |
| `headers` | object | no | extra headers: Reply-To and X- headers only |
| `html_text` | string | no | with body_format html: the plain-text part; empty means derived from the HTML |
| `idempotency_key` | string | no | 1 to 128 of A-Z a-z 0-9 . _ : -; a repeat with the same key does not send twice; derived from the input when empty |
| `inline` | array of objects | no | images the body shows inline |
| `no_signature` | boolean | no | leave out the account's configured signature |
| `subject` | string | yes | the subject line |
| `to` | array of strings | yes | recipients, each `Name <a@b>` or `a@b` |

Returns a [compose result](#compose-results). Open world. Audit action `send`, plus one `upload` row per file.

### `reply`

Replies to a message, quoting it. `all` replies to everyone. `draft` saves the reply instead of sending it.

| Argument | Type | Required | Meaning |
|---|---|---|---|
| `all` | boolean | no | reply to everyone: add the original To and Cc, less this account's own address |
| `attachments` | array of strings | no | absolute paths of files to attach, in order |
| `bcc` | array of strings | no | blind-copied recipients |
| `body` | string | no | the new text, Markdown unless body_format says otherwise |
| `body_format` | string | no | markdown (the default), text or html |
| `cc` | array of strings | no | more copied recipients |
| `draft` | boolean | no | save the reply to Drafts instead of sending it |
| `headers` | object | no | extra headers: Reply-To and X- headers only |
| `html_text` | string | no | with body_format html: the plain-text part; empty means derived from the HTML |
| `idempotency_key` | string | no | as in send_email |
| `inline` | array of objects | no | images the body shows inline |
| `no_signature` | boolean | no | leave out the account's configured signature |
| `ref` | string | yes | the message to reply to |
| `to` | array of strings | no | more recipients, added to the ones the reply goes to |

Returns a [compose result](#compose-results). Open world. Audit action `reply`, or `draft.create` with `draft`, plus one `upload` row per file.

### `forward`

Forwards a message with its attachments unless `no_attachments`. `draft` saves the forward instead of sending it.

| Argument | Type | Required | Meaning |
|---|---|---|---|
| `attachments` | array of strings | no | absolute paths of files to attach, in order |
| `bcc` | array of strings | no | blind-copied recipients |
| `body` | string | no | the new text, Markdown unless body_format says otherwise |
| `body_format` | string | no | markdown (the default), text or html |
| `cc` | array of strings | no | copied recipients |
| `draft` | boolean | no | save the forward to Drafts instead of sending it |
| `headers` | object | no | extra headers: Reply-To and X- headers only |
| `html_text` | string | no | with body_format html: the plain-text part; empty means derived from the HTML |
| `idempotency_key` | string | no | as in send_email |
| `inline` | array of objects | no | images the body shows inline |
| `no_attachments` | boolean | no | leave out the original's attachments |
| `no_signature` | boolean | no | leave out the account's configured signature |
| `ref` | string | yes | the message to forward |
| `to` | array of strings | yes | recipients, each `Name <a@b>` or `a@b` |

Returns a [compose result](#compose-results). Open world. Audit action `forward`, or `draft.create` with `draft`, plus one `upload` row per file.

### `create_draft`

Saves a new message to the account's Drafts without sending it.

| Argument | Type | Required | Meaning |
|---|---|---|---|
| `account` | string | yes | the account whose Drafts folder gets it |
| `attachments` | array of strings | no | absolute paths of files to attach, in order |
| `bcc` | array of strings | no | blind-copied recipients, kept in a Bcc header until sent |
| `body` | string | no | the new text, Markdown unless body_format says otherwise |
| `body_format` | string | no | markdown (the default), text or html |
| `cc` | array of strings | no | copied recipients |
| `headers` | object | no | extra headers: Reply-To and X- headers only |
| `html_text` | string | no | with body_format html: the plain-text part; empty means derived from the HTML |
| `inline` | array of objects | no | images the body shows inline |
| `no_signature` | boolean | no | leave out the account's configured signature |
| `subject` | string | no | the subject line |
| `to` | array of strings | no | recipients, each `Name <a@b>` or `a@b` |

Returns a [compose result](#compose-results) with `status` `draft`. Open world. Audit action `draft.create`, plus one `upload` row per file.

### `update_draft`

Replaces a draft with a new version. The result's `draft_ref` is the new ref, and the old one is gone.

| Argument | Type | Required | Meaning |
|---|---|---|---|
| `attachments` | array of strings | no | absolute paths of files to attach, in order |
| `bcc` | array of strings | no | blind-copied recipients |
| `body` | string | no | the new text, Markdown unless body_format says otherwise |
| `body_format` | string | no | markdown (the default), text or html |
| `cc` | array of strings | no | copied recipients |
| `headers` | object | no | extra headers: Reply-To and X- headers only |
| `html_text` | string | no | with body_format html: the plain-text part; empty means derived from the HTML |
| `inline` | array of objects | no | images the body shows inline |
| `no_signature` | boolean | no | leave out the account's configured signature |
| `ref` | string | yes | the draft to replace; it must be in Drafts |
| `subject` | string | no | the subject line |
| `to` | array of strings | no | recipients of the new version |

Returns a [compose result](#compose-results). Open world, and destructive while the policy allows expunge on some account. Audit action `draft.update`, plus one `upload` row per file.

### `send_draft`

Sends a draft and removes it from Drafts.

| Argument | Type | Required | Meaning |
|---|---|---|---|
| `idempotency_key` | string | no | as in send_email |
| `ref` | string | yes | the draft to send; it must be in Drafts |

Returns a [compose result](#compose-results). Open world, and destructive while the policy allows expunge on some account. Audit action `draft.send`.

### `delete_draft`

Deletes a draft. While `allow_expunge` is off, the daemon moves it to Trash.

| Argument | Type | Required | Meaning |
|---|---|---|---|
| `ref` | string | yes | the draft to delete; it must be in Drafts |

Returns `ref` and `deleted`. Idempotent, open world, and destructive while the policy allows expunge on some account. Audit action `draft.delete`.

## Organizing

The batch tools return `outcomes`, one per ref in the order given, with `succeeded` and `failed` counts. Each outcome has `ref`, `new_ref` when the message has a new ref afterwards, and `error` when that ref failed. The other refs still ran. Only when every ref fails is the call an error result, with the first failure's reason. A batch that spans accounts records one audit row per account. See [Organizing mail](../guide/organizing.md).

### `move`

Moves messages to a folder on each message's own account. Use each outcome's `new_ref` from then on.

| Argument | Type | Required | Meaning |
|---|---|---|---|
| `folder` | string | yes | the destination folder on each message's own account, as list_folders names it |
| `refs` | array of strings | yes | 1 to 500 refs from search, from any accounts |

Idempotent, open world. Audit action `move`.

### `copy`

Copies messages to a folder. `new_ref` is the copy's ref.

| Argument | Type | Required | Meaning |
|---|---|---|---|
| `folder` | string | yes | the destination folder on each message's own account, as list_folders names it |
| `refs` | array of strings | yes | 1 to 500 refs from search, from any accounts |

Open world. Audit action `copy`.

### `archive`

Moves messages out of the inbox into the account's archive: the `archive_folder` on most accounts, and removing the Inbox label on Google.

| Argument | Type | Required | Meaning |
|---|---|---|---|
| `refs` | array of strings | yes | 1 to 500 refs from search, from any accounts |

Idempotent, open world. Audit action `archive`.

### `trash`

Moves messages to the account's Trash. Nothing is deleted for good.

| Argument | Type | Required | Meaning |
|---|---|---|---|
| `refs` | array of strings | yes | 1 to 500 refs from search, from any accounts |

Idempotent, open world. Audit action `trash`.

### `expunge`

Deletes messages for good. Listed only while the policy allows expunge on some account. The daemon still checks each message's account.

| Argument | Type | Required | Meaning |
|---|---|---|---|
| `refs` | array of strings | yes | 1 to 500 refs from search, from any accounts |

Destructive, idempotent, open world. Audit action `expunge`, with the decision `denied` when the policy refuses it.

### `set_flags`

Adds and removes `seen`, `flagged`, `answered`, `draft` and keywords.

| Argument | Type | Required | Meaning |
|---|---|---|---|
| `add` | array of strings | no | flags to add: seen, flagged, answered, draft, or keywords of A-Z a-z 0-9 $ _ . - |
| `refs` | array of strings | yes | 1 to 500 refs from search, from any accounts |
| `remove` | array of strings | no | flags to remove, named as in add |

Idempotent, open world. Audit action `flags`.

### `set_labels`

Adds and removes Gmail labels. Other accounts refuse it. Use `move` there.

| Argument | Type | Required | Meaning |
|---|---|---|---|
| `add` | array of strings | no | labels to add; a missing label is created; the system labels \Inbox, \Important, \Starred, \Sent and \Draft are accepted |
| `refs` | array of strings | yes | 1 to 500 refs from search, on Google accounts |
| `remove` | array of strings | no | labels to remove |

Idempotent, open world. Audit action `labels`.

## Events and the cache

### `read_events`

Reads mail, item, rule and due events after a cursor or a group's acknowledged position, oldest first, waiting up to 60 seconds for the first. A group read acknowledges what it returns. New events carry the message's search row as untrusted. `item`, `item_mail`, `rule` and due events come only when `kinds` names them. Setting `item` also brings that item's `item`, `item_mail` and due events. See [Watching for mail](../guide/watching.md), [Work items](../guide/items.md) and [Rules](../guide/rules.md).

To wait for mail, call `read_events` with a `group` and `wait_seconds` 60, again and again.

| Argument | Type | Required | Meaning |
|---|---|---|---|
| `accounts` | array of strings | no | accounts to return events for; empty means every account |
| `cursor` | integer | no | next_cursor from an earlier read; reads the events after it; 0 reads from the oldest event kept; not with group |
| `group` | string | no | a consumer group name of lowercase letters, digits, dot, dash and underscore, up to 64 characters, not starting with cache. or rules.; reads after the group's acknowledged event and acknowledges what it returns; not with cursor |
| `in` | string | no | a folder name or Gmail label, in any case; inbox also matches the Gmail label \Inbox and sent matches \Sent |
| `item` | string | no | a work item ID: only item, item_mail and due events of that item and every live item below it, plus status and gap; new, flags, expunged and rule events are left out |
| `kinds` | array of strings | no | event kinds to return: new, flags, expunged, status, gap, item, item_mail, rule, item_due_soon, item_due, item_overdue; empty means every kind but the opt-in ones (item, item_mail, rule and the three due kinds), unless item is set, when it means item, item_mail, the due kinds, status and gap; status and gap events pass a list that names neither |
| `limit` | integer | no | events per read, 1 to 200; default 50 |
| `wait_seconds` | integer | no | seconds to wait for the first event when none is ready, 0 to 60; default 0 returns at once |

Returns `events` and `next_cursor`. Open world. Audit action `watch.read`.

### `cache_status`

Reports each account's cache: its state, backfill and body coverage per folder, today's download against the budget, and the cache's size. It holds no message content. Folder names are untrusted. See [The local cache](../concepts/cache.md).

| Argument | Type | Required | Meaning |
|---|---|---|---|
| `accounts` | array of strings | no | accounts to report; empty means every account |

Returns `accounts`, `size_bytes` and `max_size_bytes`. Read-only, idempotent. Not audited, like `chainmail cache status`.

## Work items

Item calls pass no policy gate. Deletes are soft. Titles, descriptions, criterion text, evidence and link notes come back as untrusted, whoever wrote them. Each change returns the new `revision`. See [Work items](../guide/items.md).

### `create_item`

Creates an item, with criteria when given. With `idempotency_key`, a retry returns the first item with `duplicate` set.

| Argument | Type | Required | Meaning |
|---|---|---|---|
| `area` | string | no | work or personal; required for an item with no parent_id, inherited from the parent when empty |
| `criteria` | array of strings | no | acceptance criteria, each 1 to 500 characters, in order |
| `description` | string | no | markdown, at most 64 KiB |
| `due_date` | string | no | YYYY-MM-DD |
| `estimate` | number | no | a non-negative number |
| `idempotency_key` | string | no | 1 to 128 of A-Z a-z 0-9 . _ : -; a repeat with the same key and input returns the first item instead of making another; a different input with the same key is refused |
| `parent_id` | string | no | the parent item's ID; empty makes a root item, or puts a task in the inbox |
| `priority` | string | no | p0 to p3; inherited from the parent when empty, p2 at the root |
| `start_date` | string | no | YYYY-MM-DD |
| `state` | string | no | backlog (default), in-progress, in-review or done |
| `title` | string | yes | 1 to 200 characters |
| `type` | string | yes | objective, key-result, initiative, epic, feature, story, bug, support-ticket or task |

Returns `item`, `criteria` and `duplicate`. Not read-only, not destructive, not idempotent, not open world. Audit action `item.create`, with the decision `duplicate` on a keyed retry.

### `update_item`

Changes the fields given, and clears those named in `clear`. A changed `parent_id` moves the item, and a cleared one moves it to the top, or a task to the inbox.

| Argument | Type | Required | Meaning |
|---|---|---|---|
| `area` | string | no | work or personal |
| `clear` | array of strings | no | fields to clear: description, area, priority, start_date, due_date, estimate or parent_id; a field cannot be both given and cleared |
| `description` | string | no | a new markdown description, at most 64 KiB |
| `due_date` | string | no | YYYY-MM-DD |
| `estimate` | number | no | a non-negative number |
| `id` | string | yes | the item to change |
| `parent_id` | string | no | move the item under this item; to move it to the root or the inbox, name parent_id in clear |
| `priority` | string | no | p0 to p3 |
| `start_date` | string | no | YYYY-MM-DD |
| `state` | string | no | backlog, in-progress, in-review or done; done needs every criterion ticked |
| `title` | string | no | a new title, 1 to 200 characters |
| `type` | string | no | a new type; the parent rule is checked against the parent and every child |

Returns `item`. Idempotent. Audit action `item.update`.

### `get_item`

Reads an item with its criteria, its ancestors, up to 50 live children, its live blockers both ways, and up to 50 linked threads, each with its latest message.

| Argument | Type | Required | Meaning |
|---|---|---|---|
| `id` | string | yes | the item's ID, such as it_admin_work |

Returns `item`, `criteria`, `ancestors`, `children`, `children_total`, `blocked_by`, `blocks`, `threads` and `threads_total`. Read-only, idempotent. Audit action `item.get`.

### `list_items`

Lists items in tree order, filtered by type, state, effective area, a subtree, the inbox, ongoing items or deleted items, or by `changed_since` in revision order.

| Argument | Type | Required | Meaning |
|---|---|---|---|
| `area` | string | no | only items whose effective area is this: work or personal |
| `changed_since` | integer | no | a revision from an earlier list: only items changed after it, deleted ones included, in revision order |
| `inbox` | boolean | no | only the inbox: live tasks with no parent, still to be sorted |
| `include_deleted` | boolean | no | include soft-deleted items |
| `limit` | integer | no | rows per page, 1 to 500; default 100 |
| `ongoing` | boolean | no | true for only the standing Admin items, false to leave them out; absent for both |
| `page_token` | string | no | next_page_token from the previous page of the same list |
| `state` | string | no | only items in this state: backlog, in-progress, in-review or done |
| `type` | string | no | only items of this type |
| `under` | string | no | only the items below this item, not the item itself |

Returns `items`, `revision` and `next_page_token`. Read-only, idempotent. Audit action `item.list`.

### `delete_item`

Soft-deletes an item, or with `cascade` its live subtree. The Admin items cannot be deleted.

| Argument | Type | Required | Meaning |
|---|---|---|---|
| `cascade` | boolean | no | also delete every live item below it; needed when it has live children |
| `id` | string | yes | the item to delete |

Returns `ids` and `revision`. Idempotent. Audit action `item.delete`.

### `restore_item`

Restores a deleted item, or with `cascade` the subtree deleted with it. Restore a deleted parent first.

| Argument | Type | Required | Meaning |
|---|---|---|---|
| `cascade` | boolean | no | also restore the items below it that the same delete removed |
| `id` | string | yes | the deleted item to restore |

Returns `ids` and `revision`. Idempotent. Audit action `item.restore`.

### `edit_criteria`

Removes, edits, reorders and adds an item's criteria in one call, in that order.

| Argument | Type | Required | Meaning |
|---|---|---|---|
| `add` | array of strings | no | new criteria, each 1 to 500 characters, appended in this order; applied last |
| `edit` | array of objects | no | criteria to reword; applied second |
| `item_id` | string | yes | the item whose criteria change |
| `order` | array of strings | no | every remaining criterion ID once, in the new order; applied third |
| `remove` | array of strings | no | criterion IDs to remove; applied first |

Each `edit` entry is an object with `id` (a criterion ID from `get_item`) and `text` (the new text, 1 to 500 characters). Both are required.

Returns `criteria` and `revision`. Not idempotent. Audit action `item.criteria`.

### `tick_criterion`

Ticks a criterion with evidence, or unticks it with `ticked` false. An item can be set to `done` only when every criterion is ticked.

| Argument | Type | Required | Meaning |
|---|---|---|---|
| `criterion_id` | string | yes | a criterion ID from get_item |
| `evidence` | string | no | required when ticking: how the criterion was met, 1 to 1000 characters; must be empty when unticking |
| `item_id` | string | yes | the item |
| `ticked` | boolean | yes | true ticks the criterion, false unticks it |

Returns `criterion` and `revision`. Idempotent. Audit action `item.tick`.

### `set_blocker`

Adds or removes a blocker between two items. An item cannot block itself, and a blocker that would close a cycle is refused.

| Argument | Type | Required | Meaning |
|---|---|---|---|
| `blocked_by_id` | string | yes | the item it waits on |
| `item_id` | string | yes | the item that waits |
| `linked` | boolean | yes | true records that item_id is blocked by blocked_by_id; false removes that |

Returns `changed` and `revision`. Idempotent. Audit action `item.block`.

### `link_thread`

Links a message's thread to an item, anchored on the message when `anchor` is set, with an optional note. New replies on the thread arrive as `item_mail` events.

| Argument | Type | Required | Meaning |
|---|---|---|---|
| `anchor` | boolean | no | make this message the link's anchor |
| `item_id` | string | yes | the live item to file the thread under |
| `note` | string | no | why the thread is linked, at most 500 characters; empty keeps an existing note |
| `ref` | string | yes | any message of the thread, from search, get_thread or read_events |

Returns `link`, `created` and `revision`. Idempotent. Audit action `item.link`.

### `unlink_thread`

Removes a thread link, named by the `account` and `thread_key` that `get_item` lists.

| Argument | Type | Required | Meaning |
|---|---|---|---|
| `account` | string | yes | the link's account, from get_item's threads |
| `item_id` | string | yes | the item |
| `thread_key` | string | yes | the link's thread_key, from get_item's threads |

Returns `revision`. Idempotent. Audit action `item.unlink`.

## Rules

Rule calls pass no policy gate, but every action a rule takes does. Deletes are soft. Descriptions, queries and forward notes come back as untrusted, as item text does. Every tool that names a rule takes its ID or its name, except `restore_rule`, which takes the ID. Dry-run a rule before you create it and before you run it by hand. See [Rules](../guide/rules.md).

### `create_rule`

Creates a rule from a definition and returns it. Dry-run the definition with `dry_run_rule` first.

| Argument | Type | Required | Meaning |
|---|---|---|---|
| `accounts` | array of strings | no | the accounts the rule covers; empty means every account |
| `actions` | array of objects | yes | 1 to 16 actions, run in order on each match |
| `description` | string | no | what the rule is for, at most 1000 characters |
| `enabled` | boolean | no | false saves the rule switched off; default true |
| `max_per_run` | integer | no | the most messages one run acts on per account, 1 to 5000; default 500 |
| `name` | string | yes | lowercase letters, digits and dashes, starting with a letter or digit, up to 63 characters, such as stripe-receipts; unique among live rules |
| `on_new` | boolean | no | run the rule on new mail as the local cache takes it |
| `priority` | integer | no | -1000000 to 1000000; lower runs first; default 100 |
| `query` | string | yes | the messages to act on, in the language search takes, such as from:stripe.com subject:receipt |
| `schedule` | string | no | `every:<duration>` from 15m to 30d, such as every:6h or every:7d, or at:HH:MM once a day; empty for none |
| `stop` | boolean | no | keep later rules off every message this rule matched |

Each `actions` entry is an object with these fields, of which an action sets only those its `kind` takes:

| Field | Type | Required | Meaning |
|---|---|---|---|
| `anchor` | boolean | no | link_item: make the message the link's anchor |
| `folder` | string | no | copy and move: the folder, 1 to 255 bytes |
| `item_id` | string | no | link_item: the live work item to file the message's thread under |
| `kind` | string | yes | label, unlabel, copy, flag, unflag, read, unread, link_item, forward, move, archive or trash |
| `label` | string | no | label and unlabel: the Gmail label, 1 to 255 bytes |
| `note` | string | no | forward: a note sent with the forward, at most 2000 characters |
| `to` | array of strings | no | forward: the addresses to forward to, up to the policy's max_recipients |

Returns `rule`. Not idempotent. Audit action `rule.create`.

### `update_rule`

Changes the fields given, and leaves the rest. `clear` empties `description`, `accounts` or `schedule`. With `expected_revision`, a rule that changed after that revision is refused. Setting `enabled` turns a rule on or off, and turning it on clears an auto-disable.

| Argument | Type | Required | Meaning |
|---|---|---|---|
| `accounts` | array of strings | no | the accounts the rule covers, replacing the old list; to cover every account, name accounts in clear |
| `actions` | array of objects | no | 1 to 16 actions that replace every old one, in order |
| `clear` | array of strings | no | fields to clear: description, accounts (the rule then covers every account) or schedule (the rule then runs only on new mail or through run_rule); a field cannot be both given and cleared |
| `description` | string | no | a new description, at most 1000 characters |
| `enabled` | boolean | no | switch the rule on or off; pass it alone to do only that |
| `expected_revision` | integer | no | the revision get_rule showed; the change is refused if the rule changed since |
| `max_per_run` | integer | no | 1 to 5000 |
| `name` | string | no | a new name: lowercase letters, digits and dashes, starting with a letter or digit, up to 63 characters |
| `on_new` | boolean | no | run the rule on new mail, or not |
| `priority` | integer | no | -1000000 to 1000000; lower runs first |
| `query` | string | no | a new query, in the language search takes |
| `rule` | string | yes | the rule's ID (ru_...) or name |
| `schedule` | string | no | `every:<duration>` from 15m to 30d, or at:HH:MM; to unschedule, name schedule in clear |
| `stop` | boolean | no | keep later rules off every message this rule matched, or not |

`actions` entries have the fields of `create_rule`'s.

Returns `rule`. Idempotent. Audit action `rule.update`.

### `get_rule`

Reads a rule with its next run, its last 10 runs and its counts, and with `history` up to 50 revisions, newest first.

| Argument | Type | Required | Meaning |
|---|---|---|---|
| `history` | boolean | no | also return up to 50 revisions, newest first |
| `rule` | string | yes | the rule's ID (ru_...) or name; a deleted rule by ID only |

Returns `counts`, `revisions`, `rule` and `runs`. Read-only, idempotent. Audit action `rule.get`.

### `list_rules`

Lists rules in priority order, filtered by account, by enabled, or to deleted rules only.

| Argument | Type | Required | Meaning |
|---|---|---|---|
| `account` | string | no | only rules that cover this account, by name or by covering every account |
| `deleted` | boolean | no | list the soft-deleted rules instead of the live ones |
| `enabled` | boolean | no | true for only the rules that are on, false for only those that are off; absent for both |

Returns `rules`. Read-only, idempotent. Audit action `rule.list`.

### `delete_rule`

Soft-deletes a rule. A deleted rule never runs and keeps its runs.

| Argument | Type | Required | Meaning |
|---|---|---|---|
| `rule` | string | yes | the rule's ID (ru_...) or name |

Returns `rule`. Idempotent. Audit action `rule.delete`.

### `restore_rule`

Restores a deleted rule, named by its ID. It fails with `INVALID_ARGUMENT` when a live rule took its name.

| Argument | Type | Required | Meaning |
|---|---|---|---|
| `rule` | string | yes | the deleted rule's ID (ru_...); list_rules with deleted true shows it |

Returns `rule`. Idempotent. Audit action `rule.restore`.

### `dry_run_rule`

Matches a saved rule, or an unsaved definition, without acting and without writing anything. For each account it returns the match count, the count already acted on, the first 50 matches as untrusted search rows, and the actions each would get. An account the cache cannot answer completely gets a `reason` and no matches. With trigger schedule or manual, an account whose covered folders hold messages the cache could not read gets the reason `unreadable_messages`, as a real run would. It waits up to 10 minutes.

| Argument | Type | Required | Meaning |
|---|---|---|---|
| `account` | string | no | only this account; empty means every account the rule covers |
| `definition` | object | no | an unsaved rule, with create_rule's fields; not with rule |
| `rule` | string | no | a saved rule's ID (ru_...) or name; not with definition |
| `trigger` | string | no | new tries the newest 200 cached messages per account, as for new mail; schedule and manual, the default, try the whole cache |

`definition` has the arguments of `create_rule`.

Returns `accounts`. Read-only, idempotent. Audit action `rule.dry_run`.

### `run_rule`

Runs a rule now on each account it covers, or on one, and returns one run per account. It acts on mail, so dry-run the rule first. `max` caps the messages it acts on per account. It waits up to 10 minutes.

A run whose cache was not ready comes back as a run with outcome `cache_not_ready`, not as an error. When the call fails on an account, such as when it runs out of time, it returns only the error. The runs on the other accounts are still recorded and may have acted on mail. Read the rule with `get_rule`, whose last 10 runs show what each account did, before you call `run_rule` again. An operator can list every run with `chainmail rule runs`.

| Argument | Type | Required | Meaning |
|---|---|---|---|
| `account` | string | no | only this account; empty means every account the rule covers |
| `max` | integer | no | the most messages to act on per account, 1 to 5000; default the rule's max_per_run |
| `rule` | string | yes | the rule's ID (ru_...) or name |

Returns `runs`. Destructive, open world. Audit action `rule.run`, and each action the rule takes is audited under client kind `rule`.

## Not exposed

These are operator actions and have no tool: the admin calls (`secret set`, `auth login`, `admin status`, `audit`), `send-raw`, `accounts probe`, `cache rebuild` and `cache evict`. Uploads are not a tool of their own, since the send and draft tools upload their files.

Most of the watch service has no tool either. `read_events` reads a page at a time in place of the event stream, and a group read acknowledges itself in place of a separate `Ack`. `watch status`, `watch groups` and `watch groups delete` have no tool. Use the [commands](./commands/chainmail.md) for all of these.

`ListRuleRuns` and `RulesStatus` have no tool either, since run history and the runners' state are for operators. Use `chainmail rule runs` and `chainmail rule status`. `get_rule` returns a rule's last 10 runs and its counts.
