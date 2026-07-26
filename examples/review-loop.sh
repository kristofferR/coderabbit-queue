#!/usr/bin/env bash
# Minimal agent wrapper around crq's next-action contract.
#
# One step of the loop: ask crq what to do about this PR and print it. There is
# no exit code to interpret and no delay to invent — `crq next` answers both.
# Drop this inside your own while-loop, or let your agent drive it.
set -euo pipefail

REPO="${REPO:?set REPO=owner/name}"
PR="${PR:?set PR=<number>}"
# Default OUT lives OUTSIDE the checkout on purpose. crq decides push-vs-done by
# asking whether the working tree holds changes the PR head lacks, so a report
# written into the repository is itself uncommitted work: the loop would then
# read as "push" forever and could never reach "done".
if [ -n "${OUT:-}" ]; then
  # A caller-provided path is theirs to keep; only clean up what we created.
  :
else
  OUT="$(mktemp -t crq-next.XXXXXX.json)"
  trap 'rm -f "$OUT"' EXIT
fi

crq next "$REPO" "$PR" > "$OUT"
action=$(jq -r .action "$OUT")
reason=$(jq -r '.reason // ""' "$OUT")
recheck=$(jq -r '.recheck_after // ""' "$OUT")

echo "action: $action${reason:+ — $reason}"

case "$action" in
  fix)
    echo "fix the findings in $OUT and validate locally, then resolve each addressed thread:"
    echo "  jq -r '.findings[] | select(.thread_id != null) | .thread_id' '$OUT'"
    echo "  crq resolve THREAD_ID [THREAD_ID...]"
    echo "  crq decline THREAD_ID --reason 'why not addressed'"
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
