#!/usr/bin/env bash
# Minimal agent wrapper around crq's JSON/exit-code contract.
set -euo pipefail

REPO="${REPO:?set REPO=owner/name}"
PR="${PR:?set PR=<number>}"
OUT="${OUT:-crq-feedback.json}"

set +e
crq loop "$REPO" "$PR" > "$OUT"
rc=$?
set -e

case "$rc" in
  0)
    echo "converged or no actionable findings; see $OUT"
    ;;
  10)
    echo "actionable findings written to $OUT"
    echo "fix valid findings, validate, commit, push, then resolve addressed threads:"
    echo "  jq -r '.findings[] | select(.thread_id != null) | .thread_id' '$OUT'"
    echo "  crq resolve '$REPO' '$PR' --thread THREAD_ID"
    ;;
  2)
    echo "timed out waiting for feedback; do not push a stale-feedback round; see $OUT"
    ;;
  *) echo "crq loop failed with exit $rc"; exit "$rc" ;;
esac
