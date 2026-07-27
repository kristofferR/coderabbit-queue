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

## The Loop

**Call `crq next`, do exactly what `.action` says, call it again.** That is the whole agent loop.
Do not design one of your own.

```bash
crq next "$REPO" "$PR"
crq next            # inside the checkout: crq finds the PR from the remote and branch
                    # (so do feedback, loop, cancel — every command that takes a target)
```

| `.action` | what to do |
|---|---|
| `fix` | Fix `.findings[]`, validate locally, then `crq resolve` each addressed `.thread_id` (or `crq decline` with a reason). Call again. |
| `hold` | Do NOT commit or push — a required reviewer has not answered for this head, and moving the head restarts its review (resolving threads does not). Call again at `.recheck_after`. |
| `push` | The head is released. Commit and push the accumulated fixes once. Call again. |
| `wait` | Nothing to do until `.recheck_after`. |
| `done` | Converged. Report and stop. |
| `blocked` | Needs a human; `.reason` says why (e.g. the PR was closed). |

`crq next` always exits 0 on success: read `.action`, never the exit code. It is **non-blocking and
idempotent**, and it advances the queue by one step as a side effect — so a PR in a repo outside the
autoreview fleet still progresses, and running it alongside the daemon is safe.

Three things this deliberately takes away from you:

- **Choosing a delay.** `.recheck_after` is computed by crq from the account-quota window, the
  round's retry cooldown and the poll interval, and is never less than one poll interval away. Never
  invent one: hand the wait to `crq wait` (below), and only if your harness cannot run a background
  task, schedule a single wake at exactly `.recheck_after`. Never poll in-chat and never loop on
  `crq status` or `gh api`.
- **Deciding when to push.** `hold` vs `push` is crq's answer. It already accounts for the
  rate-limit degrade: a Codex-only round while CodeRabbit is blocked returns `push`, because the
  queued CodeRabbit review fires against whatever head exists when the window opens.
- **Keeping a process alive.** There is none. If the harness kills you mid-loop, the next `crq next`
  returns the correct action from persisted state. Nothing to re-attach, nothing to babysit.

`.local_work` separates `push` from `done`: crq checks whether the working copy holds changes the PR
head lacks. **Run `crq next` from inside the repository checkout** so that answer is accurate;
`.local_work_reason` says when it could not be determined.

## Waiting

On `wait` or `hold`, do not sleep, poll, or guess a delay. Hand the wait over and end your turn:

```bash
crq wait "$REPO" "$PR"
```

It blocks until there IS something to do (`fix`, `push`, `done`, `blocked`), prints that same JSON
and exits 0. Run it as your harness's background task — its **exit is the wake event**, so you burn
no tokens idling and never narrate a countdown.

It owns no round and holds no state, so being killed costs only the process — just run it again (or
call `crq next`). While idle it watches the shared state ref with a conditional request that spends
no rate-limit quota. It is read-only in the steady state, but if nothing is advancing your PR (no
round for the head, or no daemon holding the leader lease) it drives the queue itself rather than
wait for nobody — which can request a review.

`crq next --wait` is the same wait inline, for a human at a terminal. All three share one decision
function, so they cannot disagree.

## Never Bypass crq

Never post `@coderabbitai review` directly — crq is the only trigger, because CodeRabbit's review
limit is account-wide and direct posts stampede it.

Never hand-poll the GitHub API (`gh api .../pulls/N/reviews|comments`, looping on the head) to wait
for a review or learn its outcome. That drains the shared account-wide GitHub REST quota — also spent
by the `crq autoreview` daemon and every other agent, so it exhausts fast — and competes with crq's
own polling. Use `crq next` (the loop), `crq wait` (block until actionable), `crq feedback`
(current findings, no trigger), or `crq status` (queue/quota).

Before starting, check local readiness:

```bash
crq doctor
```

`crq doctor` emits JSON covering crq config, `gh`, optional CodeRabbit CLI availability, and
`CODERABBIT_API_KEY` presence for headless local review.

## crq loop (interactive/one-shot)

`crq loop` is the older primitive: it triggers a round, blocks until feedback lands, and returns one
report with a frozen exit code (0 converged/skipped, 10 findings, 2 timeout). It remains supported
for humans and one-shot scripts.

An agent driving a PR should use `crq next` instead. `crq loop` requires the caller to interpret exit
codes, enforce the drain-first and hold-the-head rules by hand, and keep a long-lived process alive
across turns — the three things that go wrong. Use `crq next` plus `crq wait` instead.

Rate-limit degrade (default on, `CRQ_RL_CODEX_DEGRADE=0` disables): when CodeRabbit is rate-limited
and Codex demonstrably reviews the PR, crq returns Codex feedback promptly instead of waiting out the
window, and the pump posts the Codex command for blocked rounds while keeping the CodeRabbit review
queued. `crq next` folds this into its `push`/`wait` answer for you.

## User-Facing Updates

Do not send heartbeat updates while a loop is simply waiting, and do not narrate repeated stderr
lines. Report a real state change or action: a review fired, findings returned, a push, convergence,
a timeout or unexpected failure, a rate-limit window first discovered or materially changed, a
network outage or recovery, or when the user asks. If the only new information is elapsed time on the
same wait, stay silent.

## Feedback

Use this when you only need current findings and do not want to trigger a new review:

```bash
crq feedback "$REPO" "$PR"
```

`crq next` already embeds the current findings in its `fix` action, so reach for `crq feedback` only
when you want a snapshot without asking what to do about it. The output includes inline comments, GitHub review-thread IDs, collapsed/outside-diff review-body
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
crq resolve "$THREAD_ID"
crq resolve PRRT_one PRRT_two PRRT_three   # resolve a whole round in one call
```

Thread IDs are globally unique, so no repo or PR is needed. Pass every addressed thread to one
call rather than looping a subprocess per thread.

crq keys off GitHub's resolution state: an addressed finding keeps reappearing in `crq feedback`
until its thread is resolved on GitHub. Resolve only threads you actually addressed; leave the rest open.

For a finding you are **not** addressing, record why instead of leaving it silently open:

```bash
crq decline "$THREAD_ID" --reason "why this is declined"
```

This replies with your reason and resolves the thread. crq reads GitHub's resolution state, so a
thread left open keeps its finding actionable and `crq next` would repeat `fix` forever. The
disagreement is not lost: if the bot contests the decline, crq re-surfaces that reply as its own
finding. Pass `--keep-open` to leave it unresolved deliberately.

## Unattended Drain

`crq watch --dispatch` starts a fix session for every PR whose action is `fix`, in a worktree crq
checked out at that head. Sessions run concurrently (`CRQ_DISPATCH_CONCURRENCY`, default 3) and off
the decision loop, so a long one blocks nothing; the decisions stay serial, which is what keeps the
account-metered review in one queue.

One command sets it up — `crq drain install` writes the prompt, a wrapper and this platform's
service (systemd user unit, or a launchd agent on macOS), makes it survive a logout, and starts it;
`--dry-run` prints the paths first. Two rules the prompt earned the hard
way — a session must stay on a detached HEAD and push by ref (`git push origin HEAD:refs/heads/…`),
because the worktrees share one mirror and a branch checked out in one of them makes git refuse to
fetch for every PR; and it resolves threads AFTER pushing, which crq now allows by distinguishing a
superseded round from a stolen one.

Each session's output is written to `$CRQ_WORKSPACE/logs/<owner>/<name>/<pr>-<head>-<time>.log`
(last five per PR). Three passes in a row that start nothing puts `dispatch failing` on the dashboard
and the status line.

## Fleet Auto-Review

To keep all open PRs in scope reviewed while CodeRabbit native auto-review is off:

```bash
crq autoreview
crq autoreview --once
crq autoreview --no-incremental
```

Run exactly one long-lived autoreview daemon. If it is already active, do not stop, restart, or
duplicate it for a manual PR loop. `crq next`, `crq loop` and fleet autoreview all use the same
account-wide, idempotent queue entry: after a push, autoreview may enqueue the new head first and
your call only re-attaches (or vice versa). No path should post a direct CodeRabbit trigger.

For an intentionally low-risk PR that has already had enough local review, add
`<!-- crq:skip-autoreview -->` to the PR body before creating it. The marker is hidden in rendered
Markdown and prevents only fleet auto-review; an explicit `crq next`/`crq loop` still reviews the PR.

## Optional Local Preflight

If the official CodeRabbit CLI is installed, agents can run a normalized local pre-push review:

```bash
crq preflight --type uncommitted
```

Use that only to review local git changes before pushing. It does not replace `crq next`, which
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
