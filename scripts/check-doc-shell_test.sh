#!/usr/bin/env bash
# Tests scripts/check-doc-shell.sh against the fixtures in
# scripts/testdata/check-doc-shell. Run it from the repository root.
set -euo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")/.."
check=scripts/check-doc-shell.sh
data=scripts/testdata/check-doc-shell
failed=0

fail() {
  echo "FAIL: $*" >&2
  failed=1
}

# Every block in good.md parses, so the check passes and prints nothing.
if ! out=$("$check" "$data/good.md" 2>/dev/null); then
  fail "good.md: expected exit 0"
fi
if [ -n "$out" ]; then
  fail "good.md: expected no output, got:
$out"
fi

# mixed.md has one bad block per kind, and sh has one for each shell.
status=0
out=$("$check" "$data/mixed.md" 2>/dev/null) || status=$?
if [ "$status" -ne 1 ]; then
  fail "mixed.md: expected exit 1, got $status"
fi
if ! diff -u "$data/mixed.expected" <(printf '%s\n' "$out") >&2; then
  fail "mixed.md: output differs from mixed.expected"
fi

# No files is a usage error, as is a file that does not exist.
status=0
"$check" >/dev/null 2>&1 || status=$?
if [ "$status" -ne 2 ]; then
  fail "no arguments: expected exit 2, got $status"
fi
status=0
"$check" "$data/missing.md" >/dev/null 2>&1 || status=$?
if [ "$status" -ne 2 ]; then
  fail "missing file: expected exit 2, got $status"
fi

if [ "$failed" -ne 0 ]; then
  exit 1
fi
echo "check-doc-shell: all tests passed"
