#!/usr/bin/env bash
# Legacy monitor wrapper. The watcher now lives in `crq loop`.
set -euo pipefail

PR="${1:?usage: monitor.sh <PR> [owner/repo]}"
# Don't put the gh fallback in a ${2:-...} default: under set -e a failing
# gh repo view would abort before the usage error below could print.
if [ "${2:-}" ]; then
  REPO="$2"
else
  REPO="$(gh repo view --json nameWithOwner -q .nameWithOwner 2>/dev/null || true)"
fi
: "${REPO:?usage: monitor.sh <PR> [owner/repo]}"

crq loop "$REPO" "$PR"
