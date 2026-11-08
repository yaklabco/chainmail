# chainmail operations runbook

The runbook has moved to the Operations section of the documentation site: https://chainmail.sh/docs/operations/health

Cutting a release is now at https://chainmail.sh/docs/developers/releasing.

## Calendars

### Calendar sync state

`chainmail cache status` prints a second table when any account has a calendar store. It has one line per account, with the calendar state, the last sync, the number of calendars and events, and the last error. The states are:

| State | Meaning |
|---|---|
| `ready` | The last sync finished. Reads answer from `calendar.db`. |
| `syncing` | No sync has finished since the daemon started. |
| `auth_required` | The calendar server refused the account's credentials. |
| `error` | The last sync failed for another reason, such as a server that did not answer. |

Calendar reads on an account before its first sync finishes exit 7 with `CACHE_NOT_READY`. After that, reads answer from `calendar.db` in every state, with what the last good sync stored. A failing account tries again after its poll interval, then doubles the wait up to 30 minutes. A daemon restart syncs every account at once.

### `auth_required`

An `auth_required` line affects only calendars. Mail on the account keeps working, and the daemon does not park the account. Calendar calls that reach the server, and reads before the first sync, exit 5 with `AUTH_REQUIRED` and the hint `run: chainmail auth login <account>`.

On a Google account, the usual cause is a refresh token from before calendars, which lacks the calendar scopes. Add the scopes `calendar.events`, `calendar.calendarlist.readonly` and `calendar.freebusy` to the client's consent screen in the Google Cloud console, then run `chainmail auth login <account>`. The syncer picks up the new token at its next try. Restart the daemon to sync at once. To keep a Google account off calendars instead, set `store = "none"` in its `[accounts.<name>.calendar]` table.

### Keycloak and other issuer clients

An `[oauth_clients.<name>]` table with an `issuer` takes its endpoints from `<issuer>/.well-known/openid-configuration`. The daemon fetches that document once per client and keeps it until it restarts. Such a client is public, so it needs no `oauth_clients/<name>` secret in the vault, and the token requests carry no client secret.

- The client must allow PKCE and the loopback redirect `http://127.0.0.1:53682/`.
- Give the client offline access, such as the `offline_access` scope in `scopes`. Without it there is no refresh token, and the login fails with a hint that says so.
- The access token must carry the audience the mail and CalDAV server expects. For Mailwright that is an audience mapper on the Keycloak client.

After a change to the client in Keycloak, run `chainmail auth login <account>` again.

### `tls_server_name`

An account's `imap` and `smtp` tables take `tls_server_name` when the host chainmail dials is not the name on the server's certificate. An internal load balancer in front of Mailwright is one case:

```toml
imap = { host = "mailwright-nlb.internal.example.org", port = 993, security = "tls", tls_server_name = "mail.example.org" }
smtp = { host = "mailwright-nlb.internal.example.org", port = 465, security = "tls", tls_server_name = "mail.example.org" }
```

chainmail sends the name in SNI and verifies the certificate against it. A certificate error on such an account usually means the key is missing or names the wrong host.

### `calendar.db`

`calendar.db` lives in the cache directory, `[cache] dir` or the state directory by default, next to `cache.db`. It needs no backup. The daemon replaces a file of another schema version, or one SQLite reports as corrupt, and refills it with a full sync.

To rebuild it by hand, stop the daemon, delete `calendar.db` with its `-wal` and `-shm` files, and start the daemon. One loss comes with that. `calendar.db` also keeps the invites the cache found in mail, and the cache reads invites only from new mail, so invites from mail that arrived before the rebuild are not listed again.

### Upgrading to calendars

The upgrade applies `state.db` migration 7, which adds the four calendar event kinds. An older binary refuses the database afterwards, so take a snapshot of `state.db` before you upgrade.

### The Mailwright CalDAV suite

`stave test:caldav` runs the CalDAV store tests against a Mailwright container. The image comes from `CHAINMAIL_MAILWRIGHT_IMAGE`, best pinned by digest. Without the variable, the container tests skip.

```sh
CHAINMAIL_MAILWRIGHT_IMAGE=<registry>/mailwright@sha256:<digest> stave test:caldav
```

The image is in ECR, which only incus can reach, so CI does not run this suite. The harness pulls the image when Docker does not have it, so Docker must be logged in to the registry.
