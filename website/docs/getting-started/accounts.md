---
title: Accounts and providers
description: Configure Google Workspace, Gmail, iCloud, Hostinger and other IMAP and SMTP accounts, and store their secrets in the vault.
sidebar_position: 3
---

# Accounts and providers

Each account is a table `[accounts.<name>]` in the daemon's config. Its `provider` names a preset, which supplies the account's servers and the auth methods it accepts. The account's secret goes in the daemon's vault, never in the config. This page covers each preset, the secrets it needs and how to store them. [Configuration](../reference/configuration.md) lists every account key with its default and limits.

An account name is lowercase letters, digits and hyphens, starting with a letter or digit, at most 63 characters. Commands and the vault refer to the account by that name.

## Presets

| Preset | IMAP | SMTP | Auth | You store |
|---|---|---|---|---|
| `google-workspace` | `imap.gmail.com:993` TLS | `smtp.gmail.com:587` STARTTLS | `oauth2` | the OAuth client secret, then `auth login` |
| `gmail` | `imap.gmail.com:993` TLS | `smtp.gmail.com:587` STARTTLS | `oauth2` or `password` | as above, or an app password |
| `icloud` | `imap.mail.me.com:993` TLS | `smtp.mail.me.com:587` STARTTLS | `password` | an app-specific password |
| `hostinger` | `imap.hostinger.com:993` TLS | `smtp.hostinger.com:465` TLS | `password` | the mailbox password |
| `custom` | from config | from config | `password` | the account's password |

A preset that accepts one auth method uses it when `auth` is left out. `gmail` accepts two, so a `gmail` account must set `auth` to `oauth2` or `password`. `address` is required on every account and must be a bare address, such as `you@example.org`, with no display name. The login name is `username`, which defaults to the address.

## The vault

The vault, `vault.age`, holds three kinds of secret:

| Name | Holds | Written by |
|---|---|---|
| `accounts/<account>` | the password of a `password` account | `chainmail secret set` |
| `oauth_clients/<client>` | the secret of an OAuth client, `[oauth_clients.<client>]` in the config | `chainmail secret set` |
| `tokens/<account>` | the refresh token of an `oauth2` account | `chainmail auth login` only |

`secret set` accepts only the names your config uses, so a new account needs a daemon restart with the new config before its secret can be stored. It refuses `tokens/` names, which only a login writes. No command reads a secret back, and `chainmail admin status` lists the names of stored secrets only.

## Storing a secret

`secret set` prompts for the value without echo when stdin is a terminal. Otherwise it reads the value from stdin.

```sh
chainmail secret set accounts/site
```

With `--from-pass <entry>`, it reads the first line of a [pass](https://www.passwordstore.org) entry, where `<entry>` is the entry's name in your password store:

```sh
chainmail secret set accounts/site --from-pass '<entry>'
```

A secret is one line. A trailing newline is dropped, and a value with more than one line is refused, as is a password containing a NUL byte. `--from-pass` requires `<store>/<entry>.gpg` to exist, where the store is `$PASSWORD_STORE_DIR` or `~/.password-store`, so a folder or a typo fails with `NOT_FOUND`. It refuses entries containing a `..` segment or starting with `-`, runs `pass show` and keeps the first line, and gpg asks for its passphrase through pinentry as usual.

After storing a password, `secret set` measures the account's connection limit, which sizes its pool of IMAP connections. [Connection limits](../operations/connection-limits.md) explains the probe. Setting a secret clears the auth-required state of every account that uses it.

## Google OAuth2

Both Google presets log in with OAuth2 and the scope `https://mail.google.com/`. You register your own OAuth client with Google once, put its client ID in the config, and store its secret in the vault.

Google's device-code flow does not allow that scope, so `chainmail auth login` uses a paste-back flow. It prints a consent URL. You open it in any browser, on any machine, and approve. The browser is then sent to `http://127.0.0.1:53682/` and fails to load the page, which is expected. Copy the full URL from the address bar and paste it into the terminal. A login expires after 10 minutes. Desktop app clients accept loopback redirects without registering them, so there is no redirect URI to configure.

A Google Cloud project's audience is either Internal or External, never both. A Workspace account and a personal Gmail account therefore need separate projects, each with its own OAuth client.

Google shows a client secret only once, when the client is created, so download the client's JSON then. The client ID and secret are under its `installed` key. `<client-json-file>` is the JSON file you downloaded.

```sh
jq -r .installed.client_id '<client-json-file>'
jq -r .installed.client_secret '<client-json-file>' | chainmail secret set oauth_clients/workspace
```

The first command prints the client ID for the config. The second stores the client secret under the client's name, `workspace` here. To keep a copy in pass as well, pipe the secret into `pass insert -m <entry>` instead and store it with `chainmail secret set oauth_clients/workspace --from-pass <entry>`.

### Google Workspace

1. In the Google Cloud console, create a project for chainmail inside your Workspace organization.
2. Under APIs and Services, enable the Gmail API. IMAP and SMTP do not need it, but enabling it makes the Gmail scope selectable in the next steps.
3. Under Google Auth Platform, fill in Branding (app name and support email) and set Audience to **Internal**. Internal apps need no verification, and their refresh tokens do not expire after 7 days.
4. Under Data access, add the scope `https://mail.google.com/`.
5. Under Clients, create an OAuth client of type **Desktop app** and download its JSON.
6. Put the client ID in an `[oauth_clients.<client>]` table, set the account's `oauth_client` to `<client>`, and restart the daemon.
7. Store the secret with `chainmail secret set oauth_clients/<client>`.
8. Run `chainmail auth login <account>`.

```toml
[oauth_clients.workspace]
client_id = "1234.apps.googleusercontent.com"   # required in every oauth_clients table

[accounts.work]
provider = "google-workspace"     # oauth2 only, so auth can be left out
address = "you@example.org"
oauth_client = "workspace"        # required with oauth2
display_name = "Your Name"        # the From name on mail this account sends
signature = "Your Name\n[example.org](https://example.org)"   # Markdown
```

```sh
chainmail auth login work
```

`auth login` stores the refresh token as `tokens/work`.

### Personal Gmail with OAuth2

1. Create a second project. It cannot share the Workspace project, because an Internal audience accepts only users from the Workspace domain.
2. Enable the Gmail API, as above.
3. Set Audience to **External**, then publish the app so its status is **In production**. An app left in Testing has its refresh tokens expire after 7 days.
4. Publishing an External app needs more on the Branding page: the app home page, a privacy policy link and an authorized domain. Leave the logo empty, because uploading a logo forces Google's verification review.
5. Under Data access, add the scope `https://mail.google.com/`.
6. Create a **Desktop app** client and download its JSON.
7. Configure it as for Workspace, store the client secret, and run `chainmail auth login <account>`.

```toml
[oauth_clients.gmail]
client_id = "5678.apps.googleusercontent.com"

[accounts.personal]
provider = "gmail"
address = "you@gmail.example.com"
auth = "oauth2"                   # gmail accepts two methods, so auth is required
oauth_client = "gmail"
```

The app stays unverified. Unverified apps are capped at 100 users, which is enough for one person's accounts. The first login shows "Google hasn't verified this app". Click Advanced, then continue to the app.

## Gmail with an app password

Instead of OAuth2, set `auth = "password"` on a `gmail` account. Turn on 2-Step Verification for the Google account, create an app password at myaccount.google.com/apppasswords, and store it as `accounts/<account>`.

```toml
[accounts.gmail-app]
provider = "gmail"
address = "other@gmail.example.com"
auth = "password"
```

```sh
chainmail secret set accounts/gmail-app
```

## iCloud

Create an app-specific password at account.apple.com under Sign-In and Security, and store it as `accounts/<account>`. chainmail logs in with the full address. If IMAP rejects that, set `username` to the part of the address before the `@`.

```toml
[accounts.icloud]
provider = "icloud"
address = "you@icloud.example.com"
```

```sh
chainmail secret set accounts/icloud
```

## Hostinger

Use the mailbox password from hPanel, stored as `accounts/<account>`. Hostinger keeps folders under `INBOX.`, so folder names in the config are written as `chainmail folders` prints them, such as `INBOX.Archive`.

```toml
[accounts.site]
provider = "hostinger"
address = "me@example.com"
archive_folder = "INBOX.Archive"  # where archive moves mail. Default: the \Archive folder
```

```sh
chainmail secret set accounts/site
```

## Custom servers

The `custom` preset has no servers of its own, so the account must set both `imap` and `smtp`. Each takes `host` and `port`, which are required, and `security`, `tls` or `starttls`, which defaults to `tls`. The preset accepts password auth only.

```toml
[accounts.lab]
provider = "custom"
address = "me@lab.example.net"
username = "me"                   # login name. Default: the address
imap = { host = "mail.lab.example.net", port = 993 }
smtp = { host = "mail.lab.example.net", port = 587, security = "starttls" }
```

```sh
chainmail secret set accounts/lab
```

The same `imap` and `smtp` tables are accepted on the other presets too, where each replaces the preset's server.

## Checking every account

```sh
chainmail accounts list --output table
chainmail accounts test '<account>' --output table
```

Here `<account>` is an account's name from the config, such as `site`. `accounts list` shows each account's auth state, and when an account needs you, the command to run. `accounts test` logs in to the account's IMAP server, then its SMTP server, and reports each check. It exits 5 when a server rejected the credentials. After the IMAP server rejects a login, `accounts test` does not try SMTP with the same credentials.

## When a server rejects the credentials

chainmail never retries rejected credentials, because providers lock out accounts that keep failing. Instead it marks the account as needing a human, and calls for it fail with `AUTH_REQUIRED` (exit 5) and a hint saying what to run. Only a real credential failure does this:

- IMAP: a `NO` reply to LOGIN or AUTHENTICATE with no response code, or with `AUTHENTICATIONFAILED`, `AUTHORIZATIONFAILED`, `EXPIRED` or `WEBALERT`.
- SMTP: a `534` or `535` reply to AUTH.
- OAuth2: the token endpoint answering a refresh with `invalid_grant`, `invalid_client` or `unauthorized_client`.

Temporary server errors leave the account alone. These include other IMAP response codes, Gmail's `ALERT` for too many connections, SMTP `454` or `421`, timeouts and network failures. An OAuth2 token endpoint that cannot be reached, answers `5xx`, or answers a refresh with any other error code leaves the account alone too, and the call fails with `SERVER_UNAVAILABLE` (exit 7).

The hint names the fix. For a rejected password it is `chainmail secret set accounts/<account>`. For `invalid_client` it is `chainmail secret set oauth_clients/<client>`. For a rejected refresh or access token it is `chainmail auth login <account>`.

`auth login` itself reports a `4xx` answer with an error code as `AUTH_REJECTED` (exit 5), and an unreachable endpoint or a `5xx` as `SERVER_UNAVAILABLE`. For `invalid_client` the hint says to set the client secret, and the login stays open until it expires, so the same URL still works after the fix. Any other refusal, such as `invalid_grant` for a code that was already used, ends the login, and the hint says to run `auth login` again.

Setting a new secret, or a new `auth login`, clears the state. [Auth in operation](../operations/auth.md) covers finding and fixing these accounts on a running daemon.
