<h1 align="center">🐰 crq - CodeRabbit review queue</h1>

<p align="center"><b>A shared queue for CodeRabbit reviews.</b></p>

<p align="center">
Humans, agents, laptops, servers, and CI boxes all get in the same line:
one review at a time, only when your CodeRabbit account has capacity.
</p>

`crq` is a GitHub-native queue for CodeRabbit review requests. It does not review code itself; it controls when `@coderabbitai review` is posted so parallel work does not burn the same shared rate limit. On top of that queue, it provides a PR review-loop driver: trigger a queued review, wait for CodeRabbit and Codex feedback, extract all findings as JSON, resolve only the threads you actually fixed, and tell you when the PR has converged.

## The Problem, Plain English

You have several pull requests moving at once. Some may be yours, some may belong to coding agents, and each one eventually wants the same scarce thing: another CodeRabbit review.

The usual loop looks like this:

```text
fix code -> push -> ask CodeRabbit to review -> read feedback -> repeat
```

Without coordination, every person or agent posts `@coderabbitai review` on their own PR. The catch is that CodeRabbit's review limit is shared by the account or organization, not isolated per PR. On a busy account, one review slot can be hours away, and every PR only sees a stale local version of that same account-wide truth.

That creates the failure mode this repo removes:

- people and agents spam review requests while the account is blocked,
- several review loops stampede when the next review slot opens,
- one PR wins while the rest waste requests,
- nobody knows what is queued, in flight, or already reviewed.

## What crq Does

`crq` puts one queue in front of the whole account. Humans and agents do not post `@coderabbitai review` directly; crq does it for them when the account is actually ready.

- Knows the real limit by asking CodeRabbit on a calibration PR.
- Serializes every review trigger through one FIFO queue.
- Fires exactly one review request when the account is unblocked.
- Shows the line in a GitHub dashboard issue.
- Works across machines because all state lives in GitHub.
- Can also drive a full PR review round: wait for feedback, normalize findings, and resolve addressed threads.

For one PR review round, one command is enough:

```bash
crq loop owner/repo 123 > crq-feedback.json
```

> Required: CodeRabbit auto-review must stay off.
>
> crq is pull-only. Reviews should fire through `crq loop` or `crq autoreview`, not automatically on every push. If CodeRabbit reviews every push on its own, it bypasses the queue and spends the same shared limit outside crq.

## How the Queue Works

```text
   human -----\
   agent A ----+--> crq loop / crq autoreview
   CI box ----/          |
                         v
       +--------------------------------+
       | GitHub-backed FIFO queue       |
       | typed state in a git ref       |
       | dashboard rendered to an issue |
       +--------------------------------+
                         |
             asks CodeRabbit for quota
                         |
                         v
       posts "@coderabbitai review" for the next PR only when unblocked
```

Everything lives in one small gate repo, private if you want. There is no database, service account, or always-on central server required for the queue itself.

| Piece | What it does |
|---|---|
| State ref | Stores the typed queue state in `CRQ_STATE_REF` with optimistic compare-and-swap, so concurrent callers do not corrupt the queue. |
| Dashboard issue | Shows what is queued, what is in flight, recent history, warnings, and quota state. This is the human view of the line. |
| 🐰 Calibration PR | Where crq asks `@coderabbitai rate limit` so it can read the real shared quota without spending a review. |

## Common Workflows

Use the queue for a single PR review round:

```bash
crq loop owner/repo 123 > crq-feedback.json
```

Keep every open PR in scope reviewed, still rate-coordinated through the same queue:

```bash
crq autoreview
crq autoreview --once
crq autoreview --no-incremental
```

See the queue and quota state:

```bash
crq status
```

Or open the dashboard issue in the gate repo. It is the live human view of the line: queued PRs, in-flight review, recent history, warnings, and the current CodeRabbit quota reading.

Fetch normalized feedback without triggering a new review:

```bash
crq feedback owner/repo 123
```

## Review Everything Automatically

`crq autoreview` is the queueing mode for all open PRs in `CRQ_SCOPE`. It scans for PRs whose current head has not been reviewed by CodeRabbit yet, enqueues them, and lets the shared FIFO queue decide when each review can fire.

```bash
crq autoreview                  # review open PRs, including new commits
crq autoreview --once           # one pass, useful from cron or a scheduled job
crq autoreview --no-incremental # first review only; do not re-review later pushes
```

The guarantee is ordering, not speed. If ten PRs need review and CodeRabbit only has one slot, crq fires one request, records it, waits for capacity again, and continues. It also avoids double-posting the same PR head, so humans and agents can safely share the queue.

## Human PR Round

If you are working a pull request by hand, run a queued review round and keep the JSON report:

```bash
crq loop owner/repo 123 > crq-feedback.json
rc=$?
```

Then handle the exit code:

| Code | What to do |
|---:|---|
| `0` | Done. The current PR head has converged or has no actionable findings. |
| `10` | Read `crq-feedback.json`, fix still-valid `.findings[]`, validate, commit, push, resolve addressed threads, then run `crq loop` again. |
| `2` | Timed out waiting for feedback. Do not push a stale-feedback round. Retry later or inspect `crq status`. |

For a quick human-readable view of the findings:

```bash
jq -r '.findings[] | "\(.severity // "info") \(.path // "-"):\(.line // 0) - \(.title)\n\(.body)\n"' crq-feedback.json
```

Resolve only the threads you actually fixed — this marks them resolved **on GitHub**, which is how crq stops reporting them:

```bash
jq -r '.findings[] | select(.thread_id != null) | .thread_id' crq-feedback.json
crq resolve owner/repo 123 --thread PRRT_kwDO...
```

Never post `@coderabbitai review` directly. `crq loop` is the trigger, queue, watcher, feedback extractor, and convergence check.

Before relying on a machine, terminal session, or agent host, run:

```bash
crq doctor
```

It emits JSON that says whether `gh`, crq config, and the optional CodeRabbit CLI are ready.

## Install From Source

```bash
git clone https://github.com/kristofferR/coderabbit-queue.git
cd coderabbit-queue
go test ./...
go build -o ~/.local/bin/crq ./cmd/crq
```

Shortcut, if you explicitly accept running the installer from this repository:

```bash
curl -fsSL https://raw.githubusercontent.com/kristofferR/coderabbit-queue/main/install.sh | bash
```

The top-level `./crq` file is a development launcher that runs `go run ./cmd/crq`.

## Configure

Create a private gate repository, then initialize crq state:

```bash
gh repo create YOURUSER/crq-state --private --add-readme
export CRQ_REPO=YOURUSER/crq-state
crq init
```

Save the printed exports into `~/.config/crq/env`:

```bash
export CRQ_REPO=YOURUSER/crq-state
export CRQ_ISSUE=1
export CRQ_CAL_PR=1
export CRQ_SCOPE=YOURUSER
export CRQ_STATE_REF=crq-state
```

After setup, `crq loop` and `crq autoreview` are the only review triggers people and agents should use.

## Commands

```bash
crq loop owner/repo 123
```

Runs the mechanical review loop for a PR, whether a human or an agent is driving it:

1. enqueue the PR in the account-wide FIFO queue,
2. fire `@coderabbitai review` only when allowed,
3. wait for real bot feedback on the current head,
4. emit normalized JSON findings, or report convergence.

`crq loop` exit codes:

| Code | Meaning |
|---:|---|
| 0 | converged or no actionable findings |
| 10 | actionable feedback returned in JSON |
| 2 | timed out waiting for feedback |

Fetch feedback without triggering a new review:

```bash
crq feedback owner/repo 123
```

The JSON includes inline review comments, GraphQL review-thread IDs, collapsed CodeRabbit review-body findings such as outside-diff comments, source URLs, severity, path, line, commit, and bot identity.

<details>
<summary>Feedback JSON shape</summary>

```json
{
  "status": "feedback",
  "repo": "owner/repo",
  "pr": 123,
  "head": "abcdef123",
  "converged": false,
  "reviewed_by": {
    "coderabbitai[bot]": true,
    "chatgpt-codex": false
  },
  "findings": [
    {
      "id": "...",
      "bot": "coderabbitai[bot]",
      "severity": "major",
      "path": "src/file.ts",
      "line": 42,
      "title": "Short finding title",
      "body": "Full normalized finding text",
      "thread_id": "PRRT_...",
      "source": "review_thread",
      "url": "https://github.com/owner/repo/pull/123#discussion_r..."
    }
  ],
  "checked_at": "2026-06-29T00:00:00Z"
}
```

</details>

`findings` is always an array. A finding without `thread_id` can still be real; it came from a review body, outside-diff section, prompt block, or issue comment that GitHub cannot expose as a resolvable thread.

Resolve addressed threads on GitHub (crq keys off GitHub's resolution state, so a finding keeps reappearing until its thread is resolved there):

```bash
crq resolve owner/repo 123 --thread PRRT_kwDO...
```

For a finding you are declining, record the reason on its thread instead of leaving it silently open (left unresolved by default; add `--resolve` to also close it as "won't fix"):

```bash
crq decline owner/repo 123 --thread PRRT_kwDO... --reason "why this is declined"
```

Keep every open PR in scope reviewed:

```bash
crq autoreview
crq autoreview --once
crq autoreview --no-incremental
```

`crq autoreview` uses the same CAS state primitive for its leader lease; there is no separate leader ref.

Check readiness:

```bash
crq doctor
```

`crq doctor` does not mutate GitHub state. It reports crq config, `gh` availability, optional CodeRabbit CLI availability, and whether `CODERABBIT_API_KEY` is set for headless local CLI review.

## Optional Local CodeRabbit CLI

CodeRabbit also ships an official local CLI, usually available as `coderabbit` and `cr`. It is useful for pre-push review of local git changes:

```bash
cr review --agent
cr review --agent --base main
cr review --agent -t uncommitted
```

The CodeRabbit CLI supports agent-readable structured output with `--agent`, API-key auth with `coderabbit auth login --api-key ...`, and one-off API-key use with `cr review --api-key ...`. Use it to catch issues before pushing.

Do not use the CodeRabbit CLI as a replacement for `crq loop`. The CLI reviews local changes; `crq loop` coordinates account-wide PR review triggers, waits for CodeRabbit and Codex feedback on the GitHub PR head, extracts collapsed review-body findings, and resolves addressed GitHub review threads.

## Maintenance

Human and agent loops should use `crq loop`; queue internals are intentionally not public commands.
For manual diagnosis, use `crq debug state`, `crq debug refresh`, `crq debug enqueue`, or
`crq debug pump`.

## Important Environment Variables

| Variable | Default | Purpose |
|---|---|---|
| `CRQ_REPO` | required | gate repo that stores state and dashboard |
| `CRQ_ISSUE` | set by `init` | dashboard issue number |
| `CRQ_SCOPE` | owner of `CRQ_REPO` | comma-separated owners/orgs for `crq autoreview` |
| `CRQ_REPOS` | empty | optional allowlist of `owner/repo` values |
| `CRQ_EXCLUDE` | empty | optional denylist of `owner/repo` values |
| `CRQ_STATE_REF` | `crq-state` | git ref used for CAS state |
| `CRQ_REQUIRED_BOTS` | `coderabbitai[bot],chatgpt-codex` | bots required for convergence |
| `CRQ_WAIT_TIMEOUT` | `0` | seconds or Go duration; `0` means no timeout for the internal loop wait |

## Development

```bash
go test ./...
./crq help
```

Focused tests cover queue idempotency/deduplication, typed state mutation, rate-limit countdown parsing, and CodeRabbit outside-diff plus prompt-block review-body extraction.
