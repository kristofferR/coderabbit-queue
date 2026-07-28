import { useEffect, useRef, useState } from "react";
import Markdown from "react-markdown";
import remarkGfm from "remark-gfm";
import type { Cost as CostView, Finding, PRView } from "./api";
import { BotIcon, BotMarks, Card, CommitLink, Empty, Pill, PRLink, RepoIcon } from "./ui";
import { ago, clock, countdown, elapsed, useNow } from "./time";
import { act } from "./actions";
import { Confirm } from "./Confirm";
import { AutofixLog } from "./AutofixLog";

const SEV_ORDER = ["critical", "major", "potential", "minor", "unknown"];
const SEV_TONE: Record<string, "bad" | "warn" | "mut"> = {
  critical: "bad",
  major: "warn",
  potential: "warn",
  minor: "mut",
  unknown: "mut",
};

export function PRDetailPage({ repo, pr, rev }: { repo: string; pr: number; rev?: number }) {
  const now = useNow();
  const [view, setView] = useState<PRView | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [refreshing, setRefreshing] = useState(false);
  const [action, setAction] = useState<{ finding: Finding; kind: "resolve" | "decline" | "dismiss" } | null>(null);
  const [busy, setBusy] = useState(false);
  const [actErr, setActErr] = useState<string | null>(null);
  // Round-level actions, kept apart from the finding-level ones above: they
  // act on different things and one being in flight should not disable the
  // other.
  const [pending, setPending] = useState<"hold" | "cancel" | null>(null);
  const [acting, setActing] = useState(false);
  const [roundErr, setRoundErr] = useState<string | null>(null);
  const [findingQuery, setFindingQuery] = useState("");
  const [findingBot, setFindingBot] = useState("all");

  const runRound = async (kind: "hold" | "unhold" | "cancel", reason = "") => {
    setActing(true);
    setRoundErr(null);
    try {
      await act(kind, { repo, pr, reason });
      setPending(null);
      await load(true);
    } catch (e) {
      setRoundErr((e as Error).message);
    } finally {
      setActing(false);
    }
  };

  const runAction = async (reason: string) => {
    if (!action) return;
    setBusy(true);
    setActErr(null);
    try {
      const f = action.finding;
      if (action.kind === "resolve") {
        await act("resolve", { repo, pr, thread_ids: [f.thread_id!] });
      } else if (action.kind === "decline") {
        await act("decline", { repo, pr, thread_ids: [f.thread_id!], reason });
      } else {
        await act("dismiss", { repo, pr, finding_ids: [f.id], reason });
      }
      setAction(null);
      load(true); // the finding list is GitHub's answer, so re-observe
    } catch (e) {
      setActErr((e as Error).message);
    } finally {
      setBusy(false);
    }
  };

  const load = (refresh = false) => {
    setRefreshing(refresh);
    return fetch(`/api/pr/${repo}/${pr}${refresh ? "?refresh=1" : ""}`)
      .then((r) => (r.ok ? r.json() : Promise.reject(new Error(`HTTP ${r.status}`))))
      .then((v: PRView) => {
        setView(v);
        setError(null);
      })
      .catch((e: Error) => setError(e.message))
      .finally(() => setRefreshing(false));
  };

  // The state layer is cheap, but the observation behind it costs several
  // GitHub calls — so this loads once on open and only re-fetches on request.
  useEffect(() => {
    load();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [repo, pr]);

  // …and again whenever the stream says the state ref MOVED. A page left open
  // while its round fires, completes, is held, or starts a fix session showed a
  // queued round and a dead countdown indefinitely otherwise. This is the cheap
  // half only: no `refresh`, so the server answers from its current state and
  // keeps serving the cached observation for this head.
  //
  // The first revision seen is recorded rather than loaded on: the mount above
  // is already fetching it, and a second concurrent request would miss the
  // observation cache too and pay for a whole second look at GitHub.
  const loadedRev = useRef<number | undefined>(undefined);
  useEffect(() => {
    if (rev === undefined || loadedRev.current === undefined) {
      loadedRev.current = rev;
      return;
    }
    if (rev === loadedRev.current) return;
    loadedRev.current = rev;
    load();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [rev]);

  if (error) {
    return (
      <main className="mx-auto max-w-[1180px] px-6 py-16 text-mut">
        Could not load {repo}#{pr}: {error}
      </main>
    );
  }
  if (!view) {
    return (
      <main className="mx-auto max-w-[1180px] px-6 py-16 text-mut">Reading state…</main>
    );
  }

  const findings = view.observed?.findings ?? [];
  const botCounts = findings.reduce<Record<string, number>>((counts, finding) => {
    counts[finding.bot] = (counts[finding.bot] ?? 0) + 1;
    return counts;
  }, {});
  const query = findingQuery.trim().toLowerCase();
  const visibleFindings = findings.filter(
    (finding) =>
      (findingBot === "all" || finding.bot === findingBot) &&
      (query === "" ||
        `${finding.title} ${finding.body ?? ""} ${finding.path ?? ""} ${finding.bot}`
          .toLowerCase()
          .includes(query)),
  );
  const grouped = SEV_ORDER.map((sev) => ({
    sev,
    items: visibleFindings.filter((f) => (f.severity || "unknown") === sev),
  })).filter((g) => g.items.length > 0);

  return (
    <main className="mx-auto max-w-[1180px] px-6 pt-4.5 pb-16">
      <div className="mb-2 text-[12.5px] text-faint">
        <a href="#/" className="text-acc hover:underline">
          Overview
        </a>{" "}
        / {repo} / #{pr}
      </div>

      <div className="mb-3.5 rounded-[10px] border border-edge bg-card px-5 py-3.5 shadow-card">
        <div className="flex flex-wrap items-center gap-3">
          <RepoIcon repo={repo} size={24} />
          <h1 className="text-[18px] font-[650] tracking-tight">
            <PRLink repo={repo} pr={pr} />
          </h1>
          {view.title && (
            <span className="max-w-[46ch] truncate text-[13.5px] text-mut" title={view.title}>
              {view.title}
            </span>
          )}
          {view.round ? (
            <Pill tone={view.round.phase === "reviewing" ? "ok" : "acc"}>{view.round.phase}</Pill>
          ) : (
            <Pill tone="mut">no active round</Pill>
          )}
          {view.round?.fixing && <Pill tone="ok">fixing</Pill>}
          {view.hold && <Pill tone="bad">held</Pill>}
          {view.observed && (
            <Pill tone={view.observed.converged ? "ok" : "warn"}>
              {view.observed.converged ? "converged" : `${findings.length} open`}
            </Pill>
          )}
          <span className="ml-auto flex flex-wrap items-center gap-2">
            {/* A pull request opened by link was read-only: the two actions
                that matter existed only as hover buttons on an Overview row,
                which is not where you are when you have just read its
                findings. */}
            {view.hold ? (
              <button
                type="button"
                disabled={acting}
                onClick={() => void runRound("unhold")}
                className="rounded-lg border border-edge px-3 py-1.5 text-[13px] font-semibold text-mut disabled:opacity-45"
              >
                Unhold
              </button>
            ) : (
              <button
                type="button"
                disabled={acting}
                onClick={() => setPending("hold")}
                className="rounded-lg border border-edge px-3 py-1.5 text-[13px] font-semibold text-mut disabled:opacity-45"
              >
                Hold…
              </button>
            )}
            {view.round && (
              <button
                type="button"
                disabled={acting}
                onClick={() => setPending("cancel")}
                className="rounded-lg border border-bad-edge px-3 py-1.5 text-[13px] font-semibold text-bad disabled:opacity-45"
              >
                Cancel round…
              </button>
            )}
            <button
              type="button"
              onClick={() => load(true)}
              disabled={refreshing}
              className="rounded-lg border border-edge px-3 py-1.5 text-[13px] font-semibold text-mut disabled:opacity-45"
            >
              {refreshing ? "Refreshing…" : "Refresh"}
            </button>
          </span>
        </div>
        {view.round && (
          <div className="mt-2 flex flex-wrap gap-4 text-[12.5px] text-mut">
            <span>
              head <CommitLink repo={repo} sha={view.round.head} />
            </span>
            <span>enqueued {clock(view.round.enqueued_at)}</span>
            {view.round.fired_at && (
              <span>
                fired {clock(view.round.fired_at)} · attempt {view.round.attempts || 1}
              </span>
            )}
            {view.round.host && <span className="font-mono">{view.round.host}</span>}
          </div>
        )}
        {view.round?.next && <div className="mt-1.5 text-[13px] text-mut">{view.round.next}</div>}
      </div>

      {view.hold && (
        <div className="mb-3.5 rounded-[10px] border border-bad-edge border-l-4 border-l-bad bg-bad-bg px-4 py-2.5 text-[13.5px]">
          <b>Held</b> by {view.hold.by} {ago(view.hold.at, now)}
          {view.hold.reason && <span className="ml-2 text-mut">“{view.hold.reason}”</span>}
          <span className="ml-2 text-faint">— crq will not review it until this is lifted.</span>
        </div>
      )}

      {pending && (
        <Confirm
          title={pending === "hold" ? `Hold ${repo}#${pr}?` : `Cancel the round on ${repo}#${pr}?`}
          danger={pending === "cancel"}
          confirmLabel={pending === "hold" ? "Hold it" : "Cancel the round"}
          needsReason={pending === "hold"}
          reasonLabel="Why is it held"
          busy={acting}
          error={roundErr}
          body={
            pending === "hold" ? (
              "No round is enqueued or fired here until the hold is lifted. Reviews already in flight finish."
            ) : (
              <>
                The current round is abandoned. Auto-review may enqueue this pull request again on its
                next pass, at whatever head it then has.
                {view.round?.phase === "fired" && (
                  <p className="mt-2 text-warn">
                    This round holds the fire slot; cancelling releases it for the next pull request.
                  </p>
                )}
              </>
            )
          }
          onConfirm={(reason) => void runRound(pending, reason)}
          onCancel={() => setPending(null)}
        />
      )}

      {action && (
        <Confirm
          title={
            action.kind === "resolve"
              ? "Resolve this thread?"
              : action.kind === "decline"
                ? "Decline this finding?"
                : "Dismiss this finding?"
          }
          body={
            action.kind === "resolve" ? (
              <>
                Marks the review thread resolved on GitHub, where it can be reopened. Use this when the
                finding has actually been handled.
              </>
            ) : action.kind === "decline" ? (
              <>
                Posts your reasoning as a reply on the thread and resolves it. crq reads the bot's answer
                back, so a withdrawal or a stand-by-it becomes part of the record.
              </>
            ) : (
              <>
                For findings GitHub gives no way to close. It is recorded against{" "}
                <b>this head only</b> — a new head may report it again.
              </>
            )
          }
          confirmLabel={action.kind === "resolve" ? "Resolve" : action.kind === "decline" ? "Decline" : "Dismiss"}
          needsReason={action.kind !== "resolve"}
          reasonLabel={action.kind === "decline" ? "Why you disagree (posted to the PR)" : "Why (kept in state)"}
          busy={busy}
          error={actErr}
          onConfirm={runAction}
          onCancel={() => {
            setAction(null);
            setActErr(null);
          }}
        />
      )}

      <div className="grid grid-cols-[minmax(0,1fr)_360px] items-start gap-4 max-[1150px]:grid-cols-[minmax(0,1fr)]">
        <div>
          <div className="mb-3.5 flex flex-wrap items-center gap-2.5 rounded-lg border border-acc-edge bg-acc-bg px-3.5 py-2 text-[12.5px] text-mut">
            {view.observed ? (
              <>
                <b className="text-acc">Observed {ago(view.observed.checked_at, now)}</b>
                <span>
                  at <CommitLink repo={repo} sha={view.observed.head} /> · reviewed by{" "}
                  {Object.entries(view.observed.reviewed_by ?? {})
                    .filter(([, v]) => v)
                    .map(([k]) => k)
                    .join(", ") || "nobody yet"}
                </span>
              </>
            ) : (
              <span>
                {view.observe_error
                  ? `Could not reach GitHub — ${view.observe_error}`
                  : "Reading findings from GitHub…"}
              </span>
            )}
          </div>

          <Card
            title="Findings"
            count={
              view.observed
                ? `${visibleFindings.length === findings.length ? findings.length : `${visibleFindings.length} of ${findings.length}`} open${view.observed.dismissed ? ` · ${view.observed.dismissed} dismissed` : ""}`
                : "—"
            }
          >
            {!view.observed ? (
              <Empty>Findings need a GitHub read; the round above came from state.</Empty>
            ) : findings.length === 0 ? (
              <div className="px-[18px] py-6 text-center">
                <div className="text-[15px] font-semibold">No open findings</div>
                <p className="mt-1 text-[13px] text-mut">
                  {view.observed.converged
                    ? "Every required reviewer finished and nothing is blocking."
                    : "Nothing actionable is outstanding at this head."}
                </p>
              </div>
            ) : (
              <div className="px-[18px] pb-3">
                <div className="sticky top-0 z-10 -mx-[18px] mb-2 border-b border-[#EEF0F3] bg-card/95 px-[18px] py-2.5 backdrop-blur">
                  <div className="flex flex-wrap items-center gap-2">
                    <input
                      value={findingQuery}
                      onChange={(event) => setFindingQuery(event.target.value)}
                      placeholder="Filter title, detail, file…"
                      aria-label="Filter findings"
                      className="min-w-[220px] flex-1 rounded-lg border border-edge bg-white px-2.5 py-1.5 text-[12.5px]"
                    />
                    <button
                      type="button"
                      onClick={() => setFindingBot("all")}
                      className={`rounded-full border px-2.5 py-1 text-[11.5px] font-semibold ${
                        findingBot === "all" ? "border-ink bg-ink text-white" : "border-edge text-mut"
                      }`}
                    >
                      All {findings.length}
                    </button>
                    {Object.entries(botCounts)
                      .sort((a, b) => b[1] - a[1])
                      .map(([bot, count]) => (
                        <button
                          key={bot}
                          type="button"
                          onClick={() => setFindingBot(bot)}
                          className={`flex items-center gap-1.5 rounded-full border px-2.5 py-1 text-[11.5px] font-semibold ${
                            findingBot === bot ? "border-ink bg-ink text-white" : "border-edge text-mut"
                          }`}
                        >
                          <BotIcon login={bot} name={bot} size={14} />
                          {shortBot(bot)} {count}
                        </button>
                      ))}
                    {(findingBot !== "all" || query !== "") && (
                      <button
                        type="button"
                        onClick={() => {
                          setFindingBot("all");
                          setFindingQuery("");
                        }}
                        className="text-[11.5px] font-semibold text-acc hover:underline"
                      >
                        Clear
                      </button>
                    )}
                  </div>
                </div>
                {visibleFindings.length === 0 && (
                  <Empty>No open findings match this filter.</Empty>
                )}
                {grouped.map((g) => (
                  <div key={g.sev}>
                    <div className="pt-2.5 pb-1 text-[11px] font-semibold tracking-[0.05em] text-faint uppercase">
                      {g.sev} · {g.items.length}
                    </div>
                    {g.items.map((f) => (
                      <FindingRow key={f.id} f={f} onAct={(a) => setAction({ finding: f, kind: a })} />
                    ))}
                  </div>
                ))}
              </div>
            )}
          </Card>
        </div>

        <aside>
          {view.round?.fixing && (
            <Card title="Fix session" count="live">
              <div className="px-[18px] pb-3.5 text-[13px]">
                <Pill tone="ok">Running · {elapsed(view.round.fixing.since, now)}</Pill>
                <div className="mt-1.5 text-mut">
                  {view.round.fixing.host} · attempt {view.round.fixing.attempt}
                  {view.round.fixing.max_attempts ? ` of ${view.round.fixing.max_attempts}` : ""}
                  {view.round.fixing.findings
                    ? ` · working through ${view.round.fixing.findings} finding(s)`
                    : ""}
                  {view.round.fixing.heartbeat && ` · heartbeat ${clock(view.round.fixing.heartbeat)}`}
                </div>
                {view.round.fixing.log && (
                  <div className="mt-1 font-mono text-[11.5px] break-all text-faint">
                    {view.round.fixing.log}
                  </div>
                )}
                <div className="mt-2">
                  <AutofixLog repo={repo} pr={pr} />
                </div>
                <p className="mt-1.5 text-[11.5px] text-faint">
                  While a session holds this round the queue leaves it alone; the claim is released
                  when the session pushes or exits.
                </p>
              </div>
            </Card>
          )}

          {view.round && (
            <Card title="Round" count={view.round.head}>
              <div className="px-[18px] pb-3 text-[13px]">
                <KV k="Phase" v={view.round.phase} />
                <KV k="Enqueued" v={clock(view.round.enqueued_at)} />
                {view.round.fired_at && <KV k="Fired" v={clock(view.round.fired_at)} />}
                {view.round.deadline && (
                  <KV k="Deadline" v={`${countdown(view.round.deadline, now)} · ${clock(view.round.deadline)}`} />
                )}
                {view.round.retry_at && <KV k="Retries after" v={clock(view.round.retry_at)} />}
                {view.round.co_only && <KV k="Scope" v="co-reviewers only — spends no quota" />}
                {view.round.note && <KV k="Note" v={`“${view.round.note}”`} />}
              </div>
              <div className="border-t border-[#EEF0F3] px-[18px] py-2.5">
                <div className="mb-1.5 text-[11px] font-medium tracking-[0.06em] text-faint uppercase">
                  Reviewers
                </div>
                <BotMarks bots={view.round.bots} />
              </div>
            </Card>
          )}

          {view.observed?.converged && (
          <Card title="Verdict">
            <div className="px-[18px] pb-3.5 pt-1">
              <p className="text-[13.5px]">
                <b className="text-ok">Nothing left to do</b> — {view.observed.reason || "every required reviewer finished"}.
              </p>
              <div className="mt-2 text-[12.5px] text-mut">
                <div className="mb-1 text-[11px] font-medium tracking-[0.06em] text-faint uppercase">
                  Reviewed by
                </div>
                <span className="flex flex-wrap items-center gap-2.5">
                  {Object.entries(view.observed.reviewed_by ?? {}).map(([bot, done]) => (
                    <span key={bot} className="flex items-center gap-1.5">
                      <BotIcon login={bot} name={bot} size={18} />
                      <span className={done ? "text-ok" : "text-faint"}>{done ? "✓" : "pending"}</span>
                    </span>
                  ))}
                </span>
              </div>
              <p className="mt-2.5 text-[12.5px] text-faint">
                What happens next: nothing. Merge when you are ready. Push another commit and a fresh
                round is enqueued for the new head — a converged round is the record that THIS head
                was reviewed, not that the pull request is finished.
              </p>
            </div>
          </Card>
        )}

        {(view.cost || view.cost_error) && <CostCard cost={view.cost} error={view.cost_error} />}

          {(view.round?.dismissed?.length ?? 0) > 0 && (
            <Card title="Dismissed" count={view.round!.dismissed!.length}>
              <div className="px-[18px] pb-3 text-[12.5px] text-mut">
                {view.round!.dismissed!.map((d) => (
                  <div key={d.id} className="border-b border-[#EEF0F3] py-1.5 last:border-none">
                    “{d.reason}”
                    <div className="font-mono text-[11px] text-faint">{d.id.slice(0, 12)}</div>
                  </div>
                ))}
                <p className="pt-2 text-faint">Dismissals apply to this head only.</p>
              </div>
            </Card>
          )}

          <Card title="Round history" count={`${view.history.length} head(s)`}>
            <div className="px-[18px] pb-3">
              {view.history.length === 0 && <Empty>No round has run for this PR.</Empty>}
              {view.history.map((h, i) => (
                <div key={`${h.head}-${i}`} className="border-b border-[#EEF0F3] py-2 text-[13px] last:border-none">
                  <div className="flex items-center gap-2">
                    <CommitLink repo={repo} sha={h.head} />
                    {h.current && <Pill tone="acc">current</Pill>}
                  </div>
                  <div className="text-[12px] text-faint">
                    {h.outcome}
                    {h.note && ` — ${h.note}`}
                    {h.at && ` · ${clock(h.at)}`}
                  </div>
                </div>
              ))}
            </div>
          </Card>
        </aside>
      </div>
    </main>
  );
}

const DISMISSIBLE = new Set(["review_body", "review_prompt", "review_skipped", "issue_comment"]);

function FindingRow({
  f,
  onAct,
}: {
  f: Finding;
  onAct: (kind: "resolve" | "decline" | "dismiss") => void;
}) {
  const [open, setOpen] = useState(false);
  const sev = f.severity || "unknown";
  const threaded = Boolean(f.thread_id);
  const content = open ? findingContent(f) : null;
  // Only threadless findings can be dismissed — a threaded one is closed by
  // resolving or declining it, which is visible on the pull request.
  const dismissible = !threaded && DISMISSIBLE.has(f.source ?? "");
  return (
    <div className="mb-2 overflow-hidden rounded-lg border border-edge bg-white shadow-[0_1px_1px_rgba(18,24,40,0.025)]">
      <button
        type="button"
        onClick={() => setOpen(!open)}
        aria-expanded={open}
        className="flex w-full items-start gap-3 px-3.5 py-2.5 text-left hover:bg-[#F7F8FA]"
      >
        <span className="mt-0.5 text-[12px] text-faint" aria-hidden="true">
          {open ? "▾" : "▸"}
        </span>
        <span className="min-w-0 flex-1">
          <FindingTitle title={f.title || "(untitled finding)"} />
          <span className="mt-1 flex flex-wrap items-center gap-1.5">
            {f.category && <Pill tone="mut">{f.category}</Pill>}
            <Pill tone={SEV_TONE[sev] ?? "mut"}>{f.scale || sev}</Pill>
            {f.effort && <Pill tone="acc">{f.effort}</Pill>}
          </span>
          <span className="mt-1 flex flex-wrap items-center gap-x-2 gap-y-1 text-[11.5px] text-faint">
            <span className="flex items-center gap-1.5 font-medium text-mut">
              <BotIcon login={f.bot} name={f.bot} size={14} />
              {shortBot(f.bot)}
            </span>
            {f.path && (
              <span className="max-w-full truncate font-mono">
                {f.path}
                {f.line ? `:${f.line}` : ""}
              </span>
            )}
            {f.thread_id && <span>open thread</span>}
          </span>
        </span>
      </button>
      {content && (
        <div className="border-t border-[#EEF0F3] bg-[#FBFCFD] px-4 py-3 text-[13px] text-mut">
          <div className="rounded-lg border border-edge bg-white px-4 py-2.5 text-[13.5px] text-ink">
            <ReviewMarkdown body={content.description || "No detail was captured for this finding."} />
          </div>
          {content.sections.length > 0 && (
            <div className="mt-2.5 space-y-2">
              {content.sections.map((section, index) => (
                <details
                  key={`${section.title}-${index}`}
                  className="group rounded-lg border border-edge bg-white"
                >
                  <summary className="cursor-pointer list-none px-3 py-2 text-[12.5px] font-semibold text-mut marker:hidden">
                    <span className="mr-2 inline-block text-faint transition-transform group-open:rotate-90">
                      ▸
                    </span>
                    {section.title}
                  </summary>
                  <div className="border-t border-[#EEF0F3] px-4 py-2.5 text-[12.5px]">
                    <ReviewMarkdown body={section.body} />
                  </div>
                </details>
              ))}
            </div>
          )}
          <div className="mt-3 flex flex-wrap items-center gap-2">
            {threaded && (
              <>
                <button
                  type="button"
                  onClick={() => onAct("resolve")}
                  className="rounded-lg border border-edge px-3 py-1 text-[12.5px] font-semibold text-mut hover:border-edge2"
                >
                  Resolve thread
                </button>
                <button
                  type="button"
                  onClick={() => onAct("decline")}
                  className="rounded-lg border border-edge px-3 py-1 text-[12.5px] font-semibold text-mut hover:border-edge2"
                >
                  Decline…
                </button>
              </>
            )}
            {dismissible && (
              <button
                type="button"
                onClick={() => onAct("dismiss")}
                className="rounded-lg border border-edge px-3 py-1 text-[12.5px] font-semibold text-mut hover:border-edge2"
              >
                Dismiss…
              </button>
            )}
            {!threaded && !dismissible && (
              <span className="text-[12px] text-faint">
                No thread to resolve, and this source cannot be dismissed — it clears when the finding
                stops being reported.
              </span>
            )}
            {f.url && (
              <a href={f.url} target="_blank" rel="noreferrer" className="ml-auto text-[12.5px] text-acc hover:underline">
                View on GitHub ↗
              </a>
            )}
          </div>
        </div>
      )}
    </div>
  );
}

type FindingSection = { title: string; body: string };
type FindingContent = { description: string; sections: FindingSection[] };

const DETAIL_RE = /<details(?:\s[^>]*)?>\s*<summary(?:\s[^>]*)?>(.*?)<\/summary>([\s\S]*?)<\/details>/gi;
const HTML_COMMENT_RE = /<!--[\s\S]*?-->/g;

// Review bodies are not one shared dialect. CodeRabbit has a rubric plus
// collapsible analysis/fixes, while Codex encodes priority in a badge and puts
// its prose directly after the title. Keep the reviewer-specific cleanup here:
// adding a bot should not subtly change every existing bot's presentation.
export function findingContent(f: Finding): FindingContent {
  const body = (f.body || "").replace(/\r\n/g, "\n");
  const sections: FindingSection[] = [];
  let description = body.replace(DETAIL_RE, (_match, summary: string, detail: string) => {
    sections.push({
      title: plainLabel(summary) || "More detail",
      body: cleanReviewFragment(detail),
    });
    return "\n";
  });

  const bot = f.bot.toLowerCase().replace(/\[bot\]$/, "");
  if (bot === "coderabbitai") {
    description = description.replace(
      /^\s*[^|\n]*(?:Correctness|Maintainability|Security|Performance|Reliability|Quality)[^|\n]*\|[^|\n]*\|[^\n]*\n?/im,
      "",
    );
    description = stripRenderedTitle(description, f.title);
  } else if (bot === "chatgpt-codex-connector") {
    // Inline comments begin with the badge. Review-body findings add a generic
    // "Codex Review" heading and a raw blob URL first. Neither belongs in the
    // explanation; preserve useful source/instruction links in one collapsed
    // References panel instead.
    description = description.replace(/^\s*#{1,6}\s*(?:💡\s*)?Codex Review\s*/im, "");
    const references: string[] = [];
    description = description.replace(
      /^\s*(https:\/\/github\.com\/\S+\/blob\/\S+)\s*$/gim,
      (_match, url: string) => {
        references.push(`[Source location](${url})`);
        return "";
      },
    );
    description = description.replace(
      /^\s*(AGENTS\.md reference:\s*\[[^\n]+]\([^)]+\))\s*$/gim,
      (_match, reference: string) => {
        references.push(reference);
        return "";
      },
    );
    description = description.replace(
      /^\s*\*\*<sub><sub>!\[[^\]]*]\([^)]*\)<\/sub><\/sub>\s*[^*\n]*\*\*\s*/im,
      "",
    );
    description = stripRenderedTitle(description, f.title);
    description = description.replace(/^\s*Useful\?\s*React with[\s\S]*$/im, "");
    const usefulSections = sections.filter(
      (section) => !/About Codex in GitHub/i.test(section.title),
    );
    sections.splice(0, sections.length, ...usefulSections);
    if (references.length > 0) {
      sections.push({ title: "References", body: references.join("\n\n") });
    }
  } else if (bot === "cursor") {
    description = description.replace(/^\s*\*\*(?:Critical|High|Medium|Low)\s+Severity\*\*\s*/im, "");
    description = stripRenderedTitle(description, f.title);
  } else if (bot === "macroscopeapp") {
    description = description.replace(
      /^\s*\W*\*\*(?:Critical|High|Medium|Low)\*\*[^\n]*\n?/im,
      "",
    );
    description = stripRenderedTitle(description, f.title);
  }

  return {
    description: cleanReviewFragment(description),
    sections: sections.filter((section) => section.body !== ""),
  };
}

function stripRenderedTitle(body: string, title: string) {
  if (!title) return body;
  const escaped = title
    .replace(/[.*+?^${}()|[\]\\]/g, "\\$&")
    .replace(/\s+/g, "\\s+");
  const titleRE = new RegExp(
    `^\\s*(?:#{1,6}\\s+|\\*\\*|__)?${escaped}[.!]?(?:\\*\\*|__)?\\s*`,
    "i",
  );
  return body.replace(titleRE, "");
}

function cleanReviewFragment(body: string) {
  return body
    .replace(HTML_COMMENT_RE, "")
    .replace(/<\/?(?:sub|sup)>/gi, "")
    .replace(/^\s*---+\s*$/gm, "")
    .replace(/\n{3,}/g, "\n\n")
    .trim();
}

function plainLabel(markup: string) {
  return markup
    .replace(/<[^>]+>/g, "")
    .replace(/!\[[^\]]*]\([^)]*\)/g, "")
    .replace(/[*_`]/g, "")
    .replace(/\s+/g, " ")
    .trim();
}

function shortBot(login: string) {
  const key = login.toLowerCase().replace(/\[bot\]$/, "");
  if (key === "coderabbitai") return "CodeRabbit";
  if (key === "chatgpt-codex-connector") return "Codex";
  if (key === "cursor") return "Bugbot";
  if (key === "macroscopeapp") return "Macroscope";
  return login.replace(/\[bot\]$/, "");
}

function FindingTitle({ title }: { title: string }) {
  return (
    <div className="text-[13.5px] leading-5 font-[600] text-ink">
      <Markdown
        components={{
          p: ({ children }) => <span>{children}</span>,
          code: ({ children }) => (
            <code className="rounded bg-[#EEF1F4] px-1 py-0.5 font-mono text-[12px]">{children}</code>
          ),
        }}
      >
        {title}
      </Markdown>
    </div>
  );
}

// Reviewer comments are Markdown with a small amount of presentation HTML
// around badges and collapsible analysis. Strip only those wrappers, then let a
// real Markdown parser render the useful structure. Raw HTML is not enabled,
// so review text cannot inject dashboard markup or scripts.
function ReviewMarkdown({ body }: { body: string }) {
  const markdown = body
    .replace(/<\/?(?:sub|sup|details)>/gi, "")
    .replace(/<summary>(.*?)<\/summary>/gis, "\n**$1**\n")
    .replace(/<br\s*\/?>/gi, "\n");
  return (
    <div className="space-y-2 leading-5">
      <Markdown
        remarkPlugins={[remarkGfm]}
        components={{
          h1: ({ children }) => <h3 className="mt-3 text-[14px] font-bold text-ink">{children}</h3>,
          h2: ({ children }) => <h3 className="mt-3 text-[14px] font-bold text-ink">{children}</h3>,
          h3: ({ children }) => <h3 className="mt-3 text-[13.5px] font-bold text-ink">{children}</h3>,
          p: ({ children }) => <p className="my-2">{children}</p>,
          ul: ({ children }) => <ul className="my-2 list-disc space-y-1 pl-5">{children}</ul>,
          ol: ({ children }) => <ol className="my-2 list-decimal space-y-1 pl-5">{children}</ol>,
          blockquote: ({ children }) => (
            <blockquote className="my-2 border-l-2 border-edge2 pl-3 text-faint">{children}</blockquote>
          ),
          pre: ({ children }) => (
            <pre className="my-2 overflow-x-auto rounded-lg border border-edge bg-[#F3F5F7] p-3 font-mono text-[11.5px] leading-5 text-ink">
              {children}
            </pre>
          ),
          code: ({ children }) => (
            <code className="rounded bg-[#EEF1F4] px-1 py-0.5 font-mono text-[11.5px] text-ink">{children}</code>
          ),
          a: ({ href, children }) => (
            <a href={href} target="_blank" rel="noreferrer" className="text-acc underline decoration-acc/30 underline-offset-2">
              {children}
            </a>
          ),
          img: ({ alt }) => (
            <span className="inline-flex rounded bg-[#EEF1F4] px-1.5 py-0.5 text-[10.5px] font-bold text-mut">
              {alt || "review badge"}
            </span>
          ),
          table: ({ children }) => (
            <div className="my-2 overflow-x-auto">
              <table className="w-full border-collapse text-[11.5px]">{children}</table>
            </div>
          ),
          th: ({ children }) => <th className="border border-edge bg-[#F3F5F7] px-2 py-1 text-left">{children}</th>,
          td: ({ children }) => <td className="border border-edge px-2 py-1 align-top">{children}</td>,
        }}
      >
        {markdown}
      </Markdown>
    </div>
  );
}

function KV({ k, v }: { k: string; v: string }) {
  return (
    <div className="flex justify-between gap-3 border-b border-[#EEF0F3] py-1.5 last:border-none">
      <span className="text-mut">{k}</span>
      <span className="text-right font-medium">{v}</span>
    </div>
  );
}

/**
 * What the next round would cost.
 *
 * Every figure here is an estimate and the card says so — including which
 * reviewers crq could not price at all, because a total that quietly omits one
 * reads as complete. The per-reviewer basis is shown rather than tucked into a
 * tooltip: a number whose reasoning you cannot see is a number you cannot check.
 */
function CostCard({ cost, error }: { cost?: CostView; error?: string }) {
  if (!cost) {
    return (
      <Card title="Estimated cost">
        <div className="px-[18px] pb-3.5 pt-1 text-[12.5px] text-faint">
          {error ? `Could not work out a price — ${error}` : "No price could be worked out."}
        </div>
      </Card>
    );
  }
  const money = (n: number) => `$${n.toFixed(2)}`;
  return (
    <Card title="Estimated cost" count={cost.summary}>
      <div className="px-[18px] pb-3 pt-1">
        <p className="text-[12.5px] text-faint">
          {cost.diff.additions + cost.diff.deletions} changed lines across{" "}
          {cost.diff.changed_files} file(s), for one more round at this head.
        </p>
        <table className="mt-2 w-full border-collapse">
          <tbody>
            {cost.reviewers.map((r) => (
              <tr key={r.bot} className="border-b border-[#EEF0F3] last:border-none">
                <td className="py-1.5 pr-2 align-top">
                  <BotIcon login={r.bot} name={r.bot} size={18} />
                </td>
                <td className="py-1.5 pr-2 align-top text-[12.5px] text-mut">{r.basis}</td>
                <td className="py-1.5 text-right align-top font-mono text-[12.5px] whitespace-nowrap">
                  {r.unknown ? (
                    <span className="text-warn">unknown</span>
                  ) : r.high === 0 ? (
                    <span className="text-faint">included</span>
                  ) : r.low === r.high ? (
                    money(r.high)
                  ) : (
                    `${money(r.low)}–${money(r.high)}`
                  )}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
        {(cost.unpriced?.length ?? 0) > 0 && (
          <p className="mt-2 rounded-lg border border-warn-edge bg-warn-bg px-2.5 py-1.5 text-[12px] text-warn">
            The total is a floor: {cost.unpriced!.join(", ")} could not be priced.
          </p>
        )}
        <p className="mt-2 text-[11.5px] text-faint">
          Estimate. Published prices last checked {cost.prices_checked_at}; Macroscope bills
          incrementally after its first review of a PR, so later rounds cost less than this.
        </p>
      </div>
    </Card>
  );
}
