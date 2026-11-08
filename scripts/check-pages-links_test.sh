#!/usr/bin/env bash
# Tests for check-pages-links.sh: a link to a missing file fails, links to
# files, directories with index.html, extensionless .html files, anchors and
# external URLs pass.
set -euo pipefail
here=$(cd "$(dirname "$0")" && pwd)
tmp=$(mktemp -d); trap 'rm -rf "$tmp"' EXIT
fail() { echo "FAIL: $*" >&2; exit 1; }

mkdir -p "$tmp/ok/docs/guide" "$tmp/ok/img"
printf '<a href="/docs/">d</a><a href="/docs/guide">g</a><a href="/about">a</a><img src="/img/x.png"><a href="#top">t</a><a href="https://example.org/x">e</a><a href="mailto:a@b.example">m</a>' > "$tmp/ok/index.html"
: > "$tmp/ok/docs/index.html"; : > "$tmp/ok/docs/guide/index.html"; : > "$tmp/ok/about.html"; : > "$tmp/ok/img/x.png"
"$here/check-pages-links.sh" "$tmp/ok" >/dev/null || fail "a valid tree was rejected"

mkdir -p "$tmp/bad"
printf '<a href="/docs/missing/">x</a><link href="/kit/gone.css" rel="stylesheet">' > "$tmp/bad/index.html"
if out=$("$here/check-pages-links.sh" "$tmp/bad" 2>&1); then fail "a broken tree passed"; fi
grep -q '/docs/missing/' <<<"$out" || fail "missing page not reported"
grep -q '/kit/gone.css' <<<"$out" || fail "missing asset not reported"
echo "ok"
