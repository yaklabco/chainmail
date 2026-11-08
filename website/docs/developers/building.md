---
title: Building
description: The Nix dev shell, the stave targets, code generation, the flake's package and the website.
sidebar_position: 2
---

# Building

chainmail builds with plain Go. The Nix dev shell pins the tools around it, and [stave](https://github.com/yaklabco/stave) runs the build steps from `stavefile.go`. CI runs the same targets, so a local `check` runs what the pull request's first CI job runs.

## The dev shell

`flake.nix` defines a dev shell with every tool the build needs: Go, buf, golangci-lint, gotestsum, age, goreleaser, Bun and Node.js 22. Run commands inside it with `nix develop -c`, or open a shell with `nix develop`:

```sh
nix develop -c go version
nix develop -c golangci-lint --version
```

Without Nix, install Go at the version in `go.mod` (`go 1.26.10` today), [buf](https://buf.build/docs/cli/installation/), and golangci-lint v2.15.0, which is the version CI pins. Bun is needed only for the website, and goreleaser only for release snapshots.

stave, `protoc-gen-go` and `protoc-gen-connect-go` are Go tool dependencies in `go.mod`, so `go tool` runs them at the pinned version with nothing to install.

## stave targets

Run a target with `go tool stave <target>`. With no target, stave runs `check`. `go tool stave -l` lists the targets with the first sentence of each doc comment.

| Target | What it does |
|---|---|
| `check` | Runs everything CI runs: generated code check, lint, changelog validation, tests and build. It runs `generateCheck`, `lint`, `changelog`, `test:unit`, `test:stavefile` and `build` in that order. The default target. |
| `build` | Compiles `bin/chainmail` without cgo, with `-trimpath`, and sets the version from `git describe --tags --always --dirty`. |
| `generate` | Regenerates Protobuf and Connect code into `gen/`. |
| `generateCheck` | Regenerates `gen/` and the command reference, then fails if either differs from what is committed or has files that are not committed. |
| `lint` | Runs `buf lint` and `golangci-lint run ./...`. |
| `changelog` | Validates `CHANGELOG.md` against the Keep a Changelog format. |
| `test:unit` | Runs unit and component tests with the race detector. |
| `test:stavefile` | Runs the tests of the stavefile's own logic, such as the checks `release` makes before it tags. The `stave` build tag keeps them out of `test:unit`. |
| `test:integration` | Runs the integration suite: Dovecot in a Docker container and the fake Gmail server, plus the daemon's held-send test against Dovecot and the MCP server run as `chainmail mcp` against Dovecot. |
| `test:live` | Runs the smoke test against real accounts through the daemon at `$CHAINMAIL_SOCKET`. |
| `docs:commands` | Writes the command reference pages from the CLI's help. |
| `docs:build` | Builds the manual into `website/build`, for serving under `/docs/`. |
| `docs:serve` | Runs the manual locally with live reload at http://localhost:3000/docs/. |
| `site:build` | Builds the landing page into `site/dist`. |
| `site:serve` | Runs the landing page locally with live reload at http://localhost:4321/. |
| `pages` | Builds the landing page and the manual into `pages-dist`, as CI deploys them. |
| `release` | Tags the next version and publishes it with goreleaser: the GitHub release and the Homebrew Cask. |
| `snapshot` | Builds the release archives and the Cask into `dist/` as `release` would, without tagging, pushing or publishing anything. |

`test:unit` and `test:integration` cap `go test -p`, and `test:integration` also caps `-parallel`, at a quarter of the machine's cores. [Testing](./testing.md) covers the test targets, and [Releasing](./releasing.md) covers `release` and `snapshot`.

## Build the binary

```sh
nix develop -c go tool stave build
./bin/chainmail version
```

A plain `go build` works too. Its binary reports version `dev`, since only `build`, the Nix package and the release set the version with `-ldflags`:

```sh
nix develop -c go build -o bin/chainmail ./cmd/chainmail
```

CI cross-builds every package for `darwin/arm64`, `darwin/amd64`, `linux/amd64` and `linux/arm64` with cgo off, so code that builds on only one of them fails the pull request. To check one target yourself:

```sh
nix develop -c env GOOS=darwin GOARCH=arm64 CGO_ENABLED=0 go build ./...
```

## Code generation

The API is defined in `proto/chainmail/v1/*.proto`. `buf.gen.yaml` runs two plugins through `go tool`, `protoc-gen-go` and `protoc-gen-connect-go`, and writes their output to `gen/` with source-relative paths. `clean: true` empties `gen/` first, so a deleted proto file leaves no stale Go file behind. Managed mode sets the Go package prefix to `github.com/yaklabco/chainmail/gen`.

```sh
nix develop -c go tool stave generate
```

`gen/` is committed. After you change a `.proto` file, run `generate` and commit the result with the change. `generateCheck`, which `check` and CI run, fails when the committed `gen/` differs from a fresh generation. `buf lint` checks the proto files against buf's `STANDARD` rules. `buf.yaml` also names the `FILE` breaking-change rules, but no target runs `buf breaking` today.

The [API page](./api.md) describes the services that come out of it.

## The command reference

The pages under [Commands](../reference/commands/chainmail.md) are generated from the CLI's help by a hidden `chainmail docs <dir>` command:

```sh
nix develop -c go tool stave docs:commands
```

The pages are committed. `generateCheck` regenerates them, so a change to a command's help that is not followed by `docs:commands` fails `check`.

## The Nix package

`nix build` builds the flake's default package with `buildGoModule`:

```sh
nix build
./result/bin/chainmail version
```

The package is built for `x86_64-linux`, `aarch64-linux` and `aarch64-darwin`. It builds only `cmd/chainmail`, with cgo off and the version set to the commit's short revision, or `dirty` for a tree with changes. When the build platform can run the binary, `result/share` also holds the bash, zsh and fish completions and the man page. A cross build skips them.

The flake also exports `nixosModules.default`, the module described in [Deploy](../operations/deploy.mdx), and on `x86_64-linux` two checks of it, `nixos-module` and `nixos-module-assertions`.

### Updating vendorHash

`buildGoModule` downloads the Go modules in a fixed-output derivation, and `vendorHash` in `flake.nix` is the hash of that download. Any change to `go.mod` or `go.sum` changes it, and the old hash then fails the build. The CI `nix` job builds the package on every pull request, so a stale hash fails there and not for someone installing from the flake.

After a dependency change, build the package:

```sh
nix build .#default --no-link
```

If the hash is stale, Nix stops with a hash mismatch and prints the hash it expected and the hash it got. Copy the hash it got into `vendorHash` in `flake.nix`, build again, and commit `flake.nix` with the `go.mod` and `go.sum` change.

`go.mod` replaces `github.com/emersion/go-imap/v2` with a fork. The vendored download includes it, so changing the replace also changes `vendorHash`.

## The website

The site at https://chainmail.sh has two parts. The landing page is an Astro project in `site/`. The manual is a Docusaurus project in `website/`, served under `/docs/`, with its pages as Markdown under `website/docs/`. Bun from the dev shell installs and runs both.

`scripts/build-pages.sh` builds both and assembles them into `pages-dist/`: the landing page at the root and the manual under `docs/`. CI deploys `pages-dist/` to Cloudflare Pages.

To work on the landing page with live reload, at http://localhost:4321/:

```sh
nix develop -c go tool stave site:serve
```

To work on the manual with live reload, at http://localhost:3000/docs/:

```sh
nix develop -c go tool stave docs:serve
```

To build both as CI does, into `pages-dist/`:

```sh
nix develop -c go tool stave pages
```

Each runs `bun install --frozen-lockfile` first. The manual's build copies the root `CHANGELOG.md` into `website/docs/reference/changelog.md`, which is not committed, and fails on any broken link between pages. CI also runs `scripts/check-pages-links.sh pages-dist`, which fails on any link in the assembled site that points to no file. [Contributing](./contributing.md) has the rules for writing pages.
