---
title: Refs and cursors
description: How chainmail names messages with refs, pages through results with cursors, and what makes either go stale.
sidebar_position: 3
---

# Refs and cursors

## Refs

Every search row and every message carries a `ref`, which names that message to `get`, `thread`, `attachment` and the commands that change mail. A ref looks like `cm1:work:INBOX:7:42`, but it is opaque. Pass it back exactly as you got it, and do not build or edit one.

On a terminal the ref is the last column of a search table. In JSON it is the `ref` field of each row, so a pipeline can pick it out:

```sh
chainmail search is:unread --limit 1 --output json | jq -r '.rows[0].ref'
```

The commands that change mail take refs as arguments, or one per line on stdin when the only argument is `-`, so a search can feed them (see [Organizing mail](../guide/organizing.md)).

## Cursors

A page of results holds at most `--limit` rows, 50 by default and 500 at most. When there are more, the response has a `next_cursor`, and on a terminal the last line reads `more: --cursor <cursor>`. Pass the cursor to the same search, with the same query and accounts, to get the next page.

A page can hold fewer rows than the limit, or none, while `next_cursor` is still set, so keep paging until `next_cursor` is empty. `--count` takes no cursor, since a count covers every match at once. A cursor is opaque too.

```sh
chainmail search invoice --output json | jq -r .next_cursor
chainmail search invoice --cursor '<cursor>'
```

Here `<cursor>` is the value the first command printed. Cursors work across the cache and the mail server: a page the cache answered can continue on the server and the other way round, with no row missed or repeated (see [The local cache](./cache.md)).

## When a ref or cursor goes stale

A ref goes stale when its message is gone. A ref or cursor also goes stale when its folder is rebuilt on the server, which IMAP signals with a new `UIDVALIDITY`. The call then fails with `STALE_REF` (exit 6), and the fix is to search again for fresh refs.

Two things soften this:

- On Google accounts a ref also carries Gmail's message ID, so it survives a rebuilt All Mail.
- On other accounts, once the server has answered `STALE_REF`, `get` and `thread` look for the message by its Message-ID in the cache (see [Reading through the cache](./cache.md#reading-through-the-cache)).

A cursor naming a folder that was deleted or renamed since its first page fails with `INVALID_ARGUMENT`, and the fix is the same: search again.

## How fresh a search is

The daemon keeps folders open on its pooled IMAP connections. Before it uses an open folder again, it opens the folder afresh if the daemon itself has changed mail since. Otherwise it asks the server what changed there, unless it last asked, or opened the folder, less than a second ago. So a ref that a change through the daemon returned works at once.

New mail and changes made by another mail client show once the server reports them in answer to that question, which the daemon asks at most once a second on each connection. Most servers report a change in that answer. Gmail can report a message that another client moved into a folder some time later, so a search can miss such a message for longer than a second.

A `get`, `attachment` or `thread` that names a message the server leaves out of an open folder, or that Gmail cannot find there by its message ID, opens the folder afresh and asks once more before it fails with `STALE_REF`. So a ref to a message that has only just arrived reads at once too. Until the server reports such a message, a search can still miss it, and a change through its ref can still fail with `STALE_REF`. Search again after a moment.

## Partial results

An account or folder that a search or a folder list could not read is listed in `partial`, with its reason and hint. The rest of the response is still valid, and the command still succeeds. It fails only when every source failed. [Exit codes](../reference/exit-codes.md) lists the reasons.
