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
printf "<a href='../../about'>a</a><img src='../../img/x.png'><a href=\"sibling.html\">s</a>" > "$tmp/ok/docs/guide/page.html"; : > "$tmp/ok/docs/guide/sibling.html"
"$here/check-pages-links.sh" "$tmp/ok" >/dev/null || fail "a valid tree was rejected"

mkdir -p "$tmp/bad"
printf '<a href="/docs/missing/">x</a><link href="/kit/gone.css" rel="stylesheet">' > "$tmp/bad/index.html"
if out=$("$here/check-pages-links.sh" "$tmp/bad" 2>&1); then fail "a broken tree passed"; fi
grep -q '/docs/missing/' <<<"$out" || fail "missing page not reported"
grep -q '/kit/gone.css' <<<"$out" || fail "missing asset not reported"

mkdir -p "$tmp/rel/docs/guide"
cat > "$tmp/rel/docs/guide/index.html" <<'HTML'
<a href="missing.html">m</a><img src="../img/missing.png"><a href='/single-missing/'>s</a>
HTML
if out=$("$here/check-pages-links.sh" "$tmp/rel" 2>&1); then fail "broken relative and single-quoted links passed"; fi
grep -q 'missing.html' <<<"$out" || fail "missing relative page not reported"
grep -q 'img/missing.png' <<<"$out" || fail "missing relative asset not reported"
grep -q 'single-missing' <<<"$out" || fail "missing single-quoted link not reported"

if "$here/check-pages-links.sh" "$tmp/no-such-dir" >/dev/null 2>&1; then fail "a missing root passed"; fi
mkdir -p "$tmp/empty"
if "$here/check-pages-links.sh" "$tmp/empty" >/dev/null 2>&1; then fail "a tree with no pages passed"; fi
echo "ok"
