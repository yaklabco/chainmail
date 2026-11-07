#!/usr/bin/env bash
# check-doc-shell.sh checks the shell blocks in Markdown files parse.
#
# Usage: scripts/check-doc-shell.sh FILE...
#
# Each fenced block whose info string starts with sh, bash or fish is
# written to a temporary file and parsed without running it:
#
#   sh    with bash -n and fish --no-execute, since it must run in both
#   bash  with bash -n
#   fish  with fish --no-execute
#
# For each block a shell rejects it prints "FILE:LINE: SHELL rejects this
# block" on stdout, where LINE is the line of the opening fence, and the
# shell's own message on stderr. It exits 1 if any block was rejected and 2
# on a usage error.
set -euo pipefail

usage() {
  echo "usage: $0 FILE..." >&2
  exit 2
}

if [ "$#" -eq 0 ]; then
  usage
fi
for file in "$@"; do
  if [ ! -f "$file" ]; then
    echo "$0: $file: no such file" >&2
    exit 2
  fi
done
for shell in bash fish; do
  if ! command -v "$shell" >/dev/null 2>&1; then
    echo "$0: $shell is not installed" >&2
    exit 2
  fi
done

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

# extract writes each shell block of $1 to $tmp/N and prints one
# "N<TAB>LINE<TAB>LANG" row per block. A fence is three or more backticks or
# tildes, indented by any amount so blocks inside list items count. The block
# closes at a fence of the same character that is at least as long and has
# nothing after it. The opening fence's indentation is removed from the
# block's lines.
extract() {
  awk -v dir="$tmp" -v base="$2" '
    function fence_len(s, ch,    n) {
      n = 0
      while (substr(s, n + 1, 1) == ch) n++
      return n
    }
    {
      line = $0
      sub(/\r$/, "", line)
      match(line, /^[ \t]*/)
      indent = RLENGTH
      rest = substr(line, indent + 1)
      if (!open) {
        ch = substr(rest, 1, 1)
        if (ch != "`" && ch != "~") next
        n = fence_len(rest, ch)
        if (n < 3) next
        info = substr(rest, n + 1)
        if (ch == "`" && index(info, "`") > 0) next
        sub(/^[ \t]+/, "", info)
        split(info, words, /[ \t{]/)
        lang = words[1]
        open = 1
        fch = ch
        flen = n
        findent = indent
        keep = (lang == "sh" || lang == "bash" || lang == "fish")
        if (keep) {
          count++
          out = dir "/" base "-" count
          printf "" > out
          printf "%s\t%d\t%s\n", out, NR, lang
        }
        next
      }
      if (substr(rest, 1, 1) == fch && fence_len(rest, fch) >= flen) {
        tail = substr(rest, fence_len(rest, fch) + 1)
        if (tail ~ /^[ \t]*$/) {
          if (keep) close(out)
          open = 0
          next
        }
      }
      if (keep) {
        body = line
        n = 0
        while (n < findent && substr(body, 1, 1) ~ /[ \t]/) {
          body = substr(body, 2)
          n++
        }
        print body > out
      }
    }
  ' "$1"
}

# parses reports whether shell $1 accepts the script in file $2, sending the
# shell's message to stderr with each line indented.
parses() {
  local shell=$1 block=$2 status=0 msg
  case "$shell" in
    bash) msg=$(bash -n "$block" 2>&1) || status=$? ;;
    fish) msg=$(fish --no-execute "$block" 2>&1) || status=$? ;;
  esac
  if [ "$status" -ne 0 ] && [ -n "$msg" ]; then
    printf '%s\n' "$msg" | sed 's/^/    /' >&2
  fi
  return "$status"
}

failed=0
index=0
for file in "$@"; do
  index=$((index + 1))
  rows=$(extract "$file" "$index")
  [ -n "$rows" ] || continue
  while IFS=$'\t' read -r block line lang; do
    case "$lang" in
      sh) shells="bash fish" ;;
      *) shells=$lang ;;
    esac
    for shell in $shells; do
      if ! parses "$shell" "$block"; then
        echo "$file:$line: $shell rejects this block"
        failed=1
      fi
    done
  done <<<"$rows"
done

exit "$failed"
