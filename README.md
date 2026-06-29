<h1 align="center">🐰 crq — CodeRabbit review queue</h1>

<p align="center"><b>Stop your AI agents from fighting over one CodeRabbit rate limit.</b></p>

<p align="center">
One shared queue for your whole CodeRabbit account, so parallel agents request reviews
<b>in an orderly line — one at a time, only when there's capacity</b> — instead of stampeding.
On top of the queue, crq drives the whole review round: trigger, wait, normalize every finding to
JSON, resolve the threads you fixed, and tell you when the PR has converged.
</p>

---

## The problem (in plain English)

You've got several pull requests moving at once — yours and your AI agents' — each looping:
*fix code → push → ask CodeRabbit to review → read feedback → repeat.* To ask for a review, you
post `@coderabbitai review` on the PR.

Here's the catch: **CodeRabbit's review limit is per *account/organization*, not per PR.** On the
Pro plan, once you've been reviewing a lot, the refill rate drops — at the slowest tier it's
effectively **one review at a time, hours apart**, shared across *all* your PRs.

So your agents collide:

- 🔁 **They spam while blocked.** Agent A's PR is "available in 3 hours," but Agent B has no idea —
  it keeps posting `@coderabbitai review` on *its* PR and getting rate-limited too. Noise everywhere.
- 🐎 **They stampede when the window opens.** The moment one slot frees up, every agent fires at
  once. One wins; the rest waste the slot and trip the limit again.
- 🤷 **Nobody knows the real state.** Each agent only sees its own PR's stale countdown, but the
  limit is account-wide — so that countdown is usually wrong.

The result: wasted quota, redundant requests, and reviews landing in a random order.

## What crq does

`crq` puts **one queue in front of your whole account.** You don't post `@coderabbitai review`
yourself anymore — you ask `crq`, and `crq`:

- 🧠 **Knows the real limit** — it *asks CodeRabbit directly* (`@coderabbitai rate limit`, which
  doesn't cost a review) instead of guessing from a stale comment.
- 🚦 **Serializes everything** — compare-and-swap on a single git ref means reviews fire **one at a
  time, in FIFO order, only when the account is actually unblocked.** No stampede, no spam.
- 📊 **Shows you the line** — a live GitHub issue is the dashboard: who's queued, what's in flight,
  recent history, and when the next slot opens.
- 🌍 **Works across machines** — all state lives on GitHub, so agents on your laptop, a server, and
  a CI box all share the same queue with zero extra infrastructure.
- 🔁 **Drives the round** — `crq loop` doesn't just trigger; it waits for CodeRabbit *and* Codex on
  the current head, returns every finding as normalized JSON, and reports convergence.

One agent changes one line — `gh pr comment ... @coderabbitai review` becomes `crq loop <repo> <pr>` —
and the chaos is gone.

> ## ⚠️ Required: turn OFF CodeRabbit auto-review
>
> crq can only control *when* reviews happen if CodeRabbit isn't reviewing on its own.
> **If auto-review is enabled, CodeRabbit reviews every push automatically — bypassing crq's
> queue entirely and spending your shared rate limit outside it.** That defeats the whole point.
>
> So crq's model is **pull, not push** — reviews fire *only* when crq (or you) explicitly post
> `@coderabbitai review`. Disable auto-review before relying on crq:
>
> - **Account / org-wide (recommended):** CodeRabbit dashboard → your organization →
>   **Settings → Review → Automatic Review** → turn it **off** (also disable incremental/auto
>   reviews). This is the setting that matters most.
> - **Or per repository:** commit a `.coderabbit.yaml` with:
>
>   ```yaml
>   reviews:
>     auto_review:
>       enabled: false
>   ```
>
> To have reviews happen automatically across your PRs, use **`crq autoreview`** instead — it does
> the same job, rate-coordinated. See [Review all your PRs automatically](#review-all-your-prs-automatically).

## How it works

```text
   agent A ─┐
   agent B ─┼─►  crq loop <repo> <pr>
   agent C ─┘          │
                       ▼
        ┌──────────────────────────────┐     asks "any capacity?"
        │  typed state in a git ref     │ ───────────────────────►  CodeRabbit
        │  (compare-and-swap) + FIFO    │ ◄───────────────────────  "available now" / "in 3h"
        │  queue, mirrored to an issue  │
        └──────────────────────────────┘
                       │  when unblocked, fire the FIFO head — exactly one
                       ▼
        posts "@coderabbitai review" on the next PR in line, then waits for feedback
```

Everything lives in one small **gate repo** (private is fine):

| Piece | What it is |
|-------|-----------|
| 🔒 **State ref** | The typed queue state is JSON stored in a git ref (`CRQ_STATE_REF`, default `crq-state`), updated with optimistic **compare-and-swap** — a new commit is written only if the ref hasn't moved, so concurrent callers across machines never corrupt the queue. No database, service account, or always-on server. |
| 📊 **Dashboard issue** | A tracking **issue** renders the live state below a hidden machine-readable block: status, the queue, in-flight review, "recently reviewed" history, and the current quota — every PR linked. The issue **title** is a one-glance status (`🐰 crq — 2 queued`). |
| 🐰 **Calibration PR** | A throwaway draft PR where crq asks `@coderabbitai rate limit` to read your real quota *without spending a review*. crq prunes its own probe comments so the PR never hits GitHub's 2500-comment cap. |

---

## Quick start

**1. Install** (needs [`gh`](https://cli.github.com/) logged in, or `GITHUB_TOKEN`/`GH_TOKEN` set):

```bash
curl -fsSL https://raw.githubusercontent.com/kristofferR/coderabbit-queue/main/install.sh | bash
```

The installer drops a prebuilt binary into `~/.local/bin` when a release asset exists, and otherwise
builds from source (needs [Go](https://go.dev/dl/)).

<details>
<summary>Manual install (build from source)</summary>

```bash
git clone https://github.com/kristofferR/coderabbit-queue.git
cd coderabbit-queue
go test ./...
go build -trimpath -ldflags "-s -w" -o ~/.local/bin/crq ./cmd/crq   # ensure ~/.local/bin is on $PATH
crq doctor   # verify gh/auth/config readiness
```

The top-level `./crq` is a dev launcher that runs `go run ./cmd/crq`.
</details>

**2. Create your queue** (one private repo holds the state ref + dashboard + calibration PR):

```bash
gh repo create YOURUSER/crq-state --private --add-readme
export CRQ_REPO=YOURUSER/crq-state
crq init
```

`crq init` opens the calibration PR and dashboard issue and prints the `export CRQ_*` lines to save.
Drop them into `~/.config/crq/env` — crq sources that file automatically, so every machine just needs
the same handful of lines:

```bash
mkdir -p ~/.config/crq
cat > ~/.config/crq/env <<'EOF'
export CRQ_REPO=YOURUSER/crq-state
export CRQ_ISSUE=2
export CRQ_CAL_PR=1
export CRQ_SCOPE=YOURUSER
export CRQ_STATE_REF=crq-state
EOF
```

> **One-time:** make sure CodeRabbit is installed on the gate repo (so it can answer
> `@coderabbitai rate limit` on the calibration PR). If your CodeRabbit covers "all repositories"
> you're already done; otherwise add `crq-state` in the CodeRabbit dashboard.
>
> crq posts calibration comments *as you*, which re-subscribes you to the gate repo. Set it to
> **Watch ▾ → Ignore** on GitHub so the machine-only calibration PR never emails you.

**3. Use it.** In any review loop, replace this:

```bash
gh pr comment "$PR" --repo "$REPO" --body "@coderabbitai review"   # ❌ competes with other agents
```

with this:

```bash
crq loop "$REPO" "$PR" > crq-feedback.json   # ✅ queues, fires when ready, waits, emits findings
```

`crq loop` gets in line, fires the review exactly once when CodeRabbit has capacity, waits for
CodeRabbit and Codex on the current head, and writes the findings as JSON. Its exit code tells you
what to do next: `0` converged, `10` actionable findings in the JSON, `2` timed out.

---

## Review all your PRs automatically

`crq autoreview` reviews all your open PRs automatically, rate-coordinated. Run it as a background
watcher:

```bash
crq autoreview                  # auto-review every open PR + re-review on each push (FIFO, rate-aware)
crq autoreview --no-incremental # auto-review each PR ONCE only — no re-review on later pushes
crq autoreview --once           # a single pass (e.g. from cron or a timer)
```

By default `autoreview` covers every open PR in `CRQ_SCOPE`. To limit it to specific repos, set an
allowlist (`CRQ_REPOS=owner/a,owner/b`) — or exclude a few with a denylist (`CRQ_EXCLUDE=owner/c`).

Each pass enqueues any open PR in scope whose latest commit CodeRabbit hasn't reviewed yet (a new PR →
its first review; new commits → an incremental review), then fires them FIFO until **every** PR is
reviewed. The two flags mirror CodeRabbit's own toggles: default = *Automatic + Incremental*;
`--no-incremental` = *Automatic* only. (The gate repo itself is never auto-reviewed.) crq records the
commit it requested a review for, so the same commit is never reviewed twice. One process is the
leader at a time (a lease in the shared state), so running the daemon on several machines is safe.

<details>
<summary>Run it persistently (macOS launchd / Linux systemd)</summary>

**macOS — a LaunchAgent** at `~/Library/LaunchAgents/<label>.crq-autoreview.plist`:

```xml
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>com.example.crq-autoreview</string>
  <key>ProgramArguments</key>
  <array><string>/Users/YOU/.local/bin/crq</string><string>autoreview</string></array>
  <key>EnvironmentVariables</key>
  <dict>
    <key>HOME</key><string>/Users/YOU</string>
    <key>PATH</key><string>/opt/homebrew/bin:/usr/bin:/bin:/Users/YOU/.local/bin</string>
  </dict>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
  <key>ProcessType</key><string>Background</string>
  <key>StandardOutPath</key><string>/Users/YOU/Library/Logs/crq-autoreview.log</string>
  <key>StandardErrorPath</key><string>/Users/YOU/Library/Logs/crq-autoreview.log</string>
</dict>
</plist>
```

```bash
launchctl bootstrap "gui/$(id -u)" ~/Library/LaunchAgents/com.example.crq-autoreview.plist
launchctl print "gui/$(id -u)/com.example.crq-autoreview" | grep -E 'state|pid'   # check it's running
# stop with: launchctl bootout "gui/$(id -u)/com.example.crq-autoreview"
```

The `PATH` must include where `gh` and `crq` live; `crq` reads its config from `~/.config/crq/env`
and `gh`'s auth from your login keychain.

**Linux — a systemd user service** (`~/.config/systemd/user/crq-autoreview.service`):

```ini
[Unit]
Description=crq autoreview (CodeRabbit review queue)
[Service]
ExecStart=%h/.local/bin/crq autoreview
Restart=always
Environment=PATH=%h/.local/bin:/usr/local/bin:/usr/bin:/bin
[Install]
WantedBy=default.target
```

```bash
systemctl --user enable --now crq-autoreview
journalctl --user -u crq-autoreview -f
```

Or, if you don't want a long-running process, run `crq autoreview --once` from cron / a timer.
</details>

---

## ⭐ The recommended PR-review loop

This is the autonomous review loop crq was built for. `crq loop` *is* the loop primitive — it
enqueues, fires when unblocked, waits for both bots on the current head, and emits normalized JSON —
so you never hand-poll the GitHub API (which would burn the shared REST quota). Run one per PR, on as
many PRs and machines as you like; they all share the queue without competing.

```bash
#!/usr/bin/env bash
# review-loop.sh — autonomously address CodeRabbit + Codex feedback until a PR converges.
#   REPO=owner/name PR=123 ./review-loop.sh
set -uo pipefail
REPO="${REPO:?set REPO=owner/name}"; PR="${PR:?set PR=<number>}"

while :; do
  # crq loop: enqueue + rate-coordinated trigger + wait for feedback. Exit codes:
  #   0 = converged   10 = actionable findings in JSON   2 = timed out
  crq loop "$REPO" "$PR" > crq-feedback.json; rc=$?
  case "$rc" in
    0) echo "✅ $REPO#$PR converged."; break ;;
    2) echo "timed out; not pushing a stale-feedback round"; sleep 60; continue ;;
    10) : ;;  # fall through and fix
    *) echo "crq loop error ($rc)"; exit "$rc" ;;
  esac

  # Read findings, fix the real ones, run your tests/linters, then commit & push.
  jq -r '.findings[] | "\(.severity) \(.path // "-"):\(.line // 0) — \(.title)"' crq-feedback.json
  #   ... apply fixes, validate, git commit, git push ...

  # Resolve the threads you addressed; record why for any you decline.
  jq -r '.findings[] | select(.thread_id != null) | .thread_id' crq-feedback.json \
    | xargs -I{} crq resolve "$REPO" "$PR" --thread {}
  # crq decline "$REPO" "$PR" --thread <id> --reason "why this one is declined"
done
```

[`examples/review-loop.sh`](examples/review-loop.sh) is a minimal **one-shot wrapper** around the
same contract — it runs `crq loop` once, prints what to do for each exit code, and exits with crq's
code. It is the building block, not the full autonomous loop above: drop it inside your own
`while`/fix/push/resolve cycle (or let your agent drive the loop).

> 💡 **Watching the line:** run `crq status` any time to see the queue, what's in flight, and the
> next slot. Or open the gate **issue** on GitHub — it **is** the live dashboard.

---

## Commands

```bash
crq loop <repo> <pr>      # ⭐ enqueue + fire + wait for both bots + emit JSON findings (use in loops)
crq feedback <repo> <pr>  # current normalized findings as JSON, WITHOUT triggering a review
crq resolve <repo> <pr> --thread <id> [...]                 # resolve addressed review threads
crq decline <repo> <pr> --thread <id> --reason "<why>" [--resolve]   # record why a finding is declined
crq autoreview            # ⭐ review ALL open PRs automatically, rate-coordinated
                          #    (--no-incremental = first review only; --once = single pass for cron)
crq status                # show the dashboard: queue, in-flight, quota, next slot
crq doctor                # JSON readiness report (gh/auth/config/CLI) — never writes to GitHub
crq preflight [...]       # run the local CodeRabbit CLI pre-push and normalize its JSON
crq cancel <repo> <pr>    # take a PR out of the line
crq init                  # first-time setup of the gate repo
crq debug <enqueue|pump|refresh|state>   # diagnosis only — review loops should use crq loop
crq version               # print the version
crq help [command]        # help, optionally for one command
```

`<repo>` is `owner/name`; `<pr>` is the number. **`crq loop` exit codes:** `0` converged or no
actionable findings, `10` actionable findings returned in `.findings[]`, `2` timed out waiting for
feedback. crq keys resolution off GitHub's own thread state, so a finding keeps reappearing in
`feedback`/`loop` until its thread is resolved (or declined-and-resolved) on GitHub.

<details>
<summary>Feedback JSON shape</summary>

```json
{
  "status": "feedback",
  "repo": "owner/repo",
  "pr": 123,
  "head": "abcdef123",
  "converged": false,
  "reviewed_by": { "coderabbitai[bot]": true, "chatgpt-codex": false },
  "findings": [
    {
      "id": "…",
      "bot": "coderabbitai[bot]",
      "severity": "major",
      "path": "src/file.ts",
      "line": 42,
      "title": "Short finding title",
      "body": "Full normalized finding text",
      "thread_id": "PRRT_…",
      "source": "review_thread",
      "url": "https://github.com/owner/repo/pull/123#discussion_r…"
    }
  ],
  "checked_at": "2026-06-29T00:00:00Z"
}
```

`findings` is always an array. It includes inline comments, GraphQL review-thread IDs, collapsed
"Outside diff range" / `<details>` review-body findings, prompt blocks, and Codex issue comments — and
surfaces still-open findings from earlier commits, so nothing is silently dropped between passes. A
finding without `thread_id` came from a review body or comment GitHub can't expose as a resolvable
thread; CodeRabbit clears those on its next review.
</details>

## Configuration

Set these in `~/.config/crq/env` (sourced automatically) or as environment variables:

| Variable | Default | What it does |
|----------|---------|--------------|
| `CRQ_REPO` | *(required)* | the gate repo (`owner/name`) holding the state ref, dashboard, calibration PR |
| `CRQ_ISSUE` | from `init` | dashboard issue number |
| `CRQ_CAL_PR` | from `init` | calibration PR number |
| `CRQ_SCOPE` | owner of `CRQ_REPO` | which owners/orgs share this quota (comma-separated) |
| `CRQ_STATE_REF` | `crq-state` | git ref that stores the typed CAS state |
| `CRQ_REPOS` | _(all in scope)_ | `autoreview` allowlist — only these `owner/name` repos (comma-separated) |
| `CRQ_EXCLUDE` | _(none)_ | `autoreview` denylist — never these `owner/name` repos (comma-separated) |
| `CRQ_REQUIRED_BOTS` | `coderabbitai[bot],chatgpt-codex` | bots that must review the head for convergence |
| `CRQ_TZ` | `UTC` | dashboard display timezone (IANA name, e.g. `Europe/Oslo`) |
| `CRQ_MIN_INTERVAL` | `90s` | minimum time between fired reviews |
| `CRQ_POLL` | `15s` | how often `crq loop` checks its place in line |
| `CRQ_WAIT_TIMEOUT` | `0` | give up waiting for a slot after this long (`0` = never) |
| `CRQ_FEEDBACK_WAIT_TIMEOUT` | `20m` | how long `crq loop` waits for feedback after firing |
| `CRQ_CALIBRATE_TTL` | `2m` | how long to trust a quota reading before re-asking CodeRabbit |
| `CRQ_AUTOREVIEW_POLL` | `1m` | how often the `autoreview` daemon scans for PRs to enqueue |
| `CRQ_INFLIGHT_TIMEOUT` | `15m` | backstop to release a stuck in-flight review |
| `CRQ_LEADER_TTL` | `3m` | when a crashed `autoreview` leader is considered gone |
| `CRQ_GITHUB_MAX_WAIT` / `CRQ_GITHUB_RETRIES` | `120s` / `6` | GitHub rate-limit / 5xx backoff budget per request |
| `CRQ_NETWORK_MAX_WAIT` | `0` (no cap) | cap on riding out an internet/GitHub outage (retrying ~every 30s); `0` = keep trying until connectivity returns |

**Other review bots:** crq isn't CodeRabbit-specific. Point `CRQ_BOT`, `CRQ_REVIEW_CMD`,
`CRQ_RATELIMIT_CMD`, and `CRQ_RL_MARKER` at any bot with a similar command surface.

**Multiple orgs:** CodeRabbit's quota is per-org, so PRs in different orgs draw from *different*
buckets. Run a separate gate (its own `CRQ_REPO`) per org rather than mixing them — otherwise you'd
serialize reviews that don't actually compete.

---

## 🤖 For AI agents (LLM-friendly cheat sheet)

If you're an autonomous agent running a PR-review loop, here's everything you need:

- **The one rule:** never post `@coderabbitai review` yourself. To run a review round, use
  `crq loop "<owner/repo>" "<pr>"` — it blocks until feedback lands and writes findings to stdout.
- **Never hand-poll GitHub for review status.** Don't loop `gh api .../pulls/N/reviews|comments`
  waiting for a review — that drains the shared account-wide REST quota (also spent by the
  `autoreview` daemon and every other agent) and competes with crq's own polling. Use `crq loop`
  (waits + returns findings), `crq feedback` (current findings, no trigger), or `crq status`.
- **Exit codes:** `0` converged → done; `10` → read `.findings[]`, fix valid ones, validate, commit,
  push, then loop again; `2` → timed out, don't push stale-feedback fixes.
- **Resolve / decline:** after fixing a finding, `crq resolve <repo> <pr> --thread <id>`. If you're
  declining one, record why with `crq decline <repo> <pr> --thread <id> --reason "…"` instead of
  leaving it silently open.
- **A long wait is not a hang.** During queue/rate-limit waits, feedback waits, and network
  outages, crq logs progress to **stderr** (queue reason, per-bot `reviewed` status, `github
  unreachable … offline …` / `reachable again`). It keeps retrying through an internet drop until
  connectivity returns (no timeout by default), so don't kill it — watch stderr.
- **Setup check:** run `crq doctor`; if config is missing, do the Quick Start (install + `crq init`).

A drop-in **[Claude Code skill](skills/coderabbit-queue/SKILL.md)** is included, and a compact
machine contract lives in [`llms.txt`](llms.txt).

---

## Troubleshooting

| Symptom | Fix |
|---------|-----|
| `crq doctor` not ready | Set `GITHUB_TOKEN`/`GH_TOKEN` or run `gh auth login`, and finish `crq init`. |
| A PR is stuck "in flight" forever | `crq cancel <repo> <pr>`; it also auto-clears after `CRQ_INFLIGHT_TIMEOUT`. |
| Reviews fire slower than expected | That's the point — you're rate-limited. `crq status` shows the real countdown from CodeRabbit. |
| `github … rate limit hit … resets …` | crq backs off and retries automatically (up to `CRQ_GITHUB_MAX_WAIT`); past that it surfaces a clear reset time instead of a raw 403. |
| Internet drops for a while | crq rides it out — it keeps retrying (the request *is* the connectivity probe) every ~30s with **no timeout by default**, logging `github unreachable … offline …` to stderr and `reachable again` on recovery, so a long outage blocks and resumes instead of failing your loop or daemon. Set `CRQ_NETWORK_MAX_WAIT` to cap it. |
| Calibration PR rejects comments | crq prunes its own probe comments to stay under GitHub's 2500-comment cap and self-heals if it ever hits it. |

## How concurrency works (for the curious)

Every queue change is a **compare-and-swap** on the state ref: crq reads the current commit + tree,
applies the mutation in memory, writes a new blob/tree/commit, and moves the ref **only if it still
points where it did** — GitHub rejects a stale update, and crq retries. That gives a real
cross-machine guarantee with no separate lock to wedge or break. FIFO order uses a monotonic sequence
counter (not timestamps), so it's immune to clock differences between machines, and the `autoreview`
daemon coordinates via a short-lived leader lease stored in the same state.

## License

MIT © Kristoffer Risanger. See [LICENSE](LICENSE). Contributions welcome.
