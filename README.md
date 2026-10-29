# chainmail

Email for AI agents. chainmail is one Go binary that lets agents read, write and organize mail over IMAP and SMTP without ever holding a password or token. A long-running daemon owns every credential and every connection to the mail servers. Agents talk to it through the `chainmail` CLI, which is a thin client on a Unix socket. Every call except `admin status` is written to an audit log.

Sub-project 1, the foundation, is what exists today: the daemon and its socket, the vault, password and OAuth2 login, `secret set`, `auth login` and `accounts test`. Reading, sending, policy, approvals and the MCP server arrive in later sub-projects.

```mermaid
flowchart LR
  subgraph you["your user"]
    A1[agent] --> CLI[chainmail CLI]
    A2[agent] --> CLI
    H[human] --> CLI
  end
  CLI -- "ConnectRPC over a Unix socket" --> D
  subgraph daemon["chainmail daemon"]
    D[daemon] --> V[(vault.age)]
    D --> S[(state.db audit log)]
  end
  D -- "IMAP and SMTP over TLS" --> M[mail servers]
  D -- "OAuth2 token exchange and refresh" --> G[Google]
```

## Security model

- **The daemon owns the credentials.** Passwords, OAuth client secrets and refresh tokens live in the daemon's vault and nowhere else. The CLI holds no secrets and no mail logic. In production the daemon runs as its own `chainmail` system user with its state in `/var/lib/chainmail` (mode `0700`), which your user can't read. The NixOS module that sets this up arrives in sub-project 4. Until then the daemon runs with `--dev` as your own user, which is for development only: anything running as you can read a dev vault's key.
- **Secrets go in and never come out.** `chainmail secret set` and `chainmail auth login` write secrets. No API call returns one. `chainmail admin status` lists secret names only.
- **The vault is encrypted at rest.** `vault.age` is an [age](https://age-encryption.org) file. In production its key arrives through systemd `LoadCredentialEncrypted=`, sealed to the host's TPM2, at `$CREDENTIALS_DIRECTORY/vault-key`. In development it is a file passed with `--vault-key-file`. The daemon refuses a key file that others can access. The file must either be owned by the daemon's user with no group or other bits (`0600` or `0400`), or be owned by `root:root` with no other bits and at most group read. The second form exists because systemd delivers a `LoadCredential` file as `0440 root:root` and grants the service user read access through an ACL.
- **The socket has a group and a peer check.** The daemon creates its socket with mode `0660`. In production it sets the socket's group to `client_group` (default `chainmail-clients`). On every connection it reads the peer's credentials with `SO_PEERCRED` and allows only its own user and members of the client group. Membership is looked up per request, so adding a user to the group takes effect without a restart. A dev daemon allows only your own user. A refused peer gets `FORBIDDEN` (exit 3).
- **Every change is audited.** The `audit` table in `state.db` records the time, caller UID, client kind (`cli`, `live-test`), account, action, targets, result and error reason for each call. Today the audited actions are `accounts.list`, `account.test`, `secret.set`, `auth.login.begin` and `auth.login.finish`. Rows never contain secret values or message content. The table is append-only: triggers refuse updates and deletes. If the audit write fails, the call fails too, so chainmail never reports success for something it could not record. A `chainmail audit` reader arrives in sub-project 4. Until then, query `state.db` with `sqlite3`.
- **Root is out of scope.** chainmail stops accidents and prompt injection that run through chainmail. It does not stop an agent that can become root. If your user has passwordless sudo or is in the `docker` group, an agent with an unrestricted shell can read the vault through root. Closing that gap means running agents without sudo or docker, which is outside chainmail.

## Build

With Nix:

```fish
nix build
./result/bin/chainmail version
```

Or inside the dev shell, which provides Go, buf, golangci-lint, gotestsum and age:

```fish
nix develop -c go tool stave build
./bin/chainmail version
```

Other stave targets: `check` (the default, and what CI runs: generated code check, lint, unit tests and build), `generate`, `generateCheck`, `lint`, `test:unit` and `test:live`.

## Provider setup

Each account names a provider preset that supplies its servers and the auth methods it accepts.

| Preset | IMAP | SMTP | Auth |
|---|---|---|---|
| `google-workspace` | `imap.gmail.com:993` TLS | `smtp.gmail.com:587` STARTTLS | `oauth2` |
| `gmail` | `imap.gmail.com:993` TLS | `smtp.gmail.com:587` STARTTLS | `oauth2` or `password` |
| `icloud` | `imap.mail.me.com:993` TLS | `smtp.mail.me.com:587` STARTTLS | `password` |
| `hostinger` | `imap.hostinger.com:993` TLS | `smtp.hostinger.com:465` TLS | `password` |
| `custom` | from config | from config | `password` |

### Google OAuth2 in general

Both Google presets log in with OAuth2 and the scope `https://mail.google.com/`. Google's device-code flow does not allow that scope, so `chainmail auth login` uses a paste-back flow. It prints a consent URL. You open it in any browser, on any machine, and approve. The browser is then sent to `http://127.0.0.1:53682/` and fails to load the page. That is expected. Copy the full URL from the address bar and paste it back into the terminal. A login expires after 10 minutes. Desktop app clients accept loopback redirects without registering them, so there is no redirect URI to configure.

A Google Cloud project's audience is either Internal or External, never both. A Workspace account and a personal Gmail account therefore need separate projects, each with its own OAuth client.

Google shows a client secret only once, when the client is created. Download the JSON then. The client ID and secret are under the `installed` key:

```fish
jq -r .installed.client_id client_secret_*.json
jq -r .installed.client_secret client_secret_*.json | pass insert -m chainmail/oauth-workspace
```

### Google Workspace

1. In the Google Cloud console, create a project for chainmail inside your Workspace organization.
2. Under APIs and Services, enable the Gmail API. IMAP and SMTP do not need it, but enabling it is harmless and makes the Gmail scope pickable in the next steps.
3. Under Google Auth Platform, fill in Branding (app name and support email) and set Audience to **Internal**. Internal apps need no verification, and their refresh tokens do not expire after 7 days.
4. Under Data access, add the scope `https://mail.google.com/`.
5. Under Clients, create an OAuth client of type **Desktop app** and download its JSON.
6. Put the client ID in `[oauth_clients.<name>]` in the config, set the account's `oauth_client` to `<name>`, and store the secret with `chainmail secret set oauth_clients/<name>`.
7. Run `chainmail auth login <account>`.

### Personal Gmail with OAuth2

1. Create a second project. It cannot share the Workspace project, because an Internal audience only accepts users from the Workspace domain.
2. Enable the Gmail API, as above.
3. Set Audience to **External**, then publish the app so its status is **In production**. An app left in Testing has its refresh tokens expire after 7 days.
4. Publishing an External app needs more on the Branding page: the app home page, a privacy policy link and an authorized domain. Leave the logo empty. Uploading a logo forces Google's verification review.
5. Under Data access, add the scope `https://mail.google.com/`.
6. Create a **Desktop app** client and download its JSON.
7. Configure it as for Workspace and run `chainmail auth login <account>`.

The app stays unverified. Unverified apps are capped at 100 users, which is plenty. The first login shows "Google hasn't verified this app". Click Advanced, then continue to the app.

### Personal Gmail with an app password

Instead of OAuth2, set `auth = "password"` on a `gmail` account. Turn on 2-Step Verification for the Google account, create an app password at myaccount.google.com/apppasswords, and store it with `chainmail secret set accounts/<account>`.

### iCloud

Create an app-specific password at account.apple.com under Sign-In and Security, and store it with `chainmail secret set accounts/<account>`. chainmail logs in with the full address. If IMAP rejects that, set `username` to the part of the address before the `@`.

### Hostinger

Use the mailbox password from hPanel, stored with `chainmail secret set accounts/<account>`.

### Custom servers

The `custom` preset has no servers of its own, so the account must set both `imap` and `smtp`. It accepts password auth only.

### When a server rejects the credentials

chainmail never retries rejected credentials, because providers lock out accounts that keep failing. Instead it marks the account as needing a human, and calls for it fail with `AUTH_REQUIRED` and a hint saying what to run. Only a real credential failure does this:

- IMAP: a `NO` reply to LOGIN or AUTHENTICATE with no response code, or with `AUTHENTICATIONFAILED`, `AUTHORIZATIONFAILED`, `EXPIRED` or `WEBALERT`.
- SMTP: a `534` or `535` reply to AUTH.
- OAuth2: the token endpoint answering `invalid_grant`, `invalid_client` or `unauthorized_client`.

Temporary server errors, such as other IMAP response codes, Gmail's `ALERT` for too many connections, SMTP `454` or `421`, timeouts and network failures, leave the account alone. So does an OAuth2 token endpoint that cannot be reached, answers `5xx`, or answers a refresh with an error code other than those three: the call fails with `SERVER_UNAVAILABLE` (exit 7). `auth login` reports a `4xx` with an error code as `AUTH_REJECTED`, and an unreachable endpoint or a `5xx` as `SERVER_UNAVAILABLE`. After the IMAP server rejects a login, `accounts test` does not try SMTP with the same credentials. Setting a new secret, or a new `auth login`, clears the state.

## Configuration reference

The daemon reads one TOML file, `/etc/chainmail/config.toml` by default or the file given with `--config`. It holds no secrets. Unknown keys are an error. Once every key is known, all invalid values are reported together. The daemon reads the file only at startup, so restart it after an edit.

```toml
[daemon]
socket = "/run/chainmail/chainmail.sock"
client_group = "chainmail-clients"

[oauth_clients.workspace]        # secret in the vault as oauth_clients/workspace
client_id = "1234.apps.googleusercontent.com"

[oauth_clients.gmail]
client_id = "5678.apps.googleusercontent.com"

[accounts.work]
provider = "google-workspace"    # auth defaults to oauth2, the only method it accepts
address = "james@example.org"
oauth_client = "workspace"       # refresh token stored as tokens/work by auth login

[accounts.personal]
provider = "gmail"
address = "james@gmail.com"
auth = "oauth2"                  # gmail accepts two methods, so auth is required
oauth_client = "gmail"

[accounts.icloud]
provider = "icloud"
address = "james@icloud.com"     # app password in the vault as accounts/icloud

[accounts.site]
provider = "hostinger"
address = "me@example.com"

[accounts.lab]
provider = "custom"
address = "me@lab.example.net"
username = "me"

[accounts.lab.imap]
host = "mail.lab.example.net"
port = 993                       # security defaults to tls

[accounts.lab.smtp]
host = "mail.lab.example.net"
port = 587
security = "starttls"
```

| Key | Required | Default | Meaning |
|---|---|---|---|
| `daemon.socket` | no | `/run/chainmail/chainmail.sock` | Socket path for a production daemon. A dev daemon ignores it (see the quickstart). `--socket` overrides it. |
| `daemon.client_group` | no | `chainmail-clients` | Group that owns the socket and whose members may use the daemon. Production only. |
| `oauth_clients.<name>` | | | An OAuth2 client registration. `<name>` matches `^[a-z0-9][a-z0-9-]{0,62}$`. Its secret goes in the vault as `oauth_clients/<name>`. |
| `oauth_clients.<name>.client_id` | yes | | The client ID from Google. |
| `accounts.<name>` | | | A mail account. `<name>` matches `^[a-z0-9][a-z0-9-]{0,62}$`. |
| `accounts.<name>.provider` | yes | | `google-workspace`, `gmail`, `icloud`, `hostinger` or `custom`. |
| `accounts.<name>.address` | yes | | The bare email address, with no display name. |
| `accounts.<name>.username` | no | the address | Login name for IMAP and SMTP. |
| `accounts.<name>.auth` | when the provider accepts more than one method | the provider's only method | `password` or `oauth2`. |
| `accounts.<name>.oauth_client` | with `oauth2` | | Name of an `[oauth_clients.<name>]` table. Not allowed with `password`. |
| `accounts.<name>.imap` | for `custom` | the preset's server | Replaces the preset's IMAP server. |
| `accounts.<name>.smtp` | for `custom` | the preset's server | Replaces the preset's SMTP server. |
| `accounts.<name>.imap.host`, `.smtp.host` | yes, in an override | | Server host name. |
| `accounts.<name>.imap.port`, `.smtp.port` | yes, in an override | | Port, 1 to 65535. |
| `accounts.<name>.imap.security`, `.smtp.security` | no | `tls` | `tls` (TLS from the first byte, as on 993 and 465) or `starttls` (as on 587). |

Vault secret names:

| Secret | Holds | Written by |
|---|---|---|
| `accounts/<account>` | password or app password | `chainmail secret set accounts/<account>` |
| `oauth_clients/<client>` | OAuth2 client secret | `chainmail secret set oauth_clients/<client>` |
| `tokens/<account>` | OAuth2 refresh token | `chainmail auth login <account>` |

`secret set` accepts only names the config uses: `accounts/<account>` where the account has `auth = "password"`, and `oauth_clients/<client>` where the client is defined. It refuses `tokens/<account>`, because only `auth login` writes a refresh token.

Keys from the full design, such as `body_limit`, `watch`, `sync`, `policy` and `rules`, belong to later sub-projects. This version rejects them as unknown.

## Dev quickstart

These commands are fish, run inside `nix develop` or with a built `chainmail` and `age-keygen` on your `PATH`.

Create a vault key and a config. `age-keygen -o` writes the key with mode `0600`, which the daemon requires.

```fish
mkdir -p ~/.config/chainmail-dev ~/.local/state/chainmail-dev
age-keygen -o ~/.config/chainmail-dev/vault-key
$EDITOR ~/.config/chainmail-dev/config.toml
```

Point the CLI at the dev socket, then start the daemon in its own terminal. Set the variable in that terminal too, or start the daemon without it and let it use its default.

```fish
set -gx CHAINMAIL_SOCKET $XDG_RUNTIME_DIR/chainmail-dev.sock
chainmail daemon --dev \
    --config ~/.config/chainmail-dev/config.toml \
    --state-dir ~/.local/state/chainmail-dev \
    --vault-key-file ~/.config/chainmail-dev/vault-key
```

The daemon logs JSON lines to stderr and stops cleanly on Ctrl-C. `--dev` refuses `/var/lib/chainmail` as its state directory, including through a symlink. It replaces a stale socket left by a crashed daemon, and refuses to start if another daemon is listening on the path.

A dev daemon picks its socket from `--socket`, then `CHAINMAIL_SOCKET`, then `$XDG_RUNTIME_DIR/chainmail-dev.sock`. Clients pick theirs from `--socket`, then `CHAINMAIL_SOCKET`, then `/run/chainmail/chainmail.sock`. Clients never look at `XDG_RUNTIME_DIR`, so export `CHAINMAIL_SOCKET` (or pass `--socket`) wherever you run the CLI. Where `XDG_RUNTIME_DIR` is not set, as on macOS, the daemon needs `--socket` or `CHAINMAIL_SOCKET` as well.

Store the secrets. `secret set` takes the value from `--from-pass`, from a no-echo prompt when stdin is a terminal, or from stdin. A secret is one line: a trailing newline is dropped, and a value with more than one line is refused. Setting a secret clears the auth-required state of every account that uses it.

```fish
chainmail secret set oauth_clients/workspace --from-pass chainmail/oauth-workspace
chainmail secret set accounts/site
```

`--from-pass <entry>` requires `<store>/<entry>.gpg` to exist, where the store is `$PASSWORD_STORE_DIR` or `~/.password-store`, so a folder or a typo fails with `NOT_FOUND` instead of storing a tree listing. It refuses entries containing a `..` segment or starting with `-`. It runs `pass show` through a pipe and keeps only the first line. gpg asks for its passphrase through pinentry as usual.

Log in to the OAuth2 accounts, then check every account:

```fish
chainmail auth login work
chainmail accounts list --output table
chainmail accounts test work --output table
```

Finally, run the live smoke test. It logs in to every configured account through the daemon at `$CHAINMAIL_SOCKET` and fails if any IMAP or SMTP check fails. It talks to real servers, so it never runs in CI or in `go test ./...`. It only builds with the `live` tag.

```fish
nix develop -c go tool stave test:live
```

## Commands

Every command accepts `--socket <path>` and `--output json|table`. Only the `accounts` commands honour `--output table`. The rest always print JSON.

| Command | Does | Prints on success |
|---|---|---|
| `chainmail version` | Prints the client's version. Needs no daemon. | `{"version":"..."}` |
| `chainmail daemon` | Runs the daemon. Flags: `--dev`, `--config` (default `/etc/chainmail/config.toml`), `--state-dir` (default `/var/lib/chainmail`), `--vault-key-file` (default `$CREDENTIALS_DIRECTORY/vault-key`). | JSON log lines on stderr |
| `chainmail accounts list` | Lists accounts with provider, address, auth method and auth state. `auth_detail` says what to run when `auth_state` is `AUTH_STATE_REQUIRED`. | `{"accounts":[...]}` |
| `chainmail accounts test <account>` | Logs in to the account's IMAP server, then its SMTP server, and reports each check. | `{"account":...,"imap":{...},"smtp":{...}}` |
| `chainmail secret set <name> [--from-pass <entry>]` | Stores a secret in the vault. The name must be `accounts/<account>` for a configured password account, or `oauth_clients/<client>` for a configured OAuth2 client. Any other name, including `tokens/<account>`, fails with `INVALID_ARGUMENT` (exit 2). | `{"name":...,"set":true}` |
| `chainmail auth login <account>` | Runs the OAuth2 paste-back flow and stores the refresh token. | `{"account":...,"logged_in":true}` |
| `chainmail admin status` | Shows the daemon's version, start time, dev mode and stored secret names. | `{"version":...,"started_at":...,"dev_mode":...,"secret_names":[...]}` |
| `chainmail help [command]` | Prints help. `--help` on any command does the same. | help text |

Each check in `accounts test` has `ok`, `reason` (`AUTH_REJECTED`, `SERVER_UNAVAILABLE` or `TIMEOUT` when it failed), `error`, `capabilities` (IMAP only) and `latency_ms`. When a check fails, the command still prints the full result on stdout, then reports the failure on stderr and exits with its code: 5 if any server rejected the credentials, otherwise the code of the first failed check. If the account is missing a secret or already needs a human, no check runs. The command prints nothing on stdout and fails with `AUTH_REQUIRED` (exit 5).

`auth login` writes its instructions to stderr before the outcome, and `secret set` writes its prompt there when stdin is a terminal. An agent driving either should parse only the last line of stderr as the error.

## Exit codes and error output

On success a command prints one line of JSON on stdout and exits 0. Field names are snake_case and unset fields are included.

On failure it prints one line of JSON on stderr and exits non-zero. `hint` is left out when there is none.

```json
{"error":{"reason":"NOT_FOUND","message":"unknown account: \"nosuch\"","hint":"run: chainmail accounts list"}}
```

Reasons and exit codes are a contract with agents and never change. They come from `internal/api`.

| Exit | Reasons | Meaning |
|---|---|---|
| 0 | | Success |
| 1 | `INTERNAL`, and any reason not listed here | A bug or an unexpected failure |
| 2 | `USAGE`, `INVALID_ARGUMENT` | Bad invocation or a bad value |
| 3 | `POLICY_DENIED`, `FORBIDDEN` | Refused. `FORBIDDEN` means your user may not use the socket. |
| 4 | `HELD` | Held for approval (sub-project 4) |
| 5 | `AUTH_REQUIRED`, `AUTH_REJECTED` | A human must set a secret or log in. The hint says what to run. |
| 6 | `NOT_FOUND`, `STALE_REF` | No such account, pass entry or message |
| 7 | `SERVER_UNAVAILABLE`, `TIMEOUT`, `DAEMON_UNREACHABLE` | A mail server or the daemon could not be reached in time |
| 8 | `SEND_OUTCOME_UNKNOWN` | A send may or may not have gone out (sub-project 3) |
| 9 | `TOTP_INVALID`, `TOTP_LOCKED` | Approval code refused (sub-project 4) |

Usage errors always exit 2 with `USAGE`. That covers a missing or extra argument, an unknown flag, a bad `--output` value, a group command such as `chainmail accounts` run on its own, an unknown subcommand, and `chainmail help <unknown>`. Shell completion is disabled, so `chainmail completion` is an unknown command too.
