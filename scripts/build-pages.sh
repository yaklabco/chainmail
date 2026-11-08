#!/usr/bin/env bash
# build-pages.sh [--skip-build]: builds the landing page (site/) and the
# manual (website/) and assembles pages-dist/ for Cloudflare Pages: the
# site at the root and the manual under docs/. --skip-build assembles
# from existing site/dist and website/build.
set -euo pipefail
root=${PAGES_ROOT:-$(cd "$(dirname "$0")/.." && pwd)}
if [[ "${1:-}" != "--skip-build" ]]; then
  (cd "$root/site" && bun install --frozen-lockfile && bun run build)
  (cd "$root/website" && bun install --frozen-lockfile && bun run build)
fi
if [[ -e "$root/site/dist/docs" ]]; then
  echo "site/dist/docs exists: the site must not publish anything under /docs/" >&2
  exit 1
fi
out="$root/pages-dist"
rm -rf "$out"
mkdir -p "$out/docs"
cp -R "$root/site/dist/." "$out/"
cp -R "$root/website/build/." "$out/docs/"
echo "assembled $out"
