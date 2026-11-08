#!/usr/bin/env bash
# check-pages-links.sh <dir>: every internal href and src in the HTML under
# <dir> must resolve to a file in <dir>. Root-relative URLs resolve against
# <dir>, other relative URLs against the page that holds them. A path
# resolves when the file exists, when it is a directory holding index.html,
# or when <path>.html exists. Anchors, external URLs, mailto:, tel:, data:
# and javascript: are skipped. A missing root, or a tree with no HTML, fails.
set -euo pipefail

root=${1:?usage: check-pages-links.sh <dir>}
if [[ ! -d "$root" ]]; then
  echo "check-pages-links.sh: $root is not a directory" >&2
  exit 2
fi
root=$(cd "$root" && pwd)

# normalize <path>: resolves . and .. segments in an absolute path without
# touching the file system, so it works on Linux and macOS alike.
normalize() {
  local IFS=/ part
  local -a out=()
  for part in $1; do
    case "$part" in
      '' | .) ;;
      ..) ((${#out[@]})) && unset 'out[${#out[@]}-1]' ;;
      *) out+=("$part") ;;
    esac
  done
  printf '/%s' "${out[@]}"
  ((${#out[@]})) || printf '/'
}

pages=0
missing=0
declare -A seen=()
while IFS= read -r -d '' page; do
  pages=$((pages + 1))
  dir=$(dirname "$page")
  urls=$(grep -oE "(href|src)=(\"[^\"]*\"|'[^']*')" "$page" || true)
  while IFS= read -r attr; do
    [[ -z "$attr" ]] && continue
    url=${attr#*=}
    url=${url:1:${#url}-2}
    case "$url" in
      '' | '#'* | http:* | https:* | //* | mailto:* | tel:* | data:* | javascript:*) continue ;;
    esac
    path=${url%%#*}
    path=${path%%\?*}
    [[ -z "$path" ]] && continue
    if [[ "$path" == /* ]]; then
      target="$root$path"
    else
      target=$(normalize "$dir/$path")
    fi
    [[ -n "${seen[$target]+x}" ]] && continue
    seen[$target]=1
    if [[ -f "$target" || -f "${target%/}/index.html" || -f "${target%/}.html" ]]; then
      continue
    fi
    echo "missing: $url (in ${page#"$root"/})" >&2
    missing=1
  done <<<"$urls"
done < <(find "$root" -name '*.html' -print0)

if ((pages == 0)); then
  echo "check-pages-links.sh: no HTML pages under $root" >&2
  exit 2
fi
exit "$missing"
