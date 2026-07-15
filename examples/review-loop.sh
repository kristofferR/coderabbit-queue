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
    echo "fix valid findings and validate locally"
    echo "resolve each addressed thread immediately after its local fix:"
    echo "  jq -r '.findings[] | select(.thread_id != null) | .thread_id' '$OUT'"
    echo "  crq resolve '$REPO' '$PR' --thread THREAD_ID"
    echo "if any .reviewed_by value is false: HOLD THE HEAD; do not commit or push"
    echo "after every required bot is true: fix/resolve the rest, then commit/push once"
    ;;
  2)
    echo "timed out waiting for feedback; do not push a stale-feedback round; see $OUT"
    ;;
  *)
    echo "crq loop failed with exit $rc"
    ;;
esac

# Propagate crq's exit code so automation can branch on findings/timeout/convergence.
exit "$rc"
