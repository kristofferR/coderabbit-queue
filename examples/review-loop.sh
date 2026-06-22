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
# crq reads its config from here too; source it so CRQ_REPO need not be exported in this shell.
# shellcheck source=/dev/null
[ -f "${CRQ_CONFIG:-$HOME/.config/crq/env}" ] && . "${CRQ_CONFIG:-$HOME/.config/crq/env}"
: "${CRQ_REPO:?run 'crq init' once and configure ~/.config/crq/env (see the README)}"

still_open() { [ "$(gh pr view "$PR" --repo "$REPO" --json state -q .state 2>/dev/null)" = "OPEN" ]; }

# Replace this with your real logic: read CodeRabbit's findings, fix the genuine ones,
# run the project's gates (tests/lint/typecheck), then commit & push ONE round. Pushing a
# new commit is what makes the next CodeRabbit review meaningful.
process_review_and_push() {
  echo "[loop] TODO: read review, fix findings, validate, commit & push for $REPO#$PR"
}

# Block until CodeRabbit posts something newer than $1 (ISO8601), ~20 min cap. CodeRabbit can
# complete as a conversation comment OR a formal PR review, so check both.
wait_for_review() {
  local since="$1" _ n r
  for _ in $(seq 1 40); do
    # --slurp + a standalone jq with `add`: combine all pages before counting (plain --paginate
    # runs the filter per page; gh forbids --slurp with --jq, so pipe to jq).
    # A rate-limit WARNING is a fresh coderabbitai comment but NOT real feedback — exclude it,
    # else we'd "process" a round that never got reviewed (crq requeues it; we shouldn't push).
    n=$(gh api "repos/$REPO/issues/$PR/comments" --paginate --slurp 2>/dev/null \
        | jq "add | map(select(.user.login==\"coderabbitai[bot]\" and .created_at > \"$since\" and (.body|contains(\"rate limited by coderabbit.ai\")|not))) | length" 2>/dev/null)
    r=$(gh api "repos/$REPO/pulls/$PR/reviews" --paginate --slurp 2>/dev/null \
        | jq "add | map(select(.user.login==\"coderabbitai[bot]\" and .submitted_at > \"$since\")) | length" 2>/dev/null)
    { [ "${n:-0}" -gt 0 ] || [ "${r:-0}" -gt 0 ]; } && return 0
    sleep 30
  done
  return 1   # timed out — no new review landed
}

while still_open; do
  since="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
  if ! crq wait "$REPO" "$PR"; then   # coordinated, FIFO, never fires while rate-limited
    echo "[loop] crq wait did not fire a review (timeout/error) — skipping this round"
    continue
  fi
  if ! wait_for_review "$since"; then
    echo "[loop] no new review within the cap — not pushing a round on stale feedback"
    continue
  fi
  process_review_and_push
done

echo "✅ $REPO#$PR converged or closed."
