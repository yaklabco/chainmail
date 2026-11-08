---
title: Contributing
description: Branches, tests first, Conventional Commits, the changelog, the checks to run before a pull request, and review.
sidebar_position: 5
---

# Contributing

Changes reach `main` through pull requests on [yaklabco/chainmail](https://github.com/yaklabco/chainmail). This page lists what a pull request needs. [Building](./building.md) and [Testing](./testing.md) describe the tools.

Report bugs and propose features in [GitHub issues](https://github.com/yaklabco/chainmail/issues). For a large change, open an issue first, so the design can be agreed before the code is written.

## Branches

Start each change on a new branch from an up-to-date `main`. Name it in kebab-case, such as `fix-idle-reconnect` or `add-folder-sizes`, and do not put a `/` in the name.

```sh
git switch main
git pull --ff-only
git switch -c fix-idle-reconnect
```

## Tests first

Write the test before the code. A bug fix starts with a test that fails because of the bug. A feature starts with a test of the behaviour it adds. Then make the test pass without weakening it.

- Put tests next to the code, in the package's `_test.go` files, with the standard `testing` package.
- Use the fakes in `internal/testutil` and `internal/mailstore/fake` instead of the network.
- A test that needs a real IMAP server goes behind the `integration` build tag, and one that needs real accounts behind the `live` tag.
- When a change alters golden output on purpose, regenerate it with `-update` and review the diff.

## Code conventions

- golangci-lint runs with the linters and formatters in `.golangci.yml`, including gofumpt and goimports with `github.com/yaklabco/chainmail` as the local import prefix. `nix develop -c golangci-lint fmt` formats the tree.
- Every exported name has a doc comment, and every package has a package comment.
- Message text is untrusted. Never put a subject, address, body, filename or item text into an error message, a hint, a log line or an audit row. Errors carry a fixed reason from `internal/api` and fixed text.
- A reason or exit code in `internal/api`, once released, never changes meaning and is never reused.
- A schema change to `state.db` is a new numbered migration in `internal/db/migrations/state/`. Never edit one that has shipped. `cache.db` has no migrations: raise `cache.SchemaVersion` instead, and the daemon rebuilds the file.
- chainmail runs on any Linux and on macOS. Keep platform code behind file name suffixes or build tags, and let CI's cross-build and macOS jobs check it.

## Generated files

Some files are generated and committed. `generateCheck`, part of `check`, fails when they are out of date.

| You changed | Run | Commit |
|---|---|---|
| a `.proto` file | `nix develop -c go tool stave generate` | `gen/` |
| a command's help, flags or examples | `nix develop -c go tool stave docs:commands` | `website/docs/reference/commands/` |
| `go.mod` or `go.sum` | `nix build .#default --no-link`, then update `vendorHash` | `flake.nix` |

## Commits

Commit messages follow [Conventional Commits](https://www.conventionalcommits.org/): a type, an optional scope, and a short summary in the imperative.

```text
fix(imapstore): keep a watch connection's IDLE support after it drops
feat(cli): add folder sizes to chainmail folders
test(daemon): wait out an IMAP reconnect before comparing cache answers
docs: describe the event log retention
```

The scope is usually the package or area, such as `imapstore`, `daemon`, `cli`, `watch`, `release` or `ci`.

The types decide the next release's version. `release` reads the commits since the last tag through `.svu.yml`:

| Commits since the last tag | Next version |
|---|---|
| any `feat` | the minor number goes up |
| a breaking change, marked with `!` or a `BREAKING CHANGE:` footer | the minor number goes up while chainmail is before 1.0 |
| only `fix`, or other types | the patch number goes up |

## The changelog

`CHANGELOG.md` follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/). Add an entry for every change a user or operator would notice, under `## [Unreleased]` at the top, in the right subsection: `### Added`, `### Changed`, `### Deprecated`, `### Removed`, `### Fixed` or `### Security`. Write each entry as one sentence that says what changed for a user or an operator.

```markdown
## [Unreleased]

### Fixed

- A watch connection keeps using IDLE after the server drops it.
```

A change with no user-visible effect, such as a test or a refactor, needs no entry. `nix develop -c go tool stave changelog` validates the file, and `check` runs it. The release moves the `[Unreleased]` entries under the new version, as [Releasing](./releasing.md) describes.

## Documentation

A change to behaviour updates the docs in the same pull request. The pages are Markdown under `website/docs/`, and the build fails on a broken link between them. The landing page lives in `site/`, an Astro project.

- Link to another page with a relative file link, such as `../concepts/policy.md`, so the build checks it.
- Mark a shell block `sh` only when it runs unchanged in both bash and fish. A command that needs shell state, such as an exported variable, goes in an `.mdx` page as a pair of `bash` and `fish` tabs with `groupId="shell"`. `scripts/check-doc-shell.sh` checks each block with its shells, and CI runs it.
- Use example.com, example.org and example.net for addresses and hosts in examples.
- Write `<placeholder>` text in prose inside backticks, since a bare angle bracket in Markdown is HTML.
- Use Mermaid for diagrams.

`nix develop -c go tool stave docs:serve` shows the site with live reload.

## Before you open a pull request

Run the full local check once, at the end. It is what CI runs first:

```sh
nix develop -c go tool stave check
```

If the change touches `internal/mailstore/imapstore`, `internal/daemon` or `internal/mcpserver`, also run the integration suite, which needs Docker:

```sh
nix develop -c go tool stave test:integration
```

Then push the branch and open the pull request:

```sh
git push -u origin fix-idle-reconnect
gh pr create --repo yaklabco/chainmail --fill
```

## Review

CI runs these jobs on every pull request:

| Job | What it checks |
|---|---|
| `check` | `go tool stave check`, then a cross-build for macOS and Linux on arm64 and amd64 |
| `macos` | the unit tests on macOS |
| `integration` | `go tool stave test:integration` |
| `nix` | `nix build .#default`, which catches a stale `vendorHash` |
| `release-config` | `goreleaser check` and `go tool stave snapshot` |
| `docs` | the shell blocks in the docs, then the site build |

A maintainer reviews the pull request once CI passes. Answer each review comment with a change or a reply, and push follow-up commits to the same branch. Every check must pass before the pull request is merged.
