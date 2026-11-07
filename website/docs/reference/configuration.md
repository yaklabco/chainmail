---
title: Configuration
description: Every key of the daemon's config.toml, with its type, default, valid values and effect.
sidebar_position: 1
---

# Configuration

The daemon reads one TOML file at startup. This page lists every key it accepts. The keys come from the structs and validation in [`internal/config`](https://github.com/yaklabco/chainmail/tree/main/internal/config).

## The file

The daemon reads `/etc/chainmail/config.toml` unless `--config` or `CHAINMAIL_CONFIG` names another file. The flag wins over the variable.

The file holds no secrets. Passwords, OAuth2 client secrets and refresh tokens live in the daemon's vault (see [Vault secrets](#vault-secrets)).

The daemon reads the file only when it starts, so restart it after an edit.

On NixOS the module renders this file from `services.chainmail.settings`. See [Deploy](../operations/deploy.mdx).

The CLI's own settings, such as the socket and the output format, live in a different file, `cli.toml`. See [CLI settings](../guide/cli-settings.md).

### How the file is checked

- An unknown key is an error, and the message names it, such as `unknown config key: rules`.
- A key with a capital letter is an error, such as `[Policy]`. So is a key with a dot inside quotes, such as `[accounts."a.b"]`.
- A value of the wrong type is an error, such as `pool = "2"`.
- No environment variable can set or change a key.
- Once every key is known and every type is right, the daemon checks the values and reports every invalid one together, separated by `; `.

A config error stops the daemon at startup. It exits 1 with the reason `INTERNAL`. The message starts with `config <path>:`, or with `reading config:` when the file cannot be read. See [Exit codes](./exit-codes.md).

```text
config /etc/chainmail/config.toml: invalid config: policy.max_recipients must be 1 to 500; watch.retention: 30m must be between 1h and 2160h
```

Keys from the full design, such as `rules`, belong to later sub-projects. This version rejects them as unknown.

## Value formats

| Format | Written as | Examples |
|---|---|---|
| Name | 1 to 63 characters: lowercase letters, digits and hyphens, starting with a letter or digit (`^[a-z0-9][a-z0-9-]{0,62}$`). Account and OAuth2 client names use it, because they become vault secret names. | `work`, `site-2` |
| Size | A whole number of bytes, up to 12 digits, optionally followed by `B`, `KiB`, `MiB`, `GiB` or `TiB`. Units are powers of 1024. | `65536`, `64KiB`, `35MiB`, `2GiB` |
| Duration | A Go duration. | `30s`, `5m`, `168h`, `1h30m` |
| Cache duration | A whole number of days as `<n>d`, or a Go duration. Only the `[cache]` keys accept the `d` unit. | `90d`, `0d`, `250ms` |
| Allowlist entry | A bare address, or `*@` and a domain. Matching ignores case. A `*@domain` entry matches that exact domain, not its subdomains. | `ann@example.com`, `*@example.org` |

## A full example

Every key is set here, most of them to their defaults. A real file needs only the accounts and the keys you change.

```toml
[daemon]
socket = "/run/chainmail/chainmail.sock"
client_group = "chainmail-clients"
body_limit = "64KiB"               # longer bodies are cut unless --full is given

[oauth_clients.workspace]          # secret in the vault as oauth_clients/workspace
client_id = "1234-example.apps.googleusercontent.com"

[oauth_clients.gmail]
client_id = "5678-example.apps.googleusercontent.com"

[accounts.work]
provider = "google-workspace"      # auth defaults to oauth2, the only method it accepts
address = "ann@example.org"
oauth_client = "workspace"         # refresh token stored as tokens/work by auth login
display_name = "Ann Example"       # the From name on mail this account sends
signature = "Ann\n[example.org](https://example.org)"   # Markdown

[accounts.personal]
provider = "gmail"
address = "ann.example@example.com"
auth = "oauth2"                    # gmail accepts two methods, so auth is required
oauth_client = "gmail"

[accounts.personal.cache]
prefetch_bodies = "30d"            # any [cache] key but max_size and dir, for one account

[accounts.icloud]
provider = "icloud"
address = "ann@example.net"        # app password in the vault as accounts/icloud

[accounts.site]
provider = "hostinger"
address = "hello@example.com"
archive_folder = "Archive"         # where archive moves mail; default the \Archive folder
pool = 2                           # IMAP connections; default from the probe
max_message_size = "35MiB"         # largest message it sends; default from the preset
watch = ["INBOX"]                  # folders watched with IDLE; [] watches nothing
sync = { folders = ["INBOX", "Sent"], poll = "5m" }

[accounts.lab]
provider = "custom"
address = "ann@lab.example.net"
username = "ann"

[accounts.lab.imap]
host = "mail.lab.example.net"
port = 993                         # security defaults to tls

[accounts.lab.smtp]
host = "mail.lab.example.net"
port = 587
security = "starttls"

[accounts.lab.policy]
send_allow = ["*@lab.example.net"] # allowed for this account only

[policy]
send_allow = ["*@example.org", "bob@example.com"]
max_recipients = 10
allow_expunge = false
allow_folder_delete = false

[watch]
retention = "168h"                 # events and idle groups older than this are removed
max_events = 200000
expunge_sweep = "1h"
idle_fallback_poll = "30s"         # watched folders on servers without IDLE
google_check = "5s"                # how often a Google account's All Mail is synced while it idles

[cache]
enabled = true                     # false: no cache work; every search and get uses the server
prefetch_bodies = "90d"            # bodies of mail that arrived within this are fetched ahead
read_retention = "30d"             # bodies read outside the window are kept this long
max_size = "2GiB"                  # the whole cache.db file
daily_download = "1GiB"            # per account per UTC day, for backfill and prefetch
backfill_batch = 250
backfill_pause = "250ms"
fresh_lag = "30s"                  # how far the cache may trail mail events and still answer
# dir = "/var/cache/chainmail"     # default: the daemon's --state-dir
```

## `[daemon]`

| Key | Type | Default | Valid values | What it does |
|---|---|---|---|---|
| `socket` | string | `/run/chainmail/chainmail.sock` | a path | The socket path of a production daemon. `--socket` overrides it. A `--dev` daemon ignores it and uses `--socket`, then `CHAINMAIL_SOCKET`, then `$XDG_RUNTIME_DIR/chainmail-dev.sock`. On macOS, which has no `/run`, set it to a path in a directory the daemon's user owns. The NixOS module requires a path under `/run/chainmail/`. |
| `client_group` | string | `chainmail-clients` | an existing group | The group that owns the socket. Its members may use the daemon. The daemon refuses to start when the group does not exist. A `--dev` daemon ignores it. The NixOS module requires `chainmail-clients`. |
| `body_limit` | size | `64KiB` | `1KiB` to `8MiB` | The longest message body `get` and `thread` return before they cut it and set `truncated`. `--full`, and `full` in MCP, ignore it. |

## `[oauth_clients.<name>]`

One table per OAuth2 client registration. `<name>` is a [name](#value-formats). The client's secret goes in the vault as `oauth_clients/<name>`.

| Key | Type | Default | Valid values | What it does |
|---|---|---|---|---|
| `client_id` | string | none, required | any non-empty string | The client ID Google issued. |

An empty `[oauth_clients.<name>]` table is an error, because `client_id` is required.

## `[accounts.<name>]`

One table per mail account. `<name>` is a [name](#value-formats), and it is the name every command and tool uses for the account. [Accounts](../getting-started/accounts.md) walks through each provider.

| Key | Type | Default | Valid values | What it does |
|---|---|---|---|---|
| `provider` | string | none, required | `google-workspace`, `gmail`, `icloud`, `hostinger`, `custom` | The preset that gives the servers, the auth methods and the limits. See [Provider presets](#provider-presets). |
| `address` | string | none, required | a bare email address, with no display name | The account's address. From always uses it. |
| `username` | string | the `address` | any string | The login name for IMAP and SMTP. |
| `auth` | string | the preset's only method | `password`, `oauth2`, as the preset accepts | How chainmail logs in. Required for `gmail`, the only preset that accepts both. |
| `oauth_client` | string | none | the name of an `[oauth_clients.<name>]` table | The OAuth2 client to log in with. Required with `oauth2`, and an error with `password`. |
| `imap` | table | the preset's IMAP server | see [Server overrides](#accountsnameimap-and-accountsnamesmtp) | Replaces the preset's IMAP server. Required for `custom`. |
| `smtp` | table | the preset's SMTP server | see [Server overrides](#accountsnameimap-and-accountsnamesmtp) | Replaces the preset's SMTP server. Required for `custom`. |
| `display_name` | string | none | any string | The display name in the From of mail the account sends. |
| `signature` | string | none | Markdown | Added under the body of every message the account writes, after `-- ` in the text part. `--no-signature`, and `no_signature` in MCP, leave it out. |
| `archive_folder` | string | the folder with the `\Archive` attribute | a folder name | Where `archive` moves mail on accounts other than Google. Google archives by removing the Inbox label. |
| `pool` | integer | from the probe, else the preset | `0` to `12` | The IMAP connections the daemon keeps for the account. `0` is the same as leaving it out. A set value overrides the probed size, within the connection cap. See [Connections](#connections). |
| `max_message_size` | size | `25MiB` for Google and custom servers, `35MiB` for iCloud and Hostinger | `1KiB` to `1024MiB` | The largest message the account sends, attachments included. It also caps each upload. Whatever it is, a `send-raw` message, a body or HTML file, and a dry run's output are limited to 40 MiB, the most one API request or response holds. |
| `watch` | array of strings | `["INBOX"]` | folder names, none empty, none twice | The folders watched with IDLE, each on a connection of its own. `[]` watches nothing. An error on the Google presets, which always watch All Mail. See [Watching for mail](../guide/watching.md). |
| `sync` | table | see below | see [`[accounts.<name>.sync]`](#accountsnamesync) | Which folders are synced, and how often. |
| `policy` | table | none | see [`[accounts.<name>.policy]`](#accountsnamepolicy) | The account's own allowlist entries. |
| `cache` | table | the `[cache]` values | see [`[accounts.<name>.cache]`](#accountsnamecache) | Cache settings for this account only. |

An empty `[accounts.<name>]` table is an error, because `provider` and `address` are required.

### `[accounts.<name>.imap]` and `[accounts.<name>.smtp]`

A server override replaces the whole preset server, so give `host` and `port` even when only one of them changes.

| Key | Type | Default | Valid values | What it does |
|---|---|---|---|---|
| `host` | string | none, required | a host name or IP address | The server's host. |
| `port` | integer | none, required | `1` to `65535` | The server's port. |
| `security` | string | `tls` | `tls`, `starttls` | `tls` is TLS from the first byte, as on ports 993 and 465. `starttls` upgrades a plain connection, as on port 587. |

### `[accounts.<name>.sync]`

The table can also be written inline, as `sync = { folders = ["INBOX", "Sent"], poll = "5m" }`.

| Key | Type | Default | Valid values | What it does |
|---|---|---|---|---|
| `enabled` | boolean | `true` | `true`, `false` | `false` turns off watching and syncing for the account, Google presets included. |
| `folders` | array of strings | every folder a default search covers | folder names, none empty | The folders synced as well as the watched ones. Every watched folder is synced. A synced folder that is not watched is polled every `poll`. Without this key, the daemon syncs each selectable folder except Trash, Junk and a virtual All folder, and lists the folders again every `watch.expunge_sweep`. Setting it limits syncing, and so the cache, to the watch list and these folders. `[]` syncs the watch list alone. An error on the Google presets. |
| `poll` | duration | `5m` | `30s` to `24h` | How often synced folders that are not watched are checked. Also how often a missing folder is tried again. |

### `[accounts.<name>.policy]`

| Key | Type | Default | Valid values | What it does |
|---|---|---|---|---|
| `send_allow` | array of strings | `[]` | [allowlist entries](#value-formats) | Entries added to the global `policy.send_allow` for this account only. |

No other key is allowed in this table. `max_recipients`, `allow_expunge` and `allow_folder_delete` are global. See [The send policy](../concepts/policy.md).

### `[accounts.<name>.cache]`

Any `[cache]` key except `max_size` and `dir`, for one account. A key left out takes the `[cache]` value. `max_size` and `dir` bound and place the one shared `cache.db` file, so they are an error here.

## Provider presets

The `provider` key picks one of these presets. The values come from [`internal/provider`](https://github.com/yaklabco/chainmail/blob/main/internal/provider/provider.go).

| Provider | IMAP | SMTP | Auth | Connection cap | Pool before a probe | `max_message_size` |
|---|---|---|---|---|---|---|
| `google-workspace` | `imap.gmail.com:993`, tls | `smtp.gmail.com:587`, starttls | `oauth2` | 10 | 6 | `25MiB` |
| `gmail` | `imap.gmail.com:993`, tls | `smtp.gmail.com:587`, starttls | `oauth2` or `password` | 10 | 6 | `25MiB` |
| `icloud` | `imap.mail.me.com:993`, tls | `smtp.mail.me.com:587`, starttls | `password` | 3 | 3 | `35MiB` |
| `hostinger` | `imap.hostinger.com:993`, tls | `smtp.hostinger.com:465`, tls | `password` | 3 | 3 | `35MiB` |
| `custom` | from `[accounts.<name>.imap]` | from `[accounts.<name>.smtp]` | `password` | 3 | 3 | `25MiB` |

Gmail allows 15 IMAP connections per account. The Google presets take 10, which leaves 5 for your other mail clients. The Google presets watch All Mail and do not accept `watch` or `sync.folders`.

## `[policy]`

The global send and delete policy. [The send policy](../concepts/policy.md) explains how a send is checked, and [Policy in operation](../operations/policy.md) covers held sends and refusals.

| Key | Type | Default | Valid values | What it does |
|---|---|---|---|---|
| `send_allow` | array of strings | `[]` | [allowlist entries](#value-formats) | The recipients every account may send to. The addresses of the configured accounts are always allowed. A send to anyone else is held as a draft. A recipient that is not a plain `local@domain` address, such as one with a quoted local part, is always held. |
| `max_recipients` | integer | `10` | `1` to `500`, or `0` for the default | The most distinct recipients, To, Cc and Bcc together, that a send may have. A send with more is held. |
| `allow_expunge` | boolean | `false` | `true`, `false` | Allows `expunge`, and lets draft removal expunge instead of moving the old draft to Trash. |
| `allow_folder_delete` | boolean | `false` | `true`, `false` | Allows `folder delete`, including `--force` on a folder that holds mail. |

## `[watch]`

The event log and the watchers. See [Watching for mail](../guide/watching.md) and [Watching in operation](../operations/watching.md).

| Key | Type | Default | Valid values | What it does |
|---|---|---|---|---|
| `retention` | duration | `168h` | `1h` to `2160h` | How long events are kept, and how long a group may go unused before it is removed. |
| `max_events` | integer | `200000` | `1000` to `10000000`, or `0` for the default | The most events the log keeps. The oldest are removed first. |
| `expunge_sweep` | duration | `1h` | `5m` to `24h` | How often synced folders are swept for expunged mail. |
| `idle_fallback_poll` | duration | `30s` | `10s` to `10m` | How often a watched folder is synced on a server without IDLE. |
| `google_check` | duration | `5s` | `2s` to `10m` | How often a Google account's All Mail is synced while its IDLE waits. Gmail can tell an IDLE about new mail 30 seconds or more after it arrives, so this bounds how late a `new` event can be. |

## `[cache]`

The local cache of every account. Every key but `max_size` and `dir` can also be set per account in `[accounts.<name>.cache]`. See [The local cache](../concepts/cache.md) and [The cache in operation](../operations/cache.md).

| Key | Type | Default | Valid values | What it does |
|---|---|---|---|---|
| `enabled` | boolean | `true` | `true`, `false` | `false` stops all cache work, and every search and `get` uses the mail servers. Rows already cached stay in `cache.db` until you delete it. `cache rebuild` refuses an account whose cache is off. |
| `prefetch_bodies` | cache duration | `90d` | `0d` to `3650d` | The body window. Bodies of messages that arrived within it are fetched ahead and kept. `0d` fetches bodies only when they are read. |
| `read_retention` | cache duration | `30d` | `0d` to `3650d` | How long a body outside the window is kept after a `get` or `thread` read it. |
| `max_size` | size | `2GiB` | `64MiB` to `1TiB` | The most space `cache.db` may take, headers and index included. Global only. |
| `daily_download` | size | `1GiB` | `16MiB` to `100GiB` | The daily download allowance that the backfill and the body prefetch keep to, per account per UTC day, within about one batch. It counts each body's size and 2 KiB per header. A `get` of a message the cache lacks counts too. |
| `backfill_batch` | integer | `250` | `10` to `1000`, or `0` for the default | Headers fetched per backfill batch. |
| `backfill_pause` | cache duration | `250ms` | `0s` to `60s` | The pause between backfill batches. |
| `fresh_lag` | cache duration | `30s` | `1s` to `10m` | How far the cache may trail the account's mail events and still answer searches. |
| `dir` | string | the daemon's `--state-dir` | an absolute path | The directory for `cache.db`. Global only. The daemon creates a missing directory with mode `0700`. |

## Vault secrets

The vault holds the secrets the config refers to by name.

| Secret | Holds | Written by |
|---|---|---|
| `accounts/<account>` | a password or app password | [`chainmail secret set accounts/<account>`](./commands/chainmail-secret-set.md) |
| `oauth_clients/<client>` | an OAuth2 client secret | [`chainmail secret set oauth_clients/<client>`](./commands/chainmail-secret-set.md) |
| `tokens/<account>` | an OAuth2 refresh token | [`chainmail auth login <account>`](./commands/chainmail-auth-login.md) |

`secret set` accepts only names the config uses: `accounts/<account>` where the account has `auth = "password"`, and `oauth_clients/<client>` where the client is defined. It refuses `tokens/<account>`, because only `auth login` writes a refresh token. See [Auth](../operations/auth.md).

## Connections

Each watched folder holds one IMAP connection of its own, outside the pool. A Google account holds one for All Mail. These watcher connections count toward the account's connection cap: the probed cap when there is one, else the preset's.

The pool shrinks to fit what the watchers leave, never below 1, and a configured `pool` is capped the same way. The watchers never shrink. Polled folders and the cache use the pool too.

The cache's background work takes a connection only when one is idle, or when the pool has room and no request is waiting. It gives the connection back after one batch, so it slows down rather than delay a request.

A Hostinger account that watches only its inbox therefore runs 1 watcher and a pool of 2 until a probe measures it. See [Connection limits](../operations/connection-limits.md).

## Daemon flags and environment

These settings are not in the file. Each flag has an environment variable, and the flag wins.

| Flag | Variable | Default | What it does |
|---|---|---|---|
| `--config` | `CHAINMAIL_CONFIG` | `/etc/chainmail/config.toml` | The config file. |
| `--state-dir` | `CHAINMAIL_STATE_DIR` | `/var/lib/chainmail` | The directory for `vault.age`, `state.db` and, unless `[cache] dir` is set, `cache.db`. The daemon creates it with mode `0700`. |
| `--vault-key-file` | `CHAINMAIL_VAULT_KEY_FILE` | `$CREDENTIALS_DIRECTORY/vault-key` | The age identity that decrypts the vault. Without the flag, the variable or `CREDENTIALS_DIRECTORY`, the daemon refuses to start. |
| `--dev` | `CHAINMAIL_DEV` | `false` | Development mode: the daemon runs as you, needs a state directory other than `/var/lib/chainmail`, and ignores `daemon.socket` and `daemon.client_group`. The variable takes `true`, `false`, `1`, `0`, `t` or `f`. |
| `--socket` | `CHAINMAIL_SOCKET` | `daemon.socket` | The socket to listen on. A production daemon reads only the flag, then `daemon.socket`. A `--dev` daemon reads the flag, then the variable. |

The full flag list is on the [`chainmail daemon`](./commands/chainmail-daemon.md) page.
