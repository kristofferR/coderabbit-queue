#!/usr/bin/env bash
# Legacy monitor wrapper. The watcher now lives in `crq loop`.
set -euo pipefail

PR="${1:?usage: monitor.sh <PR> [owner/repo]}"
REPO="${2:-$(gh repo view --json nameWithOwner -q .nameWithOwner 2>/dev/null)}"
: "${REPO:?usage: monitor.sh <PR> [owner/repo]}"

crq loop "$REPO" "$PR"
