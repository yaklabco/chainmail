---
title: Start, stop and restart
description: Start, stop and restart the daemon, what a clean shutdown does, and why a daemon refuses to start.
sidebar_position: 3
---

## Commands

```sh
sudo systemctl start chainmail
sudo systemctl stop chainmail
sudo systemctl restart chainmail
```

On NixOS you rarely need these: switching to a configuration that changes the package or `services.chainmail.settings` restarts the unit. A config edit on another host needs a restart, since the daemon reads its config only at start.

On macOS, with your own launchd job (see [Deploy](deploy.mdx#macos)):

```sh
sudo launchctl kickstart -k system/'<label>'
sudo launchctl bootout system/'<label>'
sudo launchctl bootstrap system /Library/LaunchDaemons/'<label>'.plist
```

The first restarts the job. The second stops it and unloads it. The third loads it again, which starts it when the job has `RunAtLoad`.

## What a clean shutdown looks like

On `SIGTERM` (what `systemctl stop` sends) or Ctrl-C, the daemon:

1. cancels every running call at once, together with the watchers and the cache's background work. Running calls are not given time to finish. A call that was waiting on a mail server stops and fails, usually with `TIMEOUT`, and its audit row is still written. The exception is a send that has reached the SMTP step. From there the message goes out in full or not at all, bounded only by the SMTP connection's own timeout.
2. stops accepting connections and removes the socket, so new clients see `DAEMON_UNREACHABLE`. It gives the open connections up to 10 seconds to close, then drops the ones still open.
3. waits for every call's handler to return, with no time limit. A handler can outlive its dropped connection, such as a send in its SMTP step.
4. waits for the watchers to stop. Each ends its IDLE and logs out within 5 seconds. Then it logs out of the mail servers, all accounts in parallel, each within 5 seconds.
5. stops its background loops, then closes `cache.db` and `state.db`, which folds their write-ahead logs back into the files.
6. exits 0 and prints nothing.

If connections were still open after the 10 seconds, steps 3 to 5 still run, but the daemon then prints a `shutting down` error in the CLI's error form and exits non-zero. A send held up in step 3 can make the whole stop take longer than 10 seconds. systemd kills a daemon that is still stopping after the unit's stop timeout, 90 seconds unless your systemd sets another default. A killed daemon leaves a `state.db-wal`, which the next start applies.

After a clean stop, the journal shows only systemd's own lines:

```
systemd[1]: Stopping chainmail policy daemon...
systemd[1]: chainmail.service: Deactivated successfully.
systemd[1]: Stopped chainmail policy daemon.
```

A `logging out of mail servers` warning during the stop only means a server did not answer the logout in time. It loses nothing. After a clean stop, `/var/lib/chainmail` has no `state.db-wal` file.

`chainmail watch` clients reconnect by themselves, from 1 second up to 30 seconds apart, and resume from their group. `chainmail mcp` sessions keep running, and their calls fail with `DAEMON_UNREACHABLE` until the daemon is back.

## After a crash

A daemon that was killed leaves its socket behind. The next start replaces a stale socket by itself. Sends that were in flight are settled as unknown, with the warning `sends left in progress by an earlier daemon now have an unknown outcome`. Treat those like [a send whose outcome is unknown](policy.md#send-outcome-unknown-exit-8). Watched folders catch up from their checkpoints, so events for mail that arrived while the daemon was down appear after the start.

## The daemon refuses to start

`systemctl status chainmail` shows `activating (auto-restart)` and the unit retries every 5 seconds. Read the reason:

```sh
sudo journalctl -u chainmail -o cat -n 20 --no-pager | grep '"error"'
```

Each failure is one line such as `{"error":{"reason":"INTERNAL","message":"..."}}`. Find the message below, fix the cause, then run `sudo systemctl restart chainmail`.

| Message | Cause | Fix |
|---|---|---|
| `client group chainmail-clients: group: unknown group chainmail-clients` | The client group does not exist in `/etc/group`. | Create the group, or set `daemon.client_group` to one that exists. The NixOS module creates it and requires the name `chainmail-clients`. |
| `setting socket group: lchown /run/chainmail/chainmail.sock: operation not permitted` | The daemon's user is not in the client group, so it cannot give the socket that group. | Add the group to the unit's `SupplementaryGroups=`, as the module does. |
| `no vault key: pass --vault-key-file, or run under systemd with LoadCredentialEncrypted=vault-key` | No `--vault-key-file`, and `CREDENTIALS_DIRECTORY` is not set. Reason `USAGE`, exit 2. | Pass `--vault-key-file`, or load the credential in the unit. |
| `vault key: open <path>: no such file or directory` | The key file is missing, or a hand-written unit loads the credential under an ID other than `vault-key`. | Fix `--vault-key-file`, or load the credential as `vault-key`, as the module does. |
| `vault key <path>: mode -rw-r--r-- is too open: others have access` | Others can read the key. | `sudo chmod 0600 <path>`. |
| `vault key <path>: mode -rw-r----- is too open, want 0600 or 0400` | The key is owned by the daemon's user and its group can read it. | `sudo chmod 0600 <path>`. |
| `vault key <path>: owned by uid <n>: key file must be owned by root or the daemon user` | The key belongs to another user. | `sudo chown` it to the daemon's user, mode `0600`. |
| `vault key <path>: owned by root with group <n>: a root-owned key file must have group root` | A root-owned key with another group. | `sudo chown root:root <path>`. |
| `vault key <path>: mode ... is too open: group may only read` | A root-owned key the group can write. | `sudo chmod 0440 <path>` or stricter. |
| `decrypting vault /var/lib/chainmail/vault.age: no identity matched any of the recipients` | The key is not the one that encrypted `vault.age`. | Use the right key. After moving a dev vault, the credential must be sealed from the dev key (see [Move a dev daemon's state to production](backup-restore.mdx#move-a-dev-daemons-state-to-production)). |
| `vault /var/lib/chainmail/vault.age has format version <n>, want <m>` | The vault was written by another release. | Run the release that wrote it, or restore a matching `vault.age`. |
| `another daemon is listening on /run/chainmail/chainmail.sock` | Another process answers on the socket. | Find it with `systemctl status chainmail` and stop the extra one. A stale socket from a crash is replaced without this error. |
| `<path> exists and is not a socket` | A regular file or directory sits at the socket path. | Remove it. |
| `cannot tell whether <path> is live: ...` | The daemon cannot connect to the old socket to see whether it is live, often `permission denied`. It refuses to take over a path that might be live. | Check who owns the socket and remove a stale one by hand. |
| `listening on <path>: ...` | The daemon could not create the socket, such as when its directory is missing or not writable by the daemon's user. | Create the directory, owned by the daemon's user. Under systemd, `RuntimeDirectory=chainmail` does this at each start. |
| `reading config: open /etc/chainmail/config.toml: no such file or directory` | No config file. | Create it, or pass `--config`. |
| `config /etc/chainmail/config.toml: parsing: unknown config key: <keys>` | A misspelt key, or a key from a later release. Every unknown key is listed, such as `policy.send_allowed, rules`. | Fix or remove the key. See the [configuration reference](../reference/configuration.md). |
| `config /etc/chainmail/config.toml: parsing: key "Policy": keys must be lower case` | A key or table name with capital letters, such as `[Policy]`. | Write it in lower case. |
| `config /etc/chainmail/config.toml: invalid config: <problem>; <problem>` | Values that fail validation, such as `watch.retention: 1m must be between 1h and 2160h`. Every problem is listed. | Fix each one. |
| `migrating /var/lib/chainmail/state.db: schema version <n> is newer than this binary (<m>)` | An older binary than the one that last ran on this `state.db`, as after a rollback. | See [Roll back](upgrades.md#roll-back). |
| `--dev needs a --state-dir other than /var/lib/chainmail` | A `--dev` daemon pointed at the production state. Reason `USAGE`. | Do not run `--dev` against production state. |

Two failures come from outside chainmail:

- A credential systemd cannot decrypt, such as a `.cred` sealed on another host's TPM, fails the unit before chainmail runs. `systemctl status` shows a credentials failure and the journal has systemd's reason. Seal the key on this host (see [The vault key](backup-restore.mdx#the-vault-key)).
- On NixOS, a bad module setting stops the build, not the service, with one of the module's assertions: `services.chainmail.vaultKeyCredential must be a Nix path or an absolute path.`, `services.chainmail.settings.daemon.client_group must be "chainmail-clients"; add users with services.chainmail.clientUsers.` or `services.chainmail.settings.daemon.socket must be under /run/chainmail/.`
