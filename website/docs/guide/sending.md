---
title: Sending
description: Compose and send mail, reply and forward, keep drafts, try a dry run, and send safely more than once.
sidebar_position: 2
---

# Sending

Five commands send: `send`, `send-raw`, `reply`, `forward` and `draft send`. Each sends from one account, with that account's address as From. Every send is audited.

A send to recipients the policy allows goes out as soon as the daemon has built it. A send to anyone else is held. It is saved to Drafts, not sent, and the command exits 4 (see [held sends](#held-sends)). `--dry-run` and `--draft` send nothing, so the policy does not check them.

## Compose a message

`send` takes the account, the recipients, a subject and a body.

```sh
chainmail send --account work --to ann@example.com --subject "Q3 numbers" --body "See the *attached* report." --attach q3.pdf
```

| Flag | Meaning |
|---|---|
| `--account` | The account to send from. Required. |
| `--to`, `--cc`, `--bcc` | A recipient, as `a@b` or `"Name <a@b>"`. Each repeats. |
| `--subject` | The subject line. Required. |
| `--body`, `--body-file`, `--text`, `--html-file` | The body. See [the body](#the-body). |
| `--attach` | Attach a file. Repeat it for more files. |
| `--inline` | Show an image inside the body, as `NAME=F`. Repeatable. |
| `--no-signature` | Leave out the account's signature. |
| `--draft` | Save the message to Drafts instead of sending it. |
| `--dry-run` | Write the message as MIME to stdout instead of sending it. |
| `--idempotency-key` | The key that stops a repeat from sending again. See [idempotency](#idempotency). |

Bcc recipients get the message but are never written into it. A draft is the exception. It keeps them in a Bcc header, which is removed when the draft is sent.

### The body

The body is Markdown unless you say otherwise. chainmail sends it as two parts: the Markdown source as the plain-text part, and an HTML part rendered from it, with tables, strikethrough, autolinks and task lists. Raw HTML inside the Markdown is not passed through.

The body comes from one of these:

- `--body TEXT`, the Markdown on the command line.
- `--body-file F`, Markdown read from a file.
- `--body -`, Markdown read from stdin.
- `--text` with `--body` or `--body-file`, to send the body as plain text only, with no HTML part. A plain-text body cannot show inline images.
- `--html-file F`, to send your own HTML. `--body` then gives its plain-text part. Without it, the text part is converted from the HTML. chainmail changes the HTML only to point inline images at their parts and to add the signature and any quote before `</body>`. `--html-file` cannot be combined with `--text`.

```sh
cat notes.md | chainmail send --account work --to ann@example.com --subject "Notes" --body -
chainmail send --account work --to ann@example.com --subject "Status" --body "All green." --text
chainmail send --account work --to ann@example.com --subject "Newsletter" --html-file issue.html --body "The text version."
```

### Attachments and inline images

`--attach F` attaches a file. The media type comes from the file's extension, or else from its content. File names that are not ASCII are encoded so every mail client shows them.

`--inline NAME=F` shows the image `F` inside the body wherever the body refers to `NAME`. In Markdown that is `![chart](chart.png)`, and in HTML `src="chart.png"`. Only images can go inline, and every `--inline` name must be used by the body. An unused one fails with `INVALID_ARGUMENT`.

```sh
chainmail send --account work --to ann@example.com --subject "Trend" --body "Here it is: ![chart](chart.png)" --inline chart.png=./build/chart.png
```

The CLI uploads attachments and inline images to the daemon before the send, several at once. An upload lasts an hour and is tied to the account it was made for.

### Size limits

The whole message, attachments included, must fit the account's size limit:

| Account | Limit |
|---|---|
| Google presets and custom servers | 25 MiB |
| iCloud and Hostinger | 35 MiB |
| Any account with `max_message_size` set | that value |

A message over the limit fails with `INVALID_ARGUMENT` before anything is sent. `max_message_size` is in the [configuration reference](../reference/configuration.md).

### Signature and headers

The account's `signature` from the config goes under the body. In the text part it follows a `-- ` line, and in the HTML part it is its own block. In a reply or forward it goes under the new text and above the quoted original. `--no-signature` leaves it out.

The message gets a new `Message-ID` on the account's domain, a `Date` in the daemon's time zone, and `User-Agent: chainmail/<version>`. A subject that is not ASCII is encoded for you.

The CLI has no flag for extra headers. The MCP send tools take `Reply-To` and `X-` headers (see [working with mail](../agents/working-with-mail.md)).

## Dry runs

`--dry-run` builds the message and writes it as MIME to stdout instead of sending it.

```sh
chainmail send --account work --to ann@example.com --subject "Q3" --body-file q3.md --dry-run > q3.eml
```

- It sends nothing, opens no SMTP connection and needs no idempotency key.
- It cannot be combined with `--draft`.
- `send` with `--dry-run` connects to no server at all.
- `reply` and `forward` take `--dry-run` too. They still read the original message over IMAP, because the reply quotes and threads it and the forward carries it.

Like `get --raw`, a dry run refuses to write to a terminal, so redirect it to a file. The exception is JSON output chosen by `--output json`, `CHAINMAIL_OUTPUT` or `cli.toml`. The JSON a command prints by default when stdout is not a terminal does not count.

With JSON output chosen, the CLI prints the message with the SMTP envelope, even to a file:

```json
{"result":{"dry_run":{"raw":"...","envelope_from":"...","envelope_to":["..."]}}}
```

There `raw` is the MIME text as an ordinary JSON string, escaped but not base64. So to save a dry run as an `.eml` file, unset `CHAINMAIL_OUTPUT` and leave `output` out of `cli.toml`, or pass `--output table`, which writes the raw MIME to the file.

The API's `DryRun.raw` field is protobuf `bytes`. A client that calls the API with JSON, such as Connect's JSON encoding, gets it base64-encoded, as the protobuf JSON mapping encodes every `bytes` field.

## Send a finished message

`send-raw` sends a complete RFC 5322 message as given, from `--file F` or from stdin with `-`.

```sh
chainmail send-raw --account work --file q3.eml
cat q3.eml | chainmail send-raw --account work -
```

- The message must parse and must have a `Message-ID`.
- Its From must be the account's address.
- It goes to the addresses in its To, Cc and Bcc. Its Bcc header is removed before sending.
- The size limit applies.

Without `--idempotency-key`, the key is derived from the account and the bytes.

## After a send

On a terminal a send prints the Message-ID and the ref of the copy in Sent. With JSON output it prints:

```text
{"result":{"sent":{"message_id":...,"sent_ref":...,"duplicate":...,"sent_copy_error":...}}}
```

Google files sent mail in Sent itself. The daemon looks for that copy once, so `sent_ref` is empty when Gmail has not filed it yet, and `sent_copy_error` stays empty too.

On every other account the daemon stores a copy in the Sent folder, marked read. If storing that copy fails, the message was still sent. `sent_copy_error` says why there is no copy, and the command still succeeds.

### Outcomes

A send is never retried, by the daemon or by the CLI. Each send ends in one of these ways:

| Outcome | Reason | Exit | What to do |
|---|---|---|---|
| The server accepted it | | 0 | Nothing. |
| The policy held it and saved it to Drafts | `HELD` | 4 | Nothing. Do not send it again another way. A person reviews the draft. See [held sends](#held-sends). |
| The server refused the sender, a recipient or the message | `INVALID_ARGUMENT` | 2 | Fix the request. Nothing was sent. A refused recipient means nobody got it. |
| A temporary server error, or the connection dropped before the whole message was handed over | `SERVER_UNAVAILABLE` | 7 | Run the same command again. Nothing was sent. |
| The server rejected the credentials | `AUTH_REJECTED` or `AUTH_REQUIRED` | 5 | Follow the hint, then run the same command again. |
| The connection dropped after the whole message was handed over, before the server answered | `SEND_OUTCOME_UNKNOWN` | 8 | Check Sent before sending again, as below. |

Every reason and exit code is in [exit codes](../reference/exit-codes.md).

### When the outcome is unknown

With `SEND_OUTCOME_UNKNOWN` the server may or may not have taken the message. The daemon has already looked for its Message-ID in the Sent folder three times over 10 seconds and did not find it. Look again yourself:

```sh
chainmail search in:sent newer_than:1d --account work
```

Running the same command again does not send it again. It reports the same unknown outcome. If the message really did not go out, send it again on purpose with a new key, as described under [idempotency](#idempotency).

## Replies

`reply` replies to a message, from the account the message is on.

```sh
chainmail reply cm1:work:INBOX:7:42 --body "Thanks, that works."
chainmail reply cm1:work:INBOX:7:42 --all --body-file answer.md --attach notes.pdf
```

Who it goes to:

- The original's Reply-To, or else its From.
- `--all` adds the original's To and Cc.
- chainmail drops the account's own address from the list, along with repeats.
- A reply to the account's own message goes to that message's To instead, and with `--all` to its Cc as well.
- `--to`, `--cc` and `--bcc` add more recipients.

A reply with nobody to send to fails with `INVALID_ARGUMENT`. A reply needs a body, and takes every body and attachment option of `send`.

The subject gets `Re: ` unless it already starts with it. The reply's `In-Reply-To` and `References` point at the original, so mail clients thread it. Under the new text and the signature comes an `On <date>, <name> wrote:` line and the whole original, quoted, including its own quotes. After the reply is sent, the original is marked `\Answered`.

## Forwards

`forward` forwards a message to new recipients. It needs at least one of `--to`, `--cc` and `--bcc`.

```sh
chainmail forward cm1:work:INBOX:7:42 --to ann@example.com --body "FYI"
chainmail forward cm1:work:INBOX:7:42 --to ann@example.com --no-attachments --draft
```

The subject gets `Fwd: ` unless it already starts with `Fwd:` or `Fw:`. The new text, which is optional here, goes above a `Forwarded message` block with the original's From, Date, Subject and To, then the original's body. The original's attachments go with it unless `--no-attachments`. After the forward is sent, the original is marked `$Forwarded`.

## Drafts

`--draft` on `send`, `reply` or `forward` saves the message to the account's Drafts folder instead of sending it, and prints the draft's ref. In JSON that is `{"result":{"draft":{...}}}`.

The `draft` commands work on drafts:

| Command | Does |
|---|---|
| `draft create` | Saves a new draft. It takes the options of `send` except `--draft`, `--dry-run` and `--idempotency-key`, and prints `{"draft":{"ref":...,"message_id":...}}`. |
| `draft update <ref>` | Replaces a draft with a new version. It takes the same options as `draft create` except `--account`, since the draft's account is used. |
| `draft send <ref>` | Sends a draft and removes it from Drafts. It prints what `send` prints. |
| `draft delete <ref>` | Deletes a draft. |

```sh
chainmail draft create --account work --to ann@example.com --subject "Q3" --body-file q3.md
chainmail draft update cm1:work:Drafts:3:9 --to ann@example.com --subject "Q3 final" --body-file q3.md
chainmail draft send cm1:work:Drafts:3:10
```

A pipe can pass the ref straight on:

```sh
chainmail draft create --account work --to ann@example.com --subject "Q3" --body-file q3.md --output json | jq -r .draft.ref | xargs chainmail draft send
```

What to know about each:

- `draft update` replaces the whole message, so give the recipients, subject and body again. The new version has a new ref, which it prints. The old ref is gone. A reply saved as a draft stays a reply.
- `draft send` keeps the draft's Message-ID and sets its Date to the time of sending. If removing the draft fails, the send still succeeds and the draft stays. Sending a reply draft does not mark the original `\Answered`. Without `--idempotency-key`, its key is derived from the draft's ref.
- `draft delete` moves the draft to the account's Trash while `policy.allow_expunge` is off, the default. With it on, the draft is deleted for good. `draft update` and `draft send` remove the old version and the sent draft the same way.

## Held sends

The policy checks `send`, `send-raw`, `reply`, `reply --all`, `forward` and `draft send`. A send to a recipient the allowlist does not allow, to an address that is not a plain `local@domain`, or to more than `max_recipients` recipients is held.

A held send is never transmitted. The daemon saves the exact message it built to the account's Drafts folder. The command prints the result on stdout, then the `HELD` error on stderr, and exits 4. On a terminal it prints `held by policy, not sent: saved to Drafts` and the draft's ref, then the blocked recipients and whether there were too many.

With JSON output the result is:

```json
{"result":{"held":{"draft":{"ref":"cm1:work:Drafts:1793457333:12","message_id":"01M69669KM.MJJH0XAXCV@example.org","hint":""},"blocked":["stranger@example.net"],"too_many_recipients":false,"hint":"held by policy: saved to Drafts for James to review and send"}}}
```

Stderr, when it is not a terminal, holds:

```json
{"error":{"reason":"HELD","message":"held by policy: saved to Drafts","hint":"held by policy: saved to Drafts for James to review and send"}}
```

`draft.ref` is the saved draft's ref. On a server that does not say where it stored the draft, `ref` is empty and `draft.hint` says to search Drafts for `message_id`. `blocked` lists the recipients that are not allowed, as given. `too_many_recipients` is true when there were more than `max_recipients`.

Do not try to get the message out another way, such as from another account or with fewer recipients per send. The owner of the mailbox sends, edits or deletes the draft from a mail client. Running the same command again with the same idempotency key replays the held result and saves no second draft.

[The send policy](../concepts/policy.md) covers the allowlist, every rule that holds a send, and what the draft holds.

## Idempotency

Every send carries an idempotency key, so a send that is repeated goes out once. That covers an agent retrying and a script run twice.

- The key is compulsory for the API's `Send`, `SendRaw`, `Reply`, `Forward` and `SendDraft` calls, except for a dry run or a draft.
- A key is 1 to 128 characters of `A-Z`, `a-z`, `0-9` and `.`, `_`, `:` and `-`.
- The daemon keeps each key, per account, for 7 days.

### Derived keys

You rarely pass a key yourself. Without `--idempotency-key`, the CLI derives the key from what is sent:

| Command | The key covers |
|---|---|
| `send` | the account, the recipients, the subject, the body and its kind (Markdown, text or HTML), `--no-signature`, and the name and bytes of every attachment and inline image |
| `send-raw` | the account and the message's bytes |
| `reply`, `forward` | the original's ref, the extra recipients, the body, signature and files as for `send`, and whether it is a reply, a reply to all, or a forward with or without attachments |
| `draft send` | the draft's ref |

The MCP tools derive their own keys. See [derived idempotency keys](../agents/working-with-mail.md#derived-idempotency-keys).

### What a repeat does

A second send with the same key and the same request sends nothing. It returns the first send's outcome:

- the first send's result with `duplicate` set, which the terminal output shows as `duplicate: already sent`, or
- the same error the first send failed with.

If the first send is still running, the second waits for it.

The exception is a first send that failed in a way that is safe to retry, such as `SERVER_UNAVAILABLE` or a credential failure, where nothing was sent. Then the same key makes a new attempt.

Reusing a key for a different message fails with `INVALID_ARGUMENT` while the first send is still running, or after it failed in that retryable way. Once the first send has an outcome (sent, refused or unknown), the daemon replays that outcome for the key before it reads the new request. So a different message under the same key is not detected. It gets the first outcome and is not sent.

### Sending again on purpose

To send the same message again within those 7 days, give it a new key:

```sh
chainmail send --account work --to ann@example.com --subject "Reminder" --body "Still on for Friday?" --idempotency-key reminder-2
```

Every flag is in the command reference: [send](../reference/commands/chainmail-send.md), [send-raw](../reference/commands/chainmail-send-raw.md), [reply](../reference/commands/chainmail-reply.md), [forward](../reference/commands/chainmail-forward.md) and [draft](../reference/commands/chainmail-draft.md).
