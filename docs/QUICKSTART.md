# chainmail quick start

The quick start has moved to the documentation site: https://chainmail.sh/docs/getting-started/quick-start

For installing chainmail, see https://chainmail.sh/docs/getting-started/install.

## Calendars

These steps add calendars to a daemon that already reads mail. The commands assume a `cli.toml` that names the daemon's socket, as the quick start on chainmail.sh sets up. Restart the daemon after each config change.

### A Google account

A Google account that logs in with OAuth2 syncs its calendars with no config change. It now asks Google for three more scopes:

- `https://www.googleapis.com/auth/calendar.events`
- `https://www.googleapis.com/auth/calendar.calendarlist.readonly`
- `https://www.googleapis.com/auth/calendar.freebusy`

Add them to the OAuth consent screen of your client in the Google Cloud console. Then log in again, so the refresh token carries them:

```sh
chainmail auth login work
```

Until you do, mail keeps working and the account's calendar line in `chainmail cache status` reads `auth_required`. To keep a Google account off calendars, give it a calendar table with `store = "none"`.

### A CalDAV account

A CalDAV server needs a calendar table with `store = "caldav"` and the server's URL. chainmail finds the calendars through `/.well-known/caldav`. This Mailwright account logs in through a public Keycloak client, which needs no secret in the vault:

```toml
[oauth_clients.keycloak]
client_id = "chainmail"
issuer = "https://auth.example.org/realms/example"
scopes = ["openid", "offline_access"]

[accounts.mailwright]
provider = "custom"
address = "me@example.org"
username = "me"
auth = "oauth2"
oauth_client = "keycloak"
imap = { host = "mail.example.org", port = 993, security = "tls" }
smtp = { host = "mail.example.org", port = 465, security = "tls" }

[accounts.mailwright.calendar]
store = "caldav"
url = "https://mail.example.org/"
```

Run `chainmail auth login mailwright`, open the URL it prints, sign in, and paste the URL the browser lands on back into the command.

### Optional settings

The `[calendar]` table is optional. These are its defaults:

```toml
[calendar]
timezone = ""          # zone of all-day dates and new events, empty for the host's zone
poll = "5m"            # 1m to 24h
window_past = "30d"    # 1d to 3650d
window_future = "365d" # 1d to 3650d
```

An account's calendar table can also list calendar IDs or names in `include` and `exclude`, and name the calendar new events go to in `default`.

### First reads

Wait for the account's calendar line in `chainmail cache status` to read `ready`. Calendar reads before the first sync exit 7 with `CACHE_NOT_READY`. Then:

```sh
chainmail calendars
chainmail agenda --days 7
chainmail invites
chainmail rsvp <ref> accept
chainmail freebusy --days 2
chainmail event create --title "Planning" --start "2026-11-12 10:00" --duration 30m --attendee ann@example.org
```

Times without a zone are read in this machine's zone, or in the zone `--tz` names. An `rsvp` or `event create` that the send policy holds exits 4. A held answer by email is saved to Drafts, and a held new event is created as a draft that invites no one.
