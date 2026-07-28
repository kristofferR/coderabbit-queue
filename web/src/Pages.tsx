import type { BotCard, RepoRow, SetupView, SettingsView, Snapshot } from "./api";
import { useEffect, useState } from "react";
import { act } from "./actions";
import { Confirm } from "./Confirm";
import { BotIcon, Card, Empty, Pill, PRLink, RepoIcon, Td, Th, Toggle } from "./ui";
import { ago, clock, useNow } from "./time";
import { AddRepo, EnrollmentEditor } from "./AddRepo";
import { FleetEditor } from "./FleetEditor";
import { SolverEditor } from "./SolverEditor";

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

export function ReposPage({
  repos,
  bots,
  onSnapshot,
}: {
  repos: RepoRow[];
  bots: BotCard[];
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
              const e = ENROLLMENT[r.enrollment] ?? ENROLLMENT.off;
              const label = r.enrollment === "state" && !r.reviewed ? "Turned off" : e.label;
              const tone = r.enrollment === "state" && !r.reviewed ? "mut" : e.tone;
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
                        <Pill tone={tone}>{label}</Pill>
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
            <Pill tone={selected.reviewed ? (ENROLLMENT[selected.enrollment] ?? ENROLLMENT.off).tone : "mut"}>
              {selected.reviewed
                ? (ENROLLMENT[selected.enrollment] ?? ENROLLMENT.off).label
                : selected.enrollment === "state"
                  ? "Turned off"
                  : (ENROLLMENT[selected.enrollment] ?? ENROLLMENT.off).label}
            </Pill>
            <span className="text-[12px] text-faint">
              {(ENROLLMENT[selected.enrollment] ?? ENROLLMENT.off).note}
            </span>
            {selected.override && <Pill tone="warn">Override</Pill>}
            {selected.primary_off && <Pill tone="bad">Primary off</Pill>}
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

  useEffect(() => {
    setRuns(repo.reviewers);
    setRequired(repo.required);
  }, [repo.repo, repo.reviewers, repo.required]);

  // The primary is the one metered reviewer, so its Runs toggle is a budget
  // decision (a private repo on a free plan gets nothing from it) and travels
  // in its own field — the co-reviewer list only accepts registry bots.
  const primaryBot = bots.find((b) => b.primary);
  const primaryOn = primaryBot ? runs.includes(primaryBot.name) : false;
  const primaryWas = primaryBot ? repo.reviewers.includes(primaryBot.name) : false;

  const dirty =
    runs.join() !== repo.reviewers.join() || required.join() !== repo.required.join();
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
          primary off means this repo never spends the shared review allowance.
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
            {bots.map((b) => (
              <tr key={b.login}>
                <Td>
                  <span className="flex items-center gap-2.5">
                    <BotIcon login={b.login} name={b.name} size={20} />
                    <span className="font-[550]">{b.name}</span>
                  </span>
                </Td>
                <Td className="text-center">
                  <Toggle
                    on={runs.includes(b.name)}
                    title={b.primary ? "the metered reviewer — turning it off spends no quota here" : undefined}
                    onClick={() => toggleRuns(b.name)}
                  />
                </Td>
                <Td className="text-center">
                  <Toggle on={required.includes(b.name)} onClick={() => toggleRequired(b.name)} />
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
                {v === "default" ? "Default (on)" : v}
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

export function BotsPage({
  bots,
  onSnapshot,
}: {
  bots: BotCard[];
  onSnapshot?: (s: Snapshot) => void;
}) {
  const now = useNow(5000);
  const [busy, setBusy] = useState<string | null>(null);
  const [error, setError] = useState<string | null>(null);
  const primaryName = bots.find((b) => b.primary)?.name ?? "";

  // Turning one off writes the fleet's whole co-reviewer list, because that is
  // what the setting IS — "these bots run" rather than a flag per bot. The
  // primary is absent from it: it is not a co-reviewer, and turning it off is a
  // per-repository decision (a private repo on a free plan) rather than a fleet
  // one.
  const save = async (name: string, runs: boolean, required: boolean) => {
    setBusy(name);
    setError(null);
    try {
      const co = bots.filter((b) => !b.primary && b.enabled).map((b) => b.name);
      const req = bots.filter((b) => b.required).map((b) => b.name);
      const nextRuns = runs ? [...new Set([...co, name])] : co.filter((n) => n !== name);
      const nextReq = required ? [...new Set([...req, name])] : req.filter((n) => n !== name);
      const res = await act("fleet", {
        fleet: { cobots: nextRuns.filter((n) => n !== primaryName), required: nextReq },
      });
      onSnapshot?.(res.snapshot);
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setBusy(null);
    }
  };

  // Requiring implies running, and dropping a reviewer drops its requirement —
  // the same two rules the per-repo Reviewers card applies, because it is the
  // same pair of questions asked at a different scope.
  const toggleRuns = (b: BotCard) => save(b.name, !b.enabled, b.enabled ? false : b.required);
  const toggleRequired = (b: BotCard) => save(b.name, b.required ? b.enabled : true, !b.required);

  return (
    <main className="mx-auto max-w-[1120px] px-6 pt-5 pb-16">
      <h1 className="text-xl font-[650] tracking-tight">Review bots</h1>
      <p className="mt-1 max-w-[760px] text-[13.5px] text-mut">
        Every reviewer crq knows how to drive, running here or not. “Last seen” is what crq itself
        recorded — a trigger it posted, or a claim it observed — not a status read from the vendor.
      </p>
      {error && (
        <div className="mt-3 rounded-lg border border-bad-edge bg-bad-bg px-3 py-2 text-[12.5px] text-bad">
          {error}
        </div>
      )}
      <div className="mt-4 grid grid-cols-2 gap-4 max-[940px]:grid-cols-1">
        {bots.map((b) => (
          <section
            key={b.login}
            className={`flex flex-col rounded-[10px] border bg-card shadow-card ${
              b.enabled ? "border-edge" : "border-dashed border-edge opacity-70"
            }`}
          >
            <header className="flex items-center gap-3 px-5 pt-3.5">
              <BotIcon login={b.login} name={b.name} size={36} />
              <div>
                <h2 className="text-base font-[650]">{b.name}</h2>
                <div className="text-xs text-faint">
                  {b.primary ? "primary reviewer" : "co-reviewer"} ·{" "}
                  {b.metered ? "spends the shared quota" : "free, no crq quota"}
                </div>
              </div>
              <span className="ml-auto">
                {b.last_seen ? <Pill tone="ok">Seen {ago(b.last_seen, now)}</Pill> : <Pill tone="mut">Not seen yet</Pill>}
              </span>
            </header>
            <div className="flex-1 px-5 pt-3 text-[13px] text-mut">
              <dl className="grid grid-cols-[110px_1fr] gap-y-1.5">
                <dt className="text-faint">Runs</dt>
                <dd className="flex items-center gap-2">
                  <Toggle
                    on={b.enabled}
                    locked={b.primary || busy === b.name}
                    title={
                      b.primary
                        ? "the primary runs everywhere by default — turn it off for one project on that project's page"
                        : "runs on every repository that has not overridden its reviewers"
                    }
                    onClick={() => void toggleRuns(b)}
                  />
                  <span className="text-faint">
                    {b.enabled ? "reviews every repo following the fleet" : "not run anywhere"}
                  </span>
                </dd>
                <dt className="text-faint">Required</dt>
                <dd className="flex items-center gap-2">
                  <Toggle
                    on={b.required}
                    locked={busy === b.name}
                    title="convergence waits for it"
                    onClick={() => void toggleRequired(b)}
                  />
                  <span className="text-faint">
                    {b.required ? "convergence waits for it" : "does not gate convergence"}
                  </span>
                </dd>
                {b.command && (
                  <>
                    <dt className="text-faint">Trigger</dt>
                    <dd className="font-mono text-[12.5px]">{b.command}</dd>
                  </>
                )}
                {b.trigger && (
                  <>
                    <dt className="text-faint">Mode</dt>
                    <dd>
                      {b.trigger}
                      {b.trigger === "selfheal" && b.grace ? ` · grace ${b.grace}` : ""}
                    </dd>
                  </>
                )}
                {b.seen_on && (
                  <>
                    <dt className="text-faint">Last on</dt>
                    <dd>
                      <PRLink repo={b.seen_on.split("#")[0]} pr={Number(b.seen_on.split("#")[1] ?? 0)} />
                    </dd>
                  </>
                )}
                <dt className="text-faint">Per-repo</dt>
                <dd>{b.repo_count > 0 ? `${b.repo_count} repo override(s) name it` : "no repo overrides"}</dd>
              </dl>
            </div>
            <footer className="px-5 pt-3 pb-4 text-[11.5px] text-faint">
              {b.primary
                ? "The primary runs everywhere by default; turn it off for one project on that project's page."
                : "This switch is the fleet default. A repository that names its own reviewers is unaffected."}
            </footer>
          </section>
        ))}
      </div>
    </main>
  );
}

/* ------------------------------------------------------------------ Setup */

const STATUS_TONE: Record<string, "ok" | "warn" | "bad" | "mut"> = {
  ok: "ok",
  warn: "warn",
  bad: "bad",
  unknown: "mut",
};

export function SetupPage({ setup }: { setup: SetupView }) {
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
        <span className="ml-auto text-[12.5px] text-faint">checked continuously · rev-driven</span>
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
          This machine only — crq keeps no tool inventory for other hosts, so a fleet-wide matrix would be
          invented rather than reported. A tool also has to be visible to the <i>service</i>, not just your
          shell.
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
                <Td className="text-[12.5px] text-mut">{t.purpose}</Td>
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
