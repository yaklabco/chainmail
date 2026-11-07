---
title: The cache in operation
description: Read the cache's status, and handle a cache that is filling, out of budget, over its size cap, corrupt or on a full disk.
sidebar_position: 8
---

The daemon keeps `cache.db`, a local copy of every synced folder, and answers searches and reads from it when it can. [The local cache](../concepts/cache.md) explains what it holds and which answers come from it. This page is about keeping it healthy. Its settings are the `[cache]` and `accounts.<name>.cache` keys in the [configuration reference](../reference/configuration.md).

The cache never blocks a request. When it cannot answer, searches and reads go to the mail servers, with two exceptions that depend on it:

- `item:` searches, on every account. They are refused with `CACHE_NOT_READY` (exit 7). See [Item searches refused with CACHE_NOT_READY](items.md#item-searches-refused-with-cache_not_ready).
- `has:`, `filename:` and `label:` searches, on accounts other than Google, whose mail servers cannot search those. They are refused with `INVALID_ARGUMENT` (exit 2), and the message starts with the operator, such as `has: is answered from the local cache on this account, and the cache cannot answer yet`. Turning the cache off does not send them to the server. The hint says whether waiting helps.

## Cache status

`chainmail cache status` shows each account's cache, and the size of `cache.db` against `max_size`. The MCP tool `cache_status` reports the same, and `chainmail admin status` adds one line per account. It is not audited.

```sh
chainmail cache status --output table
chainmail cache status --account '<account>' --output table
```

```
size  12.0 MiB of 2.0 GiB

ACCOUNT  CACHE  STATE        BUDGET TODAY          ERROR
gm       on     backfilling  1.5 MiB of 1.0 GiB    -
host     on     incomplete   512.0 KiB of 1.0 GiB  -

ACCOUNT  FOLDER            STATE        HEADERS  BACKFILL         BODIES  BODY SIZE  WINDOW      FAILED  UNREADABLE  ATTACH UNKNOWN  LAST APPLIED
gm       [Gmail]/All Mail  backfilling  1200     at 4100 of 9000  300     1.2 MiB    300/310     0       0           0               2026-11-02 08:58
host     INBOX             incomplete   42       done             40      80.0 KiB   38/40 done  2       1           3               2026-11-02 09:00
```

An account's state is the most pressing of these, except `over_cap`, which its row explains:

| State | Meaning |
|---|---|
| `disabled` | `cache.enabled` is false for the account, or the account does not sync. |
| `error` | `cache.db` could not be opened, the disk holding it is full, or the cache's last step failed inside the cache. While the disk is full every search and read goes to the mail servers, and a write is tried again hourly. The cache comes back on once one succeeds. |
| `paused` | One of the account's folders is `reconnecting` or `auth_required`. The cache waits for the watcher. A folder in `error` is skipped alone. |
| `over_cap` | Headers alone fill `max_size`, so no body can be kept. `max_size` caps the whole file, so while this holds `over_cap` replaces `ready`, `incomplete` or `prefetching` on every account at once. An account in any other state keeps showing it. Searches stay correct. Raise `max_size`. |
| `budget_exhausted` | Today's `daily_download` is spent, less than one header's worth (2 KiB) is left, or the mail server refused a fetch for a quota or bandwidth limit. The header backfill resumes at 00:00 UTC. The prefetch keeps using any remainder a message fits in, except after a server refusal, which pauses it too. |
| `backfilling` | A folder's headers are still being fetched. |
| `prefetching` | Every folder is backfilled, and a folder's window still lacks bodies. |
| `incomplete` | Every folder is backfilled and its window complete, and a folder holds messages the cache could not read. Every state above wins over it, since they pass and this one lasts until those messages can be read. |
| `ready` | Every folder is backfilled and its window complete. |

A folder's state is `paused`, `backfilling`, `prefetching`, `incomplete` or `ready`, in that order of precedence. Each folder also has `unreadable` and `attachments_unknown` counts. `last_error` is a reason code such as `SERVER_UNAVAILABLE`, never server text.

The daemon logs, at info, the start and end of each folder's backfill, a complete body window, a spent budget, rebuilds and evictions, and at warn, `over_cap`, a full disk and repeated failures. The logs name accounts and folders, never message content.

## Not ready or backfilling

```sh
chainmail cache status --output table
sudo journalctl -u chainmail -o cat --since today --no-pager | grep -E 'cache backfill (started|done)|cache body window complete'
```

After a first start, a rebuild or a `recreating cache.db`, accounts go through `backfilling` (headers, newest first, `BACKFILL` shows `at <uid> of <uid>`) and `prefetching` (bodies inside the window, `WINDOW` shows `<cached>/<messages>`) to `ready`. The log has `cache backfill started` and `cache backfill done` per folder, then `cache body window complete`. Headers of a large account fit in one day of the default budget. Nothing needs doing but waiting.

`incomplete` means every folder is done but some messages could not be read. `UNREADABLE` counts them (see [Messages chainmail cannot read](#messages-chainmail-cannot-read)). `paused` means a folder is `reconnecting` or `auth_required`, and the cache waits for the watcher (see [Watching in operation](watching.md)).

## Budget exhausted

`budget_exhausted` has two causes, and the log line tells them apart:

| Log message | Cause |
|---|---|
| `cache download budget spent; background work resumes at 00:00 UTC` | The day's `daily_download` (default `1GiB` per account per UTC day) is spent. The line carries `used_bytes` and `budget_bytes`. |
| `the mail server throttled background downloads; cache background work resumes at 00:00 UTC` | The mail server refused a background fetch for a quota or bandwidth limit. Raising `daily_download` does not help. |

Either way, the header backfill and the body prefetch resume at 00:00 UTC. New mail and reads still work.

If the account has a lot of old mail and you want it cached sooner, raise the budget for that account and restart:

```nix
services.chainmail.settings.accounts.work.cache.daily_download = "2GiB";
```

Stay inside the provider's limits. Gmail allows about 2.5 GB of IMAP download a day. A pause after a server refusal is held in memory, so a restart tries once more.

### How the budget is spent

The backfill and the body prefetch keep to `daily_download` per account per UTC day, counting each body's size and 2 KiB per header. Each checks what is left before every batch of headers or body it fetches, and counts what it fetched once the download ends, so the day's spend stays within about one batch of `daily_download`. Reads and new mail spend from the same allowance.

The prefetch fetches a body only when the message's size, attachments included, fits in what is left of the day. A message larger than the whole `daily_download` is never prefetched. It is recorded as a failed body (`too_large`) and, like any failed body, tried at most 3 times, 8 hours apart, so a raised `daily_download` can still take it in. `get` still reads it from the server.

When less than one header's worth is left, the header backfill waits until 00:00 UTC and the account's state is `budget_exhausted`. The prefetch can still spend the remainder on a message whose size fits in it. The headers of new mail and a `get` of a message the cache lacks still run, and count.

A mail server can also refuse a fetch for a quota or bandwidth limit: a download of headers or bodies refused with IMAP's `OVERQUOTA` or `LIMIT` code, or any command refused with Gmail's "Account exceeded command or bandwidth limits". When that happens to a backfill or prefetch fetch, the account's backfill and prefetch pause until 00:00 UTC, whatever the budget says, and its state is `budget_exhausted` meanwhile. New mail is still applied, retrying a refused fetch with the usual backoff. The same codes on other commands, such as `OVERQUOTA` on an `APPEND` to a full mailbox or `LIMIT` on a search with too many results, are not a bandwidth limit: the command fails with chainmail's usual fixed error message and nothing pauses.

The default of 1 GiB stays well inside Gmail's IMAP download limit of 2.5 GB a day. The headers of a large Google account fit in one day of the default budget: 200,000 messages count as about 400 MB.

## A body that will not fetch

`FAILED` counts bodies given up after 3 tries, 8 hours apart, logged as `cache body fetch given up` with the folder, UID and reason. A message larger than the whole `daily_download` fails with `too_large`. Its headers stay searchable and `get` reads it from the server, so nothing is lost.

## Messages chainmail cannot read

A mail server can answer a fetch in a form chainmail cannot parse. chainmail then reads the message without its body structure, and caches it with its attachments unknown (`attachments_unknown` in `cache status`, so `has:` and `filename:` can miss it). If that fails too, the message is unreadable. The cache records it and moves on.

Each time the daemon starts, it reads up to 200 of an account's unreadable messages again, the longest-waiting first, and skips a folder whose answers cannot be parsed at all. A folder with unreadable messages, and its account, are `incomplete` in `cache status`, with the count in `unreadable`.

Searches still use the cache, whose answer then has `complete` false and `incomplete_reason` `unreadable_messages`: the server could list the message but could not read it either. A search the server answers leaves such a message out and lists its folder in `partial` with `MALFORMED_RESPONSE` and the count in `unreadable`. A `get` of it fails with `MALFORMED_RESPONSE`, exit 1, since trying again gets the same answer. The daemon logs each one at warn with its account, folder and UID.

## Body retention and over cap

The window keeps the bodies of mail that arrived within `prefetch_bodies`. A body outside it stays while it was read within `read_retention`. `max_size` caps the whole file. An eviction sweep runs hourly, and early whenever a write leaves the file over `max_size`: a body stored by the prefetch or a read, or a batch of headers added by the backfill or new mail. Each sweep:

1. removes bodies outside the window that were not read within `read_retention`.
2. if the file is still over `max_size`, removes bodies outside the window, least recently read first, then window bodies, oldest arrival first, until the file is at 90% of `max_size`.

Headers are never removed. A window body removed for space makes that folder's text searches go to the server until it is fetched again, and the prefetch waits until the file is under 80% of `max_size` before it fetches again. Changing the window or the retention takes a restart of the daemon, not a rebuild.

### Over cap

`over_cap` on every account means headers alone fill `cache.max_size` (default `2GiB`), so no body can be kept. Searches stay correct. The log says `cache.db is over max_size with message headers alone; raise [cache] max_size`. Raise it and restart:

```nix
services.chainmail.settings.cache.max_size = "4GiB";
```

`max_size` is global and between `64MiB` and `1TiB`. Check the disk has the room first.

## Rebuild and evict

```sh
chainmail cache rebuild --account '<account>' --folder '<folder>'
chainmail cache rebuild --account '<account>'
chainmail cache rebuild
chainmail cache evict
```

`cache rebuild` drops the cached rows and backfills them again: one folder, one account, or every account whose cache is on. It prints `rebuilt <account>: <folders>`, and `skipped <account>: cache disabled` for accounts it left alone. Searches use the servers for what is being rebuilt until its backfill is done. Use it when the cache disagrees with the server about a folder. It is safe to run again, and is audited as `cache.rebuild`.

`cache evict` runs a body eviction sweep now and prints `evicted <n> bodies, freed <size>`. It removes bodies outside the window that were not read within `read_retention`, then more until the file is at 90% of `max_size` if it is over. It returns the freed pages to the file system. It is audited as `cache.evict`.

## Corruption

The daemon handles a damaged cache itself. At start, a `cache.db` SQLite reports as corrupt, one of another schema version, or one with tables it does not know is deleted with its `-wal` and `-shm` and created again, with the warning `recreating cache.db` and a `reason`. A corruption found while running leaves a marker file, `cache.db.corrupt`, and the next start recreates the file. So the fix for a corrupt cache is a restart:

```sh
sudo systemctl restart chainmail
chainmail cache status --output table
```

If `cache.db` cannot be opened at all, the log says `cache.db could not be opened; searches and reads use the mail servers` and every account shows `error`. Check the directory's owner and free space. Then stop the daemon, delete the cache files as in [Restore](backup-restore.mdx#restore), and start it.

## A full disk

When a cache write meets a full disk, the daemon turns the cache off for every account and logs `the disk holding cache.db is full; searches and reads use the mail servers until a write succeeds, retried hourly`. Accounts show `error`. Once a write succeeds, it logs `cache.db accepts writes again; the cache is back on`.

`state.db` is on the same disk unless `cache.dir` moves the cache. When `state.db` cannot be written, the audit write fails (`audit write failed`), and so does every call, since chainmail never reports success for something it could not record. Free space at once:

```sh
df -h /var/lib/chainmail
sudo du -sh /var/lib/chainmail
chainmail cache evict
```

If that is not enough, lower `cache.max_size` or `cache.prefetch_bodies` and restart, or stop the daemon and delete `cache.db` with its `-wal` and `-shm`.

## How failures are contained

No cache failure blocks watching or a request. A lagging or failed cache only sends searches and reads to the mail servers. When reading the cache fails in the middle of a search with `--source auto`, that account is searched again on its server, and the answer's `source` says so. `--source cache` returns the error instead. A full disk met by any cache write, in the background or on a read, turns the cache off for every account until a write succeeds again. A corrupt `cache.db`, or one of another schema version, is deleted and built again when the daemon starts.
