---
title: Items in operation
description: Check the standing Admin items, undo a delete, fix a thread link that shows unavailable, and handle item searches the cache cannot answer yet.
sidebar_position: 11
---

Items live in `state.db`, beside the audit log and the event log, so they are backed up with it (see [Backup and restore](backup-restore.mdx)). [Work items](../guide/items.md) explains the types, fields and links. This page is about keeping them in order.

## The Admin items

At every start, before it accepts a connection, the daemon creates `it_admin_work` and `it_admin_personal` if they are missing: `initiative` items titled Admin, one per area. They are ongoing, so they cannot be set to `done`, deleted, moved or retyped.

```sh
chainmail item list --ongoing --output table
```

Good output lists both, marked `(ongoing)`. If either is missing, restart the daemon. If creating them fails, the daemon does not start.

## Soft delete and restore

`item delete` is a soft delete. Nothing is ever removed for good, so a bad delete can be undone.

```sh
chainmail audit --since 168h --action item.delete --output table
chainmail item list --deleted --output table
chainmail item show '<item-id>'
chainmail item restore '<item-id>'
chainmail item restore '<item-id>' --cascade
```

The audit row names the item as `item=<id>`, with `cascade=1` and `ids=<n>` for a subtree. `--cascade` restores the descendants deleted with it. An item whose parent is still deleted cannot be restored, so restore the parent first. A deleted item cannot be changed, linked or watched, and an `item:` search naming it fails with `NOT_FOUND` and the hint `run: chainmail item list`.

## A link showing unavailable

```sh
chainmail item show '<item-id>' --output json | jq '.threads[] | select(.state != "ok") | {account, thread_key, state, reason, source}'
```

Each linked thread has a `state`. `empty` means no message of the thread was found, and the link stays. `unavailable` means the thread could not be read, and `reason` says why:

- `NOT_FOUND`: the account is no longer in the config. Put the account back, or remove the link with `chainmail item unlink '<item-id>' '<account>' '<thread-key>'`, using the `account` and `thread_key` shown. `unlink` accepts an account that is gone.
- `MALFORMED_RESPONSE`: the mail server sends one of the thread's messages in a form chainmail cannot read. `cache status` shows the count under `UNREADABLE`. The daemon retries those messages at each start (see [Messages chainmail cannot read](cache.md#messages-chainmail-cannot-read)).
- Any other reason, such as `SERVER_UNAVAILABLE`, is the account's server. Check `chainmail accounts test '<account>'`.

## Item searches refused with CACHE_NOT_READY

`item:` searches are answered only by the cache. With the default `--source auto`, an account whose cache cannot answer completely is refused with `CACHE_NOT_READY` (exit 7). When other accounts answered, the refusal is in `partial` and the search still succeeds. The message says which case it is:

| Message | Hint | Do |
|---|---|---|
| `item: is answered from the local cache, which cannot answer this account completely yet` | `the cache has not caught up; run: chainmail cache status` | Wait for the account to be `ready` (see [Not ready or backfilling](cache.md#not-ready-or-backfilling)). |
| `item: is answered from the local cache, which holds message text only for mail inside its body window` | `add a top-level newer_than: or after: inside the account's prefetch_bodies window, or drop the text terms; ...` | Change the query. Waiting does not help. |
| `item: is answered from the local cache, which failed reading this account` | `the daemon log has the cause; see the cache with: chainmail cache status` | Read the journal for cache warnings. |

```sh
chainmail cache status --account '<account>' --output table
chainmail search item:'<item-id>' --source cache
```

`--source cache` answers anyway and marks the account incomplete with its reason, which is useful while the cache fills. `--source server` is refused with `INVALID_ARGUMENT`. Some accounts never become ready for `item:`: an account with the cache off or `sync.enabled = false`, an account whose `sync.folders` leaves out part of its default search folders, and on Google a query with `in:trash`, `in:spam` or `in:anywhere`.
