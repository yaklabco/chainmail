---
title: Install
description: Install chainmail with Homebrew, from a release archive, with Nix, or from source.
sidebar_position: 1
---

# Install

chainmail is one Go binary, `chainmail`. The same binary runs the daemon, the CLI and the MCP server for agents. It runs on Linux and on macOS, on amd64 and arm64.

Installing chainmail starts nothing. The [quick start](./quick-start.mdx) runs a daemon, and [Deploy](../operations/deploy.mdx) sets one up for production.

## What you need

- One of Homebrew, Nix with flakes enabled, or Go 1.26.10 or later, the version `go.mod` names. A release archive needs none of them.
- `age-keygen`, from [age](https://age-encryption.org), to make the vault key. The Nix dev shell provides it. Otherwise install `age` from your package manager.
- `jq`, for the examples that pick one field out of JSON output.

## Homebrew

The Homebrew tap `yaklabco/tap` holds a Cask for the latest release. It works on macOS. Recent Homebrew releases also install casks on Linux, and the Cask has Linux builds. If your Homebrew on Linux refuses it, use a [release archive](#release-archives) or [Nix](#nix) instead.

```sh
brew install yaklabco/tap/chainmail
chainmail version
```

The Cask installs the `chainmail` binary, the `chainmail(1)` man page, and shell completion scripts, which it generates by running the binary. The release binaries are not signed or notarized, so on macOS the Cask removes the quarantine attribute from the binary after it installs it. After the install, Homebrew prints a caveat that points to the quick start.

## Release archives

Each [GitHub release](https://github.com/yaklabco/chainmail/releases) has one archive per platform and a `checksums.txt` file. The archives are named `chainmail_<version>_<os>_<arch>.tar.gz`, where `<version>` has no leading `v`, `<os>` is `darwin` or `linux`, and `<arch>` is `amd64` or `arm64`. Each archive holds the `chainmail` binary, `LICENSE`, `README.md` and `man/chainmail.1`, with no top-level directory. Shell completion scripts are not in the archive. The binary prints them (see [CLI settings and shell completion](../guide/cli-settings.md)).

These commands download version 0.1.0 for Linux on amd64 and check it against `checksums.txt`. Replace `0.1.0` and `linux_amd64` with the version and platform you want.

```sh
curl -fLO https://github.com/yaklabco/chainmail/releases/download/v0.1.0/chainmail_0.1.0_linux_amd64.tar.gz
curl -fLO https://github.com/yaklabco/chainmail/releases/download/v0.1.0/checksums.txt
shasum -a 256 --check --ignore-missing checksums.txt
```

`shasum` prints `chainmail_0.1.0_linux_amd64.tar.gz: OK` for a good download. `--ignore-missing` skips the lines of `checksums.txt` for the archives you did not download. On Linux without `shasum`, `sha256sum --check --ignore-missing checksums.txt` does the same check.

Unpack the archive into a directory of its own, then put the binary in a directory on your `PATH` and the man page in a `man1` directory that `man` searches. `~/.local/bin` and `~/.local/share/man/man1` are one common choice for a single user.

```sh
mkdir -p chainmail-0.1.0 ~/.local/bin ~/.local/share/man/man1
tar -xzf chainmail_0.1.0_linux_amd64.tar.gz -C chainmail-0.1.0
install -m 0755 chainmail-0.1.0/chainmail ~/.local/bin/chainmail
install -m 0644 chainmail-0.1.0/man/chainmail.1 ~/.local/share/man/man1/chainmail.1
chainmail version
```

The release binaries are not signed or notarized. If macOS refuses to run the binary because it cannot verify the developer, as it can for an archive downloaded with a browser, remove the quarantine attribute with `xattr -d com.apple.quarantine ~/.local/bin/chainmail`.

## Nix

The flake builds the package `packages.<system>.default` for `x86_64-linux`, `aarch64-linux` and `aarch64-darwin`. Other systems, such as an Intel Mac, need a release archive or a Go build.

To add chainmail to your Nix profile from GitHub:

```sh
nix profile add github:yaklabco/chainmail
chainmail version
```

Older Nix releases call this command `nix profile install`. A Nix build prints the short commit hash it was built from as its version, or `dirty` for a checkout with uncommitted changes. The package installs the completion scripts for bash, zsh and fish and the man page with the binary.

To build from a checkout instead:

```sh
git clone https://github.com/yaklabco/chainmail
cd chainmail
nix build
./result/bin/chainmail version
```

`result/bin` holds the binary, and `result/share` the completion scripts and the man page. `nix profile add .` adds the checkout's package to your profile.

The flake also has a NixOS module, `nixosModules.default`, which runs the daemon as a system service. [Deploy](../operations/deploy.mdx) describes it.

## From source

Building from source needs Go 1.26.10 or later. From a checkout:

```sh
git clone https://github.com/yaklabco/chainmail
cd chainmail
env CGO_ENABLED=0 go build -trimpath -o bin/chainmail ./cmd/chainmail
mkdir -p ~/.local/bin
cp bin/chainmail ~/.local/bin/chainmail
chainmail version
```

`CGO_ENABLED=0` matches the release build, which is pure Go. That matters on Linux, where a pure Go build reads users and groups from `/etc/passwd` and `/etc/group` (see [Security model](../concepts/security-model.md)). This build prints its version as `dev`.

`go tool stave build` builds the same way and writes `bin/chainmail`, with the version from `git describe` in place of `dev`. Inside the Nix dev shell that is `nix develop -c go tool stave build`. [Building](../developers/building.md) lists the other stave targets.

A Go build installs no completion scripts and no man page. `chainmail completion` prints the completion scripts (see [CLI settings and shell completion](../guide/cli-settings.md)).

## Next

The [quick start](./quick-start.mdx) starts a development daemon, adds an account and runs the first commands.
