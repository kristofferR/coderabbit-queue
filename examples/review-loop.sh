#!/usr/bin/env bash
# Minimal agent wrapper around crq's next-action contract.
#
# One step of the loop: ask crq what to do about this PR and print it. There is
# no exit code to interpret and no delay to invent — `crq next` answers both.
# Drop this inside your own while-loop, or let your agent drive it.
set -euo pipefail

REPO="${REPO:?set REPO=owner/name}"
PR="${PR:?set PR=<number>}"
OUT="${OUT:-crq-next.json}"

crq next "$REPO" "$PR" > "$OUT"
action=$(jq -r .action "$OUT")
reason=$(jq -r '.reason // ""' "$OUT")
recheck=$(jq -r '.recheck_after // ""' "$OUT")

echo "action: $action${reason:+ — $reason}"

case "$action" in
  fix)
    echo "fix the findings in $OUT and validate locally, then resolve each addressed thread:"
    echo "  jq -r '.findings[] | select(.thread_id != null) | .thread_id' '$OUT'"
    echo "  crq resolve '$REPO' '$PR' --thread THREAD_ID"
    echo "  crq decline '$REPO' '$PR' --thread THREAD_ID --reason 'why not addressed'"
    ;;
  hold)
    echo "DO NOT commit or push — moving the head restarts the pending review"
    echo "pending reviewers: $(jq -r '(.pending // []) | join(", ")' "$OUT")"
    echo "call crq next again at $recheck"
    ;;
  push)
    echo "the head is released; commit and push your accumulated fixes once, then call crq next again"
    ;;
  wait)
    echo "nothing to do; call crq next again at $recheck"
    ;;
  done)
    echo "converged"
    ;;
  blocked)
    echo "needs a human"
    ;;
esac
