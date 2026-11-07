---
title: Policy and sends
description: Find and release held sends, settle a send whose outcome is unknown, and handle refused expunges and folder deletes.
sidebar_position: 9
---

[The send policy](../concepts/policy.md) explains the allowlist, held sends and the destructive defaults. [Sending](../guide/sending.md) covers idempotency keys in full. This page is what to do when the policy acts.

## A held send

A send to anyone off the allowlist, to an address that is not a plain `local@domain`, or to more than `policy.max_recipients` recipients is held. It was not sent. The daemon saved the exact message to the account's Drafts folder, marked `\Draft` and `\Seen`, with Bcc recipients kept in a `Bcc` header. The command exited 4 with `HELD`. On a terminal it printed `held by policy, not sent: saved to Drafts <ref>`, then the blocked recipients.

Find recent holds and their drafts:

```sh
chainmail audit --since 24h --action send --output table
chainmail audit --since 24h --action draft.send --output table
chainmail search in:drafts newer_than:1d --account '<account>' --output table
```

A held row has `DECISION` `held`, `RESULT` `ok`, `REASON` `HELD`, and as targets the draft's ref, its Message-ID, `blocked:<n>` and `too_many` when there were too many recipients. Rows never hold the addresses. Check `reply`, `forward` and `send_raw` the same way. Read the draft with `chainmail get '<ref>'`.

What to do, as the person who owns the account:

- Send it, edit it or delete it from your own mail client. That is what the hold is for.
- Delete it with `chainmail draft delete '<ref>'`. While `allow_expunge` is off, the draft moves to Trash.
- If the recipient should always be allowed, add it to `policy.send_allow`, or to `accounts.<name>.policy.send_allow` for one account, deploy, then send the draft:

```sh
chainmail draft send '<ref>' --idempotency-key '<new-key>'
```

A new key matters when a `draft send` of the same ref was held before, since a repeat with the same key replays the hold. A key is 1 to 128 characters of letters, digits and `.`, `_`, `:` and `-`.

Do not get the message out another way, such as from another account or in smaller batches. An agent that hits a hold must stop and tell the person who owns the account.

If saving the draft failed too, the command fails with `held by policy, but saving the draft failed; nothing was sent` (or `making the draft`). Nothing was sent or saved, and running the same command again tries again.

## Send outcome unknown (exit 8)

`SEND_OUTCOME_UNKNOWN` means the connection dropped after the whole message was handed to the SMTP server and before it answered. The message may or may not have gone out. The daemon has already looked for it in Sent three times over 10 seconds.

```sh
sudo journalctl -u chainmail -o cat --since today --no-pager | grep 'send outcome unknown'
chainmail search 'in:sent newer_than:1d' --account '<account>' --output table
```

The log line has the `account` and the `message_id`. Look in Sent for the message by subject or recipients, and compare its Message-ID with `chainmail get '<ref>' --output json`.

- Found: it was sent. Do nothing.
- Not found after a few minutes: send it again on purpose with a new key, such as `--idempotency-key '<new-key>'`. Running the same command again does not send. It reports the same unknown outcome, because the key is settled as unknown.

The same applies to the sends the warning `sends left in progress by an earlier daemon now have an unknown outcome` counts after a crash.

## Idempotency keys

Every send carries a key, which the CLI and `chainmail mcp` derive from the message when you give none. The daemon keeps each key per account for 7 days. A second send with the same key and request replays the first outcome: `duplicate: already sent` on a terminal, `duplicate` in the audit. A first send that failed safely, such as `SERVER_UNAVAILABLE` or a credential failure, can be retried with the same key. To send the same message twice on purpose, give it a new key. [Sending](../guide/sending.md) has the rules.

## Expunge and folder delete refused

`expunge` fails with `POLICY_DENIED` (exit 3), the message `expunge is disabled by policy` and the hint `set policy.allow_expunge in the chainmail NixOS config`. `folder delete` fails the same way with `folder delete is disabled by policy` and `policy.allow_folder_delete`. Both are off by default. The refusal is audited with decision `denied`.

```sh
chainmail accounts list --output json | jq '.accounts[] | {name, capabilities}'
```

This is the policy working. Only turn a setting on when someone really means to let clients destroy mail. On NixOS:

```nix
services.chainmail.settings.policy.allow_expunge = true;
```

On other hosts, set `allow_expunge = true` under `[policy]` in the config and restart. Deploy, do the work, then set it back to `false` and deploy again. With `allow_folder_delete` on, `folder delete --force` deletes the mail in a folder even while `allow_expunge` is off. While `allow_expunge` is on, removing a draft deletes it for good instead of moving it to Trash.
