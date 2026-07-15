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

## Drain Findings Before Waiting

An autonomous review loop is a work loop, not a review-status waiter. Before starting or
restarting a review round, drain all currently actionable feedback:

1. run `crq feedback "$REPO" "$PR"`,
2. if `.findings` is non-empty, verify and fix genuine findings immediately,
3. validate, commit, push, and resolve or explicitly decline every addressed thread,
4. repeat until current feedback is empty,
5. only then call `crq loop` for a fresh review round.

`crq loop` enforces this for a new round by returning existing findings before it queues or
waits. After any loop result, inspect `.findings` **before** interpreting the exit code. Findings
always mean work now—even if a required reviewer timed out. Never report “still waiting” while
the JSON already contains actionable findings.
The loop returns as soon as any configured feedback bot reports a finding, even if another
required bot is pending. Fix and validate it locally immediately, but **hold the PR head** while
any `.reviewed_by` value is false: do not commit, push, or resolve yet, because changing the head
restarts the pending checks. Keep the queued review alive and poll `crq feedback` using the same
`CRQ_REQUIRED_BOTS`. Once every required bot is true, combine all fixes into one commit, push once,
and resolve the combined thread set.

Thread-less review-body summaries from a previous commit have no GitHub thread to resolve. After
their fixes are pushed they do not gate the next round; the current-head review supersedes them
or re-reports anything that remains valid.

The agent fixes genuine findings, validates, commits, pushes, resolves addressed threads, then
calls `crq loop` again. A round counts only after its findings are drained and the resulting head
has received the required reviews.

Minimal implementation:

```bash
set +e
crq loop "$REPO" "$PR" > crq-feedback.json
rc=$?
set -e

case "$rc" in
  0) echo "converged" ;;
  10) jq '.findings[] | {bot,severity,path,line,title,thread_id,source}' crq-feedback.json ;;
  2)
    if jq -e '.findings | length > 0' crq-feedback.json >/dev/null; then
      jq '.findings[] | {bot,severity,path,line,title,thread_id,source}' crq-feedback.json
    else
      echo "timed out with no findings; retry later"
    fi
    ;;
  *) exit "$rc" ;;
esac
```

Long waits are expected when the queue is blocked, GitHub is rate-limited, a required bot has not
reviewed yet, or the network is down. crq logs progress to stderr; do not kill it just because stdout
is quiet.

`crq loop` does not emit a round merely because an extraction-only bot such as Codex responds first.
It buffers those findings until every `CRQ_REQUIRED_BOTS` reviewer (normally CodeRabbit) has reviewed
the current head, then returns the complete round. Do not act on an early `crq feedback` snapshot as
if the round had completed.

Codex's clean summary (`Codex Review: Didn't find any major issues. Keep them coming!`) is a successful
review signal, not a finding. crq suppresses it when Codex is extraction-only and counts it in
`reviewed_by` when `chatgpt-codex-connector[bot]` is included in `CRQ_REQUIRED_BOTS`. The signal is
accepted only when it was posted after the persisted wait for the current head began, because GitHub
issue comments do not contain a commit SHA.

## Keeping the Loop Alive in an Agent Harness

A single `crq loop` call can wait an hour or more when the queue is deep or the account is
rate-limited. Agent harnesses commonly kill plain background shell jobs between turns, which
silently orphans the wait. Run `crq loop` under the harness's *persistent* long-running-task
primitive instead of a fire-and-forget background shell. In Claude Code that is the Monitor tool
with `persistent: true`, redirecting the findings JSON to a file and emitting one final event line:

```js
Monitor({
  command: 'set +e; crq loop OWNER/REPO PR > /path/to/crq-feedback.json; echo "CRQ_EXIT:$?"',
  description: 'crq review loop on OWNER/REPO#PR',
  persistent: true,
})
```

The `set +e` matters: `crq loop` reports actionable outcomes as non-zero exits (10 = findings,
2 = timeout), and a shell with errexit inherited would exit before the `echo` — the completion
event would never arrive even though `crq-feedback.json` was written.

The `CRQ_EXIT:<code>` line is the completion event (map it to the exit codes above); crq's stderr
progress stays in the task's output file for diagnosis without generating event noise.

Do **not** replace this wait with a timer, scheduled wake-up, reminder, heartbeat automation, or a
guessed CodeRabbit response delay. Codex and CodeRabbit have independent latency and quota windows,
so time-based wake-ups routinely run before the required review exists. The persistent `crq loop`
process is the waiter. In a harness without a dedicated monitor primitive, keep the foreground PTY
session attached; if the turn is interrupted, re-run the same idempotent `crq loop` command to resume.

If a loop runner is killed anyway, nothing is lost: the PR stays enqueued. Re-running the same
`crq loop` command is safe and re-attaches to the wait — enqueueing is idempotent. Do not
substitute a hand-rolled `crq status` polling loop for the runner.

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

Review-body findings have no GitHub resolution state. Before a new review round starts, crq keeps
the newest body so failed-to-post comments are not lost after a rebase. Once a round is persisted
for the current head, body findings written before that round are suppressed; the current reviewer
must report them again. Cross-commit unresolved threads are still surfaced normally.

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

For an intentionally low-risk PR that has already had enough local review, add
`<!-- crq:skip-autoreview -->` to the PR body before creating it. The marker is hidden in rendered
Markdown and prevents only fleet auto-review; an explicit `crq loop` still reviews the PR.

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
