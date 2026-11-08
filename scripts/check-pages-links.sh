#!/usr/bin/env bash
# check-pages-links.sh <dir>: every internal href and src in the HTML under
# <dir> must resolve to a file in <dir>. A path resolves when the file
# exists, when it is a directory holding index.html, or when <path>.html
# exists. Anchors, external URLs, mailto: and data: are skipped.
set -euo pipefail
root=${1:?usage: check-pages-links.sh <dir>}
missing=0
while IFS= read -r url; do
  path=${url%%#*}; path=${path%%\?*}
  [[ -z "$path" ]] && continue
  target="$root$path"
  if [[ -f "$target" || -f "${target%/}/index.html" || -f "${target%/}.html" ]]; then continue; fi
  echo "missing: $path" >&2
  missing=1
done < <(find "$root" -name '*.html' -print0 | xargs -0 grep -ohE '(href|src)="/[^"/][^"]*"|(href|src)="/"' | sed -E 's/^(href|src)="//; s/"$//' | sort -u)
exit "$missing"
