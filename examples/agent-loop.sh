#!/usr/bin/env bash
# Example: an autonomous PR-review loop that uses crq to coordinate CodeRabbit
# review requests with every other agent sharing your account's rate limit.
#
# The ONLY crq-specific line is `crq wait "$REPO" "$PR"`. Everything else is your
# normal review-loop work. Because crq serializes firing account-wide, you can run
# this same loop for many PRs across many machines at once without competing.
#
#   REPO=owner/name PR=123 ./agent-loop.sh
set -uo pipefail

REPO="${REPO:?set REPO=owner/name}"
PR="${PR:?set PR=<number>}"

# crq config (or put these in ~/.config/crq/env, which crq sources automatically)
: "${CRQ_REPO:?set CRQ_REPO=youruser/crq-state (run 'crq init' once first)}"

review_cycle_should_continue() {
  # Stop when the PR is closed/merged. Replace with your own convergence check
  # (e.g. both review bots reviewed the latest commit with nothing actionable left).
  [ "$(gh pr view "$PR" --repo "$REPO" --json state -q .state 2>/dev/null)" = "OPEN" ]
}

wait_for_review_to_land() {
  # Block until CodeRabbit posts a review newer than 'since' (ISO8601), ~20 min cap.
  local since="$1" deadline=$(( $(date +%s) + 1200 ))
  while [ "$(date +%s)" -lt "$deadline" ]; do
    local n
    n="$(gh api "repos/$REPO/issues/$PR/comments" --paginate \
         --jq "[.[]|select(.user.login==\"coderabbitai[bot]\")|select(.created_at > \"$since\")]|length" 2>/dev/null)"
    [ "${n:-0}" -gt 0 ] && return 0
    sleep 30
  done
  return 0
}

apply_fixes_and_push() {
  # YOUR work: read the review, fix real findings, run the project's gates,
  # commit and push one round. Left as a stub here.
  echo "[agent] processing review feedback for $REPO#$PR ..."
}

while review_cycle_should_continue; do
  fired_at="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
  crq wait "$REPO" "$PR"          # <-- coordinated, FIFO, never fires while rate-limited
  wait_for_review_to_land "$fired_at"
  apply_fixes_and_push
done

echo "[agent] $REPO#$PR converged / closed."
