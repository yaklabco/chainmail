---
title: Work items
description: Keep a tree of work items, link mail threads to them, find their mail and hear about replies.
sidebar_position: 4
---

# Work items

chainmail keeps a tree of work items in `state.db`, beside the audit log and the event log. The types run from an objective down to a task, with the hierarchy of the Lathe roadmap plus a `task` leaf. An item can have acceptance criteria, blockers and links to mail threads.

Agents create and change items with `chainmail item` or the MCP item tools. They link each mail thread to the item it belongs to, find that mail again with `item:`, and hear about replies through `item_mail` events.

Items never live in `cache.db`, so deleting the cache loses none of them. [Items in operation](../operations/items.md) covers backups and repairs.

## Fields

| Field | Values |
|---|---|
| ID | `it_` followed by 26 lowercase letters and digits for a new item. chainmail accepts any ID of `it_` followed by 1 to 60 lowercase letters, digits and underscores, which the two Admin IDs fit. An ID never changes. |
| Title | 1 to 200 characters after trimming. |
| Description | Markdown of at most 64 KiB. |
| State | `backlog`, `in-progress`, `in-review` or `done`. A new item starts in `backlog`. |
| Area | `work` or `personal`. |
| Priority | `p0` to `p3`. |
| Start and due dates | `YYYY-MM-DD`. An open item with a due date raises [due events](#due-events). |
| Estimate | A number of zero or more, in a unit of your choosing. |

An item without an area or a priority of its own takes its nearest ancestor's. A root item must have an area of its own, and a root without a priority counts as `p2`. `item show` gives both the item's own values and the effective ones, and `item list` gives the effective ones.

## Types and the parent rule

The types that can have children are, ranked from the top: `objective`, `key-result`, `initiative`, `epic`, `feature` and `story`. A parent must rank above its child.

`bug`, `support-ticket` and `task` are leaves and have no children.

- A `bug` or a `support-ticket` sits under a `feature` or any type above it, and always has a parent.
- A `task` sits under any type that is not a leaf, or under nothing.
- Every other type can be a root.

Changing an item's type checks the rule against its parent and against every live child. A move that would put an item under itself or its own descendant is refused, and so is a move under a deleted item.

## Admin and the inbox

When the daemon starts, before it accepts a connection, it creates two items if they are missing: `it_admin_work` and `it_admin_personal`. Each is an `initiative` titled Admin, with the area its ID names. The daemon writes no event and no audit row for them.

Both are ongoing. An ongoing item cannot be set to `done`, deleted, moved or retyped. Ongoing items come first among their siblings, and `item list --ongoing` lists them. File known chores under Admin.

The inbox holds every live task with no parent, which is work nobody has sorted yet. `chainmail item list --inbox` lists it. `--inbox` cannot be combined with `--under`. A task in the inbox needs an area of its own, like any root.

## Criteria and blockers

An item can have acceptance criteria of 1 to 500 characters each. Each criterion has an ID, `cr_` followed by 26 lowercase letters and digits.

- Setting an item to `done` is refused while any of its criteria is unticked.
- Ticking a criterion needs evidence of 1 to 1000 characters, such as a ref or a link.
- Unticking drops the evidence, and an untick that passes evidence is refused.
- An edited criterion keeps its tick.

`item criteria` adds, edits, removes, reorders, ticks and unticks criteria:

```sh
chainmail item criteria add it_01jcb7m2k9r8q3v4w5x6y7z8a9 "Receipts attached" "Submitted"
chainmail item criteria tick it_01jcb7m2k9r8q3v4w5x6y7z8a9 cr_01jcb7m2k9r8q3v4w5x6y7z8c1 --evidence "receipt in cm1:work:INBOX:7:42"
chainmail item criteria untick it_01jcb7m2k9r8q3v4w5x6y7z8a9 cr_01jcb7m2k9r8q3v4w5x6y7z8c1
```

`item criteria order` names every criterion once, in the new order. Over MCP, `edit_criteria` applies its removals, then its edits, then the new order, then its additions, in one call.

`item block <id> <blocked-by-id>` records that one item waits on another, and `--remove` takes it back.

```sh
chainmail item block it_01jcb7m2k9r8q3v4w5x6y7z8a9 it_01jcb7m2k9r8q3v4w5x6y7z8b0
chainmail item block it_01jcb7m2k9r8q3v4w5x6y7z8a9 it_01jcb7m2k9r8q3v4w5x6y7z8b0 --remove
```

Adding a blocker needs both items live, and removing one works on deleted items too. An item cannot block itself, and a blocker that would close a cycle is refused. `item show` lists the live items an item waits on and the live items waiting on it.

## Deleting and restoring

`item delete` is a soft delete. A deleted item:

- is left out of `item list` unless you pass `--deleted`,
- has its links ignored by `item:none`,
- makes an `item:` search or a `--item` watch that names it fail with `NOT_FOUND`,
- gets no more `item_mail` events,
- keeps its criteria, blockers and links, and
- cannot be changed, and neither can its criteria or links, until it is restored.

An item with live children needs `--cascade`, which deletes the whole live subtree at one revision. The Admin items cannot be deleted.

`item restore` brings an item back, and `--cascade` also restores the descendants deleted with it. An item whose parent is still deleted cannot be restored, and neither can one whose parent's type no longer holds it.

Nothing is ever removed for good. Soft delete and the audit log are how a bad change is undone.

## Revisions and retries

Every change to an item, its criteria, its blockers or its links raises one global revision number, in the same transaction. It stamps every item it touched with that number. A call that changes nothing raises nothing.

`item list --changed-since <revision>` lists the items changed after a revision, deleted ones included, in revision order.

`item create --idempotency-key <key>` makes a retry safe. A key is 1 to 128 characters of `A-Z`, `a-z`, `0-9`, `.`, `_`, `:` and `-`. A second call with the same key and the same request returns the first item with `duplicate` set. The same key with a different request fails with `INVALID_ARGUMENT`. The daemon keeps each key for 7 days.

## Links and anchors

`item link <id> <ref>` links the thread of the message at the ref to the item.

```sh
chainmail item link it_01jcb7m2k9r8q3v4w5x6y7z8a9 cm1:work:INBOX:7:42 --anchor --note "the renewal notice"
```

The link stores the account and the thread's key:

- On Google accounts the key is Gmail's thread ID in lowercase hex.
- Elsewhere it is 16 hex digits, a hash of the thread's root Message-ID. The root is the first `References` entry, or else the first `In-Reply-To` entry, or else the message's own Message-ID.

Moving or archiving a linked message does not change the key, so the link survives it.

`--anchor` also stores the message's own Message-ID as the link's anchor. `--note` adds a note of up to 500 characters. Linking a thread the item already has changes only the anchor and the note, and a link without `--note` keeps the note it has.

A thread can link to several items, and an item to several threads. A message with no thread key cannot be linked, and `--anchor` on a message without a Message-ID is refused. A deleted item cannot be linked.

A rule's `link_item` action links threads the same way, as mail arrives (see [Rules](./rules.md#actions)).

`item unlink <id> <account> <thread-key>` removes a link. It takes an account that is no longer in the config too. `item show` lists each link's account and thread key.

```sh
chainmail item unlink it_01jcb7m2k9r8q3v4w5x6y7z8a9 work 18c2f0a1b2c3d4e5
```

### Linked threads in item show

`item show` resolves the 50 most recently made links and gives the total. It lists the threads with mail first, newest latest message first, then the empty ones, then the unavailable ones. Each thread has a state:

| State | Meaning |
|---|---|
| `ok` | The thread's messages were found. `latest` is the newest message as a search row, `messages` is the count, and `anchor_ref` is the anchor's current ref when the anchor was found. |
| `empty` | No message of the thread was found. The link stays. |
| `unavailable` | The thread could not be read. `reason` holds the error's reason, such as `NOT_FOUND` for an account that is no longer in the config, or `MALFORMED_RESPONSE` when the mail server sends one of its messages in a form chainmail cannot read. |

`source` says what found the messages. The cache finds them when every folder of the account's default search is backfilled and fresh in the cache, and none of those folders holds a message the cache recorded as unreadable. Otherwise the mail server finds them, and a linked thread that holds an unreadable message is `unavailable` with `MALFORMED_RESPONSE`. On Google the server then searches All Mail by thread ID. Elsewhere it searches the default folders for messages whose Message-ID, `References` or `In-Reply-To` names the root.

Off Google, a reply whose `References` do not start with the thread's root, as from a client that drops them, gets another key. `item:` does not find it, and it raises no `item_mail` event. The server lookup in `item show` can still count it, since that lookup also matches `In-Reply-To`.

## Finding mail by item

`item:<id>` matches the messages in threads linked to that item or to any live item below it. `item:none` matches the messages in threads that no live item links. Both combine with the other operators of [the query language](./reading-and-searching.md#the-query-language).

```sh
chainmail search 'item:none newer_than:7d'
chainmail search item:it_admin_work is:unread
```

The cache answers them, and the mail server never does. Google accounts follow the same rules.

- With `--source auto`, an account whose cache cannot answer completely is refused with `CACHE_NOT_READY` (exit 7) and the hint `the cache has not caught up; run: chainmail cache status`. That covers an account with no cache, and a cache still backfilling or stale.
- Two causes that waiting does not clear are still `CACHE_NOT_READY` but carry their own message and hint. Words without a `newer_than:` or `after:` bound inside the body window get the hint to add such a bound or drop the words. A cache that failed reading the account gets the hint that the daemon log has the cause.
- When other accounts answered, the refusal appears in `partial` and the search still succeeds.
- A query with a value the cache never evaluates, such as `label:\Seen` off Google, is refused with `INVALID_ARGUMENT` instead.
- `--source cache` answers anyway and marks the account incomplete with the usual reason.
- `--source server` is refused with `INVALID_ARGUMENT`.
- An unknown or deleted item ID fails with `NOT_FOUND` and the hint `run: chainmail item list`.
- An account whose `sync.folders` leaves out part of its default search folders always gets `CACHE_NOT_READY` for `item:`. On Google, so does `item:` with `in:trash`, `in:spam` or `in:anywhere`.

## Item events

Two event kinds report item activity. A watcher gets them only when it asks for them, so existing watchers and groups see nothing new.

| Kind | Fields | Meaning |
|---|---|---|
| `item` | `item` | An item changed. `item` holds `item_id`, `type`, `revision`, `op` and `fields`. The event's `account` and `folder` are empty. |
| `item_mail` | `ref`, `message`, `item_id`, `from_self` | New mail arrived on a thread linked to the item. `message` is the message's search row, as in a `new` event. `from_self` is true when a From address is the account's own address, ignoring case. |

`op` is `created`, `updated`, `moved`, `deleted`, `restored`, `linked`, `unlinked`, `criteria` or `blockers`. `fields` names the changed fields of an `updated` or `moved` event by their API names, such as `due_date` or `parent_id`, never their values. An `item` event never holds item text. A cascade delete or restore writes one event per item, all with the same revision. A blocker change writes one event for each of the two items.

### When item_mail fires

The daemon writes an `item_mail` event in the same transaction as the message's `new` event, one per linked item and message.

- It keys the events by Message-ID, or by ref for a message without one. So a message filed in two folders, such as the inbox and an archive, raises one event.
- A message that arrived on the server more than 5 minutes before the link was made, by its IMAP arrival time, raises none. So a linked message that arrived more than 5 minutes before the link raises none when it is moved into another synced folder, since a move keeps its arrival time.
- A message that arrived within those 5 minutes, and that the daemon first sees after the link, can raise one. The 5 minutes allow for IMAP's whole-second arrival times and a server clock behind the daemon's, so a reply in the same second as the link still counts.
- Only an item linked directly gets the event. Its parents do not.
- A deleted item gets none.

The daemon trims its record of which messages raised an event hourly, together with the event log, once a row is older than `watch.retention`. After that, a copy of the message that arrives in another folder raises the event again.

### Due events

Three more opt-in kinds report due dates: `item_due_soon`, `item_due` and `item_overdue`. Each fires at `[rules] due_time` in `[rules] timezone`, 08:00 in the daemon's local zone by default.

| Kind | Fires on |
|---|---|
| `item_due_soon` | `due_soon_days` days before the due date, 2 by default. `due_soon_days = 0` turns it off. |
| `item_due` | The due date. |
| `item_overdue` | The day after the due date. |

The event's `due` field holds `item_id`, `type`, `due_date`, `state` and `parent_id`. The event has no account or folder. It never holds item text, so read the title with `item show` or `get_item`.

- Only open items count: items with a due date that are not `done` and not deleted.
- Each stage fires once for each item and due date. A new due date gives new stages, which fire again.
- When an item first reaches a stage, only the most advanced stage it has reached fires, and the earlier ones never will. An item created already overdue gets one `item_overdue`. An item made due tomorrow, with `due_soon_days = 2`, gets `item_due_soon` at once.
- No stage repeats, so an overdue item gets one `item_overdue` for each due date.
- The daemon checks every open item at `due_time` each day and when it starts. A machine that was asleep at `due_time` gets the check within about 5 minutes of waking. It also checks an item in the same transaction as its creation, its restore, or an update that changes its due date or state.

The `[rules]` keys are in the [configuration reference](../reference/configuration.md#rules).

### Watching for item events

`chainmail watch --kind item,item_mail` asks for both kinds, and `--kind item_due,item_overdue` for two of the due kinds.

```sh
chainmail watch --group lathe --kind item
chainmail watch --kind item,item_mail --item it_admin_work
chainmail watch --group follow-ups --kind item_due,item_overdue
```

- `--item <id>` limits the item kinds, the due kinds included, to that item and every live item below it, resolved when the watch starts.
- With `--item` and no `--kind`, a watch gets `item`, `item_mail`, due, `status` and `gap` events, and never `new`, `flags`, `expunged` or `rule` events.
- `--account` applies to `item_mail` events and not to `item` or due events, which have no account.
- `item` and due events pass any `--in`, and `item_mail` events match `--in` as `new` events do.
- An unknown or deleted item fails with `NOT_FOUND`.

The MCP tool `read_events` takes the same kinds and an `item` input.

The `item` events are the change feed for a later sync with Lathe. A consumer reads them through a group. A consumer that falls further behind than `watch.retention` gets a `trimmed` gap, and catches up with `item list --changed-since <revision>`. [Watching for mail](./watching.md) covers groups and gaps.

## Working with items

The `item` commands print a table or a line of text on a terminal and JSON otherwise, as the other commands do. This session files an invoice thread against a new task, moves the task under Admin and closes it. Each command prints the ID or ref the next one takes.

Find the invoice and note its `ref`:

```sh
chainmail search 'from:acme subject:invoice newer_than:7d' --limit 1
```

Create a task in the inbox, with one criterion, and note its ID:

```sh
chainmail item create task 'Pay the Acme invoice' --area work --due 2026-11-20 --criterion 'Paid in full'
```

Link the thread, sort the task under Admin, tick its criterion and close it:

```sh
chainmail item link it_01jcb7m2k9r8q3v4w5x6y7z8a9 cm1:work:INBOX:7:42 --anchor --note 'the invoice thread'
chainmail item list --inbox --output table
chainmail item move it_01jcb7m2k9r8q3v4w5x6y7z8a9 it_admin_work
chainmail item show it_01jcb7m2k9r8q3v4w5x6y7z8a9
chainmail item criteria tick it_01jcb7m2k9r8q3v4w5x6y7z8a9 cr_01jcb7m2k9r8q3v4w5x6y7z8c1 --evidence 'paid on 2026-11-18'
chainmail item update it_01jcb7m2k9r8q3v4w5x6y7z8a9 --state done
chainmail search item:it_01jcb7m2k9r8q3v4w5x6y7z8a9
```

### The item commands

| Command | Does | Prints with JSON output |
|---|---|---|
| `item create <type> <title>` | Creates an item. Flags: `--description` or `--description-file`, `--parent`, `--area`, `--priority`, `--state`, `--start`, `--due`, `--estimate`, `--criterion` (repeatable) and `--idempotency-key`. An item with no `--parent`, a task in the inbox included, needs `--area`. | `{"item":{...},"criteria":[...],"duplicate":...}` |
| `item show <id>` | Shows an item with its own and effective area and priority, its criteria, its ancestors, up to 50 live children, its live blockers both ways, and up to 50 linked threads, each with its latest message. Deleted items show too. | `{"item":{...},"criteria":[...],"ancestors":[...],"children":[...],"children_total":...,"blocked_by":[...],"blocks":[...],"threads":[...],"threads_total":...}` |
| `item list` | Lists items in tree order. Flags: `--type`, `--state`, `--area` (the effective area), `--under`, `--inbox`, `--ongoing` or `--no-ongoing`, `--deleted`, `--changed-since`, `--limit` (1 to 500, default 100) and `--page-token`. | `{"items":[...],"next_page_token":...,"revision":...}` |
| `item update <id>` | Changes the fields its flags name: `--type`, `--title`, `--description` or `--description-file`, `--state`, `--area`, `--priority`, `--start`, `--due` and `--estimate`. `--clear` takes a comma-separated list of `area`, `priority`, `start`, `due`, `estimate` and `description`. | `{"item":{...}}` |
| `item move <id> <parent-id>`, `item move <id> --root` | Moves an item under another item, or to the top with `--root`. A task moved to the top goes to the inbox. | as `item update` |
| `item delete <id>`, `item restore <id>` | Soft-deletes or restores an item, or with `--cascade` its subtree. | `{"ids":[...],"revision":...}` |
| `item criteria add`, `edit`, `remove`, `order` | Adds criteria (`add <id> <text>...`), edits one (`edit <id> <criterion-id> <text>`), removes some (`remove <id> <criterion-id>...`) or sets their order (`order <id> <criterion-id>...`). | `{"criteria":[...],"revision":...}` |
| `item criteria tick`, `untick` | Ticks a criterion with `--evidence`, or unticks it and drops its evidence. | `{"criterion":{...},"revision":...}` |
| `item block <id> <blocked-by-id>` | Records that the item waits on another, or with `--remove` takes that back. | `{"changed":...,"revision":...}` |
| `item link <id> <ref>`, `item unlink <id> <account> <thread-key>` | Links the thread of the message at the ref to the item, or removes a link. | `{"link":{...},"created":...,"revision":...}` for `link`, `{"revision":...}` for `unlink` |

Every `revision` in this output is a JSON string, as for other 64-bit fields. Titles, descriptions, criterion text, evidence and link notes arrive wrapped as `{"untrusted": "..."}`, as mail does, since agents often copy them from messages. The `item` tables clean them of terminal control characters. See [untrusted content](../concepts/untrusted-content.md).

Every flag is in the [item command reference](../reference/commands/chainmail-item.md).

## Over MCP

Over MCP the same calls are the item tools listed in the [MCP tool reference](../reference/mcp-tools.md). Their inputs mirror the commands' arguments.

`update_item` takes the fields to change and a `clear` list in place of the flags, and a `parent_id` in place of `item move`. Its `clear` names the API fields: `description`, `area`, `priority`, `start_date`, `due_date`, `estimate` and `parent_id`. In the MCP results every `revision` is a JSON number.
