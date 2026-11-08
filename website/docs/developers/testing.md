---
title: Testing
description: The unit tests, the Dovecot integration suite and the live tests against real accounts, with their safety rules.
sidebar_position: 3
---

# Testing

chainmail has four test suites. They differ in what they need and in what they can touch.

| Suite | Target | Needs | Touches real mail | Runs in CI |
|---|---|---|---|---|
| Unit and component | `test:unit` | nothing | no | yes, on Linux and macOS |
| Stavefile | `test:stavefile` | nothing | no | yes, in `check` |
| Integration | `test:integration` | Docker | no | yes, on Linux |
| Live | `test:live` | a running daemon with real accounts | yes | never |

Write the test first. A change to behaviour comes with a test that failed before the change and passes after it. Tests use the standard `testing` package, and most call `t.Parallel()`.

## Unit tests

```sh
nix develop -c go tool stave test:unit
```

The target runs `go test -race -p <n> ./...`, where `<n>` is a quarter of the machine's cores. It needs no network, no Docker and no accounts. Mail servers are replaced with in-process fakes from `internal/testutil`:

- `fakegmail`, an IMAP server that speaks enough of Gmail's dialect for imapstore's tests and is meant to be no more lenient than Gmail.
- `imaptest`, a plain in-memory IMAP server.
- `smtptest`, an SMTP submission server that captures mail and can inject faults at `MAIL`, `RCPT`, `DATA` and around the final dot.
- `oauthtest`, a fake OAuth2 token endpoint.
- `daemontest`, an in-process daemon on a temporary socket, usually over `internal/mailstore/fake`.

`internal/mailstore/storetest` is a conformance suite that every `mailstore.Store` runs, so the fake store and imapstore cannot drift apart.

To run one package or one test while you work:

```sh
nix develop -c go test -race ./internal/query/
nix develop -c go test -race -run TestCompleteRefs ./internal/cli/
```

### Golden files

Four packages compare output with files under their `testdata/`: `internal/cli`, `internal/compose`, `internal/render` and `internal/mcpserver`, whose `tools_list.golden` holds the MCP tool list. After a change that is meant to alter the output, rewrite the files with `-update` and read the diff before you commit it:

```sh
nix develop -c go test ./internal/mcpserver/ -run TestToolsListGolden -update
```

### Fuzz tests

`FuzzParse` in `internal/query`, `FuzzBuild` in `internal/compose` and `FuzzCompile` in `internal/cache` run their seed corpus as ordinary tests in every unit run. To fuzz one for a short, bounded time:

```sh
nix develop -c go test -run '^$' -fuzz FuzzParse -fuzztime 60s ./internal/query/
```

Long fuzzing runs load the machine heavily. On a shared machine, agree on them first.

### The stavefile's own tests

`stavefile.go` has the `stave` build tag, so `test:unit` never compiles it. `test:stavefile` runs `stavefile_test.go`, which covers the checks `release` makes before it tags.

```sh
nix develop -c go tool stave test:stavefile
```

## Parallelism

The test targets cap `go test -p` at a quarter of the cores, and `test:integration` caps `-parallel` the same way. When you run `go test` yourself on a shared machine, pass `-p` too. Run one heavy suite at a time: a full unit run, the integration suite and a live run each use real CPU, and the integration suite also starts containers.

CI runs the unit tests on Linux inside `check`, and on macOS with `go test -short -race -p 2 ./...`. So a change that only works on Linux fails the pull request.

## Build tags

| Tag | Files | Run by |
|---|---|---|
| `integration` | tests against Dovecot in Docker | `test:integration` |
| `live` | tests against real accounts | `test:live` |
| `stave` | `stavefile.go` and its tests | stave itself and `test:stavefile` |

golangci-lint builds with all three tags, so `lint` checks these files even though `test:unit` never compiles them.

## The integration suite

The integration tests run the IMAP store, the daemon and the MCP server against a real Dovecot in Docker. They change no real mail.

```sh
docker info --format '{{.ServerVersion}}'
nix develop -c go tool stave test:integration
```

The target runs `go test -race -tags integration -count=1 -timeout 15m` over `./internal/mailstore/imapstore/`, `./internal/daemon/` and `./internal/mcpserver/`. Good output is `ok` for each of the three packages.

### What it covers

- **The IMAP store.** Its package runs again against Dovecot and against the fake Gmail server, including a Dovecot whose personal namespace is under `INBOX.`.
- **Held sends.** The daemon sends through the policy and checks that a held send lands in Dovecot's Drafts folder with its Bcc and reaches no SMTP server.
- **Watching.** The daemon watches a Dovecot mailbox over IDLE with CONDSTORE. Another IMAP client delivers, flags and expunges a message, and a group stream gets the `new`, `flags` and `expunged` events. Then the daemon stops, three messages arrive while it is down, and after a restart on the same state the group resumes with exactly those three.
- **The cache.** A mailbox is seeded across the inbox, Sent, Archive and Trash. After the backfill and the body prefetch, the cache and the server must return the same rows for a fixed set of queries, page for page, with a cursor carried from one to the other. The tests check that text outside the body window and Trash go to the server, and that `has:`, `filename:` and `label:` are answered by the cache. A `get` of a cached message must make no read of the mail server. The cache must follow a delivery, a flag and an expunge within seconds. `cache rebuild` and `cache evict` must work and be audited. A renumbered folder must be rebuilt, with an old ref found again by its Message-ID. Deleting `cache.db` while the daemon is stopped must rebuild it while a watch group loses and repeats nothing.
- **Items.** With the cache off, a link resolves on the mail server by the thread's root Message-ID and still finds its anchor after the message is archived. The archived message keeps its arrival time, so it raises no `item_mail` event. A reply filed in both the inbox and Archive raises one `item_mail` event. A stream with the item filter sees the item's own events and its `item_mail` event, and no `new`, `flags` or `expunged` events. A reply that arrives while the daemon is stopped raises its `item_mail` event after the restart, with `from_self` set for the account's own mail. The item audit rows name the items and hold no titles or notes.
- **Rules.** On Dovecot and the fake Gmail server, an `on_new` rule acts on a delivered message within seconds. On Google it labels the message, links its thread to a task and archives it, and on Dovecot it links and archives. The audit log records each action under client kind `rule` with the rule and run IDs, and the archived copy is not acted on again. A forward to an address off the allowlist becomes a draft, counts as `held` and reaches no SMTP server. A daemon stopped while a forward waits for the SMTP reply, then restarted, sends each forward exactly once. A scheduled rule runs once on a fake clock, and once after five hours of downtime. Due events fire once each across a simulated midnight and across restarts.
- **The MCP server.** The tests build the binary and run it as `chainmail mcp` against a real daemon. One searches, reads, files and archives a message, saves a draft, has a send held, and checks that the audit log records every call with client kind `mcp`. One waits for mail with `read_events` and a group, and checks that each read is audited as `watch.read`. One searches and reads through the cache and calls `cache_status`. One creates a task with `create_item`, links a thread with `link_thread`, has another client deliver a reply, reads its `item_mail` event with `read_events`, and checks with `get_item` that the reply is the thread's latest message. One dry-runs an unsaved rule with `dry_run_rule`, creates it with `create_rule`, runs it with `run_rule` and a cap of 1, then dry-runs and runs it again, and checks that each action is audited under client kind `rule`. Every call must be audited with client kind `mcp`, and no row may hold item text.

### Containers

The tests start their own containers from the pinned image `dovecot/dovecot:2.3.21.1`. The IMAP store's package shares one container across its tests, started in `TestMain`. The daemon's and the MCP server's tests start one per test. Every container is removed when its tests end, pass or fail. A container left by a killed run stops itself after 20 minutes, and the next run removes it along with its temporary directory. To see any that are left:

```sh
docker ps -a --filter label=chainmail-test=dovecot
```

To run one integration test:

```sh
nix develop -c go test -race -tags integration -count=1 -run TestPolicyHeldSendLandsInDovecotDrafts ./internal/daemon/
```

The integration tests are the `Test` functions in the files with the `integration` build tag, such as `internal/daemon/policy_integration_test.go`. A run of one test in the IMAP store's package still starts that package's shared container first.

## Live tests

The live tests talk to real accounts through a running daemon. They build only with the `live` tag, so they never run in CI or in `go test ./...`. Run them from a chainmail checkout, by hand, when you mean to.

They take the daemon's socket from `CHAINMAIL_SOCKET` and have no flag for it, so pass it with `env` for the one command. Their audit rows have client kind `live-test`. A [dev daemon](../getting-started/quick-start.mdx) is the usual target.

### The read tests

Without `CHAINMAIL_LIVE_WRITES`, a run changes no mail:

- It checks every account's IMAP and SMTP login, and fails if any check fails.
- It fails if an account has no inbox or no mail.
- It lists folders, searches, and reads the newest message and its thread.
- It reads one unread message per account with `get`, `thread`, `get --raw` and, when the message has one, `attachment`, and fails if any of those marks it read. It prefers an unread message with an attachment. An account with no unread mail skips this check, so mark a message unread in another client first.
- It runs the cache comparison below.

The read tests never change a flag. The log holds counts and refs, never subjects, addresses or bodies. Such a run is safe against the production daemon:

```sh
env CHAINMAIL_SOCKET=/run/chainmail/chainmail.sock nix develop -c go tool stave test:live
```

`test:live` gives `go test` a 2 hour timeout in place of its default 10 minutes, because the write tests wait minutes at a time for Gmail's search.

### The cache comparison

The cache comparison waits up to 30 minutes for every account's cache to report `ready`. It then requires 10 header queries per account to return the same refs from the cache and from the server, a default search to be answered by the cache, and a second read of a message to come from the cache. An account whose cache is still not ready after the wait is skipped with the reason. It changes no mail. To run it alone:

```sh
env CHAINMAIL_SOCKET=/run/chainmail/chainmail.sock nix develop -c go test -tags live -count=1 -timeout 60m -v -run TestCacheMatchesServer ./internal/live/
```

### The write tests

The write tests send real mail and probe connection limits. They skip unless `CHAINMAIL_LIVE_WRITES=1` is set too. Each account sends only to its own address, which the policy always allows, so no `send_allow` entry is needed.

- **Sends.** For each account they send a message, check that sending it again with the same idempotency key sends nothing, reply to it, forward it, and create, update, send and delete drafts.
- **Organizing.** Another message goes through a `chainmail-test-<random>` folder, a label on Google: copy, flag, unflag, labels on Google, move, archive, trash and expunge. Then the folder is deleted. Archive is skipped on an account with no archive folder. After an archive, the test checks that the message left the test folder for a folder other than the inbox or Trash, since `archive_folder` can name any folder.
- **Watching.** A watch test opens a live stream of each account's `new` events, as `chainmail watch --account <account> --kind new` does, sends a message to the account's own address, and requires the event for the delivered copy within 30 seconds. On Google that event is for the one All Mail message and must carry the `\Sent` label.
- **Items.** An items test creates an inbox task for each account, sends a message to the account's own address, links its thread to the task, and replies from the account. It requires exactly one `item_mail` event for the reply within 30 seconds, and `item show` must list the thread with both messages and its anchor found. Then it soft-deletes the task, so each run leaves one deleted task per account in `state.db`.
- **Rules.** A rules test sends a message to the first Google account's own address and creates a manual-only rule that labels it with a fresh label. It dry-runs the rule, runs it with a cap of 1, checks the label and the audit row, runs it again to see the message skipped, and deletes the rule. Cleanup expunges the message and deletes the label.
- **Connection limits.** Last, each account's connection limit is probed.

### Safety rules of the write tests

- Every message they make has a random token in its subject, and they touch no other message.
- They clean up after themselves even when a test fails. Cleanup waits for the delivered copy of every send, for up to 8 minutes after the last send. It then expunges every message with the token, in the inbox, Sent, Drafts and every other folder, and deletes the test folder.
- Cleanup expunges both the messages the test knows by ref and those a search for the token finds, so a message Gmail's search does not list yet is still removed. It reads each known ref first and expunges only a message whose subject holds the token.
- If the folder create fails or its answer is lost, cleanup lists the account's folders and deletes the one with the generated name, with or without the namespace prefix, and no other.
- A send whose delivered copy never shows fails the cleanup, because it may still arrive afterwards. A failed cleanup names the token, so you can find what is left with `chainmail search subject:<token> in:anywhere`.
- The waits, searches and sends stop early enough to keep 28 minutes of the 2 hour timeout for cleanup. When only that much is left, a test fails as out of time instead of sending or searching more, and its cleanups still run.

### Delivery on Google and other servers

Each step waits for the delivered copy of what it sent, which is the copy in the inbox except on Google. Gmail files a message an account sends to its own address once, in Sent, and adds no inbox copy. So on Google the one All Mail message with the token counts as delivered, and the reply, forward and draft steps run against it.

The test finds the delivered copy by searching. Gmail's search can trail a change by several minutes, so each step waits up to 6 minutes for it, searching less often as the wait goes on. Where the test already knows a message's ref, as after copy, move, archive and trash, it reads that ref instead of searching.

On Hostinger and other servers that keep folders under a personal namespace prefix, the test folder is created as `INBOX.chainmail-test-<random>`, and the test uses the name the server gave it.

A full run against four accounts has taken about 17 minutes, and longer when Gmail is slow.

### Running the write tests

The write tests expunge messages and delete their test folder, so the daemon they run against needs both destructive actions turned on:

```toml
[policy]
allow_expunge = true
allow_folder_delete = true
```

Do not turn those on in production for this. Use a dev daemon with a config made for the run, and set both back to `false` afterwards. With either off, the expunge and folder steps and the cleanup fail with `POLICY_DENIED`.

```sh
env CHAINMAIL_SOCKET="$XDG_RUNTIME_DIR/chainmail-dev.sock" CHAINMAIL_LIVE_WRITES=1 nix develop -c go tool stave test:live
```

The rules the write tests share, such as when a send counts as delivered and how the folder cleanup finds its folder, need no daemon. Their unit tests in `internal/live` have no build tag and run in every `test:unit`.
