import type { BotCard, HeldRow, HostTools, RepoRow, SetupView, SettingsView, Snapshot } from "./api";
import { useEffect, useState } from "react";
import { act } from "./actions";
import { Confirm } from "./Confirm";
import { BotIcon, Card, Empty, Pill, PRLink, RepoIcon, Td, Th, Toggle } from "./ui";
import { ago, clock, useNow } from "./time";
import { AddRepo, EnrollmentEditor } from "./AddRepo";
import { FleetEditor } from "./FleetEditor";
import { SolverEditor } from "./SolverEditor";
import { EnvEditor } from "./EnvEditor";
import { sameSet, setKey } from "./sets";

/* ------------------------------------------------------------------ Repos */

// The label says the ANSWER; the note says where it came from. Both matter: a
// repository excluded by a host's env cannot be turned on from here, and one
// enrolled by a record can.
const ENROLLMENT: Record<
  string,
  { tone: "ok" | "acc" | "mut" | "bad"; label: string; note: string }
> = {
  state: { tone: "ok", label: "Reviewed", note: "recorded here, so every host agrees" },
  env: { tone: "acc", label: "Reviewed", note: "listed in CRQ_REPOS on this host" },
  scope: { tone: "ok", label: "Reviewed", note: "its owner is in CRQ_SCOPE and there is no allow-list" },
  excluded: { tone: "mut", label: "Excluded", note: "CRQ_EXCLUDE on this host, or the gate repo — a kill switch state cannot override" },
  off: { tone: "mut", label: "Not reviewed", note: "no record, and this host's allow-list omits it" },
};

function enrollmentLabel(enrollment: string, reviewed: boolean) {
  const entry = ENROLLMENT[enrollment] ?? ENROLLMENT.off;
  return enrollment === "state" && !reviewed ? { ...entry, tone: "mut" as const, label: "Turned off" } : entry;
}

export function ReposPage({
  repos,
  bots,
  held,
  onSnapshot,
}: {
  repos: RepoRow[];
  bots: BotCard[];
  held: HeldRow[];
  onSnapshot?: (s: Snapshot) => void;
}) {
  const now = useNow(5000);
  const [picked, setPicked] = useState<string | null>(null);
  const [adding, setAdding] = useState(false);
  const selected = repos.find((r) => r.repo === picked) ?? repos[0];
  return (
    <main className="mx-auto grid max-w-[1400px] grid-cols-[320px_minmax(0,1fr)] items-start gap-4.5 px-6 pt-4.5 pb-16 max-[1400px]:grid-cols-[minmax(0,1fr)]">
      <div>
      <Card
        title="Repositories"
        count={repos.length}
        end={
          <button
            type="button"
            onClick={() => setAdding(true)}
            className="rounded-lg bg-ink px-2.5 py-1 text-[12px] font-semibold text-white"
          >
            Add repository
          </button>
        }
      >
        {repos.length === 0 ? (
          <Empty>No repository has been seen yet.</Empty>
        ) : (
          <ul>
            {repos.map((r) => {
              const e = enrollmentLabel(r.enrollment, r.reviewed);
              const on = selected?.repo === r.repo;
              return (
                <li key={r.repo} className="border-t border-[#EEF0F3]">
                  <button
                    type="button"
                    onClick={() => setPicked(r.repo)}
                    title={e.note}
                    className={`w-full px-4 py-2.5 text-left ${on ? "border-l-[3px] border-acc bg-acc-bg pl-[13px]" : "hover:bg-[#F7F8FA]"}`}
                  >
                    <div className="flex items-center gap-2 text-[13.5px] font-[550]">
                      <RepoIcon repo={r.repo} />
                      {short(r.repo)}
                      <span className="ml-auto">
                        <Pill tone={e.tone}>{e.label}</Pill>
                      </span>
                    </div>
                    <div className="mt-0.5 ml-6 text-xs text-faint">
                      {r.override ? "override" : "fleet default"}
                      {r.active_rounds > 0 && ` · ${r.active_rounds} active`}
                      {r.held_prs > 0 && ` · ${r.held_prs} held`}
                      {r.fixing > 0 && ` · ${r.fixing} fixing`}
                    </div>
                  </button>
                </li>
              );
            })}
          </ul>
        )}
        <p className="border-t border-[#EEF0F3] px-4 py-2.5 text-xs text-faint">
          A record in shared state wins over a host's CRQ_REPOS in both directions; CRQ_EXCLUDE wins
          over everything, because it is a per-host kill switch.
        </p>
      </Card>
      </div>

      {selected && (
        <div>
          <div className="mb-3.5 flex flex-wrap items-center gap-3.5 rounded-[10px] border border-edge bg-card px-5 py-4 shadow-card">
            <RepoIcon repo={selected.repo} size={26} />
            <h1 className="font-mono text-[18px] font-[650] tracking-tight">{selected.repo}</h1>
            <Pill tone={enrollmentLabel(selected.enrollment, selected.reviewed).tone}>
              {enrollmentLabel(selected.enrollment, selected.reviewed).label}
            </Pill>
            <span className="text-[12px] text-faint">
              {enrollmentLabel(selected.enrollment, selected.reviewed).note}
            </span>
            {selected.override && <Pill tone="warn">Override</Pill>}
            {selected.primary_off && <Pill tone="bad">Primary off</Pill>}
            {selected.solver?.overridden && <Pill tone="warn">Fix settings</Pill>}
            <span className="text-[12.5px] text-faint">
              {selected.active_rounds} active · {selected.queued_rounds} queued
              {selected.override_by && ` · set by ${selected.override_by}`}
              {selected.override_at && ` ${ago(selected.override_at, now)}`}
            </span>
            <a
              href={`https://github.com/${selected.repo}`}
              target="_blank"
              rel="noreferrer"
              className="ml-auto text-[12.5px] text-acc hover:underline"
            >
              Open on GitHub ↗
            </a>
          </div>

          <EnrollmentEditor
            key={`${selected.repo}-enroll`}
            repo={selected.repo}
            source={selected.enrollment}
            reviewed={selected.reviewed}
            envConflict={selected.env_conflict}
            reason={selected.enroll_reason}
            by={selected.enroll_by}
            active={selected.active_rounds}
            onSnapshot={onSnapshot}
          />
          <HeldHere
            key={`${selected.repo}-held`}
            repo={selected.repo}
            held={held.filter((h) => h.repo.toLowerCase() === selected.repo.toLowerCase())}
            elsewhere={held.filter((h) => h.repo.toLowerCase() !== selected.repo.toLowerCase()).length}
            now={now}
            onSnapshot={onSnapshot}
          />
          <ReviewerEditor key={selected.repo} repo={selected} bots={bots} onSnapshot={onSnapshot} />
          <AutofixEditor key={`${selected.repo}-autofix`} repo={selected} now={now} onSnapshot={onSnapshot} />
          {selected.solver && (
            <SolverEditor
              key={`${selected.repo}-solver`}
              repo={selected.repo}
              solver={selected.solver}
              onSnapshot={onSnapshot}
            />
          )}
        </div>
      )}
      <AddRepo open={adding} onClose={() => setAdding(false)} onSnapshot={onSnapshot} />
    </main>
  );
}

/**
 * Holds on this repository, and a way to place one.
 *
 * A hold could only be placed from an Overview row, which means from the one
 * page that shows every repository at once — so the answer to "stop reviewing
 * this PR while I rework it" was to go and find it in a list. It belongs where
 * the repository's other decisions are.
 */
function HeldHere({
  repo,
  held,
  elsewhere,
  now,
  onSnapshot,
}: {
  repo: string;
  held: HeldRow[];
  elsewhere: number;
  now: number;
  onSnapshot?: (s: Snapshot) => void;
}) {
  const [adding, setAdding] = useState(false);
  const [pr, setPr] = useState("");
  const [reason, setReason] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const run = async (kind: "hold" | "unhold", num: number, why = "") => {
    setBusy(true);
    setError(null);
    try {
      const res = await act(kind, { repo, pr: num, reason: why });
      onSnapshot?.(res.snapshot);
      setAdding(false);
      setPr("");
      setReason("");
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setBusy(false);
    }
  };

  return (
    <Card
      title="Held pull requests"
      count={held.length}
      end={elsewhere > 0 ? `${elsewhere} held elsewhere` : undefined}
    >
      <div className="px-[18px] pb-3.5 pt-1">
        {held.length === 0 ? (
          <p className="text-[12.5px] text-faint">
            Nothing is held here. A hold stops crq enqueuing or firing for one pull request until
            somebody lifts it; reviews already in flight finish.
          </p>
        ) : (
          <ul className="text-[13px]">
            {held.map((h) => (
              <li key={h.key} className="flex items-start gap-2 border-b border-[#EEF0F3] py-1.5 last:border-none">
                <span className="min-w-0">
                  <PRLink repo={h.repo} pr={h.pr} />
                  {h.title && <span className="ml-2 text-[12.5px] text-mut">{h.title}</span>}
                  <div className="text-[12.5px] text-faint">
                    “{h.reason}”{h.by && ` — ${h.by}`} {h.at && ago(h.at, now)}
                  </div>
                </span>
                <button
                  type="button"
                  disabled={busy}
                  onClick={() => void run("unhold", h.pr)}
                  className="ml-auto shrink-0 rounded-lg border border-edge px-2.5 py-0.5 text-[12px] font-semibold text-mut disabled:opacity-45"
                >
                  Unhold
                </button>
              </li>
            ))}
          </ul>
        )}

        {error && (
          <div className="mt-2.5 rounded-lg border border-bad-edge bg-bad-bg px-3 py-2 text-[12.5px] text-bad">
            {error}
          </div>
        )}

        {adding ? (
          <div className="mt-2.5 flex flex-wrap items-center gap-2">
            <input
              autoFocus
              value={pr}
              inputMode="numeric"
              placeholder="PR #"
              onChange={(e) => setPr(e.target.value.replace(/[^0-9]/g, ""))}
              className="w-20 rounded-lg border border-edge bg-[#FBFBFC] px-2 py-1 font-mono text-[12.5px]"
            />
            <input
              value={reason}
              placeholder="why — every screen that shows the hold shows this"
              onChange={(e) => setReason(e.target.value)}
              className="min-w-[260px] flex-1 rounded-lg border border-edge bg-[#FBFBFC] px-2 py-1 text-[12.5px]"
            />
            <button
              type="button"
              disabled={busy || !pr || reason.trim() === ""}
              onClick={() => void run("hold", Number(pr), reason.trim())}
              className="rounded-lg bg-ink px-3 py-1 text-[12.5px] font-semibold text-white disabled:opacity-45"
            >
              Hold it
            </button>
            <button
              type="button"
              onClick={() => setAdding(false)}
              className="rounded-lg border border-edge px-3 py-1 text-[12.5px] font-semibold text-mut"
            >
              Cancel
            </button>
          </div>
        ) : (
          <button
            type="button"
            onClick={() => setAdding(true)}
            className="mt-2.5 rounded-lg border border-edge px-3 py-1 text-[12.5px] font-semibold text-mut"
          >
            Hold a pull request…
          </button>
        )}
      </div>
    </Card>
  );
}

// Set-up bots first, then unproven, then ones crq has asked and never heard
// from — worst last, because that is the order in which they are worth
// considering.
function rank(b: BotCard) {
  return b.status === "working" ? 0 : b.status === "silent" ? 2 : 1;
}

/** Runs and Required are separate ideas, so they get separate toggles. */
function ReviewerEditor({
  repo,
  bots,
  onSnapshot,
}: {
  repo: RepoRow;
  bots: BotCard[];
  onSnapshot?: (s: Snapshot) => void;
}) {
  const [runs, setRuns] = useState<string[]>(repo.reviewers);
  const [required, setRequired] = useState<string[]>(repo.required);
  const [confirming, setConfirming] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [warning, setWarning] = useState<string | null>(null);

  // Depend on the reviewer lists' CONTENTS, not on the arrays: every SSE
  // revision rebuilds the row objects, so the identity dependency reset the
  // toggles — discarding a half-made selection — whenever anything unrelated in
  // the queue moved.
  const runsRev = setKey(repo.reviewers);
  const requiredRev = setKey(repo.required);
  useEffect(() => {
    setRuns(repo.reviewers);
    setRequired(repo.required);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [repo.repo, runsRev, requiredRev]);

  // The primary is the one metered reviewer, so its Runs toggle is a budget
  // decision (a private repo on a free plan gets nothing from it) and travels
  // in its own field — the co-reviewer list only accepts registry bots.
  const primaryBot = bots.find((b) => b.primary);
  const primaryOn = primaryBot ? runs.includes(primaryBot.name) : false;
  const primaryWas = primaryBot ? repo.reviewers.includes(primaryBot.name) : false;

  const dirty = !sameSet(runs, repo.reviewers) || !sameSet(required, repo.required);
  const newlyOn = runs.filter((b) => !repo.reviewers.includes(b) && b !== primaryBot?.name);

  const toggleRuns = (name: string) => {
    setRuns((cur) => (cur.includes(name) ? cur.filter((n) => n !== name) : [...cur, name]));
    // Dropping a reviewer must drop its requirement too, or convergence would
    // wait for a bot that never runs.
    setRequired((cur) => (runs.includes(name) ? cur.filter((n) => n !== name) : cur));
  };
  const toggleRequired = (name: string) => {
    setRequired((cur) => (cur.includes(name) ? cur.filter((n) => n !== name) : [...cur, name]));
    // Requiring a reviewer implies running it.
    setRuns((cur) => (cur.includes(name) ? cur : [...cur, name]));
  };

  const save = async (clear = false) => {
    setBusy(true);
    setError(null);
    try {
      const res = await act("reviewers", {
        repo: repo.repo,
        cobots: runs.filter((n) => n !== primaryBot?.name),
        required,
        primary: primaryBot ? primaryOn : undefined,
        clear,
      });
      onSnapshot?.(res.snapshot);
      setWarning(res.warning ?? null);
      setConfirming(false);
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setBusy(false);
    }
  };

  return (
    <Card
      title="Reviewers"
      end={repo.override ? "override" : "inherited from the fleet"}
    >
      <div className="px-[18px] pb-4">
        <p className="mb-2.5 text-[12.5px] text-faint">
          <b>Runs</b> — the bot reviews this repo. <b>Required</b> — convergence waits for it.
          Requiring a bot turns Runs on; the required set cannot be empty. Turning the
          primary off means this repo never spends the shared review allowance. A bot crq has
          never seen work here is marked — enabling one is allowed, since a bot cannot prove
          itself until it is asked, but an unset-up one just collects trigger comments.
        </p>
        <table className="w-full border-collapse">
          <thead>
            <tr>
              <Th>Reviewer</Th>
              <Th className="w-20 text-center">Runs</Th>
              <Th className="w-24 text-center">Required</Th>
              <Th className="c-host">Role</Th>
            </tr>
          </thead>
          <tbody>
            {/* Bots crq has actually seen work here come first and are the
                real options. One it has never seen is offered too — a fresh
                bot cannot produce evidence until it is enabled, so hiding it
                would make the first one impossible to turn on — but it is
                marked, because enabling a bot nobody has an account for means
                crq posts a trigger on every round and waits for an answer that
                never comes. */}
            {[...bots].sort((a, b) => rank(a) - rank(b)).map((b) => (
              <tr key={b.login} className={b.status === "working" ? "" : "opacity-75"}>
                <Td>
                  <span className="flex items-center gap-2.5">
                    <BotIcon login={b.login} name={b.name} size={20} />
                    <span className="font-[550]">{b.name}</span>
                    {b.status !== "working" && (
                      <a
                        href="#/bots"
                        title={
                          b.status === "silent"
                            ? "crq has asked it and never seen an answer — most likely not set up"
                            : "crq has never seen this bot work here"
                        }
                      >
                        <Pill tone={b.status === "silent" ? "bad" : "mut"}>
                          {b.status === "silent" ? "never answered" : "not set up?"}
                        </Pill>
                      </a>
                    )}
                  </span>
                </Td>
                <Td className="text-center">
                  <Toggle
                    on={runs.includes(b.name)}
                    label={`Runs ${b.name}`}
                    title={b.primary ? "the metered reviewer — turning it off spends no quota here" : undefined}
                    onClick={() => toggleRuns(b.name)}
                  />
                </Td>
                <Td className="text-center">
                  <Toggle
                    on={required.includes(b.name)}
                    label={`Requires ${b.name}`}
                    onClick={() => toggleRequired(b.name)}
                  />
                </Td>
                <Td className="c-host text-[12.5px] text-faint">
                  {b.primary
                    ? primaryOn
                      ? "primary · metered against the shared allowance"
                      : "primary · turned off here"
                    : "co-reviewer"}
                </Td>
              </tr>
            ))}
          </tbody>
        </table>

        {warning && (
          <div className="mt-3 rounded-lg border border-warn-edge bg-warn-bg px-3 py-2 text-[12.5px] text-warn">
            {warning}
          </div>
        )}
        {error && !confirming && (
          <div className="mt-3 rounded-lg border border-bad-edge bg-bad-bg px-3 py-2 text-[12.5px] text-bad">
            {error}
          </div>
        )}

        <div className="mt-3.5 flex flex-wrap items-center gap-2.5">
          <button
            type="button"
            disabled={!dirty || busy}
            onClick={() => setConfirming(true)}
            className="rounded-lg bg-ink px-4 py-1.5 text-[13px] font-semibold text-white disabled:opacity-45"
          >
            Save reviewers
          </button>
          <button
            type="button"
            disabled={!dirty || busy}
            onClick={() => {
              setRuns(repo.reviewers);
              setRequired(repo.required);
            }}
            className="rounded-lg border border-edge px-4 py-1.5 text-[13px] font-semibold text-mut disabled:opacity-45"
          >
            Discard
          </button>
          {dirty && <span className="text-[12.5px] text-warn">unsaved changes</span>}
          {repo.override && !dirty && (
            <button
              type="button"
              onClick={() => save(true)}
              className="ml-auto text-[12.5px] text-acc hover:underline"
            >
              Reset to fleet default
            </button>
          )}
        </div>
      </div>

      {confirming && (
        <Confirm
          title={`Save reviewers for ${repo.repo.split("/").pop()}?`}
          body={
            <>
              This repository will stop following the fleet default and keep its own list.
              {primaryBot && primaryOn !== primaryWas && (
                <>
                  {" "}
                  <b>
                    {primaryBot.name} will {primaryOn ? "review" : "no longer review"} this
                    repository.
                  </b>{" "}
                  {primaryOn
                    ? "Its rounds go back to waiting for the fire slot and the account quota."
                    : "Its rounds stop taking the fire slot and stop waiting on the account quota — the co-reviewers resolve them alone."}
                </>
              )}
              {newlyOn.length > 0 && (
                <>
                  {" "}
                  <b>{newlyOn.join(", ")}</b> {newlyOn.length === 1 ? "is" : "are"} newly enabled and may be
                  triggered on {repo.active_rounds > 0 ? `${repo.active_rounds} active round(s)` : "the next round"} at
                  their current heads.
                </>
              )}{" "}
              No metered reviews are spent by saving.
            </>
          }
          confirmLabel="Save"
          busy={busy}
          error={error}
          onConfirm={() => save(false)}
          onCancel={() => {
            setConfirming(false);
            setError(null);
          }}
        />
      )}
    </Card>
  );
}

/** Autofix is a tri-state: the fleet default is a real choice, not just "on". */
function AutofixEditor({
  repo,
  now,
  onSnapshot,
}: {
  repo: RepoRow;
  now: number;
  onSnapshot?: (s: Snapshot) => void;
}) {
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [offReason, setOffReason] = useState<boolean>(false);

  const apply = async (enabled?: boolean, reason = "") => {
    setBusy(true);
    setError(null);
    try {
      const res = await act("autofix", { repo: repo.repo, enabled, reason });
      onSnapshot?.(res.snapshot);
      setOffReason(false);
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setBusy(false);
    }
  };

  const choice = repo.autofix;
  return (
    <Card title="Autofix" end={choice === "default" ? "fleet default" : "override"}>
      <div className="px-[18px] pb-4">
        <p className="mb-2.5 text-[12.5px] text-faint">
          Permission only — whether an agent exists is a per-host question, on the Setup page.
        </p>
        <div className="flex flex-wrap items-center gap-3">
          <span className="inline-flex overflow-hidden rounded-lg border border-edge">
            {(["off", "on", "default"] as const).map((v) => (
              <button
                key={v}
                type="button"
                disabled={busy}
                onClick={() => (v === "off" ? setOffReason(true) : apply(v === "on" ? true : undefined))}
                className={`border-r border-edge px-3 py-1 text-[12.5px] last:border-r-0 ${
                  choice === v ? "bg-ok-bg font-medium text-ok" : "text-mut hover:bg-bg"
                }`}
              >
                {v === "default" ? "Default" : v}
              </button>
            ))}
          </span>
          {repo.autofix_reason && <span className="text-[13px] text-faint">“{repo.autofix_reason}”</span>}
          {repo.autofix_by && (
            <span className="text-[12.5px] text-faint">
              set by {repo.autofix_by} {ago(repo.autofix_at, now)}
            </span>
          )}
        </div>
        {error && (
          <div className="mt-3 rounded-lg border border-bad-edge bg-bad-bg px-3 py-2 text-[12.5px] text-bad">
            {error}
          </div>
        )}
      </div>

      {offReason && (
        <Confirm
          title={`Turn autofix off for ${repo.repo.split("/").pop()}?`}
          body={
            <>
              crq will keep reviewing this repository but stop writing fixes to it. Any session already
              running finishes.
            </>
          }
          confirmLabel="Turn it off"
          needsReason
          reasonLabel="Why (shown wherever this switch appears)"
          busy={busy}
          error={error}
          onConfirm={(reason) => apply(false, reason)}
          onCancel={() => {
            setOffReason(false);
            setError(null);
          }}
        />
      )}
    </Card>
  );
}

/** A switch that can be locked — the primary always runs, and says so. */
/* ------------------------------------------------------------------- Bots */

const STATUS: Record<
  string,
  { tone: "ok" | "warn" | "bad" | "mut" | "acc"; label: string; note: string }
> = {
  working: { tone: "ok", label: "Working", note: "crq saw it answer here in the last week" },
  quiet: { tone: "warn", label: "Quiet", note: "crq saw it answer once, but not lately" },
  silent: {
    tone: "bad",
    label: "Never answered",
    note:
      "crq has asked it and never seen an answer — most likely it is not set up on this account, " +
      "and every trigger crq posts for it is a comment nobody reads",
  },
  unverified: {
    tone: "mut",
    label: "Not verified",
    note:
      "enabled, but crq has no evidence either way yet — it records a bot's answers only as it " +
      "observes rounds, so this is normal for a while after an upgrade",
  },
  off: { tone: "mut", label: "Not enabled", note: "crq does not ask for its review on this fleet" },
};

/**
 * The review-bot guide.
 *
 * Deliberately NOT a control surface. Which bots run is a property of a
 * repository (its Reviewers card) or of the fleet default (Settings), and
 * offering the same switch a third time here would mean three places to look
 * for one answer. This page exists for the question those pages cannot answer:
 * what IS this bot, what does it cost, and is it actually set up.
 *
 * "Set up" is the honest part. crq cannot ask any vendor whether you have an
 * account — it can only report what it has seen the bot do here. A bot enabled
 * by a default nobody chose, on an account nobody has, is precisely the case
 * that looks configured and reviews nothing, so it gets its own status rather
 * than being folded into "enabled".
 */
export function BotsPage({ bots }: { bots: BotCard[] }) {
  const now = useNow(5000);
  return (
    <main className="mx-auto max-w-[1120px] px-6 pt-5 pb-16">
      <h1 className="text-xl font-[650] tracking-tight">Review bots</h1>
      <p className="mt-1 max-w-[760px] text-[13.5px] text-mut">
        Every reviewer crq knows how to drive. Status is what crq itself recorded — a trigger it
        posted, a claim it observed — never a status read from the vendor, which none of them
        offers. To change which bots run, use a repository's <b>Reviewers</b> card, or the fleet
        default under <b>Settings</b>.
      </p>

      <div className="mt-4 grid grid-cols-2 gap-4 max-[940px]:grid-cols-1">
        {bots.map((b) => {
          const st = STATUS[b.status] ?? STATUS.off;
          const seen = seenOn(b.seen_on);
          return (
            <section
              key={b.login}
              className={`flex flex-col rounded-[10px] border bg-card shadow-card ${
                b.enabled ? "border-edge" : "border-dashed border-edge"
              }`}
            >
              <header className="flex items-start gap-3 px-5 pt-4">
                <BotIcon login={b.login} name={b.name} size={38} />
                <div className="min-w-0">
                  <h2 className="text-base font-[650]">{b.name}</h2>
                  <div className="text-xs text-faint">
                    {b.primary ? "primary reviewer" : "co-reviewer"} ·{" "}
                    {b.metered ? "spends the shared quota" : "free, no crq quota"}
                  </div>
                </div>
                <span className="ml-auto shrink-0" title={st.note}>
                  <Pill tone={st.tone}>{st.label}</Pill>
                </span>
              </header>

              <div className="flex-1 px-5 pt-3 text-[13px] text-mut">
                {b.pitch && <p>{b.pitch}</p>}
                {b.cost && (
                  <p className="mt-2">
                    <span className="text-faint">Cost — </span>
                    {b.cost}
                    {b.prices_checked_at && (
                      <span className="text-faint"> (checked {b.prices_checked_at})</span>
                    )}
                  </p>
                )}
                {b.suggested && b.because && (
                  <p className="mt-2 rounded-lg border border-ok-edge bg-ok-bg px-2.5 py-1.5 text-[12.5px] text-ok">
                    <b>Suggested here</b> — {b.because}.
                  </p>
                )}
                {b.suited_to && (
                  <p className="mt-2 rounded-lg border border-acc-edge bg-acc-bg px-2.5 py-1.5 text-[12.5px] text-acc">
                    Worth it for {b.suited_to}.
                  </p>
                )}

                <p className="mt-2.5 text-[12.5px] text-faint">
                  {st.note}
                  {b.last_seen && seen && (
                    <>
                      {" — last on "}
                      <PRLink repo={seen.repo} pr={seen.pr} />{" "}
                      {ago(b.last_seen, now)}
                    </>
                  )}
                  {b.repo_count > 0 && ` · ${b.repo_count} repo override(s) name it`}
                </p>

                {b.status === "silent" && b.last_asked && (
                  <p className="mt-2 rounded-lg border border-bad-edge bg-bad-bg px-2.5 py-1.5 text-[12.5px] text-bad">
                    Last asked {ago(b.last_asked, now)} and it has never answered. Turn it off on the
                    repositories that use it, or finish setting it up — until then crq posts a
                    trigger comment on every round and waits out the grace period for nothing.
                  </p>
                )}

                {b.status !== "working" && (b.setup?.length ?? 0) > 0 && (
                  <details className="mt-2.5 rounded-lg border border-edge bg-[#FBFBFC] px-2.5 py-1.5">
                    <summary className="cursor-pointer text-[12.5px] font-[550]">Setting it up</summary>
                    <ol className="mt-1.5 list-decimal pl-4 text-[12.5px]">
                      {b.setup!.map((step) => (
                        <li key={step} className="py-0.5">
                          {step}
                        </li>
                      ))}
                    </ol>
                  </details>
                )}
              </div>

              <footer className="mt-3 flex flex-wrap items-center gap-3 border-t border-[#EEF0F3] px-5 py-2.5 text-[12.5px]">
                {b.site && (
                  <a href={b.site} target="_blank" rel="noreferrer" className="text-acc hover:underline">
                    {b.status === "off" || b.status === "unverified" ? "Sign up ↗" : "Vendor ↗"}
                  </a>
                )}
                {b.docs && (
                  <a href={b.docs} target="_blank" rel="noreferrer" className="text-acc hover:underline">
                    Docs ↗
                  </a>
                )}
                <a href="#/repos" className="ml-auto text-mut hover:underline">
                  {b.enabled ? "Choose where it runs →" : "Turn it on →"}
                </a>
              </footer>
            </section>
          );
        })}
      </div>

      <p className="mt-4 max-w-[760px] text-[12px] text-faint">
        Links go straight to each vendor. There are no referral links here: if any are added they
        will say so on the link itself, and suggestions will stay based on what fits your setup
        rather than on what pays.
      </p>
    </main>
  );
}

function seenOn(value?: string): { repo: string; pr: number } | null {
  if (!value) return null;
  const split = value.lastIndexOf("#");
  if (split <= 0) return null;
  const repo = value.slice(0, split);
  const rawPR = value.slice(split + 1);
  if (!/^[1-9]\d*$/.test(rawPR)) return null;
  const pr = Number(rawPR);
  return repo.includes("/") && Number.isSafeInteger(pr) ? { repo, pr } : null;
}

/* ------------------------------------------------------------------ Setup */

const STATUS_TONE: Record<string, "ok" | "warn" | "bad" | "mut"> = {
  ok: "ok",
  warn: "warn",
  bad: "bad",
  unknown: "mut",
};

export function SetupPage({
  setup,
  bots,
  repos,
}: {
  setup: SetupView;
  bots: BotCard[];
  repos: RepoRow[];
}) {
  const now = useNow(5000);
  const problems = setup.checks.filter((c) => c.status === "bad" || c.status === "warn").length;
  return (
    <main className="mx-auto max-w-[1120px] px-6 pt-5 pb-16">
      <h1 className="text-xl font-[650] tracking-tight">Setup</h1>
      <p className="mt-1 max-w-[820px] text-[13.5px] text-mut">
        What crq needs, and whether this fleet has it. Useful after the first run too — it is where to look
        when a host stops fixing or a lease goes missing.
      </p>

      <div className="my-3.5 flex flex-wrap items-center gap-3 rounded-[10px] border border-edge bg-card px-5 py-4 shadow-card">
        <h2 className="text-[15px] font-[650]">
          {problems === 0 ? "Everything crq checks is in place" : `${problems} thing(s) need attention`}
        </h2>
        <span className="flex flex-wrap items-center gap-2 text-[12.5px]">
          <Pill tone="ok">{setup.ready} ready</Pill>
          {setup.attention > 0 && <Pill tone="warn">{setup.attention} need attention</Pill>}
          {setup.optional > 0 && <Pill tone="mut">{setup.optional} optional missing</Pill>}
        </span>
        <span className="ml-auto text-[12.5px] text-faint">
          {setup.fleet?.length ?? 0} host(s) reporting · updates as they do
        </span>
      </div>

      {(setup.fleet?.length ?? 0) > 0 && <FleetTools fleet={setup.fleet!} now={now} />}

      <div className="grid grid-cols-2 gap-4 max-[860px]:grid-cols-1">
        <Card title="Review bots" count={bots.filter((b) => b.enabled).length}>
          <div className="px-[18px] pb-3.5 pt-1 text-[13px]">
            {bots.filter((b) => b.enabled).length === 0 ? (
              <Empty>No reviewer is enabled.</Empty>
            ) : (
              <ul>
                {bots
                  .filter((b) => b.enabled)
                  .map((b) => (
                    <li key={b.login} className="flex items-center gap-2 py-1">
                      <BotIcon login={b.login} name={b.name} size={18} />
                      <span className="font-[550]">{b.name}</span>
                      <span className="ml-auto">
                        <Pill tone={b.status === "working" ? "ok" : b.status === "silent" ? "bad" : "mut"}>
                          {b.status === "working"
                            ? "working"
                            : b.status === "silent"
                              ? "never answered"
                              : "not verified"}
                        </Pill>
                      </span>
                    </li>
                  ))}
              </ul>
            )}
            <a href="#/bots" className="mt-1.5 inline-block text-[12.5px] text-acc hover:underline">
              Compare and set them up →
            </a>
          </div>
        </Card>

        <Card title="Repositories" count={repos.filter((r) => r.reviewed).length}>
          <div className="px-[18px] pb-3.5 pt-1 text-[13px]">
            {repos.filter((r) => r.reviewed).length === 0 ? (
              <Empty>Nothing is enrolled.</Empty>
            ) : (
              <ul>
                {repos
                  .filter((r) => r.reviewed)
                  .slice(0, 8)
                  .map((r) => (
                    <li key={r.repo} className="flex items-center gap-2 py-1">
                      <RepoIcon repo={r.repo} />
                      <span>{short(r.repo)}</span>
                      <span className="ml-auto text-[12px] text-faint">{r.enrollment}</span>
                    </li>
                  ))}
              </ul>
            )}
            {/* An env-enrolled repository is one the other hosts may not agree
                about, which is the whole reason enrollment records exist. */}
            {repos.filter((r) => r.reviewed && r.enrollment === "env").length > 0 && (
              <p className="mt-1.5 rounded-lg border border-warn-edge bg-warn-bg px-2.5 py-1.5 text-[12px] text-warn">
                {repos.filter((r) => r.reviewed && r.enrollment === "env").length} still enrolled by
                this host's env file — the other hosts decide for themselves.{" "}
                <code>crq fleet adopt</code> records them for everyone.
              </p>
            )}
            <a href="#/repos" className="mt-1.5 inline-block text-[12.5px] text-acc hover:underline">
              Manage repositories →
            </a>
          </div>
        </Card>
      </div>

      <Card title="Checks" count={setup.checks.length}>
        <div className="px-[18px] pb-3">
          {setup.checks.map((c) => (
            <div key={c.key} className="flex items-baseline gap-3 border-b border-[#EEF0F3] py-2 text-[13.5px] last:border-none">
              <Pill tone={STATUS_TONE[c.status] ?? "mut"}>{c.status}</Pill>
              <span className="font-[550]">{c.label}</span>
              <span className="text-faint">{c.detail}</span>
            </div>
          ))}
        </div>
      </Card>

      <Card title="Command-line tools" end={`on ${setup.tools_host}`}>
        <p className="px-[18px] pt-1 text-[12.5px] text-faint">
          This machine, probed from this server's own PATH. The per-host table above is the reported
          one — a tool has to be visible to the <i>service</i>, not just to your shell, and only the
          host itself can say whether it is.
        </p>
        <table className="mt-1.5 w-full border-collapse">
          <thead>
            <tr>
              <Th>Tool</Th>
              <Th>Purpose</Th>
              <Th>Status</Th>
              <Th className="c-host">Path</Th>
            </tr>
          </thead>
          <tbody>
            {setup.tools.map((t) => (
              <tr key={t.name} className="hover:bg-[#F7F8FA]">
                <Td className="font-mono text-[13px] font-semibold">{t.name}</Td>
                <Td className="text-[12.5px] text-mut">
                  {t.purpose}
                  {!t.found && (t.fix?.length ?? 0) > 0 && (
                    <details className="mt-1 rounded-lg border border-edge bg-[#FBFBFC] px-2 py-1">
                      <summary className="cursor-pointer text-[12px] font-[550]">How to fix it</summary>
                      <pre className="mt-1 overflow-x-auto text-[11.5px] whitespace-pre-wrap">
                        {t.fix!.join("\n")}
                      </pre>
                      <p className="mt-1 text-[11.5px] text-faint">
                        Installing it for your shell is not enough: the service gets its own PATH. Add
                        the directory with <code>systemctl --user edit crq-autofix</code> and an{" "}
                        <code>Environment=PATH=…</code> line, or reinstall so crq writes it for you.
                      </p>
                    </details>
                  )}
                </Td>
                <Td>
                  {t.found ? (
                    <Pill tone="ok">found</Pill>
                  ) : t.required ? (
                    <Pill tone="bad">missing</Pill>
                  ) : (
                    <Pill tone="mut">not installed</Pill>
                  )}
                  {t.required && <span className="ml-2 text-[11.5px] text-faint">required</span>}
                </Td>
                <Td className="c-host font-mono text-[12px] text-faint">{t.path ?? "—"}</Td>
              </tr>
            ))}
          </tbody>
        </table>
      </Card>

      <Card title="Hosts" count={setup.hosts.length}>
        <div className="flex flex-wrap gap-3 px-[18px] py-3">
          {setup.hosts.length === 0 && <Empty>No host has written state in the last day.</Empty>}
          {setup.hosts.map((h) => (
            <div
              key={h.name}
              className={`min-w-[220px] flex-1 rounded-lg border px-3.5 py-2.5 text-[12.5px] text-mut ${
                h.health === "unhealthy" ? "border-bad-edge bg-bad-bg" : "border-edge"
              }`}
            >
              <div className="flex items-center gap-2">
                <span className="font-mono text-[13.5px] font-[650] text-ink">{h.name}</span>
                {h.health && (
                  <Pill tone={h.health === "healthy" ? "ok" : h.health === "unhealthy" ? "bad" : "mut"}>
                    {h.health === "unknown" ? "no recent signal" : h.health}
                  </Pill>
                )}
              </div>
              <div className="mt-1">{h.roles?.length ? h.roles.join(" · ") : "writes state"}</div>
              {h.last_seen && <div className="mt-0.5">last write {ago(h.last_seen, now)}</div>}
              {h.last_error && <div className="mt-1 font-mono text-[11.5px] break-words">{h.last_error}</div>}
            </div>
          ))}
        </div>
      </Card>
    </main>
  );
}

/**
 * What each host can actually reach.
 *
 * The single most useful table in the product, because every question about a
 * fleet turns into a per-host question and crq could previously only answer for
 * whichever machine you happened to be asking from. The failure it exists to
 * show is the quiet one: a tool installed for your shell and invisible to the
 * service, which looks fine everywhere except in the fix session that needs it.
 *
 * A host reports its OWN PATH, and a daemon reports the service's — so a tick
 * here means the thing that runs fix sessions can find it, not that you can.
 */
function FleetTools({ fleet, now }: { fleet: HostTools[]; now: number }) {
  const names = [...new Set(fleet.flatMap((h) => h.tools.map((t) => t.name)))];
  return (
    <Card title="Tools, per host" count={`${fleet.length} host(s)`}>
      <div className="overflow-x-auto px-[18px] pb-3">
        <table className="w-full border-collapse text-[12.5px]">
          <thead>
            <tr>
              <Th>Host</Th>
              {names.map((n) => (
                <Th key={n} className="text-center">
                  {n}
                </Th>
              ))}
            </tr>
          </thead>
          <tbody>
            {fleet.map((h) => (
              <tr key={h.host} className="border-t border-[#EEF0F3]">
                <Td>
                  <div className="font-[550]">{h.host}</div>
                  <div className="text-[11.5px] text-faint">
                    {h.version && (
                      <span className={h.behind ? "text-warn" : ""}>
                        crq {h.version}
                        {h.behind ? " · behind" : ""}
                      </span>
                    )}
                    {(h.roles?.length ?? 0) > 0 && ` · ${h.roles!.join(", ")}`}
                    {h.stale && h.at && <span className="text-warn"> · last heard {ago(h.at, now)}</span>}
                  </div>
                </Td>
                {names.map((n) => {
                  const t = h.tools.find((x) => x.name === n);
                  return (
                    <Td key={n} className="text-center">
                      {t?.path ? (
                        <span title={`${t.path}${t.version ? ` · ${t.version}` : ""}`} className="text-ok">
                          ✓
                        </span>
                      ) : (
                        <span className="text-faint" title="not on the PATH this host's service runs with">
                          —
                        </span>
                      )}
                    </Td>
                  );
                })}
              </tr>
            ))}
          </tbody>
        </table>
        <p className="pt-2 text-[11.5px] text-faint">
          <b>These are command-line tools, not the review bots.</b> The Codex and CodeRabbit
          reviewers are GitHub apps and need nothing installed here — a host missing the{" "}
          <code>codex</code> CLI still gets Codex reviews. Whether a REVIEWER works is on the{" "}
          <a href="#/bots" className="text-acc hover:underline">
            Bots
          </a>{" "}
          page. What these decide is whether this host can run a fix session or a local preflight.
          <br />
          Each host probes the PATH its own crq process runs with. A daemon reports the SERVICE's
          PATH, which is the one that decides whether a fix session can start — a tool installed for
          your shell and missing here is exactly the failure that looks fine until it is needed. Fix
          it by adding the directory to the unit: <code>systemctl --user edit crq-autofix</code>, then{" "}
          <code>Environment=PATH=…</code>, then reinstall with <code>crq autofix install</code>.
        </p>
      </div>
    </Card>
  );
}

/* --------------------------------------------------------------- Settings */

export function SettingsPage({
  settings,
  bots,
  onSnapshot,
}: {
  settings: SettingsView;
  bots: BotCard[];
  onSnapshot?: (s: Snapshot) => void;
}) {
  const c = settings.config;
  return (
    <main className="mx-auto max-w-[1120px] px-6 pt-5 pb-16">
      <h1 className="text-xl font-[650] tracking-tight">Fleet settings</h1>
      <p className="mt-1 max-w-[840px] text-[13.5px] text-mut">
        The defaults every repository inherits. The editable ones live in shared state, so one host's env
        file is no longer the fleet's source of truth; everything below the first card is this server's own
        environment, shown for reference.
      </p>

      {settings.fleet && (
        <FleetEditor fleet={settings.fleet} bots={bots} onSnapshot={onSnapshot} />
      )}

      {settings.env && <EnvEditor env={settings.env} onSnapshot={onSnapshot} />}

      <Card title="CodeRabbit account" end="the metered lane's shared quota">
        <div className="flex flex-wrap gap-4 px-[18px] py-3">
          <Box k="Scope" v={settings.quota.scope || "—"} />
          <Box
            k="Remaining"
            v={settings.quota.remaining === null || settings.quota.remaining === undefined ? "unknown" : String(settings.quota.remaining)}
          />
          <Box k="Blocked until" v={settings.quota.blocked_until ? clock(settings.quota.blocked_until) : "not blocked"} />
          <Box k="Source" v={settings.quota.source || "—"} />
          <Box k="Checked" v={clock(settings.quota.checked_at)} />
        </div>
      </Card>

      <Card title="Reviewers" count={c.reviewers.length}>
        <table className="mt-1.5 w-full border-collapse">
          <thead>
            <tr>
              <Th>Reviewer</Th>
              <Th>Role</Th>
              <Th>Required</Th>
              <Th>Trigger</Th>
              <Th className="c-host">Command</Th>
            </tr>
          </thead>
          <tbody>
            {c.reviewers.map((r) => (
              <tr key={r.login} className="hover:bg-[#F7F8FA]">
                <Td className="font-[550]">{r.name}</Td>
                <Td>{r.primary ? <Pill tone="warn">primary · metered</Pill> : <Pill tone="ok">co-reviewer · free</Pill>}</Td>
                <Td>{r.required ? "waits for it" : <span className="text-faint">no</span>}</Td>
                <Td>
                  {r.trigger || "—"}
                  {r.trigger === "selfheal" && r.grace ? ` · ${r.grace}` : ""}
                </Td>
                <Td className="c-host font-mono text-[12.5px] text-mut">{r.command || "—"}</Td>
              </tr>
            ))}
          </tbody>
        </table>
      </Card>

      <Card title="Pacing &amp; limits" end="protects the shared quota and GitHub's REST budget">
        <div className="px-[18px] pb-3">
          <Row k="Minimum interval between fires" v={c.min_interval} d="the queue's main throttle" />
          <Row k="In-flight timeout" v={c.inflight_timeout} d="release a round whose bot never answered" />
          <Row k="Watch interval" v={c.watch_interval} d="how often open PRs are driven forward" />
        </div>
      </Card>

      <Card title="Autofix defaults">
        <div className="px-[18px] pb-3">
          <Row k="Agent command" v={c.autofix_command?.join(" ") || "not configured"} d="one argv for the fleet" />
          <Row k="Max attempts per head" v={String(c.autofix_max_attempts ?? "—")} />
          <Row k="Concurrency" v={c.autofix_concurrency ? String(c.autofix_concurrency) : "uncapped"} />
          <Row k="Fix fork PRs" v={c.autofix_forks ? "yes" : "no"} />
          <Row k="Workspace" v={c.workspace_root || "—"} />
        </div>
      </Card>

      <Card title="Automatic review">
        <div className="px-[18px] pb-3">
          <Row k="Scope" v={c.scope?.join(", ") || "—"} d="owners searched for open PRs" />
          <Row k="Allowlist" v={c.allow_repos?.join(", ") || "everything in scope"} d="CRQ_REPOS" />
          <Row k="Excluded" v={c.exclude_repos?.join(", ") || "none"} d="CRQ_EXCLUDE" />
          <Row k="Skip authors" v={c.skip_authors?.join(", ") || "none"} />
          <Row k="Skip marker" v={c.skip_marker || "—"} d="put this in a PR body to keep the fleet off it" />
        </div>
      </Card>

      <Card title="Plumbing" end="read-only — crq init owns these">
        <div className="px-[18px] pb-3">
          {settings.plumbing.map((p) => (
            <Row key={p.key} k={p.key} v={p.value} d={p.detail} />
          ))}
        </div>
      </Card>
    </main>
  );
}

function Row({ k, v, d }: { k: string; v: string; d?: string }) {
  return (
    <div className="grid grid-cols-[260px_1fr] gap-3 border-b border-[#EEF0F3] py-2 text-[13.5px] last:border-none max-[1150px]:grid-cols-[minmax(0,1fr)] max-[1150px]:gap-1">
      <span className="font-medium">
        {k}
        {d && <span className="block text-xs font-normal text-faint">{d}</span>}
      </span>
      <span className="font-mono text-[12.5px] break-words text-mut">{v}</span>
    </div>
  );
}

function Box({ k, v }: { k: string; v: string }) {
  return (
    <div className="min-w-[120px] rounded-lg border border-edge px-3.5 py-2">
      <div className="text-[11px] font-medium tracking-[0.06em] text-faint uppercase">{k}</div>
      <div className="mt-0.5 text-[15px] font-[650]">{v}</div>
    </div>
  );
}

function short(repo: string): string {
  return repo.split("/").pop() ?? repo;
}
