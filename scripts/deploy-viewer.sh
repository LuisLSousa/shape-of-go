#!/usr/bin/env bash
# Deploys the built galaxy viewer (web/dist, ~79 MB with data assets).
# This is the author's deploy tooling, kept in the repo so the hosting
# setup is reproducible; running it from a clone deploys to your own
# Cloudflare account / fork, not to the URLs below.
#
# Targets:
#
#   ./scripts/deploy-viewer.sh cloudflare
#       Direct upload to the "shape-of-go" Cloudflare Pages project
#       (https://shape-of-go.pages.dev). Run `npx wrangler login` once
#       first. Unmetered bandwidth.
#
#   ./scripts/deploy-viewer.sh github
#       Pushes web/dist to an orphan gh-pages branch, served at
#       https://luislsousa.github.io/shape-of-go/ (enable in Settings,
#       Pages, deploy from branch). Needs a public repo on the free
#       plan. GitHub applies a soft 100 GB/month bandwidth guideline;
#       the cold load is ~18.6 MB, so send heavy traffic to Cloudflare.
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
