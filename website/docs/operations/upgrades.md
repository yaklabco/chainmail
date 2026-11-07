---
title: Upgrades
description: Upgrade a production daemon, check that the upgrade worked, and roll back when it did not.
sidebar_position: 5
---

## Before you upgrade

1. Note what runs now: `chainmail admin status --output json | jq -r .version`.
2. Take a snapshot of `state.db` (see [Take an online snapshot](backup-restore.mdx#take-an-online-snapshot)). A release that adds a migration changes `state.db` in a way the older binary refuses, so this snapshot is your rollback.
3. See whether the release adds migrations. They live in `internal/db/migrations/state`, one file per schema version, numbered from `0001`:

```sh
git -C '<chainmail-checkout>' ls-tree --name-only '<rev>' internal/db/migrations/state/
```

`'<rev>'` is the commit or tag you are about to deploy, such as `v0.2.0`. The number of files is the `user_version` that `state.db` will have after the start. The [changelog](../reference/changelog.md) also says what each release changed.

## Upgrade on NixOS

In your host's flake, update the input and deploy:

```sh
nix flake update chainmail
sudo nixos-rebuild switch --flake .#'<host>'
```

With Nix older than 2.19, the first command is `nix flake lock --update-input chainmail`. A host deployed by a GitOps tool gets the change by committing and pushing `flake.lock` instead of running `nixos-rebuild`.

The switch restarts `chainmail.service`, because its `ExecStart` now points at the new package. The daemon applies any new migrations at start, before it listens, each in its own transaction.

## Check the upgrade

```sh
systemctl status chainmail --no-pager
chainmail admin status --output json | jq -r .version
jq -r .nodes.chainmail.locked.rev flake.lock
sudo -u chainmail $(nix build --no-link --print-out-paths nixpkgs#sqlite.bin)/bin/sqlite3 -readonly /var/lib/chainmail/state.db 'PRAGMA user_version'
```

Good output:

- The status shows the new store path in the command line, and `active (running)`.
- `version` is the first 7 characters of the locked `rev`. A dirty build reports `dirty`. A release binary, as on other hosts, reports the release version, such as `0.2.0`.
- `user_version` equals the number of migration files at that commit, such as `5` for a tree whose last migration is `0005_items.sql`.
- The journal has a fresh `daemon listening` line. A migration that fails stops the start with `migrating /var/lib/chainmail/state.db: migration <file>: ...`.

The `sqlite3` line runs SQLite as the `chainmail` user, read-only. Never open the state files as root: SQLite can create `state.db-wal` and `state.db-shm` owned by root, and the daemon then cannot open its own database. Where `sqlite3` is installed, as on most other hosts, `sudo -u chainmail sqlite3 -readonly ...` does the same.

A `recreating cache.db` warning with reason `version` after an upgrade is normal. The release changed the cache's schema, so the cache starts again and backfills (see [Not ready or backfilling](cache.md#not-ready-or-backfilling)). Nothing else is lost.

Finish with the [health checks](health.md).

## Roll back

On NixOS, go back to the previous generation, or revert the lock change and deploy again:

```sh
sudo nixos-rebuild switch --rollback
```

If the newer release applied no migration, that is all. If it did, the older binary refuses `state.db` with `schema version <n> is newer than this binary (<m>)`. Restore the snapshot you took before the upgrade, as in [Restore](backup-restore.mdx#restore). Everything recorded since the snapshot is lost: audit rows, events, items changes and idempotency keys. The cache needs nothing, since a `cache.db` of another schema is rebuilt.

## Upgrade on other hosts

Get the new binary, put it where your unit's `ExecStart` or your launchd job points, and restart. From the flake:

```sh
nix build github:yaklabco/chainmail
./result/bin/chainmail version
```

Or, from a checkout, `nix develop -c go tool stave build` writes `bin/chainmail`. A release archive from [GitHub releases](https://github.com/yaklabco/chainmail/releases) holds the binary too (see [Install](../getting-started/install.md)). With Homebrew, `brew upgrade chainmail` replaces the binary, and the launchd job needs a restart afterwards (see [Start, stop and restart](start-stop.md#commands)).

Keep the old binary until the checks in [Check the upgrade](#check-the-upgrade) pass, so you can put it back. The same migration rule applies as on NixOS: an older binary refuses a `state.db` that a newer one migrated, so a rollback past a migration needs the snapshot.
