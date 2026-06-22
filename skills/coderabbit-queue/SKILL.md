---
name: coderabbit-queue
description: Coordinate CodeRabbit (or any review-bot) PR-review requests across multiple parallel agents so they don't compete for one shared, account-wide rate limit. Use whenever you are about to post "@coderabbitai review" (or otherwise re-trigger a review bot) inside an autonomous PR-review loop, especially when several agents/PRs run at once. Provides the `crq` CLI (enqueue / FIFO pump / wait / live dashboard).
---

# coderabbit-queue (`crq`)

CodeRabbit's PR-review limit is **per-developer / per-organization (account-wide)**, not
per-PR. When multiple agents each loop on their own PR and post `@coderabbitai review`,
they fight over one quota: firing while globally rate-limited, then stampeding when the
window opens. `crq` serializes this — review requests are queued and fired **FIFO, one at
a time, only when the account is unblocked**, with a live GitHub-issue dashboard.

## The one rule

**Never post `@coderabbitai review` directly inside a review loop. Call `crq wait` instead.**

```bash
crq wait "$REPO" "$PR"
```

This enqueues `(repo, pr)`, blocks until it's *your* turn and the account is unblocked,
then posts the review command exactly once. It is safe to run for many PRs across many
machines simultaneously — `crq` guarantees no two reviews fire at the same time.

## Required prerequisite

**CodeRabbit auto-review must be OFF.** crq is pull-only — it controls *when* reviews happen by
posting `@coderabbitai review`. If auto-review is enabled, CodeRabbit reviews every push on its
own, bypassing the queue and consuming the shared rate limit, which defeats crq. Disable it
org-wide in the CodeRabbit dashboard (Settings → Review → Automatic Review) or per-repo via
`.coderabbit.yaml` (`reviews.auto_review.enabled: false`).

## Setup (once per account)

```bash
export CRQ_REPO=<youruser>/crq-state    # private gate repo
crq init                                # creates gate repo, dashboard issue, calibration PR
# save the printed CRQ_* exports into ~/.config/crq/env (crq sources it automatically)
```

If `crq` is not installed: `curl -fsSL https://raw.githubusercontent.com/kristofferR/coderabbit-queue/main/install.sh | bash`

> **Trust note:** `curl … | bash` runs remote code without local review — fine if you trust this
> repo, but for autonomous agents or stricter setups, prefer downloading `install.sh` (or the single
> `crq` script) and reading it before running, as the README's manual-install section shows.

## Commands you'll use

- `crq wait <repo> <pr>` — the loop primitive (enqueue + block until fired).
- `crq autoreview` — emulate native auto-review + incremental review (which are off) for ALL open
  PRs, rate-coordinated. Run as a background watcher. `--no-incremental` = first review only;
  `--once` = single pass. Use this to keep every PR reviewed hands-off without native auto-review.
- `crq status` — show the queue + rate-limit state (good for a quick check).
- `crq pump` — fire ≤1 queued review if the window is open (any agent can call it; the
  lock serializes it). `crq wait` calls this internally; you rarely call it directly.
- `crq cancel <repo> <pr>` — drop a PR you no longer want reviewed.
- `crq refresh` — force a fresh account-wide rate-limit reading, then print status.

## How it decides when to fire

- **Primary signal:** posts `@coderabbitai rate limit` (non-consuming) on a dedicated
  calibration PR and parses remaining reviews + refill time. Cached/throttled so command
  volume stays tiny.
- **Corroboration:** scans your open PRs for CodeRabbit's "available in X" warning.
- Fires the FIFO head only when `now ≥ blocked_until`, nothing is in flight, and a minimum
  interval has passed.

## When to use this skill

Use it any time you are operating an autonomous PR-review / CodeRabbit feedback loop —
particularly the multi-PR / multi-agent case. If you find yourself about to run
`gh pr comment ... "@coderabbitai review"`, route it through `crq wait` instead.
