---
title: Watching in operation
description: Read the sync status, fix folders that reconnect or fail, manage watch groups and the event log, and run long-lived watchers.
sidebar_position: 7
---

The daemon watches each account's folders, records what changes as events, and serves them to `chainmail watch` and the MCP tool `read_events`. [Watching for mail](../guide/watching.md) explains what is watched and what each event holds. This page is about keeping it running.

## Sync status

`chainmail watch status`, and the `sync` section of `chainmail admin status`, show each synced folder's mode, state, whether it has CONDSTORE, its last sync and sweep, and its last error. Both print the same table. Neither is audited. `chainmail watch status` lists every synced folder, so an account without `sync.folders` shows each folder of its default search.

```sh
chainmail watch status --output table
```

The mode is `idle` for a folder watched with IDLE and `poll` for a polled one, including a watched folder on a server without IDLE. The state is one of these, and each change of state is also published as a `status` event:

| State | Meaning |
|---|---|
| `baselined` | The folder's first sync recorded where it stands. |
| `watching` | The folder is watched with IDLE and its last sync worked. |
| `polling` | The folder is polled and its last sync worked. |
| `reconnecting` | The connection or the poll failed. The daemon tries again after 1 second, then twice as long each time up to 5 minutes, give or take a fifth. A polled folder waits at most `sync.poll`. The last error is `SERVER_UNAVAILABLE`, or `MALFORMED_RESPONSE` when the server sent something chainmail cannot read. A watch connection that ends because new mail arrived in such a form is dialled again at once instead, since the daemon read that mail on another connection, and the folder does not show this state. |
| `auth_required` | The server rejected the account's credentials, or it has no secret. Every loop of the account stops and every folder shows this state, with the last error `AUTH_REQUIRED`. One `status` event with an empty folder stands for the whole account, and its `detail` is the command to run, such as `run: chainmail auth login work`. Watching starts again within 15 seconds of `secret set` or `auth login` clearing the state. |
| `error` | The folder does not exist. The last error is `FOLDER_NOT_FOUND`, and the folder is tried again every `sync.poll`. The account's other folders keep running. |

The event schema also names a `catching_up` state. It is reserved and not published yet: a folder catches up inside one sync, which reports its events as it commits them.

When the daemon shuts down, each watcher ends its IDLE and logs out, within 5 seconds.

## A folder stuck in reconnecting

```sh
chainmail watch status --output table
sudo journalctl -u chainmail -o cat --since '1 hour ago' --no-pager | grep -E 'watch connection failed|polling a folder failed|connection limit'
chainmail accounts test '<account>' --output table
```

A folder in `reconnecting` retries after 1 second, then twice as long each time, up to 5 minutes. A polled folder waits at most `sync.poll`. `ERROR` says why:

- `SERVER_UNAVAILABLE`: the connection or the poll failed. Check `accounts test`, the network, and the provider. A burst of `watch connection failed` lines during a provider outage is normal and clears by itself.
- `MALFORMED_RESPONSE`: the server sent something chainmail cannot read. Waiting does not help if it keeps happening. `cache status` shows the messages concerned under `UNREADABLE`.
- A `server refused a watch connection for its connection limit` warning: the account has too many connections. See [Connection limits](connection-limits.md).

The daemon recovers by itself once the server answers. A restart only resets the backoff.

## A folder in error

A folder in `error` with `FOLDER_NOT_FOUND` does not exist on the server. The log says `a watched folder does not exist`. The daemon tries it again every `sync.poll`, and the account's other folders keep running.

Fix it one of three ways:

- Create the folder: `chainmail folder create '<folder>' --account '<account>'`.
- Remove it from `accounts.<name>.watch` or `accounts.<name>.sync.folders` in the config, then deploy or restart.
- Correct the name. On servers with a personal namespace, such as Hostinger, folders live under `INBOX.`. `chainmail folders --account '<account>'` shows the names.

## An account in auth_required

Every folder of the account shows `auth_required`. Follow [Auth](auth.md). Watching resumes within 15 seconds of the fix.

## Watch groups

```sh
chainmail watch groups --output table
chainmail watch groups delete '<group>'
```

`watch groups` lists each group's `NAME`, `ACKED` (the last event it acknowledged), `LAG` (events past it) and `LAST SEEN`. A consumer that runs keeps its lag near zero. A large, growing lag means the consumer has stopped or cannot keep up.

`watch groups delete` prints `deleted group <name>`, or `no group named <name>` when there was none, which also succeeds. The group's next use starts at the newest event, so its consumer skips everything before. Delete a group when its consumer is gone for good, or to make a consumer skip a backlog on purpose. A group unused for longer than `watch.retention` is removed by itself.

One group serves one consumer. Two consumers sharing a group each skip what the other acknowledged.

## Event log retention

The daemon trims the event log when it starts and every hour. It removes events older than `watch.retention` (default `168h`) and beyond `watch.max_events` (default `200000`), oldest first, whatever any group has acknowledged. To keep more, raise them in the config and restart. On NixOS:

```nix
services.chainmail.settings.watch = {
  retention = "336h";
  max_events = 500000;
};
```

`retention` is between `1h` and `2160h`, and `max_events` between 1000 and 10000000. The [configuration reference](../reference/configuration.md) lists the other `[watch]` keys.

## A consumer that fell behind

A consumer whose position the trim passed gets a `gap` event with reason `trimmed` and a `next_id`, then the events from `next_id` on. The events in between are gone.

To see where the log now starts, and so whether a group is past saving:

```sh
chainmail watch --since 0 --kind gap --once --output json
chainmail watch groups --output json
```

The first command prints a `trimmed` gap whose `next_id` is the oldest event kept, when anything has been trimmed. It reads the whole log, so it can take a moment. A group whose `acked_id` is below `next_id` minus 1 gets the gap on its next read. Do not read the consumer's own group to check, since a read with `--group` moves it.

What the consumer does about it:

- For mail, search for what it missed instead of waiting for events, such as `chainmail search 'in:inbox newer_than:7d'`, with the date of the last event it handled.
- For items, run `chainmail item list --changed-since '<revision>' --output json` with the last revision it saw.
- Then keep reading its group as before.

To stop it happening again, raise `watch.retention` and `watch.max_events`, or keep the consumer running. A `uidvalidity` gap is different: the server renumbered a folder, and refs from that folder's earlier events are stale. Search the folder again for fresh refs.

## Long-running watchers

`chainmail watch` runs until it is stopped. When the daemon goes away, as in a deploy, or the stream ends, it reconnects by itself, waiting 1 second, then twice as long each time up to 30 seconds. It resumes from its group, or from the last event or heartbeat it saw. The daemon sends a heartbeat as soon as the stream has sent every event already in the log, then every 30 seconds while nothing happens. A stream silent for 75 seconds is reconnected. `--heartbeat` prints heartbeats, as `{"heartbeat":{"at":...,"position":...}}` lines.

The watch stops with an error only when the daemon refuses the request, such as an invalid group name, an unknown account or a refused socket peer, or when it cannot write its output, as when the program reading it exits. Ctrl-C or `SIGTERM` ends it with exit 0 after its last acknowledgement.

`--once` exits at the first heartbeat, once every event already in the log is printed, which suits cron:

```sh
chainmail watch --group digest --once >> ~/mail-events.jsonl
```

A script that handles new inbox mail can read the stream through a pipe. Its stdout is not a terminal, so it gets JSON lines:

```sh
chainmail watch --group triage --kind new --in inbox | ~/bin/triage
```

As a systemd user service, so that it comes back if the script fails:

```ini
[Unit]
Description=Triage new mail

[Service]
ExecStart=/bin/sh -c 'chainmail watch --group triage --kind new --in inbox | %h/bin/triage'
Restart=always
RestartSec=10

[Install]
WantedBy=default.target
```

Save it as `~/.config/systemd/user/triage.service` and start it with `systemctl --user enable --now triage`. `chainmail` must be on the user manager's `PATH`, or `ExecStart` must give its full path. The service's user must be in the `chainmail-clients` group, as for the CLI. On macOS, a launchd agent in `~/Library/LaunchAgents` with `KeepAlive` set does the same.

Give each long-running consumer its own group, so a restart picks up where it stopped.
