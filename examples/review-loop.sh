#!/usr/bin/env bash
# review-loop.sh — autonomously address CodeRabbit feedback on a PR until it converges.
#
# Run one of these per PR, on as many PRs/machines as you like. Because every review
# request goes through `crq`, they all share ONE account-wide queue and fire one at a
# time, in order, only when CodeRabbit has capacity — no stampede, no rate-limit spam.
#
# The only crq-specific line is `crq wait "$REPO" "$PR"`. Everything else is your loop.
#
#   REPO=owner/name PR=123 ./review-loop.sh
set -uo pipefail

REPO="${REPO:?set REPO=owner/name}"
PR="${PR:?set PR=<number>}"
: "${CRQ_REPO:?run 'crq init' once and configure ~/.config/crq/env (see the README)}"

still_open() { [ "$(gh pr view "$PR" --repo "$REPO" --json state -q .state 2>/dev/null)" = "OPEN" ]; }

# Replace this with your real logic: read CodeRabbit's findings, fix the genuine ones,
# run the project's gates (tests/lint/typecheck), then commit & push ONE round. Pushing a
# new commit is what makes the next CodeRabbit review meaningful.
process_review_and_push() {
  echo "[loop] TODO: read review, fix findings, validate, commit & push for $REPO#$PR"
}

# Block until CodeRabbit posts something newer than $1 (ISO8601), ~20 min cap.
wait_for_review() {
  local since="$1" _
  for _ in $(seq 1 40); do
    local n
    n=$(gh api "repos/$REPO/issues/$PR/comments" --paginate \
        --jq "[.[]|select(.user.login==\"coderabbitai[bot]\")|select(.created_at > \"$since\")]|length" 2>/dev/null)
    [ "${n:-0}" -gt 0 ] && return 0
    sleep 30
  done
}

while still_open; do
  since="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
  crq wait "$REPO" "$PR"          # <-- coordinated, FIFO, never fires while rate-limited
  wait_for_review "$since"
  process_review_and_push
done

echo "✅ $REPO#$PR converged or closed."
