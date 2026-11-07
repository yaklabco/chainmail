---
title: Auth
description: Bring an account back after its credentials were rejected, log in again with OAuth2, and rotate passwords and client secrets.
sidebar_position: 4
---

chainmail never retries rejected credentials, because providers lock out accounts that keep failing. It marks the account as needing a human. Calls for it fail with `AUTH_REQUIRED` (exit 5), watching stops for that account, and a `status` event with state `auth_required` carries the command to run. [Accounts and providers](../getting-started/accounts.md) says exactly which server replies count as a rejection, and how each provider is set up.

## An account shows AUTH_STATE_REQUIRED

```sh
chainmail accounts list --output table
chainmail watch status --output table
```

`DETAIL` in `accounts list`, and the hint of any failing call, says what to run:

| Detail | Do |
|---|---|
| `the server rejected the password; run: chainmail secret set accounts/<account>` | [Rotate a password](#rotate-a-password) |
| `the server rejected the access token; run: chainmail auth login <account>` | [Log in again](#log-in-again-with-oauth2) |
| `the refresh token was rejected; run: chainmail auth login <account>` | [Log in again](#log-in-again-with-oauth2). Also check that the Google app is published, since an app left in Testing has its refresh tokens expire after 7 days. |
| `the refresh token was issued to another OAuth client; run: chainmail auth login <account>` | [Log in again](#log-in-again-with-oauth2), after a change of `oauth_client`. |
| `the OAuth client was rejected; run: chainmail secret set oauth_clients/<client>` | [Rotate an OAuth client secret](#rotate-an-oauth-client-secret) |
| `run: chainmail auth login <account>` | No refresh token is stored yet. [Log in again](#log-in-again-with-oauth2) |
| `run: chainmail secret set <secret>` | The secret is not in the vault yet. |

In `watch status` every folder of the account shows `auth_required` with the error `AUTH_REQUIRED`. Once the state clears, watching starts again within 15 seconds.

## Log in again with OAuth2

Run this as a member of `chainmail-clients`, in a terminal:

```sh
chainmail auth login '<account>'
```

It prints a consent URL on stderr. Open it in a browser on any machine and approve. The browser is then sent to `http://127.0.0.1:53682/` and fails to load the page. That is expected. Copy the full URL from the address bar, paste it into the terminal and press Enter. The login expires after 10 minutes, and the prompt says when.

Good output:

```
logged in to work
connections: cap 10, pool 8
```

The second line is the connection probe that runs after every login (see [Connection limits](connection-limits.md)). A failed probe prints `warning: connection probe failed: ...` on stderr and does not fail the login. Then check:

```sh
chainmail accounts test '<account>' --output table
```

Failures:

- `no redirect URL was pasted` (`USAGE`): you pressed Enter on an empty line. Run `auth login` again.
- `AUTH_REJECTED` for `invalid_client`: the hint says to set the client secret. The login stays open until it expires, so set the secret (see [Rotate an OAuth client secret](#rotate-an-oauth-client-secret)) and run `auth login` again.
- `AUTH_REJECTED` for anything else, such as a code already used: the login has ended. Run it again.
- `SERVER_UNAVAILABLE` (exit 7): Google's token endpoint could not be reached or answered `5xx`. Try again later.

## Rotate a password

Change the password at the provider first, then store the new one at once. Until you do, a new connection with the old password is rejected and the account is marked as needing a human.

```sh
chainmail secret set accounts/'<account>'
chainmail secret set accounts/'<account>' --from-pass '<pass-entry>'
```

The first form prompts without echo. The second reads the first line of a `pass` entry, where `'<pass-entry>'` is the entry's name, such as `mail/site`. `secret set` also reads the value from stdin when stdin is not a terminal. A secret is one line.

Good output:

```
stored secret accounts/site
connections: cap 5, pool 3
```

Setting a secret clears the auth-required state of every account that uses it, and the probe line shows the new credentials work. Confirm with `chainmail accounts test '<account>'`. `secret set` accepts only names the config uses: `accounts/<account>` for an account with `auth = "password"`, and `oauth_clients/<client>` for a defined client. Anything else, including `tokens/<account>`, fails with `INVALID_ARGUMENT` (exit 2).

## Rotate an OAuth client secret

Create the new secret in the Google Cloud console, then:

```sh
chainmail secret set oauth_clients/'<client>'
chainmail accounts test '<account>' --output table
```

The refresh tokens stay valid when only the client secret changes. A new OAuth client is a config change (a new `client_id`), and every account on it then needs `chainmail auth login`.

## Gmail app passwords

A `gmail` account can use an app password instead of OAuth2. Turn on 2-Step Verification for the Google account and create an app password at myaccount.google.com/apppasswords. Then set the account to password auth. On NixOS:

```nix
services.chainmail.settings.accounts.personal = {
  provider = "gmail";
  address = "me@example.com";
  auth = "password";
};
```

Remove `oauth_client` from the account, since it is not allowed with `password`. Deploy, which restarts the daemon, then store the app password:

```sh
chainmail secret set accounts/'<account>'
chainmail accounts test '<account>' --output table
```

`google-workspace` accounts accept OAuth2 only.
