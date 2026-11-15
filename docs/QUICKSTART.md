# chainmail quick start

The full quick start is on the documentation site: https://chainmail.sh/docs/getting-started/quick-start

For installing chainmail, see https://chainmail.sh/docs/getting-started/install.

## Set up

Run `chainmail bootstrap` as yourself, not as root. It sets up a production daemon that runs as its own user, with one account, and asks for sudo once:

```sh
chainmail bootstrap you@example.com
```

Bootstrap finds the provider from the address, or takes it from `--provider`. It prints its plan, asks for the account's app password, and asks you to confirm. It creates the `chainmail` user (`_chainmail` on macOS) and the `chainmail-clients` group. It installs the binary, `/etc/chainmail/config.toml`, the vault key and the service. It starts the daemon, stores the password and tests the login. `--dry-run` shows the plan without changing anything.

When bootstrap makes a new vault key, it asks whether to back it up. The default is no backup. The key then stays on this machine, and only the daemon's user can read it. If the key is lost, nothing can read the stored passwords, but no mail is lost. [If the vault key is lost](#if-the-vault-key-is-lost) says how to recover. The other choice shows the key once on the terminal, for you to save in a password manager. Anything that can see the terminal at that moment sees the key too, including an agent watching the session. `--key-backup none` or `--key-backup show` answers the question in advance.

Log out and back in, or run `newgrp chainmail-clients`, so your shell joins the group. Then check the account:

```sh
chainmail accounts list
chainmail accounts test you
```

Run bootstrap again with another address to add an account.

An agent that can run sudo without a password can get around all of this. Run agents from an account that cannot.

## If the vault key is lost

Bootstrap does not make a new key while the old vault is still there, because a new key could never open it. The error says `the vault /var/lib/chainmail/vault.age exists but the vault key /etc/chainmail/vault-key does not`.

If you saved the key, put it back. Paste it into a file called `vault-key`, install that file as the daemon's key with mode 0400, and delete the copy. Then run bootstrap again to check the install and start the daemon:

```sh
sudo install -o chainmail -g chainmail -m 0400 vault-key /etc/chainmail/vault-key
rm vault-key
chainmail bootstrap you@example.com
```

On macOS, use `-o _chainmail -g chainmail-clients` instead.

Without a saved key, the stored passwords and logins cannot be recovered. Move the old vault aside, then run bootstrap again:

```sh
sudo mv /var/lib/chainmail/vault.age /var/lib/chainmail/vault.age.lost
chainmail bootstrap you@example.com
```

Bootstrap makes a new key and asks for that account's password again. Run it once more for each other account, with the same address and options as before, to enter its password again. For an OAuth account, bootstrap asks for the OAuth client secret again and logs in through the browser. Without a terminal it cannot log in, and it tells you to run `chainmail auth login <name>`.

## Developing chainmail

`chainmail daemon --dev` runs a daemon as your own user, for work on chainmail itself. It prints this warning when it starts, and `chainmail admin status` repeats it:

> dev mode: this daemon runs as you, so any program running as you, including agents, can read the vault key and every stored password. Use it only to develop chainmail, never with real accounts.

Use bootstrap for any real account.

## Calendars

These steps add calendars to a daemon that already reads mail, such as the one bootstrap set up. The config is `/etc/chainmail/config.toml`, which you edit with sudo. Restart the daemon after each config change, with `sudo systemctl restart chainmail` on Linux or `sudo launchctl kickstart -k system/sh.chainmail.daemon` on macOS.

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
