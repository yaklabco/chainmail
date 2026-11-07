---
title: Connection limits
description: How the daemon measures each account's IMAP connection limit and sizes its pool, and what to do when a server keeps refusing connections.
sidebar_position: 10
---

The daemon keeps a pool of IMAP connections per account and works on several folders at once, up to the pool's size. It also holds one connection per watched folder, or one for All Mail on Google. Mail servers limit how many connections one account may have, and a pool over the limit gets connections refused. So chainmail measures each account's limit, its cap, and sizes the pool from it.

## Probe an account

```sh
chainmail accounts probe '<account>' --output table
chainmail accounts probe --output table
```

`accounts probe` measures the accounts named, or every account when none is named. Good output:

```
ACCOUNT  CAP  POOL  PROBED            ERROR
work     10   8     2026-11-06 09:00
site     5    3     2026-11-06 09:00
```

Each result has `account`, `imap_cap`, `pool_size`, `probed_at` and `error`. A probe whose very first connection fails reports an error rather than a cap. When any probe fails, the command prints every result, then exits with the first failure's code. An unknown or invalid account name fails the whole command before any probe runs. The probe is audited as `limits.probe`.

## How a probe measures the cap

For each account the probe opens IMAP connections one at a time, 250 ms apart, logging each one in, until the server refuses one or 16 are open. Then it logs them all out. The number that worked, plus the connections the daemon itself holds open to the account, is the account's cap.

The daemon counts the most of its own connections it held at once from the start of the probe to its end, connections that opened and closed again meanwhile included. So a connection a call opens while the probe runs is not mistaken for part of the limit, even if it has closed by the time the server refuses the probe. The connections the probe counts as the daemon's own include the watcher connections. Up to 4 accounts are probed at once, and each probe times out after 2 minutes.

## How the pool is sized

The pool size is:

1. `pool` from the config, when it is set.
2. Otherwise the cap less 2, between 1 and 12. The 2 left over are for your other mail clients on the same account.
3. Before any probe, the preset's default: 6 on Google accounts and 3 on the others.

Then the pool shrinks to leave room for the account's watcher connections, which the daemon dials outside the pool (see [Watching for mail](../guide/watching.md)). Pool and watchers together stay within the probed cap, or before any probe the preset's cap of 10 on Google accounts and 3 on the others. A `pool` set in the config shrinks too. The pool never drops below 1, even when the watchers alone fill the cap, and the watchers never shrink.

## When probes run

Besides `accounts probe`, the daemon probes:

- after `auth login` for an account and after `secret set accounts/<account>`, which print the result, such as `connections: cap 10, pool 8`. A failed probe there is a warning and does not fail the command.
- in the background, on an account's first use, when the account has no stored result or one older than 30 days.

While a probe runs, the account's other calls go on as usual. Once it ends, the pool is rebuilt at the new size: calls already running finish first, and calls that start meanwhile wait only for the switch.

## Refusals in normal use

When a server refuses a connection for its limit in normal use (an IMAP `LIMIT` code, or an `ALERT` saying too many connections, as Gmail sends), the daemon lowers that account's pool by one, never below 1. It logs `server refused a connection; recorded the connection limit` with the new `imap_cap`. It records a cap of the new pool size plus 2, or plus the account's watcher connections when it has any, unless the stored cap is already lower. The refusal counts as a fresh measurement, so it also postpones the next background probe.

A refusal while a probe of the account runs still lowers the pool, but records no cap, since the probe's own connections may have filled the server. The probe's result is stored and the pool is rebuilt from it.

A refused watcher logs `server refused a watch connection for its connection limit` with `pool`, `open` and `watchers`. Gmail's `ALERT` is never taken as a credential failure.

## If refusals keep coming

1. Count the account's other clients: phones, desktop mail apps and other servers all use the same cap.
2. Run `chainmail accounts probe '<account>'` while those clients are connected, so the cap reflects them.
3. Watch fewer folders. Each folder in `accounts.<name>.watch` holds a connection of its own. Polled folders use the pool instead.
4. Set `accounts.<name>.pool` to a fixed size, from 1 to 12, and restart.

Setting `pool` fixes the pool size whatever a probe finds, though the probes still run and record the cap. A refusal in normal use still lowers the pool below the configured `pool` until a probe rebuilds the pool or the daemon restarts.

A probe ramps up slowly and stops at the first refusal, but it does open many connections at once, which some providers watch for abuse. Run it when something changed, not on a timer.
