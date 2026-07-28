import { useEffect, useState } from "react";
import type { RepoSolver, Snapshot } from "./api";
import { act } from "./actions";
import { Card, Pill } from "./ui";

/**
 * How this repository's fix sessions run.
 *
 * The agent is deliberately not editable here. It is chosen by `crq autofix
 * install` and baked into the session script, because switching between claude
 * and codex is a different command line rather than a different flag — so the
 * card names it and stops there, instead of offering a control that would
 * quietly do nothing.
 *
 * Everything else resolves through three layers and each row says which one
 * answered: a value reading `env` is this host's file, `fleet` is the shared
 * default, `repo` is this repository's own.
 */
export function SolverEditor({
  repo,
  solver,
  onSnapshot,
}: {
  repo: string;
  solver: RepoSolver;
  onSnapshot?: (s: Snapshot) => void;
}) {
  const [models, setModels] = useState(
    solver.models?.length ? solver.models : solver.model ? [solver.model] : [""],
  );
  const [effort, setEffort] = useState(solver.effort ?? "");
  const [prompt, setPrompt] = useState(solver.prompt ?? "");
  const [attempts, setAttempts] = useState(String(solver.max_attempts));
  const [forks, setForks] = useState(solver.forks);
  const [authors, setAuthors] = useState((solver.skip_authors ?? []).join(", "));
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [warning, setWarning] = useState<string | null>(null);

  const server = {
    models: solver.models?.length ? solver.models : solver.model ? [solver.model] : [""],
    effort: solver.effort ?? "",
    prompt: solver.prompt ?? "",
    attempts: String(solver.max_attempts),
    forks: solver.forks,
    authors: (solver.skip_authors ?? []).join(", "),
  };
  // Every SSE revision rebuilds this view object, so depending on its IDENTITY
  // re-ran the reset whenever anything at all moved in the queue — and typing
  // into the prompt box while a round fired somewhere else lost the edit. The
  // dependency is the settings' own VALUES: they change when the settings do.
  const serverRev = JSON.stringify(server);
  useEffect(() => {
    setModels(server.models);
    setEffort(server.effort);
    setPrompt(server.prompt);
    setAttempts(server.attempts);
    setForks(server.forks);
    setAuthors(server.authors);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [repo, serverRev]);

  const changed = {
    models: JSON.stringify(models) !== JSON.stringify(server.models),
    effort: effort !== server.effort,
    prompt: prompt !== server.prompt,
    attempts: attempts !== server.attempts,
    forks: forks !== server.forks,
    authors: authors !== server.authors,
  };
  const dirty = Object.values(changed).some(Boolean);

  const post = async (edited: NonNullable<Parameters<typeof act>[1]["solver"]>) => {
    setBusy(true);
    setError(null);
    setWarning(null);
    try {
      const res = await act("solver", { repo, solver: edited });
      onSnapshot?.(res.snapshot);
      setWarning(res.warning ?? null);
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setBusy(false);
    }
  };

  // Hand one setting back to the fleet. The other rows say it in their own
  // values — an empty model, 0 attempts — but "Blocked" is a real fork policy
  // and an empty author list means "skip nobody", so these two can only return
  // to inheritance by asking for it.
  const inherit = (field: "models" | "effort" | "forks" | "skip_authors") =>
    void post(
      field === "models"
        ? { unset_models: true }
        : field === "effort"
          ? { unset_effort: true }
        : field === "forks"
          ? { unset_forks: true }
          : { unset_skip_authors: true },
    );

  // Only what was actually edited. The values shown here are EFFECTIVE ones,
  // resolved through env, fleet and repository — so posting all six recorded
  // the inherited ones as this repository's own answer, and a later fleet
  // change to the attempt count or the fork policy stopped reaching a
  // repository whose owner had only ever edited its prompt. The API omits to
  // mean inherit; that is what this preserves.
  const save = (clear = false) =>
    void post(
      clear
        ? { clear: true }
        : {
            ...(changed.models ? { models: models.filter(Boolean) } : {}),
            ...(changed.effort ? { effort } : {}),
            ...(changed.prompt ? { prompt } : {}),
            ...(changed.attempts ? { max_attempts: Number(attempts) || 0 } : {}),
            ...(changed.forks ? { forks } : {}),
            ...(changed.authors
              ? {
                  skip_authors: authors
                    .split(",")
                    .map((a) => a.trim())
                    .filter(Boolean),
                }
              : {}),
          },
    );

  const src = (key: string) => solver.sources?.[key] ?? "env";
  const modelChoices = ["", ...models.filter(Boolean), ...(solver.model_choices ?? [])].filter(
    (model, index, all) => all.indexOf(model) === index,
  );
  const setRankedModel = (index: number, model: string) =>
    setModels((current) => current.map((value, i) => (i === index ? model : value)));
  const moveModel = (index: number, direction: -1 | 1) =>
    setModels((current) => {
      const next = [...current];
      const target = index + direction;
      if (target < 0 || target >= next.length) return current;
      [next[index], next[target]] = [next[target], next[index]];
      return next;
    });
  const addFallback = () => {
    const unused = modelChoices.find((model) => model !== "" && !models.includes(model));
    if (unused !== undefined) setModels((current) => [...current, unused]);
  };

  return (
    <>
      {warning && (
        <div className="mb-3 rounded-lg border border-warn-edge bg-warn-bg px-3 py-2 text-[12.5px] text-warn">
          {warning}
        </div>
      )}
    <Card
      title="Fix sessions"
      end={solver.overridden ? `override${solver.by ? ` by ${solver.by}` : ""}` : "following the fleet"}
    >
      <div className="px-[18px] pb-4 pt-1">
        <p className="text-[12.5px] text-faint">
          {solver.agent ? (
            <>
              Agent: <b className="font-mono">{solver.agent.split("/").pop()}</b> — chosen at install time
              and the same for every repository.{" "}
            </>
          ) : (
            <>
              The agent is baked into the installed session script and this server does not read it —
              CRQ_DISPATCH_CMD is set for the autofix service, not for this one.{" "}
            </>
          )}
          Each row below says which layer its value came from.
        </p>

        {(solver.agent_on?.length ?? 0) > 0 && (
          <p className="mt-2 flex flex-wrap items-center gap-2 text-[12px]">
            <span className="text-faint">Agent available on:</span>
            {solver.agent_on!.map((h) => (
              <span
                key={h.host}
                title={
                  h.stale
                    ? h.has
                      ? `last reported at ${h.path}`
                      : "the autofix service's last probe is stale"
                    : h.has
                      ? h.path
                      : "not on the PATH that host's service runs with"
                }
                className={`rounded-full border px-2 py-0.5 ${
                  h.stale || h.has === undefined
                    ? "border-edge text-faint"
                    : h.has
                      ? "border-ok-edge bg-ok-bg text-ok"
                      : "border-bad-edge bg-bad-bg text-bad"
                }`}
              >
                {h.host} {h.stale ? "· stale" : h.has === undefined ? "· unknown" : h.has ? "✓" : "missing"}
              </span>
            ))}
          </p>
        )}

        {solver.lagging_hosts && solver.lagging_hosts.length > 0 && (
          <div className="mt-2.5 rounded-lg border border-warn-edge bg-warn-bg px-3 py-2 text-[12.5px] text-warn">
            These hosts run a binary that predates per-repository fix settings and will use their own
            install-time values: {solver.lagging_hosts.join(", ")}
          </div>
        )}

        <table className="mt-2.5 w-full border-collapse">
          <tbody>
            <Row label="Models" source={src("models")}>
              <div className="space-y-1.5">
                {models.map((model, index) => (
                  <div key={index} className="flex items-center gap-1.5">
                    <span className="w-4 text-right font-mono text-[11px] text-faint">{index + 1}</span>
                    <select
                      value={model}
                      onChange={(e) => setRankedModel(index, e.target.value)}
                      className="w-56 rounded-lg border border-edge bg-[#FBFBFC] px-2 py-1 font-mono text-[12.5px]"
                    >
                      {modelChoices.map((choice) => (
                        <option
                          key={choice || "default"}
                          value={choice}
                          disabled={choice !== model && models.includes(choice)}
                        >
                          {choice || "the agent's own default"}
                        </option>
                      ))}
                    </select>
                    <button
                      type="button"
                      aria-label="Move model up"
                      disabled={index === 0}
                      onClick={() => moveModel(index, -1)}
                      className="rounded border border-edge px-1.5 py-0.5 text-[12px] text-mut disabled:opacity-30"
                    >
                      ↑
                    </button>
                    <button
                      type="button"
                      aria-label="Move model down"
                      disabled={index === models.length - 1}
                      onClick={() => moveModel(index, 1)}
                      className="rounded border border-edge px-1.5 py-0.5 text-[12px] text-mut disabled:opacity-30"
                    >
                      ↓
                    </button>
                    {models.length > 1 && (
                      <button
                        type="button"
                        aria-label="Remove model"
                        onClick={() => setModels((current) => current.filter((_, i) => i !== index))}
                        className="rounded px-1.5 py-0.5 text-[12px] text-bad hover:bg-bad-bg"
                      >
                        remove
                      </button>
                    )}
                  </div>
                ))}
                <div className="flex items-center gap-2 pl-[22px]">
                  <button
                    type="button"
                    disabled={!modelChoices.some((model) => model !== "" && !models.includes(model))}
                    onClick={addFallback}
                    className="text-[12px] font-semibold text-acc hover:underline disabled:opacity-40"
                  >
                    + add fallback
                  </button>
                  <span className="text-[12px] text-faint">
                    provider outages advance without spending an attempt
                  </span>
                  <Inherit
                    shown={src("models") === "repo"}
                    busy={busy}
                    onClick={() => inherit("models")}
                  />
                </div>
              </div>
            </Row>
            <Row label="Effort" source={src("effort")}>
              <select
                value={effort}
                onChange={(e) => setEffort(e.target.value)}
                className="rounded-lg border border-edge bg-[#FBFBFC] px-2 py-1 text-[12.5px]"
              >
                <option value="">the agent's own default</option>
                {["low", "medium", "high", "xhigh", "max"].map((e) => (
                  <option key={e} value={e}>
                    {e}
                  </option>
                ))}
              </select>
              <Inherit
                shown={src("effort") === "repo"}
                busy={busy}
                onClick={() => inherit("effort")}
              />
            </Row>
            <Row label="Attempts" source={src("max_attempts")}>
              <input
                value={attempts}
                inputMode="numeric"
                onChange={(e) => setAttempts(e.target.value.replace(/[^0-9]/g, ""))}
                className="w-16 rounded-lg border border-edge bg-[#FBFBFC] px-2 py-1 font-mono text-[12.5px]"
              />
              <span className="ml-2 text-[12px] text-faint">
                failures per cycle; outages do not count, exhausted cycles cool down and retry
              </span>
            </Row>
            <Row label="Fork PRs" source={src("forks")}>
              <button
                type="button"
                onClick={() => setForks((v) => !v)}
                className={`rounded-lg border px-3 py-1 text-[12.5px] font-semibold ${
                  forks ? "border-warn-edge bg-warn-bg text-warn" : "border-edge text-mut"
                }`}
              >
                {forks ? "Allowed" : "Blocked"}
              </button>
              <span className="ml-2 text-[12px] text-faint">
                a session runs an agent over that branch's code with approvals bypassed
              </span>
              <Inherit shown={src("forks") === "repo"} busy={busy} onClick={() => inherit("forks")} />
            </Row>
            <Row label="Skip authors" source={src("skip_authors")}>
              <input
                value={authors}
                onChange={(e) => setAuthors(e.target.value)}
                placeholder="dependabot[bot], …"
                className="w-full rounded-lg border border-edge bg-[#FBFBFC] px-2 py-1 font-mono text-[12.5px]"
              />
              <Inherit
                shown={src("skip_authors") === "repo"}
                busy={busy}
                onClick={() => inherit("skip_authors")}
              />
            </Row>
            <Row label="Extra prompt" source={src("prompt")}>
              <textarea
                value={prompt}
                rows={2}
                onChange={(e) => setPrompt(e.target.value)}
                placeholder="standing instruction appended to every fix session here"
                className="w-full rounded-lg border border-edge bg-[#FBFBFC] px-2 py-1 text-[12.5px]"
              />
            </Row>
          </tbody>
        </table>

        {error && (
          <div className="mt-3 rounded-lg border border-bad-edge bg-bad-bg px-3 py-2 text-[12.5px] text-bad">
            {error}
          </div>
        )}

        <div className="mt-3.5 flex flex-wrap items-center gap-2.5">
          <button
            type="button"
            disabled={!dirty || busy}
            onClick={() => void save()}
            className="rounded-lg bg-ink px-4 py-1.5 text-[13px] font-semibold text-white disabled:opacity-45"
          >
            {busy ? "Saving…" : "Save fix settings"}
          </button>
          {dirty && <span className="text-[12.5px] text-warn">unsaved changes</span>}
          {solver.overridden && !dirty && (
            <button
              type="button"
              disabled={busy}
              onClick={() => void save(true)}
              className="ml-auto text-[12.5px] text-acc hover:underline disabled:opacity-45"
            >
              Reset to the fleet default
            </button>
          )}
        </div>

        <p className="mt-2.5 text-[11.5px] text-faint">
          These reach the session through its environment, not its arguments — the watcher's argv is
          fixed when it starts, and one watcher handles every repository. A session script from an
          install that predates this ignores them, so reinstalling autofix is what turns them on.
        </p>
      </div>
    </Card>
    </>
  );
}

/**
 * The way back to the fleet for a setting whose own values cannot say it.
 * Shown only where this repository actually holds an override, because on the
 * other layers there is nothing to give back.
 */
function Inherit({ shown, busy, onClick }: { shown: boolean; busy: boolean; onClick: () => void }) {
  if (!shown) return null;
  return (
    <button
      type="button"
      disabled={busy}
      onClick={onClick}
      className="ml-2 text-[12px] text-acc hover:underline disabled:opacity-45"
    >
      follow the fleet
    </button>
  );
}

function Row({
  label,
  source,
  children,
}: {
  label: string;
  source: string;
  children: React.ReactNode;
}) {
  return (
    <tr className="border-b border-[#EEF0F3] last:border-none">
      <td className="w-32 py-2 pr-3 align-top">
        <div className="text-[13px] font-[550]">{label}</div>
        <Pill tone={source === "repo" ? "ok" : source === "fleet" ? "acc" : "mut"}>{source}</Pill>
      </td>
      <td className="py-2 align-middle">{children}</td>
    </tr>
  );
}
