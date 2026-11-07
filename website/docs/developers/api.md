---
title: The daemon API
description: The ConnectRPC services the daemon serves on its Unix socket, the peer check, and how to call them from curl or another Go program.
sidebar_position: 4
---

# The daemon API

The daemon serves its API with [ConnectRPC](https://connectrpc.com/) on a Unix socket. The CLI and `chainmail mcp` are clients of this API and have no other way in. Any program running as a user the peer check allows can call it the same way.

The API is defined in `proto/chainmail/v1/*.proto`, in the Protobuf package `chainmail.v1`. chainmail has not reached 1.0, so the API can change between releases, and the [changelog](../reference/changelog.md) records each change.

## The socket

| Daemon | Socket | Who may connect |
|---|---|---|
| Production | `[daemon] socket` in the config, default `/run/chainmail/chainmail.sock` | the daemon's own user, and members of `[daemon] client_group`, default `chainmail-clients` |
| Dev (`--dev`) | `--socket`, then `CHAINMAIL_SOCKET`, then `$XDG_RUNTIME_DIR/chainmail-dev.sock` | the daemon's own user only |

The daemon creates the socket with mode `0660` and, in production, sets its group to the client group. It replaces a stale socket a crashed daemon left behind, and refuses to start when another daemon is listening on it.

The server speaks HTTP/1.1 and HTTP/2 without TLS (h2c) on the socket, and ignores the host in the URL. Each RPC is at the path `/chainmail.v1.<Service>/<Method>`. connect-go serves the Connect, gRPC and gRPC-Web protocols, with Protobuf or JSON bodies.

## The peer check

Every connection passes a peer check before any handler runs. The daemon reads the connecting process's credentials from the kernel: `SO_PEERCRED` on Linux and `LOCAL_PEERCRED` on macOS. Other systems have no peer credentials, so the daemon refuses every request there.

A request is allowed when the peer's uid is the daemon's own uid, or when the peer is in the client group, by its primary group or a supplementary one. Group membership is looked up on every request, so adding a user to the group takes effect without a restart. A dev daemon has no client group. Any other request gets HTTP 403 with the body `forbidden`.

The [security model](../concepts/security-model.md) describes why the daemon trusts the kernel's answer and nothing the client says.

## Request headers

| Header | Meaning |
|---|---|
| `Chainmail-Client` | The kind of client, such as `cli`, `mcp` or `live-test`, recorded in the audit log. It must match `^[a-z0-9][a-z0-9._-]{0,31}$`. A request without it is recorded as `unknown`, and one with an invalid value as `(invalid)`. |

The client kind is self-reported. The audit log's authoritative identity is the peer's uid, which the client cannot choose. Pick a kind of your own for a new client, so its rows are easy to find with `chainmail audit`.

## Limits

- A request is at most 1 MiB, except on `ComposeService`, where requests and responses may reach 40 MiB to carry a whole message or one part of an upload. The account's own message size limit still applies.
- Responses of `MessageService`, `OrganizeService`, `WatchService`, `CacheService` and `ItemsService` are at most 16 MiB.
- The server sets no overall request timeout, because `accounts test` can take a minute and a `Subscribe` stream stays open as long as its watcher. Each handler bounds its own work.

## Errors

Every error is a Connect error with a code, such as `not_found`, and one `chainmail.v1.ErrorDetail`:

```protobuf
message ErrorDetail {
  // Stable machine-readable reason, such as AUTH_REQUIRED or NOT_FOUND.
  string reason = 1;
  // What a human or agent should do next, such as the command to run.
  string hint = 2;
  // Set when reason is HELD.
  string approval_id = 3;
}
```

Act on `reason`, never on the message text. The reasons are defined in `internal/api` and never change meaning or get reused. [Exit codes and errors](../reference/exit-codes.md) lists every reason, and the CLI exit code each one maps to.

## Services

| Service | File | RPCs |
|---|---|---|
| `AccountService` | `account.proto` | `ListAccounts`, `TestAccount`, `ProbeLimits` |
| `AdminService` | `admin.proto` | `SetSecret`, `BeginAuthLogin`, `FinishAuthLogin`, `GetStatus`, `ReadAudit` |
| `MessageService` | `message.proto` | `ListFolders`, `Search`, `Get`, `GetRaw`, `GetThread`, `DownloadAttachment` |
| `ComposeService` | `compose.proto` | `Send`, `SendRaw`, `Reply`, `Forward`, `CreateDraft`, `UpdateDraft`, `SendDraft`, `DeleteDraft`, `UploadAttachment` |
| `OrganizeService` | `organize.proto` | `Move`, `Copy`, `Archive`, `Trash`, `Expunge`, `SetFlags`, `SetLabels`, `CreateFolder`, `RenameFolder`, `DeleteFolder` |
| `WatchService` | `watch.proto` | `Subscribe`, `ReadEvents`, `Ack`, `GetWatchStatus`, `ListGroups`, `DeleteGroup` |
| `CacheService` | `cache.proto` | `GetCacheStatus`, `RebuildCache`, `EvictCache` |
| `ItemsService` | `items.proto` | `CreateItem`, `UpdateItem`, `DeleteItem`, `RestoreItem`, `GetItem`, `ListItems`, `EditCriteria`, `TickCriterion`, `SetBlocker`, `LinkThread`, `UnlinkThread` |

`errors.proto` holds `ErrorDetail` and no service.

### AccountService

- `ListAccounts` returns every configured account and its auth state.
- `TestAccount` logs in to the account's IMAP and SMTP servers and reports what happened.
- `ProbeLimits` opens connections one at a time until the server refuses one or 16 are open, then sets the pool size from the count.

### AdminService

- `SetSecret` stores a secret in the vault. Secrets can be written but never read back.
- `BeginAuthLogin` starts the OAuth2 consent flow for an account.
- `FinishAuthLogin` completes the flow with the URL the browser landed on.
- `GetStatus` reports the daemon's version, uptime, stored secret names, the sync state of each synced folder and the cache state of each account.
- `ReadAudit` returns audit rows, newest first, paged by `before_id`.

### MessageService

- `ListFolders` lists the folders of every account, or the ones named.
- `Search` finds messages on one account, several or all, newest first.
- `Get` reads up to 50 messages.
- `GetRaw` streams the original bytes of one message.
- `GetThread` reads every message in the thread of a ref, oldest first.
- `DownloadAttachment` streams one attachment, decoded.

### ComposeService

- `Send` sends a new message, builds it as a dry run, or saves it as a draft.
- `SendRaw` sends a complete message as given.
- `Reply` replies to a message, quoting it.
- `Forward` forwards a message, with its attachments unless asked not to.
- `CreateDraft` saves a new message to Drafts.
- `UpdateDraft` replaces a draft with a new version.
- `SendDraft` sends a draft and removes it from Drafts.
- `DeleteDraft` removes a draft: it moves it to Trash, or expunges it when `policy.allow_expunge` is set.
- `UploadAttachment` is a client stream that stores a file for a later send to attach or show inline.

Every call that sends needs an `idempotency_key`. `Send`, `Reply` and `Forward` need none for a dry run or a draft.

### OrganizeService

- `Move` moves messages to a folder.
- `Copy` copies messages to a folder.
- `Archive` moves messages out of the inbox to the account's archive.
- `Trash` moves messages to Trash.
- `Expunge` deletes messages for good. The policy refuses it unless `allow_expunge` is on.
- `SetFlags` adds and removes IMAP flags.
- `SetLabels` adds and removes Gmail labels.
- `CreateFolder` creates a folder.
- `RenameFolder` renames a folder.
- `DeleteFolder` deletes a folder. The policy refuses it unless `allow_folder_delete` is on.

### WatchService

- `Subscribe` streams events, with a heartbeat every 30 seconds.
- `ReadEvents` returns one page, waiting up to `wait_seconds` for the first event.
- `Ack` raises a group's acknowledged event.
- `GetWatchStatus` reports each synced folder.
- `ListGroups` lists the watch groups with their acknowledged position and lag.
- `DeleteGroup` deletes a watch group.

### CacheService

- `GetCacheStatus` reports the cache of every account, or of those named.
- `RebuildCache` drops the cached rows of an account, or one of its folders, and backfills them again.
- `EvictCache` runs a body eviction sweep now.

### ItemsService

- `CreateItem` adds an item with its acceptance criteria, idempotent with `idempotency_key`.
- `UpdateItem` changes the fields `update_mask` names. Changing `parent_id` moves the item.
- `DeleteItem` marks an item deleted, and with `cascade` every live item below it.
- `RestoreItem` brings back a deleted item, and with `cascade` the items deleted with it.
- `GetItem` returns an item with its criteria, ancestors, children, blockers and linked threads.
- `ListItems` returns compact rows in tree order, or in revision order with `changed_since`.
- `EditCriteria` removes, edits, reorders and adds an item's acceptance criteria as one change.
- `TickCriterion` ticks a criterion with evidence, or unticks it.
- `SetBlocker` records, or removes, that one item is blocked by another.
- `LinkThread` links the mail thread of a message to an item, or updates an existing link's anchor and note.
- `UnlinkThread` removes a thread's link from an item.

The comments in the `.proto` files document every field, its limits and its audit action. Read them before you call an RPC for the first time.

## Untrusted text

The API carries text that came from a message in the `chainmail.v1.Untrusted` message instead of a plain string. That covers subjects, names, addresses, bodies, filenames and header values. Item text that a person or an agent wrote, such as titles and notes, is carried the same way. A client that shows it to a model must keep it marked as data. [Untrusted content](../concepts/untrusted-content.md) explains how the CLI and the MCP server do that.

## The generated code

`buf generate` writes the Go code into `gen/chainmail/v1` (the message types) and `gen/chainmail/v1/chainmailv1connect` (the clients and handlers). [Building](./building.md) describes the generation and the check that keeps `gen/` current. `gen/` is outside `internal/`, so another Go module can import it.

## Calling the API with curl

The Connect protocol accepts JSON, so curl can call a unary RPC over the socket. Responses name fields in lower camel case, as the Protobuf JSON mapping does.

```sh
curl --unix-socket /run/chainmail/chainmail.sock -H 'Content-Type: application/json' -H 'Chainmail-Client: curl' -d '{}' http://chainmail.local/chainmail.v1.AdminService/GetStatus
```

A dev daemon answers like this, before its account is authenticated:

```json
{"version":"dev", "startedAt":"2026-11-07T10:06:27.169574098Z", "devMode":true, "sync":[{"account":"lab", "folder":"INBOX", "mode":"idle", "state":"auth_required", "lastError":"AUTH_REQUIRED"}], "cache":[{"account":"lab", "state":"disabled", "budgetBytes":"1073741824"}]}
```

An error comes back with its detail, decoded under `debug`:

```sh
curl --unix-socket /run/chainmail/chainmail.sock -H 'Content-Type: application/json' -d '{"account":"missing"}' http://chainmail.local/chainmail.v1.AccountService/TestAccount
```

```json
{"code":"not_found","message":"not found","details":[{"type":"chainmail.v1.ErrorDetail","value":"CglOT1RfRk9VTkQSHHJ1bjogY2hhaW5tYWlsIGFjY291bnRzIGxpc3Q","debug":{"reason":"NOT_FOUND","hint":"run: chainmail accounts list"}}]}
```

Streaming RPCs, such as `Subscribe` and `GetRaw`, use Connect's enveloped stream format, which curl cannot read as plain JSON. Use a Connect client for those.

## Calling the API from Go

`internal/client` is not importable from another module, but the generated clients are. Add the module:

```sh
go get github.com/yaklabco/chainmail@latest
```

This program prints the daemon's version and each account's auth state. It dials the socket for every request, speaks HTTP/2 without TLS as the CLI does, and names itself `my-tool` in the audit log:

```go
// Command status prints the chainmail daemon's version and each account.
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"

	"connectrpc.com/connect"

	chainmailv1 "github.com/yaklabco/chainmail/gen/chainmail/v1"
	"github.com/yaklabco/chainmail/gen/chainmail/v1/chainmailv1connect"
)

func main() {
	socket := os.Getenv("CHAINMAIL_SOCKET")
	if socket == "" {
		socket = "/run/chainmail/chainmail.sock"
	}

	// Every request goes over the Unix socket, as HTTP/2 without TLS.
	protocols := new(http.Protocols)
	protocols.SetUnencryptedHTTP2(true)
	hc := &http.Client{Transport: &http.Transport{
		Protocols: protocols,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socket)
		},
	}}

	// The daemon ignores the host. The header names this client in the audit log.
	const baseURL = "http://chainmail.local"
	named := connect.WithInterceptors(connect.UnaryInterceptorFunc(
		func(next connect.UnaryFunc) connect.UnaryFunc {
			return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
				req.Header().Set("Chainmail-Client", "my-tool")
				return next(ctx, req)
			}
		}))
	admin := chainmailv1connect.NewAdminServiceClient(hc, baseURL, named)
	accounts := chainmailv1connect.NewAccountServiceClient(hc, baseURL, named)

	ctx := context.Background()
	status, err := admin.GetStatus(ctx, connect.NewRequest(&chainmailv1.GetStatusRequest{}))
	if err != nil {
		log.Fatal(describe(err))
	}
	fmt.Println("daemon", status.Msg.GetVersion())

	list, err := accounts.ListAccounts(ctx, connect.NewRequest(&chainmailv1.ListAccountsRequest{}))
	if err != nil {
		log.Fatal(describe(err))
	}
	for _, a := range list.Msg.GetAccounts() {
		fmt.Println(a.GetName(), a.GetProvider(), a.GetAuthState())
	}
}

// describe returns the reason and hint the daemon attaches to every error.
func describe(err error) string {
	var cerr *connect.Error
	if !errors.As(err, &cerr) {
		return err.Error()
	}
	for _, d := range cerr.Details() {
		v, verr := d.Value()
		if verr != nil {
			continue
		}
		if detail, ok := v.(*chainmailv1.ErrorDetail); ok {
			return detail.GetReason() + ": " + detail.GetHint()
		}
	}
	return cerr.Error()
}
```

Against the dev daemon above it prints:

```text
daemon dev
lab custom AUTH_STATE_REQUIRED
```

The interceptor sets the header on unary calls only. For a streaming call such as `Subscribe`, also implement `WrapStreamingClient` and set the header on the connection's request headers, as `internal/client` does.

The calls write audit rows like any other client's. `ListAccounts` writes one with action `accounts.list`, and `GetStatus` writes none.
