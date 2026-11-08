#!/usr/bin/env bash
# Tests for build-pages.sh --skip-build: the site lands at the root, the
# docs under docs/, and a collision between the two fails.
set -euo pipefail
here=$(cd "$(dirname "$0")" && pwd)
tmp=$(mktemp -d); trap 'rm -rf "$tmp"' EXIT
fail() { echo "FAIL: $*" >&2; exit 1; }

mkdir -p "$tmp/site/dist" "$tmp/website/build/guide"
echo home > "$tmp/site/dist/index.html"; echo intro > "$tmp/website/build/index.html"; echo g > "$tmp/website/build/guide/index.html"
PAGES_ROOT="$tmp" "$here/build-pages.sh" --skip-build >/dev/null || fail "assembly failed"
grep -q home "$tmp/pages-dist/index.html" || fail "site not at root"
grep -q intro "$tmp/pages-dist/docs/index.html" || fail "docs not under docs/"
test -f "$tmp/pages-dist/docs/guide/index.html" || fail "docs subpages missing"

mkdir -p "$tmp/site/dist/docs"; echo clash > "$tmp/site/dist/docs/index.html"
if PAGES_ROOT="$tmp" "$here/build-pages.sh" --skip-build >/dev/null 2>&1; then fail "a site/dist/docs collision passed"; fi
echo "ok"
