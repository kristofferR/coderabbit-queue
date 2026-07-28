import { useEffect, useMemo, useState } from "react";
import type { Candidate, EnrollImpact, Snapshot } from "./api";
import { act } from "./actions";
import { Card, Empty, Pill, RepoIcon } from "./ui";
import { Confirm } from "./Confirm";
import { ago, useNow } from "./time";

/**
 * The repository picker.
 *
 * Adding a repository is one write — the enrollment record — and this screen
 * says so rather than pretending to be a wizard. Reviewers and autofix are left
 * at the fleet default deliberately: they are editable on the repository's own
 * page a click away, and asking four questions before a repository can be added
 * is how a two-second job becomes a form nobody finishes.
 *
 * The listing behind it is the one genuinely expensive call in the dashboard (a
 * multi-page REST walk per owner in CRQ_SCOPE), so it is cached server-side and
 * refreshed only on request.
 */
export function AddRepo({
  open,
  onClose,
  onSnapshot,
}: {
  open: boolean;
  onClose: () => void;
  onSnapshot?: (s: Snapshot) => void;
}) {
  const now = useNow(30000);
  const [rows, setRows] = useState<Candidate[] | null>(null);
  // Owners whose listing hit the per-owner bound. The rows below are then the
  // most recently pushed of them and not the whole set, and a filter that finds
  // nothing is not the same as a repository that is not there.
  const [truncated, setTruncated] = useState<string[]>([]);
  const [error, setError] = useState<string | null>(null);
  const [loading, setLoading] = useState(false);
  const [query, setQuery] = useState("");
  const [busy, setBusy] = useState<string | null>(null);
  // A save can succeed and still not be in force everywhere: on a mixed-version
  // fleet the response names the hosts too old to read the record. Discarding it
  // here let this screen report a fleet-wide decision that some of the fleet
  // cannot honour, which is the one thing the warning exists to prevent.
  const [warning, setWarning] = useState<string | null>(null);
  // The backlog contract: enrolling a repository with a dozen open pull
  // requests becomes a dozen metered reviews on the next pass. Nothing is
  // written until this has been shown and confirmed.
  const [pending, setPending] = useState<{ repo: string; impact?: EnrollImpact; error?: string } | null>(
    null,
  );

  const load = async (refresh = false) => {
    setLoading(true);
    setError(null);
    try {
      const res = await fetch(`/api/discover${refresh ? "?refresh=1" : ""}`, {
        headers: { "X-CRQ-Dashboard": "1" },
      });
      const body = await res.json();
      if (!res.ok) throw new Error(body.error ?? `HTTP ${res.status}`);
      setRows(body.repos as Candidate[]);
      setTruncated((body.truncated as string[] | null) ?? []);
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setLoading(false);
    }
  };

  useEffect(() => {
    if (open && rows === null && !loading) void load();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [open]);

  // Escape closes it, as every dialog should. No focus trap here: this one is
  // a picker with a long scrolling list, and trapping Tab in it would strand a
  // keyboard user who tabs past the last row.
  useEffect(() => {
    if (!open) return;
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape") onClose();
    };
    document.addEventListener("keydown", onKey);
    return () => document.removeEventListener("keydown", onKey);
  }, [open, onClose]);

  const shown = useMemo(() => {
    if (!rows) return [];
    const q = query.trim().toLowerCase();
    return q ? rows.filter((r) => r.repo.toLowerCase().includes(q)) : rows;
  }, [rows, query]);

  if (!open) return null;

  const preview = async (repo: string) => {
    setPending({ repo });
    try {
      const res = await fetch(`/api/enroll-preview?repo=${encodeURIComponent(repo)}`, {
        headers: { "X-CRQ-Dashboard": "1" },
      });
      const body = await res.json();
      if (!res.ok) throw new Error(body.error ?? `HTTP ${res.status}`);
      setPending({ repo, impact: body as EnrollImpact });
    } catch (e) {
      // A price we could not work out must not silently become a free-looking
      // Add: the dialog stays, and says why.
      setPending({ repo, error: (e as Error).message });
    }
  };

  const add = async (repo: string) => {
    setBusy(repo);
    setError(null);
    setWarning(null);
    try {
      const res = await act("enroll", { repo, enabled: true });
      onSnapshot?.(res.snapshot);
      setWarning(res.warning ? `${repo}: ${res.warning}` : null);
      setPending(null);
      // Reflect it locally too: the listing is cached server-side and would
      // otherwise keep offering an Add button for a repository already added.
      setRows(
        (cur) =>
          cur?.map((r) =>
            r.repo.toLowerCase() === repo.toLowerCase()
              ? { ...r, enrollment: { source: "state", enabled: true } }
              : r,
          ) ?? cur,
      );
    } catch (e) {
      setPending({ repo, impact: pending?.impact, error: (e as Error).message });
    } finally {
      setBusy(null);
    }
  };

  return (
    <div className="fixed inset-0 z-50 flex items-start justify-center bg-[rgb(27_36_48/0.28)] px-4 pt-[8vh] max-[600px]:px-0 max-[600px]:pt-0">
      <div
        role="dialog"
        aria-modal="true"
        aria-label="Add a repository"
        className="flex max-h-[80vh] w-full max-w-[720px] flex-col overflow-hidden rounded-[10px] border border-edge bg-card shadow-[0_16px_48px_rgb(27_36_48/0.24)] max-[600px]:h-full max-[600px]:max-h-none max-[600px]:rounded-none max-[600px]:border-0"
      >
        <div className="flex flex-wrap items-center gap-3 border-b border-edge px-5 py-3.5 max-[600px]:px-3.5">
          <h2 className="text-[15px] font-[650]">Add a repository</h2>
          <span className="text-[12.5px] text-faint max-[600px]:order-3 max-[600px]:basis-full">
            everything in CRQ_SCOPE, most recently pushed first
          </span>
          <button
            type="button"
            onClick={onClose}
            className="ml-auto rounded-lg border border-edge px-3 py-1 text-[13px] font-semibold text-mut"
          >
            Close
          </button>
        </div>

        <div className="flex items-center gap-2.5 border-b border-edge px-5 py-2.5 max-[600px]:px-3.5">
          <input
            autoFocus
            value={query}
            onChange={(e) => setQuery(e.target.value)}
            placeholder="filter by name…"
            className="flex-1 rounded-lg border border-edge bg-[#FBFBFC] px-2.5 py-1.5 text-[13px]"
          />
          <button
            type="button"
            disabled={loading}
            onClick={() => void load(true)}
            className="rounded-lg border border-edge px-3 py-1.5 text-[12.5px] font-semibold text-mut disabled:opacity-45"
          >
            {loading ? "Loading…" : "Refresh"}
          </button>
        </div>

        {error && (
          <div className="border-b border-bad-edge bg-bad-bg px-5 py-2 text-[12.5px] text-bad">{error}</div>
        )}
        {warning && (
          <div className="border-b border-warn-edge bg-warn-bg px-5 py-2 text-[12.5px] text-warn">
            {warning}
          </div>
        )}
        {truncated.length > 0 && (
          <div className="border-b border-warn-edge bg-warn-bg px-5 py-2 text-[12.5px] text-warn">
            This is not everything {truncated.join(", ")} {truncated.length === 1 ? "has" : "have"} —
            the listing stops at the 1000 most recently pushed. A repository below that line is still
            eligible: add it by name with{" "}
            <span className="font-mono">crq repos add owner/name</span>.
          </div>
        )}

        <div className="min-h-0 flex-1 overflow-auto">
          {rows === null ? (
            <Empty>{loading ? "Listing repositories…" : "Nothing loaded."}</Empty>
          ) : shown.length === 0 ? (
            <Empty>No repository matches.</Empty>
          ) : (
            <ul>
              {shown.map((r) => {
                const e = r.enrollment;
                const already = e?.enabled === true;
                const blocked = e?.source === "excluded";
                return (
                  <li
                    key={r.repo}
                    className="relative flex flex-wrap items-center gap-2.5 border-b border-[#EEF0F3] px-5 py-2 text-[13px] last:border-none max-[600px]:pr-24 max-[600px]:pl-3.5"
                  >
                    <RepoIcon repo={r.repo} />
                    <span className="min-w-0 break-all font-[550]">{r.repo}</span>
                    {r.private && <Pill tone="mut">private</Pill>}
                    {r.archived && <Pill tone="mut">archived</Pill>}
                    {r.fork && <Pill tone="mut">fork</Pill>}
                    <span className="basis-full pl-[26px] text-[12px] text-faint">
                      {r.issues > 0 && `${r.issues} open issue/PR${r.issues === 1 ? "" : "s"}`}
                      {r.pushed_at && ` · pushed ${ago(r.pushed_at, now)}`}
                    </span>
                    <span className="ml-auto max-[600px]:absolute max-[600px]:right-3.5">
                      {blocked ? (
                        <span className="text-[12px] text-faint" title="CRQ_EXCLUDE is a per-host kill switch">
                          excluded by env
                        </span>
                      ) : already ? (
                        <Pill tone="ok">
                          {e?.source === "state" ? "added" : e?.source === "env" ? "via env" : "in scope"}
                        </Pill>
                      ) : (
                        <button
                          type="button"
                          disabled={busy === r.repo}
                          onClick={() => void preview(r.repo)}
                          className="rounded-lg bg-ink px-3 py-1 text-[12.5px] font-semibold text-white disabled:opacity-45"
                        >
                          {busy === r.repo ? "Adding…" : "Add"}
                        </button>
                      )}
                    </span>
                  </li>
                );
              })}
            </ul>
          )}
        </div>

        {pending && (
          <Confirm
            title={`Review ${pending.repo}?`}
            confirmLabel={busy ? "Adding…" : "Add it"}
            busy={busy === pending.repo || (!pending.impact && !pending.error)}
            error={pending.error}
            body={
              pending.error ? (
                <>Could not work out what this would enqueue. Adding anyway is still safe — the daemon
                will pick up whatever is open — but the cost is unknown from here.</>
              ) : pending.impact ? (
                <>
                  <p>{pending.impact.summary}.</p>
                  {Object.entries(pending.impact.skipped ?? {}).map(([why, n]) => (
                    <p key={why} className="mt-1 text-[12.5px] text-faint">
                      {n} skipped — {why}.
                    </p>
                  ))}
                  <p className="mt-2 text-[12px] text-faint">
                    Estimate; published prices last checked {pending.impact.prices_checked_at}. Reviews
                    start on the daemon's next pass, one metered review at a time across the fleet.
                  </p>
                </>
              ) : (
                <>Working out what this would enqueue, and what it would cost…</>
              )
            }
            onConfirm={() => void add(pending.repo)}
            onCancel={() => setPending(null)}
          />
        )}

        <p className="border-t border-edge px-5 py-2.5 text-[12px] text-faint max-[600px]:px-3.5">
          Adding records the decision in shared state, so every host agrees. Its pull requests are
          picked up on the daemon's next pass, with the fleet's default reviewers — change those on
          the repository's own page.
        </p>
      </div>
    </div>
  );
}

/**
 * The enrollment switch on a repository's page. Turning one off needs a reason
 * for the same purpose a hold does: it disappears from every queue, and the
 * next person to wonder why deserves an answer from the fleet itself.
 */
export function EnrollmentEditor({
  repo,
  source,
  reviewed,
  envConflict,
  reason,
  by,
  active,
  onSnapshot,
}: {
  repo: string;
  source: string;
  reviewed: boolean;
  envConflict?: boolean;
  reason?: string;
  by?: string;
  /** Rounds in flight here, so stopping can say what happens to them. */
  active: number;
  onSnapshot?: (s: Snapshot) => void;
}) {
  const [confirming, setConfirming] = useState(false);
  const [why, setWhy] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [warning, setWarning] = useState<string | null>(null);

  const run = async (body: Parameters<typeof act>[1]) => {
    setBusy(true);
    setError(null);
    try {
      const res = await act("enroll", body);
      onSnapshot?.(res.snapshot);
      setWarning(res.warning ?? null);
      setConfirming(false);
      setWhy("");
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setBusy(false);
    }
  };

  const excluded = source === "excluded";
  return (
    <Card title="Automatic review" end={source === "state" ? "recorded here" : `from ${source}`}>
      <div className="px-[18px] pb-4 pt-1">
        <p className="text-[12.5px] text-mut">
          {excluded
            ? "CRQ_EXCLUDE names this repository on a host. That is a per-host kill switch and shared state does not override it — edit that host's env file."
            : reviewed
              ? "crq enqueues review rounds for this repository's open pull requests."
              : "crq does not review this repository."}
          {envConflict && (
            <>
              {" "}
              <b className="text-warn">
                A host's CRQ_REPOS still lists it; this record wins, and that file is now out of
                date.
              </b>
            </>
          )}
        </p>
        {reason && (
          <p className="mt-1.5 text-[12.5px] text-faint">
            “{reason}”{by && ` — ${by}`}
          </p>
        )}

        {warning && (
          <div className="mt-3 rounded-lg border border-warn-edge bg-warn-bg px-3 py-2 text-[12.5px] text-warn">
            {warning}
          </div>
        )}
        {error && (
          <div className="mt-3 rounded-lg border border-bad-edge bg-bad-bg px-3 py-2 text-[12.5px] text-bad">
            {error}
          </div>
        )}

        {!excluded && (
          <div className="mt-3 flex flex-wrap items-center gap-2.5">
            {reviewed ? (
              confirming ? (
                <>
                  <input
                    autoFocus
                    value={why}
                    onChange={(e) => setWhy(e.target.value)}
                    placeholder="why — every screen that shows this will show it"
                    className="min-w-[280px] flex-1 rounded-lg border border-edge bg-[#FBFBFC] px-2.5 py-1.5 text-[13px]"
                  />
                  <button
                    type="button"
                    disabled={busy || why.trim() === ""}
                    onClick={() => void run({ repo, enabled: false, reason: why.trim() })}
                    className="rounded-lg bg-bad px-4 py-1.5 text-[13px] font-semibold text-white disabled:opacity-45"
                  >
                    {busy ? "Working…" : "Stop reviewing"}
                  </button>
                  <span className="basis-full text-[12px] text-faint">
                    No new rounds are enqueued here.{" "}
                    {active > 0
                      ? `The ${active} round(s) already in flight finish — cancel them on their own pages to stop sooner.`
                      : "Nothing is in flight, so this takes effect immediately."}{" "}
                    Reviewer, autofix and fix-session settings are kept, so turning it back on
                    restores them.
                  </span>
                  <button
                    type="button"
                    onClick={() => setConfirming(false)}
                    className="rounded-lg border border-edge px-4 py-1.5 text-[13px] font-semibold text-mut"
                  >
                    Cancel
                  </button>
                </>
              ) : (
                <button
                  type="button"
                  onClick={() => setConfirming(true)}
                  className="rounded-lg border border-bad-edge px-4 py-1.5 text-[13px] font-semibold text-bad"
                >
                  Stop reviewing this repository
                </button>
              )
            ) : (
              <button
                type="button"
                disabled={busy}
                onClick={() => void run({ repo, enabled: true })}
                className="rounded-lg bg-ink px-4 py-1.5 text-[13px] font-semibold text-white disabled:opacity-45"
              >
                {busy ? "Working…" : "Review this repository"}
              </button>
            )}
            {source === "state" && (
              <button
                type="button"
                disabled={busy}
                onClick={() => void run({ repo, clear: true })}
                className="ml-auto text-[12.5px] text-acc hover:underline disabled:opacity-45"
              >
                Follow this host's env instead
              </button>
            )}
          </div>
        )}
      </div>
    </Card>
  );
}
