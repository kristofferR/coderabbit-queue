---
name: coderabbit-queue
description: Drive autonomous CodeRabbit/Codex PR-review loops through crq without competing for the shared account-wide CodeRabbit rate limit. Use whenever you need to trigger CodeRabbit, fetch actionable bot feedback, resolve addressed review threads, run local pre-push review preflight, or keep PRs reviewed automatically.
---

# coderabbit-queue (`crq`)

CodeRabbit's PR-review limit is account-wide. Multiple agents posting `@coderabbitai review`
directly will stampede the same quota. `crq` owns that mechanical loop:

1. enqueue the PR in one FIFO queue,
2. trigger CodeRabbit only when the shared account can spend a review,
3. wait for every configured required bot (`CRQ_REQUIRED_BOTS`) on the current head,
4. emit normalized JSON findings or report convergence,
5. resolve the review threads the agent says it addressed.

## The Rule

Never post `@coderabbitai review` directly. Use `crq loop` for an agent round:

```bash
crq loop "$REPO" "$PR" > crq-feedback.json
```

Don't bypass crq to read review status either: never hand-poll the GitHub API
(`gh api .../pulls/N/reviews|comments`, looping on the head) to wait for a review
or its outcome. That drains the shared account-wide GitHub REST quota — also spent
by the `crq autoreview` daemon and every other agent, so it exhausts fast — and
competes with crq's own polling. Use `crq loop` (waits and returns findings),
`crq feedback` (current findings, no trigger), or `crq status` (queue/quota).

Before starting, check local readiness:

```bash
crq doctor
```

`crq doctor` emits JSON covering crq config, `gh`, optional CodeRabbit CLI availability,
and `CODERABBIT_API_KEY` presence for headless local review.

Exit codes:

- `0`: converged or no actionable findings
- `10`: actionable findings were written to JSON
- `2`: timed out waiting for feedback

The agent reads `crq-feedback.json`, fixes genuine findings, validates, commits, pushes, then
calls `crq loop` again.

Minimal implementation:

```bash
set +e
crq loop "$REPO" "$PR" > crq-feedback.json
rc=$?
set -e

case "$rc" in
  0) echo "converged" ;;
  10) jq '.findings[] | {bot,severity,path,line,title,thread_id,source}' crq-feedback.json ;;
  2) echo "timed out; do not push stale-feedback fixes" ;;
  *) exit "$rc" ;;
esac
```

Long waits are expected when the queue is blocked, GitHub is rate-limited, a required bot has not
reviewed yet, or the network is down. crq logs progress to stderr; do not kill it just because stdout
is quiet.

## User-Facing Updates During Waits

This section overrides generic agent progress-update habits. While `crq loop` is in an ordinary
waiting state, do not send periodic heartbeat updates to the user and do not narrate repeated stderr
lines such as "waiting for a review slot" or "waiting for review feedback".

Send a user update only for a real state change or action:

- review command fired
- feedback wait started or resumed for a head
- findings, convergence, timeout, or unexpected failure returned
- rate-limit/window state is first discovered or changes materially
- network outage or recovery is detected
- findings were fixed, declined, or resolved
- the user asks for status

If the only new information is elapsed time on the same wait, stay silent. If crq reports a long
blocked-until window, summarize it once with the absolute unblock time, then stay silent until the
state changes or the user asks.

## Feedback

Use this when you only need current findings and do not want to trigger a new review:

```bash
crq feedback "$REPO" "$PR"
```

The output includes inline comments, GitHub review-thread IDs, collapsed/outside-diff review-body
findings, prompt-block findings, Codex issue-comment findings, severity, path, line, source URL,
commit, and bot.

`findings` is always an array. Verify each against current code and fix the bugs and flaws it
reports. It also surfaces still-open findings from earlier commits (any unresolved, non-outdated
review thread), so there is no need to audit past reviews by hand.

Parse fields defensively. Each finding has `bot`, `severity`, `title`, `body`, and `source`; `path`,
`line`, `url`, and `thread_id` are optional. Review-body/outside-diff findings often have no
resolvable `thread_id`.

## Resolving Threads

After fixing a finding that has a `thread_id`, resolve that thread **on GitHub**:

```bash
crq resolve "$REPO" "$PR" --thread "$THREAD_ID"
```

crq keys off GitHub's resolution state: an addressed finding keeps reappearing in `crq feedback`
until its thread is resolved on GitHub. Resolve only threads you actually addressed; leave the rest open.

For a finding you are **not** addressing, record why instead of leaving it silently open:

```bash
crq decline "$REPO" "$PR" --thread "$THREAD_ID" --reason "why this is declined"
```

This replies on the thread with your reason and leaves it unresolved (add `--resolve` to also close it
as "won't fix"), so the next reviewer and CodeRabbit can see the decision rather than an ignored finding.

## Fleet Auto-Review

To keep all open PRs in scope reviewed while CodeRabbit native auto-review is off:

```bash
crq autoreview
crq autoreview --once
crq autoreview --no-incremental
```

## Optional Local Preflight

If the official CodeRabbit CLI is installed, agents can run a normalized local pre-push review:

```bash
crq preflight --type uncommitted
```

Use that only to review local git changes before pushing. It does not replace `crq loop`, which
coordinates queued GitHub PR review triggers and extracts GitHub PR feedback.

## Maintenance Commands

Do not use queue internals in agent loops. For diagnosis only:

```bash
crq doctor
crq status
crq debug state
crq debug refresh
crq debug enqueue "$REPO" "$PR"
crq debug pump
crq cancel "$REPO" "$PR"
```

## Required Prerequisite

CodeRabbit auto-review must be off. crq is pull-only: reviews fire through crq, not from
CodeRabbit automatically on every push.
