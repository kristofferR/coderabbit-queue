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
# Honor crq's configured review bot + rate-limit marker (defaults = CodeRabbit, which crq
# coordinates). Override CRQ_BOT / CRQ_RL_MARKER for another bot.
# shellcheck source=/dev/null
[ -f "${CRQ_CONFIG:-$HOME/.config/crq/env}" ] && . "${CRQ_CONFIG:-$HOME/.config/crq/env}"
BOT="${CRQ_BOT:-coderabbitai[bot]}"
RL="${CRQ_RL_MARKER:-rate limited by coderabbit.ai}"
IDLE_CAP=$(( $(date -u +%s) + 4500 ))

bot_count() {
  # --slurp + a standalone jq with `add`: combine all pages before counting. Plain --paginate --jq
  # runs the filter per page, so a new (non-bot) page could shift the string and false-wake the loop.
  # Exclude rate-limit WARNING posts from EVERY counter (case-insensitive) — they're not review
  # feedback and would otherwise wake the loop as NEW_FEEDBACK when no actual review arrived.
  local f='add|map(select(.user.login==$bot and ((.body//"")|ascii_downcase|contains(($rl|ascii_downcase))|not)))|length'
  c=$(gh api "repos/$REPO/pulls/$PR/comments" --paginate --slurp 2>/dev/null | jq --arg bot "$BOT" --arg rl "$RL" "$f" 2>/dev/null)
  r=$(gh api "repos/$REPO/pulls/$PR/reviews"  --paginate --slurp 2>/dev/null | jq --arg bot "$BOT" --arg rl "$RL" "$f" 2>/dev/null)
  i=$(gh api "repos/$REPO/issues/$PR/comments" --paginate --slurp 2>/dev/null | jq --arg bot "$BOT" --arg rl "$RL" "$f" 2>/dev/null)
  # A successful-but-empty response yields "0"; an EMPTY string means the gh call failed. Don't
  # fabricate 0:0:0 from a transient failure (it would look like feedback vanished and false-wake) —
  # signal an error so the caller skips this tick.
  [ -n "$c" ] && [ -n "$r" ] && [ -n "$i" ] || return 1
  echo "$c:$r:$i"
}
cr_last_review() {   # echoes the last CodeRabbit-reviewed commit (short); returns 1 if the lookup FAILED
  local out
  out="$(gh api "repos/$REPO/pulls/$PR/reviews" --paginate \
    --jq ".[]|select(.user.login==\"$BOT\" and .commit_id!=null)|.commit_id" 2>/dev/null)" || return 1
  printf '%s' "$out" | tail -1 | cut -c1-9
}

BASE=""; while [ -z "$BASE" ]; do BASE=$(bot_count) || sleep 5; done   # establish a real baseline (retry past transient failures)
echo "monitor PR#$PR repo=$REPO base=$BASE"
while true; do
  CUR=$(bot_count) || { sleep 60; continue; }   # transient API failure -> skip this tick, don't false-wake
  [ "$CUR" != "$BASE" ] && { echo "NEW_FEEDBACK $BASE -> $CUR"; exit 0; }
  # compare the REMOTE PR head (not the local checkout, which may be ahead/behind/elsewhere).
  # Skip the enqueue decision if EITHER lookup fails — don't treat an unreadable head/review as
  # "needs a review" and enqueue redundantly (CRREV=$(...) fails -> the && chain short-circuits).
  HEAD=$(gh api "repos/$REPO/pulls/$PR" --jq '.head.sha // empty' 2>/dev/null | cut -c1-9)
  # Require a real short SHA (a failed lookup leaves a non-empty non-hex error body) AND a successful
  # cr_last_review — don't enqueue on an unreadable head/review.
  if [ -n "$HEAD" ] && [ -z "${HEAD//[0-9a-f]/}" ] && CRREV=$(cr_last_review) && [ "$CRREV" != "$HEAD" ]; then
    crq enqueue "$REPO" "$PR" >/dev/null 2>&1           # join the account-wide FIFO queue
    crq pump >/dev/null 2>&1 && echo "CRQ_PUMP $HEAD"   # fire <=1 review if globally unblocked
  fi
  [ "$(date -u +%s)" -ge "$IDLE_CAP" ] && { echo "IDLE_TIMEOUT"; exit 0; }
  sleep 60
done
