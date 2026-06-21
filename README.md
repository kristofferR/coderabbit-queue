# crq — CodeRabbit review queue

**An account-wide, cross-host coordinator so many parallel coding agents don't compete for one shared CodeRabbit PR-review rate limit.**

If you run several autonomous agents (Claude Code, etc.), each looping on its own
pull request and periodically posting `@coderabbitai review`, they will fight over
your **per-developer / per-organization** CodeRabbit quota. On the Pro plan at the
lowest adaptive refill rate that quota is effectively *one review at a time, hours
apart* — so the agents (a) keep firing review requests while globally rate-limited
(noise), and (b) stampede the moment the window reopens.

`crq` fixes this. Agents call `crq wait <repo> <pr>` instead of posting the review
command directly. `crq` then:

- knows the **true, account-wide** rate-limit state (it *asks* CodeRabbit, it doesn't
  guess from one PR's stale countdown),
- **queues** review requests and fires them **FIFO, one at a time**, the instant the
  window opens — no stampede,
- keeps a **live GitHub issue dashboard** of the queue and status,
- works **across machines** (laptop, WSL, a server) because all shared state lives on
  GitHub.

There is no off-the-shelf tool for this; community "rate-limit" helpers are
single-agent. `crq` is built for the multi-agent, multi-host case.

---

## How it works

All coordination state lives in one small **gate repo** on GitHub (private is fine):

| Piece | What it is |
|-------|------------|
| **Lock** | An atomic git ref (`refs/heads/crq-lock`). Created via the GitHub *create-if-not-exists* ref API; the holder is proven by a nonce in the lock commit. This is a real distributed mutex — every state mutation and every review-fire happens under it, so there are no lost updates and **at most one** review is ever fired at a time across all hosts. |
| **Dashboard issue** | Its body holds the queue/status as JSON (in a hidden HTML comment) plus a rendered table. The issue **title** shows a one-glance status. |
| **Calibration PR** | A throwaway draft PR where `crq` posts `@coderabbitai rate limit` to read your remaining quota + refill time **without consuming a review**. Auto-review is disabled on the gate repo so this PR costs nothing. |

The rate-limit reading is **calibration-first** (ask CodeRabbit, cached and throttled
so command volume stays tiny) and corroborated by scanning your open PRs for
CodeRabbit's "rate limited… available in X" warning comments. The max of the two is
the account-wide `blocked_until`.

FIFO order uses a monotonic integer `seq` assigned under the lock (not timestamps, so
it is immune to clock skew between hosts).

---

## Install

Requires [`gh`](https://cli.github.com/) (authenticated) and [`jq`](https://jqlang.github.io/jq/).

```bash
curl -fsSL https://raw.githubusercontent.com/kristofferR/coderabbit-queue/main/install.sh | bash
```

or manually:

```bash
git clone https://github.com/kristofferR/coderabbit-queue.git
install -m 0755 coderabbit-queue/crq ~/.local/bin/crq   # ensure ~/.local/bin is on PATH
```

## Setup

```bash
export CRQ_REPO=youruser/crq-state     # the gate repo crq will create/use (private OK)
crq init                               # creates the repo, dashboard issue, calibration PR
```

`crq init` prints the `export CRQ_*` lines to save (e.g. in `~/.config/crq/env`, which
`crq` sources automatically). Put the same config on every host that runs agents.

```bash
mkdir -p ~/.config/crq
crq init >> ~/.config/crq/env          # then trim it to just the export lines
```

## Usage

```bash
crq wait <repo> <pr>     # enqueue and BLOCK until OUR review is fired — use this in agent loops
crq enqueue <repo> <pr>  # add to the FIFO queue (idempotent)
crq pump                 # fire <=1 queued review if the window is open (safe to call anywhere)
crq status               # show the dashboard
crq refresh              # force a fresh rate-limit calibration, then show status
crq cancel <repo> <pr>   # remove from queue / clear a stuck in-flight entry
crq gc                   # drop closed/merged PRs; clear a timed-out in-flight
crq unlock [--force]     # inspect / break the global lock
```

`<repo>` is `owner/name`, `<pr>` is the number.

### Integrate into an agent review loop

Replace any direct `@coderabbitai review` posting with a single `crq wait` call:

```bash
while review_cycle_should_continue; do
  crq wait "$REPO" "$PR"          # blocks until OUR review is fired — FIFO, no competition
  # crq has now posted the review command exactly once, only while unblocked.
  wait_for_review_to_land "$REPO" "$PR"   # your logic: read CodeRabbit's review
  apply_fixes_and_push                    # do the work, then loop to re-enqueue
done
```

See [`examples/agent-loop.sh`](examples/agent-loop.sh) for a fuller version.

---

## Configuration

All via environment (or `~/.config/crq/env`, sourced automatically):

| Var | Default | Meaning |
|-----|---------|---------|
| `CRQ_REPO` | *(required)* | gate repo holding lock ref + dashboard issue + calibration PR |
| `CRQ_ISSUE` | from `init` | dashboard issue number |
| `CRQ_CAL_PR` | from `init` | calibration PR number |
| `CRQ_SCOPE` | owner of `CRQ_REPO` | comma-list of owners/orgs = rate-limit buckets (one gate each) |
| `CRQ_CALIBRATE_TTL` | `120` | max age (s) of the cached calibration before re-asking |
| `CRQ_LOCK_TTL` | `180` | seconds before a held lock is considered stale and stealable |
| `CRQ_MIN_INTERVAL` | `90` | minimum seconds between fires |
| `CRQ_INFLIGHT_TIMEOUT` | `900` | backstop to clear a stuck in-flight review |
| `CRQ_POLL` | `15` | `crq wait` poll interval (+ jitter) |
| `CRQ_WAIT_TIMEOUT` | `0` | give up `wait` after N seconds (0 = never) |
| `CRQ_WARNING_SCAN` | `1` | also corroborate via open-PR warning comments (`0` to disable) |
| `CRQ_BOT` | `coderabbitai[bot]` | review-bot login to watch |
| `CRQ_REVIEW_CMD` | `@coderabbitai review` | command posted to fire a review |
| `CRQ_RATELIMIT_CMD` | `@coderabbitai rate limit` | non-consuming quota-check command |
| `CRQ_RL_MARKER` | `rate limited by coderabbit.ai` | substring identifying a rate-limit warning comment |

### Other review bots / multiple orgs

`crq` is bot-agnostic: point `CRQ_BOT`, `CRQ_REVIEW_CMD`, `CRQ_RATELIMIT_CMD`, and
`CRQ_RL_MARKER` at any bot with a similar command surface.

CodeRabbit's quota is per **organization**, so PRs in different orgs draw from
*separate* buckets. Run one independent gate per org (a distinct `CRQ_REPO` /
`CRQ_SCOPE`); don't lump unrelated orgs into one queue or you'll serialize reviews
that don't actually compete.

---

## Notes & caveats

- **`@coderabbitai rate limit` is PR-scoped and non-consuming**, but subject to a
  commands-per-minute cap and a chat rate limit — hence the centralized, cached
  `CRQ_CALIBRATE_TTL` throttle (only the lock holder ever asks; everyone else reads the
  cached value).
- The exact wording of CodeRabbit's `rate limit` reply and its review-acknowledgement
  reaction may change; `crq`'s parser and in-flight detector are deliberately tolerant
  and fall back to a warning-scan + timeout. Tune `parse_quota` / `inflight_status` in
  `crq` if your account shows different wording.
- The gate repo must have CodeRabbit **auto-review disabled** (`crq init` commits a
  `.coderabbit.yaml` that does this) so the calibration PR never burns a real review.

## License

MIT © Kristoffer Risanger. See [LICENSE](LICENSE).
