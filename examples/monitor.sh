#!/usr/bin/env bash
# monitor.sh — background watcher for one PR, for fully-unattended review loops.
#
# It does two things:
#   1. Wakes you (exits NEW_FEEDBACK) when a review bot posts new feedback, so your agent
#      can process it.
#   2. Between rounds, keeps the PR in crq's account-wide queue with the NON-blocking
#      `crq enqueue` + `crq pump`. It never posts "@coderabbitai review" directly — crq
#      owns that, so this monitor never competes with your other PRs/agents.
#
# Run it with your agent runner's background mode and re-arm it after each round. It exits:
#   NEW_FEEDBACK <a> -> <b>   new bot comment/review landed — go process it
#   CRQ_PUMP <sha>            (log line only; keeps running) crq pumped the queue
#   IDLE_TIMEOUT              ~75 min with nothing — re-arm if still working
#
#   ./monitor.sh <PR> [owner/repo]      (repo auto-detected from the current git repo)
set -u
PR="${1:?usage: monitor.sh <PR> [owner/repo]}"
REPO="${2:-$(gh repo view --json nameWithOwner -q .nameWithOwner 2>/dev/null)}"
: "${REPO:?usage: monitor.sh <PR> [owner/repo] — REPO not given and not inside a GitHub repo}"
BOTS='coderabbitai|chatgpt-codex'
IDLE_CAP=$(( $(date -u +%s) + 4500 ))

bot_count() {
  c=$(gh api "repos/$REPO/pulls/$PR/comments" --paginate --jq "[.[]|select(.user.login|test(\"$BOTS\"))]|length" 2>/dev/null)
  r=$(gh api "repos/$REPO/pulls/$PR/reviews"  --paginate --jq "[.[]|select(.user.login|test(\"$BOTS\"))]|length" 2>/dev/null)
  echo "${c:-0}:${r:-0}"
}
cr_last_review() {
  gh api "repos/$REPO/pulls/$PR/reviews" \
    --jq 'map(select(.user.login=="coderabbitai[bot]" and .commit_id!=null))|last|.commit_id // ""' 2>/dev/null | cut -c1-9
}

BASE=$(bot_count); echo "monitor PR#$PR repo=$REPO base=$BASE"
while true; do
  CUR=$(bot_count)
  [ "$CUR" != "$BASE" ] && { echo "NEW_FEEDBACK $BASE -> $CUR"; exit 0; }
  HEAD=$(git rev-parse --short=9 HEAD 2>/dev/null); CRREV=$(cr_last_review)
  if [ -n "$HEAD" ] && [ "$CRREV" != "$HEAD" ]; then    # new commit needs a review
    crq enqueue "$REPO" "$PR" >/dev/null 2>&1           # join the account-wide FIFO queue
    crq pump >/dev/null 2>&1 && echo "CRQ_PUMP $HEAD"   # fire <=1 review if globally unblocked
  fi
  [ "$(date -u +%s)" -ge "$IDLE_CAP" ] && { echo "IDLE_TIMEOUT"; exit 0; }
  sleep 60
done
