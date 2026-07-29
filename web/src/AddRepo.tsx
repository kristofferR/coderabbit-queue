import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { act } from "./actions";
import type { Candidate, EnrollImpact, Snapshot } from "./api";
import { discover, enrollmentImpact } from "./api";
import { Confirm } from "./Confirm";
import { ago, useNow } from "./time";
import { Card, Empty, Pill, RepoIcon } from "./ui";
import { Dialog, DialogContent, DialogDescription, DialogTitle } from "./ui/dialog";
import { useOperation } from "./useOperation";

type EnrollPreview = {
  impact?: EnrollImpact;
  error?: string;
  errorKind?: "preview" | "enroll";
};

function EnrollImpactCopy({ preview }: { preview: EnrollPreview }) {
  if (preview.errorKind === "preview") {
    return (
      <>
        Could not work out what this would enqueue. Enabling anyway is still safe — the daemon will
        pick up whatever is open — but the cost is unknown from here.
      </>
    );
  }
  if (!preview.impact) {
    return <>Working out what this would enqueue, and what it would cost…</>;
  }
  return (
    <>
      <p>{preview.impact.summary}.</p>
      {Object.entries(preview.impact.skipped ?? {}).map(([why, n]) => (
        <p key={why} className="mt-1 text-[12.5px] text-faint">
          {n} skipped — {why}.
        </p>
      ))}
      <p className="mt-2 text-[12px] text-faint">
        Estimate; published prices last checked {preview.impact.prices_checked_at}. Reviews start on
        the daemon's next pass, one metered review at a time across the fleet.
      </p>
    </>
  );
}

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
  const [truncated, setTruncated] = useState<string[]>([]);
  const [manualRepo, setManualRepo] = useState("");
  const [loading, setLoading] = useState(false);
  const [query, setQuery] = useState("");
  const [busy, setBusy] = useState<string | null>(null);
  const requested = useRef(false);
  const { run: runRequest, error } = useOperation();
  const { run: runPreview, running: previewing } = useOperation();
  const { run: runAdd, error: addError, clearError: clearAddError } = useOperation();
  // The backlog contract: enrolling a repository with a dozen open pull
  // requests becomes a dozen metered reviews on the next pass. Nothing is
  // written until this has been shown and confirmed.
  const [pending, setPending] = useState<(EnrollPreview & { repo: string }) | null>(null);

  const load = useCallback(
    (refresh = false) => {
      setLoading(true);
      runRequest(discover(refresh), {
        onSuccess: ({ repos, truncated: bounded }) => {
          setRows(repos);
          setTruncated(bounded ?? []);
        },
        onFinally: () => setLoading(false),
      });
    },
    [runRequest],
  );

  useEffect(() => {
    if (!open) {
      requested.current = false;
      return;
    }
    if (rows !== null || requested.current) return;
    requested.current = true;
    load();
  }, [load, open, rows]);

  const shown = useMemo(() => {
    if (!rows) return [];
    const q = query.trim().toLowerCase();
    return q ? rows.filter((r) => r.repo.toLowerCase().includes(q)) : rows;
  }, [rows, query]);
  const trimmedManualRepo = manualRepo.trim();
  const manualRepoValid = /^[^/\s]+\/[^/\s]+$/.test(trimmedManualRepo);

  if (!open) return null;

  const preview = (repo: string) => {
    clearAddError();
    setPending({ repo });
    runPreview(enrollmentImpact(repo), {
      onSuccess: (impact) => setPending({ repo, impact }),
      // A price we could not work out must not silently become a free-looking
      // Add: the dialog stays, and says why.
      onFailure: (failure) => setPending({ repo, error: failure.message, errorKind: "preview" }),
    });
  };

  const add = (repo: string) => {
    setBusy(repo);
    runAdd(act("enroll", { repo, enabled: true }), {
      onSuccess: ({ snapshot }) => {
        onSnapshot?.(snapshot);
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
      },
      onFailure: (failure) =>
        setPending((current) =>
          current?.repo === repo
            ? { ...current, error: failure.message, errorKind: "enroll" }
            : current,
        ),
      onFinally: () => setBusy(null),
    });
  };

  return (
    <Dialog open={open} onOpenChange={(nextOpen) => !nextOpen && onClose()}>
      <DialogContent className="top-[8vh] flex max-h-[84vh] max-w-[720px] flex-col overflow-hidden p-0">
        <div className="flex items-center gap-3 border-b border-edge px-5 py-3.5">
          <DialogTitle>Add a repository</DialogTitle>
          <DialogDescription className="pr-8 text-[12.5px] text-faint">
            everything in CRQ_SCOPE, most recently pushed first
          </DialogDescription>
        </div>

        <div className="flex items-center gap-2.5 border-b border-edge px-5 py-2.5">
          <input
            aria-label="Filter repositories"
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
          <div className="border-b border-bad-edge bg-bad-bg px-5 py-2 text-[12.5px] text-bad">
            {error}
          </div>
        )}

        {truncated.length > 0 && (
          <div className="border-b border-warn-edge bg-warn-bg px-5 py-2.5 text-[12.5px] text-warn">
            GitHub capped discovery for {truncated.join(", ")}. Enter a missing repository by its
            full owner/name:
            <form
              className="mt-2 flex gap-2"
              onSubmit={(event) => {
                event.preventDefault();
                if (manualRepoValid) preview(trimmedManualRepo);
              }}
            >
              <input
                aria-label="Repository owner and name"
                value={manualRepo}
                onChange={(event) => setManualRepo(event.target.value)}
                placeholder="owner/repository"
                className="min-w-0 flex-1 rounded-lg border border-warn-edge bg-white px-2.5 py-1 text-ink"
              />
              <button
                type="submit"
                disabled={!manualRepoValid}
                className="rounded-lg bg-ink px-3 py-1 font-semibold text-white disabled:opacity-45"
              >
                Add by name
              </button>
            </form>
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
                    className="flex flex-wrap items-center gap-2.5 border-b border-[#EEF0F3] px-5 py-2 text-[13px] last:border-none"
                  >
                    <RepoIcon repo={r.repo} />
                    <span className="min-w-[160px] flex-1 break-all font-[550]">{r.repo}</span>
                    {r.private && <Pill tone="mut">private</Pill>}
                    {r.archived && <Pill tone="mut">archived</Pill>}
                    {r.fork && <Pill tone="mut">fork</Pill>}
                    <span className="text-[12px] text-faint max-sm:order-last max-sm:basis-full max-sm:pl-[26px]">
                      {r.issues > 0 && `${r.issues} open issue/PR${r.issues === 1 ? "" : "s"}`}
                      {r.pushed_at && ` · pushed ${ago(r.pushed_at, now)}`}
                    </span>
                    <span className="ml-auto shrink-0">
                      {blocked ? (
                        <span
                          className="text-[12px] text-faint"
                          title="CRQ_EXCLUDE is a per-host kill switch"
                        >
                          excluded by env
                        </span>
                      ) : already ? (
                        <Pill tone="ok">
                          {e?.source === "state"
                            ? "added"
                            : e?.source === "env"
                              ? "via env"
                              : "in scope"}
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
            busy={previewing || busy === pending.repo}
            error={pending.error ?? addError}
            body={<EnrollImpactCopy preview={pending} />}
            onConfirm={() => void add(pending.repo)}
            onCancel={() => setPending(null)}
          />
        )}

        <p className="border-t border-edge px-5 py-2.5 text-[12px] text-faint">
          Adding records the decision in shared state, so every host agrees. Its pull requests are
          picked up on the daemon's next pass, with the fleet's default reviewers — change those on
          the repository's own page.
        </p>
      </DialogContent>
    </Dialog>
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
  const { run: runOperation, running: busy, error } = useOperation();
  const { run: runPreview, running: previewing } = useOperation();
  const [warning, setWarning] = useState<string | null>(null);
  const [enablePreview, setEnablePreview] = useState<EnrollPreview | null>(null);

  const run = (body: Parameters<typeof act>[1]) =>
    runOperation(act("enroll", body), {
      onSuccess: ({ snapshot, warning: nextWarning }) => {
        onSnapshot?.(snapshot);
        setWarning(nextWarning ?? null);
        setConfirming(false);
        setEnablePreview(null);
        setWhy("");
      },
    });

  const previewEnable = () => {
    setEnablePreview({});
    runPreview(enrollmentImpact(repo), {
      onSuccess: (impact) => setEnablePreview({ impact }),
      onFailure: (failure) => setEnablePreview({ error: failure.message, errorKind: "preview" }),
    });
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
                    aria-label="Reason for stopping automatic review"
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
                disabled={busy || enablePreview !== null}
                onClick={() => void previewEnable()}
                className="rounded-lg bg-ink px-4 py-1.5 text-[13px] font-semibold text-white disabled:opacity-45"
              >
                {enablePreview ? "Checking backlog…" : "Review this repository"}
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
      {enablePreview && (
        <Confirm
          title={`Review ${repo}?`}
          confirmLabel="Review this repository"
          busy={previewing || busy}
          error={enablePreview.error ?? error}
          body={<EnrollImpactCopy preview={enablePreview} />}
          onConfirm={() => void run({ repo, enabled: true })}
          onCancel={() => setEnablePreview(null)}
        />
      )}
    </Card>
  );
}
