---
title: Reading and searching
description: List folders, search mail with Gmail-style operators, and read messages, threads and attachments.
sidebar_position: 1
---

# Reading and searching

Five commands read mail: `folders`, `search`, `get`, `thread` and `attachment`.

Reading never changes read state. chainmail opens every folder read-only with `EXAMINE` and fetches every body with `BODY.PEEK`. A message an agent reads stays unread in your other mail clients. The integration tests and the live test check this after `get`, `get --raw`, `thread` and `attachment`.

Every message in a result carries a `ref`, which names it to the other commands. Refs and cursors are opaque, so pass them back exactly as you got them. [Refs and cursors](../concepts/refs-and-cursors.md) explains how they work and when they go stale.

## List folders

`folders` lists every account's folders with their role, message count and unseen count. `--account` limits it to the named accounts, and repeats.

```sh
chainmail folders --account work
```

Folders that cannot be opened, such as Gmail's `[Gmail]` parent, are left out. Each folder has a role: `inbox`, `sent`, `drafts`, `archive`, `trash`, `spam`, `all` or `none`.

With JSON output it prints `{"accounts":[{"account":...,"folders":[...]}],"partial":[...]}`. An account it could not read is listed in `partial` with its reason and hint, and the command still succeeds.

## Search

`search` takes a query, described in [the query language](#the-query-language), and lists the matching messages newest first by arrival time. Without `account:` or `--account` it searches every account.

```sh
chainmail search invoice from:acme newer_than:30d
```

| Flag | Default | Meaning |
|---|---|---|
| `--account` | every account | Only this account. Repeat it for more. |
| `--limit` | `50` | Rows per page, at most 500. |
| `--cursor` | none | Continue from the previous page's `next_cursor`. |
| `--count` | off | Count matches per account instead of listing them. A count takes no cursor. |
| `--snippet` | off | Add up to 200 characters of each body to each row. |
| `--threads` | off | Show only the newest message of each thread. |
| `--source` | `auto` | What answers: `auto`, `cache` or `server`. See [the local cache](../concepts/cache.md). |

With JSON output it prints:

```text
{"rows":[...],"next_cursor":...,"count":...,"account_counts":[{"account":...,"count":...,"source":...,"synced_at":...,"complete":...,"incomplete_reason":...}],"partial":[...],"source":...,"fetched_at":...}
```

When there are more rows, the response has a `next_cursor`, and on a terminal the last line reads `more: --cursor <cursor>`. Pass the cursor to the same search to get the next page:

```sh
chainmail search invoice --cursor 'cursor-from-the-previous-page'
```

A page can hold fewer rows than the limit, or none, while `next_cursor` is still set. Keep paging until `next_cursor` is empty.

An account or folder that a search could not read is listed in `partial` with its reason and hint. The rest of the response is still valid, and the command still succeeds. It fails only when every source failed.

## The query language

`search` takes Gmail-style operators. `chainmail search --help` shows the same list.

| Operator | Matches |
|---|---|
| `word`, `"a phrase"` | text anywhere in the message |
| `from:`, `to:`, `cc:`, `bcc:` | an address or display name containing the value |
| `subject:` | a subject containing the value |
| `is:unread`, `is:read`, `is:starred` | read state, or the starred flag |
| `newer_than:`, `older_than:` | an age in days, weeks, months or years, such as `7d`, `2w`, `3m` or `1y` |
| `after:`, `before:` | a date as `YYYY-MM-DD` |
| `larger:`, `smaller:` | a size in bytes, or in KiB with `K` or MiB with `M`, such as `10K` or `5M` |
| `has:attachment`, `filename:`, `label:` | an attachment, an attachment file name containing the value, or a Gmail label. On other accounts `label:` matches an IMAP keyword, and all three are answered from the cache once it is complete. |
| `in:` | `inbox`, `sent`, `drafts`, `archive`, `trash`, `spam` (or `junk`), `anywhere`, or a folder name |
| `account:` | the named account |
| `item:<id>`, `item:none` | messages in threads linked to the item or to a live item below it, or in threads no live item links. Only the cache answers these. See [finding mail by item](./items.md#finding-mail-by-item). |

### Combining terms

- Terms separated by spaces must all match.
- `OR` between two terms matches either. It binds tighter than the spaces, as in Gmail, so `a b OR c` means a and (b or c).
- Parentheses group terms.
- A `-` before a term negates it.
- Quote a value that holds spaces, such as `subject:"q3 report"`.

`in:` and `account:` choose where to search. So they cannot be negated or joined with `OR`, and a query takes at most one `in:`. Each `account:` term and each `--account` adds an account, so `account:work account:personal` searches both.

### Which folders are searched

Every folder except Trash and Spam is searched unless `in:` says otherwise. `in:anywhere` searches those too.

On Google accounts the search runs in All Mail. There, `in:` values other than `trash`, `spam`, `junk` and `anywhere` filter within All Mail, as in Gmail.

On other accounts a virtual All folder, such as Dovecot's, is searched only when `in:` names it, since it holds copies of the other folders' messages.

### Operators only the cache answers

On accounts other than Google, `has:`, `filename:` and `label:` are answered only by the cache. They work only when the cache can answer the whole query:

- the account's cache has backfilled and is fresh,
- words in the query have a `newer_than:` or `after:` bound inside the body window, and
- the query covers no folder the account does not sync.

Otherwise they are refused with `INVALID_ARGUMENT` under `--source auto`, and always under `--source server`. The hint names the cause, and every hint points to `chainmail cache status`. Only a cache that is still backfilling, downloading the bodies of its window or catching up is worth waiting for. A value the cache never evaluates, or words without a `newer_than:` or `after:` bound inside the body window, are refused however long you wait. With `--source cache` the cache answers anyway and marks the account incomplete. When other accounts answered, the refusal appears in `partial` and the search still succeeds.

On those accounts `label:x` matches the IMAP keyword `x`, ignoring case. System flags such as `\Seen` are not labels, so a query with `label:\Seen` there is always refused, whatever `--source` says. Use `is:read` instead.

`item:` is answered only by the cache too, on every account, Google included. With `--source auto`, an account whose cache cannot answer completely is refused with `CACHE_NOT_READY`. With `--source cache` the cache answers anyway and marks the account incomplete. With `--source server` the search is refused with `INVALID_ARGUMENT`. [Finding mail by item](./items.md#finding-mail-by-item) has the details.

### Dates

On accounts other than Google, `newer_than:`, `older_than:`, `after:` and `before:` work on the arrival date at day granularity. So `newer_than:1d` there matches since the start of yesterday.

A search answered from the cache uses the same rules, with one difference on Google accounts. Dates there are also compared by arrival day, where Gmail's own search reads `newer_than:` its own way. Text matching differs between the cache and the server too. [How the cache matches text](../concepts/cache.md) explains it.

### Quoting in the shell

The shell removes quotes before chainmail sees them. Quote the whole query when it holds a phrase or a quoted value. Put `--` before the query when a term starts with `-`, so it is not read as a flag.

```sh
chainmail search 'subject:"q3 report" OR has:attachment' --account gmail
chainmail search -- invoice -from:acme
chainmail search is:unread in:inbox --count
chainmail search '(from:acme OR from:globex) newer_than:7d'
```

## Read messages

`get` reads up to 50 messages by ref.

```sh
chainmail get cm1:work:INBOX:7:42
chainmail get --full cm1:work:INBOX:7:42
chainmail get --raw cm1:work:INBOX:7:42 > message.eml
```

A search can feed `get` through a pipe:

```sh
chainmail search is:unread --limit 1 --output json | jq -r '.rows[0].ref' | xargs chainmail get
```

Each ref gets its own item, which holds either the message or an error, so one bad ref does not fail the rest. A response holds at most 16 MiB. The message that would push it past that, and every message after it, fail with `INVALID_ARGUMENT`. Ask for those in a smaller batch.

The command prints every item on stdout. It exits 0 if any item succeeded. When every item failed, it exits with the first item's reason. With JSON output it prints `{"items":[{"ref":...,"message":{...,"source":...}}],"source":...,"fetched_at":...}`. Each message's `source` says whether the cache or the server answered.

`--full` keeps quotes and signatures and skips the `body_limit` cut (see [bodies](#bodies)).

`get --raw` takes exactly one ref and writes the message's original RFC 5322 bytes to stdout. It cannot be combined with `--full`. It refuses to write to a terminal, since raw bytes skip the cleaning described in [untrusted content](../concepts/untrusted-content.md).

## Read a thread

`thread` reads every message in the thread of the one you name, oldest first by `Date`. It takes `--full`, as `get` does.

```sh
chainmail thread cm1:work:INBOX:7:42
```

On Google accounts the thread is Gmail's own, read from All Mail. Elsewhere it is every message outside Trash, Junk and any virtual All folder whose `Message-ID` is the thread's root `Message-ID`, or whose `References` or `In-Reply-To` contains it. The message you name is always included. A member deleted or moved since the thread was listed is left out without a trace.

A thread response holds at most 16 MiB. When the thread does not fit, the response holds the oldest messages that fit and lists the refs of the newer ones in `omitted_refs`, oldest first. A member that cannot be read, such as a message too malformed to parse, does not fail the thread. Its ref goes at the end of `omitted_refs`. `truncated` is set whenever `omitted_refs` is not empty. Pass those refs to `get`, which returns each message or its error.

With JSON output it prints `{"thread_id":...,"messages":[...],"source":...,"fetched_at":...,"truncated":...,"omitted_refs":[...]}`.

## Download an attachment

`attachment` downloads one attachment by its part number. `get` lists the part numbers: `attachments[].part` in JSON, or the number after `Attachment:` in text.

```sh
chainmail attachment cm1:work:INBOX:7:42 2 --out ~/Downloads/invoice.pdf
chainmail attachment cm1:work:INBOX:7:42 2 --out - > invoice.pdf
```

The bytes are decoded, not base64. `--out` is required. A file is written under a temporary name and renamed into place, so a failed download leaves nothing behind. `--out -` writes to stdout, and is refused when stdout is a terminal.

With JSON output it prints `{"path":...,"bytes":...,"content_type":...,"filename":{"untrusted":...}}`. On a terminal it prints how many bytes it saved.

Attachments and raw messages are never cached, so `attachment` and `get --raw` always read from the mail server.

## Bodies

`get` and `thread` return the text of each body. An HTML-only body is converted to text.

- They trim a trailing quoted reply or signature and set `quoted_trimmed` and `trimmed_bytes`. A forward is the exception, since its quoted text is the point.
- They cut a body longer than `[daemon] body_limit` (default `64KiB`) and set `truncated`.
- In text output, a line after the body says which of these happened.

`--full` returns the body untrimmed and without the `body_limit` cut. The daemon reads at most 4 MiB of one text part, so a longer part is still cut and has `truncated` set, even with `--full`. `get --raw` returns the whole message.

## Reads through the cache

When the cache holds a message, `get` can answer from it. A message's body never changes for a given UID, so a cached body is always valid. `get` renders it with `body_limit`, `--full` and quote trimming exactly as it renders one from the server. Flags and labels come from the cache when the folder is fresh, and from one quick fetch from the server otherwise.

A message the cache does not hold is read from the server. Its body is kept when the cache tracks its folder. Every read sets the body's last-read time, which body retention uses (see [the cache in operation](../operations/cache.md)).

`thread` comes from the cache when the cache holds the message and every folder a default search covers is fresh and backfilled.

On accounts other than Google, a read the cache cannot answer goes to the server. When the server answers `STALE_REF`, because the folder was renumbered or the message moved, the daemon looks the message up by its Message-ID in the cache. When that Message-ID names exactly one other message, the read is retried on its new ref. The message in the response then carries the new ref, while the item's own `ref` in `get` stays the one you passed. Zero or several matches give `STALE_REF` as before. This covers `get` and `thread` only. Writes with a stale ref still fail with `STALE_REF`. Google accounts recover by Gmail's message ID instead.

## Output

On a terminal, `folders` and `search` print tables, `get` and `thread` print each message as headers then the body, and `attachment` prints how many bytes it saved. Piped, every command prints one line of JSON, except `get --raw` and `attachment --out -`, which write the message or attachment bytes. `--output`, `CHAINMAIL_OUTPUT` or `output` in `cli.toml` chooses either format explicitly (see [CLI settings](./cli-settings.md)).

Text that comes from a message, such as subjects, names, addresses, bodies and file names, arrives in JSON wrapped as `{"untrusted": "..."}`. [Untrusted content](../concepts/untrusted-content.md) explains why, and which fields arrive as JSON strings.

Every flag of these commands is in the [command reference](../reference/commands/chainmail.md): [search](../reference/commands/chainmail-search.md), [get](../reference/commands/chainmail-get.md), [thread](../reference/commands/chainmail-thread.md), [attachment](../reference/commands/chainmail-attachment.md) and [folders](../reference/commands/chainmail-folders.md).
