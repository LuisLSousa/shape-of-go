#!/usr/bin/env bash
# Deploys the built galaxy viewer (web/dist, ~79 MB with data assets).
#
# Two targets, pick with the first argument:
#
#   ./scripts/deploy-viewer.sh cloudflare
#       Direct upload to Cloudflare Pages (project "shape-of-go" →
#       https://shape-of-go.pages.dev). Needs a one-time
#       `npx wrangler login` first. No repo, no DNS; unmetered
#       bandwidth — this is the host that survives a front-page day.
#
#   ./scripts/deploy-viewer.sh github
#       Publishes web/dist to an orphan gh-pages branch and pushes it.
#       GitHub Pages (Settings → Pages → deploy from gh-pages branch)
#       then serves it at https://luislsousa.github.io/shape-of-go/.
#       NOTE: on the free plan this only works once the repo is public.
#       Soft bandwidth guideline is 100 GB/month (~5,400 cold visitors
#       at the measured 18.6 MB cold load) — fine for steady state, not
#       for a front-page spike; keep Cloudflare as the pressure valve.
#
# The build is reproduced from scratch each time; data assets come from
# web/public/data (exported by cmd/export, gitignored on main).
set -euo pipefail
cd "$(dirname "$0")/.."

target="${1:?usage: deploy-viewer.sh cloudflare|github}"

(cd web && npm run build)
touch web/dist/.nojekyll

case "$target" in
cloudflare)
  (cd web && npx wrangler pages deploy dist --project-name shape-of-go --branch main)
  ;;
github)
  tmp=$(mktemp -d)
  git worktree add --detach "$tmp"
  (
    cd "$tmp"
    git checkout --orphan gh-pages
    git rm -rf --quiet . || true
    cp -R "$OLDPWD"/web/dist/. .
    git add -A
    git commit -m "deploy viewer $(date +%Y-%m-%d)"
    git push -f origin gh-pages
  )
  git worktree remove --force "$tmp"
  ;;
*)
  echo "unknown target: $target (want cloudflare or github)" >&2
  exit 1
  ;;
esac
