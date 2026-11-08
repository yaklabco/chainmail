---
title: CLI settings and completion
description: Point the CLI at a daemon socket, choose JSON or table output, and install shell completion.
sidebar_position: 8
---

# CLI settings and completion

Two settings apply to every client command:

- `socket`, the daemon socket.
- `output`, `json` or `table`.

## Where each setting comes from

Each setting is read from the first of these that sets it:

| Order | `socket` | `output` |
|---|---|---|
| 1. The flag | `--socket` | `--output` |
| 2. The environment | `CHAINMAIL_SOCKET` | `CHAINMAIL_OUTPUT` |
| 3. The file | `socket` in `cli.toml` | `output` in `cli.toml` |
| 4. The default | `/run/chainmail/chainmail.sock` | `table` when stdout is a terminal, `json` otherwise |

When nothing sets the output, a command prints a table or plain text on a terminal and JSON otherwise. So an agent piping the output gets JSON without asking for it.

```sh
chainmail folders --socket /run/user/1000/chainmail-dev.sock --output json
```

A dev daemon puts its socket at `$XDG_RUNTIME_DIR/chainmail-dev.sock` by default, but clients never read `XDG_RUNTIME_DIR`. Point them at it with `--socket`, `CHAINMAIL_SOCKET` or `socket` in `cli.toml`. The [quick start](../getting-started/quick-start.mdx) sets this up.

## cli.toml

The file is `$XDG_CONFIG_HOME/chainmail/cli.toml` when `XDG_CONFIG_HOME` is an absolute path, else `$HOME/.config/chainmail/cli.toml`.

```toml
socket = "/run/user/1000/chainmail-dev.sock"
output = "table"
```

`socket` takes a literal path. Neither `$XDG_RUNTIME_DIR` nor `~` is expanded there, so write the full path, such as a dev daemon's socket at `/run/user/1000/chainmail-dev.sock`.

A missing file holds no settings. The file is a usage error (exit 2) naming it when it:

- cannot be read,
- is not a regular file, such as a directory or a FIFO,
- is larger than 64 KiB,
- does not parse,
- holds a key other than `socket` and `output`, or
- holds a value that is not a string.

An `output` other than `json` or `table` is a usage error too, naming where it came from, such as `--output must be json or table, got "yaml" from CHAINMAIL_OUTPUT`.

## Which commands read the settings

Every client command, `chainmail mcp` included, checks the settings before it runs. `help`, `completion`, `man` and `daemon` do not, so a bad `cli.toml` or `CHAINMAIL_OUTPUT` never stops them.

The daemon never reads `cli.toml` and ignores `CHAINMAIL_OUTPUT`. Its own config file is separate (see [configuration](../reference/configuration.md)). The daemon's flags `--config`, `--state-dir`, `--vault-key-file` and `--dev` can also come from `CHAINMAIL_CONFIG`, `CHAINMAIL_STATE_DIR`, `CHAINMAIL_VAULT_KEY_FILE` and `CHAINMAIL_DEV`, but no environment variable changes a setting inside the config file.

## Shell completion

`chainmail completion <shell>` prints a completion script for bash, zsh, fish or PowerShell. Every command, subcommand, argument and flag completes, with a description where one helps.

The Homebrew Cask and the Nix package install the scripts for bash, zsh and fish, and the man page `chainmail(1)`. A Nix cross build cannot run the binary it builds, so it installs neither. With a binary from a release archive or `go build`, install the script yourself.

For bash, add this line to `~/.bashrc`:

```bash
source <(chainmail completion bash)
```

For zsh, write the script to a directory on `$fpath`:

```zsh
chainmail completion zsh > "${fpath[1]}/_chainmail"
```

For fish:

```fish
mkdir -p ~/.config/fish/completions
chainmail completion fish > ~/.config/fish/completions/chainmail.fish
```

For PowerShell:

```powershell
chainmail completion powershell | Out-String | Invoke-Expression
```

`chainmail man` prints the man page in roff. It needs no daemon.

### What completes

Some candidates complete locally, without the daemon:

- search operators, such as `from:` and `in:`,
- the entries of the password store for `secret set --from-pass` (`$PASSWORD_STORE_DIR` or `~/.password-store`),
- item types, states, areas, priorities and the fields `--clear` takes,
- rule action kinds, `--every` durations, run triggers and run outcomes, and
- the client kinds `audit --client` takes.

Others come from the daemon, through the same socket, policy and audit as any other command: accounts, folders, Gmail labels, message refs, attachment parts, watch groups, item IDs, criterion IDs, linked threads, rule names, secret names and audit actions.

- Message refs list 20 messages, each with its date, sender and subject. Inbox mail comes first, newest first, then the newest mail from any folder, so a ref still completes when every inbox is empty. `archive` offers inbox mail only, and `draft send`, `update` and `delete` offer drafts only. Type the start of a ref, such as `cm1:work:`, to narrow them to one account.
- Item IDs list up to 500 items, each with its type and title as the description. `item restore` offers only deleted items.
- Rule names list the live rules, each described by its ID and description. `rule restore` offers only deleted rules, by ID, each described by its name. After `--action link_item=`, the IDs of live items complete.
- `item:` in a query completes `none` and the IDs of live items.
- Criterion IDs, and the accounts and thread keys `item unlink` takes, come from the item's view, as `item show` reads it.

### When nothing completes

Each lookup gives up after 1.5 seconds. A daemon that is down or refuses you gets no candidates rather than an error.

So does a `cli.toml` or `CHAINMAIL_OUTPUT` that is not valid. The candidates that come from the daemon are left out, while search operators, file names and password-store entries still complete.

Set `BASH_COMP_DEBUG_FILE` to a path to log why a lookup offered nothing:

```sh
env BASH_COMP_DEBUG_FILE=/tmp/chainmail-complete.log chainmail __complete search in:
```

See the [completion command reference](../reference/commands/chainmail-completion.md).
