---
title: Organizing
description: Move, copy, archive, trash and flag mail, and manage folders and Gmail labels, on every provider.
sidebar_position: 3
---

# Organizing

These commands change mail on the server. Each takes one or more refs, from any accounts, and acts on each message within its own account.

| Command | Does |
|---|---|
| `move <ref>... --to <folder>` | Moves messages to a folder. |
| `copy <ref>... --to <folder>` | Copies messages to a folder. |
| `archive <ref>...` | Moves messages out of the inbox into the account's archive. |
| `trash <ref>...` | Moves messages to Trash. A message already in Trash stays there. |
| `expunge <ref>...` | Deletes messages for good. This cannot be undone. Refused with `POLICY_DENIED` (exit 3) unless `policy.allow_expunge` is true. |
| `flag`, `unflag <ref>...` | Adds or removes `\Flagged`, the star. |
| `read`, `unread <ref>...` | Adds or removes `\Seen`. |
| `flags <ref>... --add <flag> --remove <flag>` | Adds and removes flags and keywords, such as `\Answered` or `$Label1`. Both repeat. |
| `label <ref>... --add <label> --remove <label>` | Adds and removes Gmail labels. Other accounts refuse it with `INVALID_ARGUMENT`. |

```sh
chainmail archive cm1:work:INBOX:7:42
chainmail move cm1:work:INBOX:7:42 --to Receipts
chainmail flags cm1:work:INBOX:7:42 --add '$Todo' --remove '\Seen'
chainmail label cm1:gmail:INBOX:3:17:g1790000000000000001 --add Projects/Q3
```

## Flags and keywords

`flags` accepts the system flags `\Seen`, `\Flagged`, `\Answered` and `\Draft`, and keywords of 1 to 64 letters, digits and `$ _ . -`.

It refuses `\Deleted`, since `expunge` is the only way to delete, and every other system flag. A refused flag fails the whole call with `INVALID_ARGUMENT` before any message changes. So does a call with neither `--add` nor `--remove`.

Quote flags in the shell, since `\` and `$` mean something to it.

A folder or label name, in any of these commands or in `folder`, is at most 255 bytes, with no control characters.

## Refs from a search

Refs can also come on stdin, one per line, when the only argument is `-`. That lets a search feed an organize command:

```sh
chainmail search from:newsletter@example.com older_than:30d --limit 500 --output json | jq -r '.rows[].ref' | chainmail archive -
```

## Results

Each ref succeeds or fails on its own, so one bad ref does not stop the rest.

On a terminal the command prints a count, such as `archived 12, failed 1`, then one line per failure. With JSON output it prints `{"outcomes":[...]}`, one outcome per ref in the order given. Each has `ref`, `new_ref` and `error`.

`new_ref` is the message's ref after a move, archive or trash, or the copy's ref after a copy. It is empty when the ref did not change, when the message is gone, or when the server did not say where the message landed.

After a move, archive or trash with a `new_ref`, use `new_ref` from then on. The old ref no longer names the message. After a copy the original keeps its ref.

When every ref succeeds the command exits 0. Otherwise it exits with the code of the first failure.

## Large batches

The daemon takes up to 500 refs per call. It works on the folders and accounts in a batch in parallel.

The CLI splits a longer list into calls of 500 and runs up to 4 of them at once. When some of those calls fail as a whole, each ref in a failed call gets that call's error. When all of them fail, the command fails with the first one's error.

## What each action does on each provider

On iCloud, Hostinger and custom servers, folders are IMAP folders. On Google accounts, refs live in All Mail and folders are labels.

| Action | iCloud, Hostinger and custom servers | Google accounts |
|---|---|---|
| `move` | Moves the messages, with IMAP `MOVE` where the server has it. On a server with UIDPLUS and no `MOVE`, it copies them and deletes the originals. A server with neither refuses `move`, `archive` and `trash` with `INVALID_ARGUMENT`. | To a label: adds that label and removes the Inbox label. The ref does not change. The label must exist, or the move fails with `NOT_FOUND`. `move --to INBOX` puts the Inbox label back. To Trash or Spam: moves the message there and gives it a new ref. Out of Trash or Spam: brings it back to All Mail first. To Gmail's other own folders, such as Starred: fails with `INVALID_ARGUMENT`. |
| `copy` | Copies the messages. | Adds the label, which must exist. A copy to Trash or Spam is a move there, as in Gmail. A copy out of Trash or Spam into All Mail is refused, so move it instead. To Gmail's other own folders: fails with `INVALID_ARGUMENT`. |
| `archive` | Moves them to `archive_folder` from the config, or else to the folder with the `\Archive` attribute. With neither, it fails with `NOT_FOUND`. | Removes the Inbox label, as Gmail's own Archive does. |
| `trash` | Moves them to Trash. | Moves the message to Trash. |
| `expunge` | Deletes them from their folder. A server without UIDPLUS refuses it with `INVALID_ARGUMENT`. | Moves the message to Trash, if it is not there already, and deletes it from Trash. |
| `flag` | Adds `\Flagged`. | Stars the message. |
| `label` | Refused with `INVALID_ARGUMENT`. | `--add` creates a label that does not exist yet. It also takes the system labels `\Inbox`, `\Important`, `\Starred`, `\Sent` and `\Draft`, and refuses All Mail, Trash and Spam. |

### Where a moved message landed

Some servers do not say where a moved or copied message landed. Then a `MOVE` or a copy succeeds with an empty `new_ref`, and you search again to find the message.

A move done by copying keeps every original whose copy the server did not confirm, so a message is never lost in a move. Those refs fail with `SERVER_UNAVAILABLE` and still name the original, and a copy may exist in the destination.

### Read state and retries

Moving a message never marks it read. Changes open the folder read-write, and reads still use `BODY.PEEK`, so reading mail keeps leaving it unread.

A change is never retried once the server has answered it. If the connection drops while a change is under way, the refs it covered fail with `SERVER_UNAVAILABLE` and may or may not have changed. Search again for fresh refs before you retry.

## Folders

`folder create`, `folder rename` and `folder delete` manage folders on one account, named with `--account`. On Google accounts they manage labels.

```sh
chainmail folder create Projects/Q3 --account work
chainmail folder rename Projects/Q3 Projects/Q3-2026 --account work
chainmail folder delete Projects/Q3-2026 --account work
```

`create` also creates any missing parent folders, and so does `rename` for the new name.

### Folder names

Join the levels of a name with `/` or with the server's delimiter, as `folders` shows the names. chainmail writes `/` as the server's delimiter.

Some servers, such as Hostinger's, keep every folder under a personal namespace prefix. There it is `INBOX.`, with `.` between levels. On those servers, `create` and the new name of `rename` go under the prefix unless the name is INBOX or starts with the prefix already. So `chainmail folder create Projects/Q3 --account site` makes `INBOX.Projects.Q3`. The commands print the name the server has.

Later commands take that name, or the name as `create` was given it. `move --to`, `copy --to`, `folder rename` and `folder delete` look for the folder as given, then as `create` would write it. So on `site`, `move --to Projects/Q3` and `folder delete Projects/Q3` both reach `INBOX.Projects.Q3`. A folder outside the personal namespace, such as a shared `Shared.Team` or a top-level `Keep` beside `INBOX.Keep`, is reached by the name `folders` shows.

Google matches folder names without regard to case.

### Errors

- A folder that already exists fails `create` or `rename` with `INVALID_ARGUMENT`.
- A missing folder fails `rename` or `delete`, and each ref of a `move` or `copy`, with `NOT_FOUND`. The message is `no folder "<name>"`, naming the folder as it was given, such as `no folder "Projects/Q3"`, never with the server's namespace prefix. The hint is `list the account's folders: chainmail folders (MCP: list_folders)`.
- A missing folder the command did not name, such as the Trash of `trash`, has the message `a folder this action needs does not exist on the account`.

### What rename and delete refuse

`rename` and `delete` refuse INBOX and every folder with a role, such as Sent, Trash or Google's `[Gmail]` folders, with `INVALID_ARGUMENT`.

`rename` also refuses a folder with such a folder under it, and a new name under the folder itself.

`delete` refuses a folder that has folders under it. It refuses a folder that holds mail unless you add `--force`. On other accounts `--force` deletes the mail with the folder. On Google it deletes only the label, and the messages stay in All Mail.

`folder delete` is refused with `POLICY_DENIED` (exit 3) unless `policy.allow_folder_delete` is true. With it on, `--force` deletes the mail with the folder even while `policy.allow_expunge` is off.

### Output

On a terminal the commands print what they did, such as `created folder Projects/Q3 on work`, or `created folder INBOX.Projects.Q3 on site` on Hostinger. With `--output json`, `create` and `rename` print `{"name":...}`, the folder's name on the server, and `delete` prints `{}`.

## Destructive actions

`expunge` and `folder delete` are off unless the config turns them on. `trash`, `move`, `copy`, `archive` and the flag and label commands are not gated. Mail in Trash can be got back until the server empties it.

[Destructive defaults](../concepts/policy.md) explains both settings and why to keep them off.

Every flag is in the command reference: [move](../reference/commands/chainmail-move.md), [copy](../reference/commands/chainmail-copy.md), [archive](../reference/commands/chainmail-archive.md), [trash](../reference/commands/chainmail-trash.md), [expunge](../reference/commands/chainmail-expunge.md), [flags](../reference/commands/chainmail-flags.md), [label](../reference/commands/chainmail-label.md) and [folder](../reference/commands/chainmail-folder.md).
