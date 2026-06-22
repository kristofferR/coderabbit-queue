<h1 align="center">🐰 crq — CodeRabbit review queue</h1>

<p align="center"><b>Stop your AI agents from fighting over one CodeRabbit rate limit.</b></p>

<p align="center">
One shared queue for your whole CodeRabbit account, so parallel agents request reviews
<b>in an orderly line — one at a time, only when there's capacity</b> — instead of stampeding.
</p>

---

## The problem (in plain English)

You've got several AI coding agents working at once — each on its own pull request, each looping:
*fix code → push → ask CodeRabbit to review → read feedback → repeat.* To ask for a review, an
agent posts `@coderabbitai review` on its PR.

Here's the catch: **CodeRabbit's review limit is per *account/organization*, not per PR.** On the
Pro plan, once you've been reviewing a lot, the refill rate drops — at the slowest tier it's
effectively **one review at a time, hours apart**, shared across *all* your PRs.

So your agents collide:

- 🔁 **They spam while blocked.** Agent A's PR is "available in 3 hours," but Agent B has no idea —
  it keeps posting `@coderabbitai review` on *its* PR and getting rate-limited too. Noise everywhere.
- 🐎 **They stampede when the window opens.** The moment one review slot frees up, every agent fires
  at once. One wins; the rest waste the slot and trip the limit again.
- 🤷 **Nobody knows the real state.** Each agent only sees its own PR's stale countdown, but the
  limit is account-wide — so that countdown is usually wrong.

The result: wasted quota, redundant requests, and reviews landing in a random order.

## What crq does

`crq` puts **one queue in front of your whole account.** Agents don't post `@coderabbitai review`
themselves anymore — they ask `crq`, and `crq`:

- 🧠 **Knows the real limit** — it *asks CodeRabbit directly* (`@coderabbitai rate limit`, which
  doesn't cost a review) instead of guessing from a stale comment.
- 🚦 **Serializes everything** — a single GitHub-native lock means reviews fire **one at a time, in
  FIFO order, only when the account is actually unblocked.** No stampede, no spam.
- 📊 **Shows you the line** — a live GitHub issue is the dashboard: who's queued, what's in flight,
  and when the next slot opens.
- 🌍 **Works across machines** — all state lives on GitHub, so agents on your laptop, a server, and
  a CI box all share the same queue with zero extra infrastructure.

One agent changes one line — `gh pr comment ... @coderabbitai review` becomes `crq wait <repo> <pr>` —
and the chaos is gone.

> ## ⚠️ Required: turn OFF CodeRabbit auto-review
>
> crq can only control *when* reviews happen if CodeRabbit isn't reviewing on its own.
> **If auto-review is enabled, CodeRabbit reviews every push automatically — bypassing crq's
> queue entirely and spending your shared rate limit outside it.** That defeats the whole point:
> your agents would still stampede and rate-limit themselves, crq or not.
>
> So crq's model is **pull, not push** — reviews fire *only* when crq (or you) explicitly post
> `@coderabbitai review`. Disable auto-review before relying on crq:
>
> - **Account / org-wide (recommended):** CodeRabbit dashboard → your organization →
>   **Settings → Review → Automatic Review** → turn it **off** (also disable incremental/auto
>   reviews). This is the setting that matters most.
> - **Or per repository:** commit a `.coderabbit.yaml` with:
>   ```yaml
>   reviews:
>     auto_review:
>       enabled: false
>   ```
>
> To have reviews happen automatically across your PRs, use **`crq autoreview`** instead — it does
> the same job rate-coordinated. See [Review all your PRs automatically](#review-all-your-prs-automatically).

## How it works

```
   agent A ─┐
   agent B ─┼─►  crq wait <repo> <pr>
   agent C ─┘          │
                       ▼
        ┌──────────────────────────────┐     asks "any capacity?"
        │  global lock (a git ref)      │ ───────────────────────►  CodeRabbit
        │  + FIFO queue (in an issue)   │ ◄───────────────────────  "available now" / "in 3h"
        └──────────────────────────────┘
                       │  when unblocked, fire the FIFO head — exactly one
                       ▼
        posts "@coderabbitai review" on the next PR in line
```

Everything lives in one small **gate repo** (private is fine):

| Piece | What it is |
|-------|-----------|
| 🔒 **Lock** | An atomic git ref. GitHub's "create ref only if it doesn't exist" gives a real cross-machine mutex, so only one agent acts at a time and the queue never corrupts. |
| 📋 **Dashboard** | Published to the gate repo's **`README.md`** — status, queue, and a "recently reviewed" history, every PR linked (committed only when something material changes, not on every tick). A tracking **issue** holds the machine-readable state (its **title** is a one-glance status: `crq · 🟡 2 queued`). |
| 🐰 **Calibration PR** | A throwaway draft PR where crq asks `@coderabbitai rate limit` to read your real quota *without spending a review*. (crq disables auto-review on this repo so the PR itself costs nothing.) |

---

## Quick start

**1. Install** (needs [`gh`](https://cli.github.com/) logged in, and [`jq`](https://jqlang.github.io/jq/)):

```bash
curl -fsSL https://raw.githubusercontent.com/kristofferR/coderabbit-queue/main/install.sh | bash
```

<details>
<summary>Manual install (if you'd rather not pipe a script from the net)</summary>

`crq` is a single self-contained bash script — read it first if you like, then drop it on your PATH:

```bash
# clone (or just download the one file) and review it
git clone https://github.com/kristofferR/coderabbit-queue.git
less coderabbit-queue/crq

# install it onto your PATH (make sure ~/.local/bin is on $PATH)
install -m 0755 coderabbit-queue/crq ~/.local/bin/crq

# or without cloning — fetch just the CLI:
#   curl -fsSL https://raw.githubusercontent.com/kristofferR/coderabbit-queue/main/crq -o ~/.local/bin/crq
#   chmod +x ~/.local/bin/crq

crq help   # verify (needs gh + jq installed)
```
</details>

**2. Create your queue** (one private repo holds the lock + dashboard + calibration PR):

```bash
export CRQ_REPO=YOURUSER/crq-state
crq init
```

`crq init` creates the repo, opens the calibration PR and dashboard issue, and prints the
`export CRQ_*` lines to save. Drop them into `~/.config/crq/env` — crq sources that file
automatically, so every machine just needs the same four lines:

```bash
mkdir -p ~/.config/crq
cat > ~/.config/crq/env <<'EOF'
export CRQ_REPO=YOURUSER/crq-state
export CRQ_ISSUE=2
export CRQ_CAL_PR=1
export CRQ_SCOPE=YOURUSER
EOF
```

> **One-time:** make sure CodeRabbit is installed on the gate repo (so it can answer
> `@coderabbitai rate limit` on the calibration PR). If your CodeRabbit covers "all repositories"
> you're already done; otherwise add `crq-state` in the CodeRabbit dashboard.

**3. Use it.** In any review loop, replace this:

```bash
gh pr comment "$PR" --repo "$REPO" --body "@coderabbitai review"   # ❌ competes with other agents
```

with this:

```bash
crq wait "$REPO" "$PR"   # ✅ joins the queue, blocks until YOUR review is actually fired
```

That's the whole integration. `crq wait` gets in line, waits its turn, and posts the review command
for you — exactly once, only when CodeRabbit has capacity.

---

## Review all your PRs automatically

`crq autoreview` reviews all your open PRs automatically, rate-coordinated. Run it as a background watcher:

```bash
crq autoreview                  # auto-review every open PR + re-review on each push (FIFO, rate-aware)
crq autoreview --no-incremental # auto-review each PR ONCE only — no re-review on later pushes
crq autoreview --once           # a single pass (e.g. from cron or your monitor)
```

By default `autoreview` covers every open PR in `CRQ_SCOPE`. To limit it to specific repos, set an
allowlist (`CRQ_REPOS=owner/a,owner/b`) — or exclude a few with a denylist (`CRQ_EXCLUDE=owner/c`).

Each pass enqueues any open PR in scope (minus deny / outside allow) whose latest commit CodeRabbit hasn't reviewed yet
(a brand-new PR → its first review; new commits → an incremental review), then fires them FIFO until
**every** PR is reviewed. The two flags mirror CodeRabbit's own toggles: default = *Automatic +
Incremental review*; `--no-incremental` = *Automatic review* only. (The gate repo itself is never auto-reviewed.)

The guarantee is that **all your PRs get auto-reviewed** — none are skipped. The queue isn't a cap;
it only orders the reviews so they never exceed your rate limit. (`crq wait` is the on-demand version
for when an agent needs *its* PR reviewed right now.) `autoreview` and `crq wait` never double-post:
crq records the commit it requested a review for, so the same commit is never reviewed twice.

<details>
<summary>Run it persistently (macOS launchd / Linux systemd)</summary>

So it survives reboots and always keeps your PRs reviewed, run `crq autoreview` as a service.

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

The `PATH` must include where `gh`, `jq`, and `crq` live; `crq` reads its config from
`~/.config/crq/env`. `gh`'s auth is read from your login keychain, so this works while you're
logged in.

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

Here's the autonomous review loop this tool was built for. Run one per PR — on as many PRs and
machines as you like — and they'll all share the queue without competing.

```bash
#!/usr/bin/env bash
# review-loop.sh — autonomously address CodeRabbit feedback until a PR converges.
#   REPO=owner/name PR=123 ./review-loop.sh
set -uo pipefail
REPO="${REPO:?set REPO=owner/name}"; PR="${PR:?set PR=<number>}"

still_open() { [ "$(gh pr view "$PR" --repo "$REPO" --json state -q .state)" = "OPEN" ]; }

while still_open; do
  since="$(date -u +%Y-%m-%dT%H:%M:%SZ)"

  # 1. Ask for a review — coordinated. Blocks until it's our turn and the account is unblocked,
  #    then crq posts "@coderabbitai review" for us. No stampede, no spam, FIFO across all agents.
  crq wait "$REPO" "$PR"

  # 2. Wait for CodeRabbit's review to land (give it ~20 min).
  for _ in $(seq 1 40); do
    new=$(gh api "repos/$REPO/issues/$PR/comments" --paginate \
      --jq "[.[]|select(.user.login==\"coderabbitai[bot]\")|select(.created_at > \"$since\")]|length")
    [ "${new:-0}" -gt 0 ] && break
    sleep 30
  done

  # 3. Do the work: read the findings, fix the real ones, run your tests/linters, commit & push.
  #    (Pushing a new commit is what makes the next CodeRabbit review meaningful.)
  process_review_and_push "$REPO" "$PR"   # <- your logic here

  # 4. Loop: the next round re-enters the queue and waits again.
done
echo "✅ $REPO#$PR converged or closed."
```

**Running fully unattended?** Add the lightweight **monitor** ([`examples/monitor.sh`](examples/monitor.sh)):
a background watcher that wakes you when new bot feedback lands and, between rounds, keeps the PR in
the queue with the non-blocking `crq enqueue` + `crq pump`. It never posts `@coderabbitai review`
directly — `crq` owns that, account-wide.

> 💡 **Watching the line:** run `crq status` any time to see the queue, what's in flight, and the
> next slot. Or just open your gate repo on GitHub — its README **is** the live dashboard.

---

## Commands

```bash
crq wait <repo> <pr>     # ⭐ enqueue + block until OUR review is fired (use this in loops)
crq autoreview           # ⭐ review ALL open PRs automatically, rate-coordinated
                         #    (--no-incremental = first review only; --once = single pass for cron)
crq enqueue <repo> <pr>  # add to the queue and return immediately (idempotent)
crq pump                 # fire the next review if the window is open (safe to call from anywhere)
crq status               # show the dashboard: queue, in-flight, quota, next slot
crq refresh              # re-check the account quota right now, then show status
crq cancel <repo> <pr>   # take a PR out of the line
crq gc                   # tidy up: drop closed/merged PRs, clear a stuck in-flight entry
crq unlock [--force]     # inspect (or break) the global lock if something wedged
crq init                 # first-time setup of the gate repo
crq version              # print the version
crq help                 # this list
```

`<repo>` is `owner/name`; `<pr>` is the number. **Exit codes:** `crq wait` returns `0` once your
review is fired (or `2` if `CRQ_WAIT_TIMEOUT` is hit); other commands return `0` on success.

## Configuration

Set these in `~/.config/crq/env` (sourced automatically) or as environment variables:

| Variable | Default | What it does |
|----------|---------|--------------|
| `CRQ_REPO` | *(required)* | the gate repo (`owner/name`) holding the lock, dashboard, calibration PR |
| `CRQ_ISSUE` | from `init` | dashboard issue number |
| `CRQ_CAL_PR` | from `init` | calibration PR number |
| `CRQ_SCOPE` | owner of `CRQ_REPO` | which owners/orgs share this quota (comma-separated) |
| `CRQ_REPOS` | _(all in scope)_ | `autoreview` allowlist — only review these `owner/name` repos (comma-separated) |
| `CRQ_EXCLUDE` | _(none)_ | `autoreview` denylist — never review these `owner/name` repos (comma-separated) |
| `CRQ_TZ` | `UTC` | dashboard display timezone (IANA name, e.g. `Europe/Oslo`) |
| `CRQ_CALIBRATE_TTL` | `120` | how long (s) to trust a quota reading before re-asking CodeRabbit |
| `CRQ_MIN_INTERVAL` | `90` | minimum seconds between fired reviews |
| `CRQ_POLL` | `15` | how often (s) `crq wait` checks its place in line |
| `CRQ_WAIT_TIMEOUT` | `0` | give up `crq wait` after N seconds (`0` = never) |
| `CRQ_LOCK_TTL` | `180` | when a crashed lock holder is considered gone and stealable |
| `CRQ_INFLIGHT_TIMEOUT` | `900` | backstop to release a stuck in-flight review |
| `CRQ_WARNING_SCAN` | `1` | also corroborate via open-PR "rate limited" comments (`0` to disable) |

**Other review bots:** crq isn't CodeRabbit-specific. Point `CRQ_BOT`, `CRQ_REVIEW_CMD`,
`CRQ_RATELIMIT_CMD`, and `CRQ_RL_MARKER` at any bot with a similar command surface.

**Multiple orgs:** CodeRabbit's quota is per-org, so PRs in different orgs draw from *different*
buckets. Run a separate gate (its own `CRQ_REPO`) per org rather than mixing them — otherwise you'd
serialize reviews that don't actually compete.

---

## 🤖 For AI agents (LLM-friendly cheat sheet)

If you're an autonomous agent running a PR-review loop, here's everything you need:

- **The one rule:** never post `@coderabbitai review` yourself. To request a review, run
  `crq wait "<owner/repo>" "<pr-number>"`. It blocks until the review is actually fired, then returns `0`.
- **Check state without acting:** `crq status` (human-readable) shows the queue, what's in flight,
  remaining quota, and when the next slot opens.
- **Don't busy-wait or retry on your own** — `crq wait` already handles the waiting, ordering, and
  rate-limit backoff. Call it once per review round.
- **Setup check:** if `crq` says `CRQ_REPO is not set`, run the Quick Start above (install + `crq init`)
  once, then retry.

A drop-in **[Claude Code skill](skills/coderabbit-queue/SKILL.md)** is included — copy
`skills/coderabbit-queue/` into your agent's skills directory and it'll know when and how to use `crq`.

---

## Troubleshooting

| Symptom | Fix |
|---------|-----|
| `crq status` shows `source: warning` and never `calibrate` | CodeRabbit isn't answering on the calibration PR — make sure it's **installed on the gate repo**. crq still works via the warning-scan fallback. |
| A PR is stuck "in flight" forever | `crq gc` clears it after `CRQ_INFLIGHT_TIMEOUT`; or `crq cancel <repo> <pr>`. |
| "could not acquire lock" | A crashed holder. `crq unlock` shows who holds it; `crq unlock --force` breaks it. |
| Reviews fire slower than expected | That's the point — you're rate-limited. `crq status` shows the real countdown from CodeRabbit. |

## How the lock works (for the curious)

GitHub's `POST /git/refs` atomically creates a ref *only if it doesn't already exist* — a genuine
distributed mutex. The lock ref points at a tiny commit whose message carries a random **nonce**;
after creating the ref, crq reads it back and checks the nonce, so even under a thundering herd
exactly one agent ever owns the lock (and on release it only deletes a lock that's still *its own*).
A crashed holder is reclaimed after `CRQ_LOCK_TTL`. Every queue change happens under this lock, so
the dashboard never has lost updates, and FIFO order uses a monotonic counter (not timestamps) so
it's immune to clock differences between machines.

## License

MIT © Kristoffer Risanger. See [LICENSE](LICENSE). Contributions welcome.
