import { Link } from "@tanstack/react-router";
import { type ReactNode, useEffect, useState } from "react";
import { act } from "./actions";
import type { BotCard, FleetImpact, FleetSettings, Snapshot } from "./api";
import { fleetImpact } from "./api";
import { Confirm } from "./Confirm";
import { BotIcon, Card, Pill, Toggle } from "./ui";
import { sameMembers } from "./ui/utils";
import { useOperation } from "./useOperation";

/**
 * The fleet defaults, editable.
 *
 * Two things make this different from the per-repo panel, and both are visible
 * in the UI rather than assumed. First, every setting says where its current
 * value comes from: a value sourced from `env` is this host's file, so changing
 * it here starts a fleet record that overrides that file everywhere — which is
 * the point, but not something to discover afterwards. Second, saving asks the
 * server what the change would do before doing it, because a fleet save reaches
 * every repository that has not overridden the setting, and adding one required
 * reviewer can reopen dozens of finished rounds.
 */
export function FleetEditor({
  fleet,
  bots,
  onSnapshot,
}: {
  fleet: FleetSettings;
  bots: BotCard[];
  onSnapshot?: (s: Snapshot) => void;
}) {
  const [runs, setRuns] = useState<string[]>([]);
  const [required, setRequired] = useState<string[]>([]);
  const [minInterval, setMinInterval] = useState(fleet.min_interval);
  const [weekly, setWeekly] = useState(String(fleet.weekly_limit));
  const [autofix, setAutofix] = useState(fleet.autofix_default);

  const [impact, setImpact] = useState<FleetImpact | null>(null);
  const { run: runOperation, running: busy, error } = useOperation();

  const fleetRuns = fleet.reviewers
    .filter((r) => r.budget !== "account")
    .map((r) => short(r.login));
  const fleetRequired = fleet.reviewers.filter((r) => r.required).map((r) => short(r.login));
  const fleetRunsKey = [...fleetRuns].sort().join("\0");
  const fleetRequiredKey = [...fleetRequired].sort().join("\0");

  useEffect(() => {
    setRuns(fleetRunsKey === "" ? [] : fleetRunsKey.split("\0"));
    setRequired(fleetRequiredKey === "" ? [] : fleetRequiredKey.split("\0"));
    setMinInterval(fleet.min_interval);
    setWeekly(String(fleet.weekly_limit));
    setAutofix(fleet.autofix_default);
    setImpact(null);
  }, [
    fleetRunsKey,
    fleetRequiredKey,
    fleet.min_interval,
    fleet.weekly_limit,
    fleet.autofix_default,
  ]);

  const dirty =
    !sameMembers(runs, fleetRuns) ||
    !sameMembers(required, fleetRequired) ||
    minInterval !== fleet.min_interval ||
    weekly !== String(fleet.weekly_limit) ||
    autofix !== fleet.autofix_default;

  const change = () =>
    fleetChange(fleet, fleetRuns, fleetRequired, {
      runs,
      required,
      minInterval,
      weekly,
      autofix,
    });

  // Ask first. The answer is the confirmation's whole body — there is no
  // generic "are you sure", because the useful question is always "what does
  // this actually do to the 7 repositories following the fleet".
  const preview = () => runOperation(fleetImpact(change()), { onSuccess: setImpact });

  const save = (clear = false) =>
    runOperation(act("fleet", { fleet: clear ? { clear: true } : change() }), {
      onSuccess: ({ snapshot }) => {
        onSnapshot?.(snapshot);
        setImpact(null);
      },
    });

  const source = (key: string) => fleet.sources?.[key] ?? "env";

  return (
    <Card
      title="Fleet defaults"
      end={
        fleet.recorded
          ? `recorded${fleet.by ? ` by ${fleet.by}` : ""}`
          : "not recorded — this host's env"
      }
    >
      <div className="px-[18px] pb-4 pt-1">
        <p className="text-[12.5px] text-faint">
          Three layers, least specific first: this host's env, then this record, then a repository's
          own override. A setting still reading <b>env</b> below is this host's file alone —
          changing it here records it for every host.
        </p>

        {(fleet.overriding?.length ?? 0) > 0 && (
          <p className="mt-2 text-[12.5px] text-faint">
            {fleet.overriding?.length} repositor
            {fleet.overriding?.length === 1 ? "y has" : "ies have"} their own reviewers and are not
            reached by the default below:{" "}
            {fleet.overriding?.map((r, i) => (
              <span key={r}>
                {i > 0 && ", "}
                <Link to="/repos" className="text-acc hover:underline">
                  {r.split("/").pop()}
                </Link>
              </span>
            ))}
            .
          </p>
        )}

        {fleet.lagging_hosts && fleet.lagging_hosts.length > 0 && (
          <div className="mt-2.5 rounded-lg border border-warn-edge bg-warn-bg px-3 py-2 text-[12.5px] text-warn">
            These hosts run a binary that predates fleet defaults and will keep deciding from their
            own env: {fleet.lagging_hosts.join(", ")}
          </div>
        )}

        <table className="mt-2.5 w-full border-collapse">
          <tbody>
            <Row label="Reviewers" source={source("reviewers")}>
              <div className="flex flex-wrap items-center gap-3">
                {bots.map((b) => {
                  const reviewer = short(b.login);
                  return (
                    <div
                      key={b.login}
                      className="flex items-center gap-2.5 rounded-lg border border-edge px-2.5 py-1.5"
                    >
                      <BotIcon login={b.login} name={b.name} size={18} />
                      <span className="text-[12.5px] font-[550]">{b.name}</span>
                      <span className="ml-1 flex items-center gap-1.5">
                        <Toggle
                          on={b.primary || runs.includes(reviewer)}
                          label={`${b.name} runs by default`}
                          locked={b.primary}
                          title={
                            b.primary
                              ? "the primary runs everywhere by default — turn it off for one project on that project's page"
                              : "runs on every repository that has not overridden its reviewers"
                          }
                          onClick={() =>
                            setRuns((current) => {
                              const on = current.includes(reviewer);
                              if (on)
                                setRequired((items) => items.filter((name) => name !== reviewer));
                              return on
                                ? current.filter((name) => name !== reviewer)
                                : [...current, reviewer];
                            })
                          }
                        />
                        <span className="text-[12px] text-faint">runs</span>
                      </span>
                      <span className="flex items-center gap-1.5">
                        <Toggle
                          on={required.includes(reviewer)}
                          label={`${b.name} required by default`}
                          title="convergence waits for it"
                          onClick={() =>
                            setRequired((current) => {
                              const on = current.includes(reviewer);
                              if (!on && !b.primary)
                                setRuns((items) =>
                                  items.includes(reviewer) ? items : [...items, reviewer],
                                );
                              return on
                                ? current.filter((name) => name !== reviewer)
                                : [...current, reviewer];
                            })
                          }
                        />
                        <span className="text-[12px] text-faint">required</span>
                      </span>
                    </div>
                  );
                })}
              </div>
            </Row>

            <Row label="Pacing" source={source("min_interval")}>
              <input
                aria-label="Minimum interval between review fires"
                value={minInterval}
                onChange={(e) => setMinInterval(e.target.value)}
                className="w-28 rounded-lg border border-edge bg-[#FBFBFC] px-2 py-1 font-mono text-[12.5px]"
              />
              <span className="ml-2 text-[12px] text-faint">
                minimum gap between metered fires, fleet-wide (e.g. 90s, 2m)
              </span>
            </Row>

            <Row label="Weekly limit" source={source("weekly_limit")}>
              <input
                aria-label="Weekly CodeRabbit review limit"
                value={weekly}
                inputMode="numeric"
                onChange={(e) => setWeekly(e.target.value.replace(/[^0-9]/g, ""))}
                className="w-20 rounded-lg border border-edge bg-[#FBFBFC] px-2 py-1 font-mono text-[12.5px]"
              />
              <span className="ml-2 text-[12px] text-faint">
                the vendor's fair-use threshold — 60 on Pro, 90 on Pro+; 0 counts without
                forecasting
              </span>
            </Row>

            <Row label="Autofix default" source={source("autofix_default")}>
              <button
                type="button"
                onClick={() => setAutofix((v) => !v)}
                className={`rounded-lg border px-3 py-1 text-[12.5px] font-semibold ${
                  autofix ? "border-ok-edge bg-ok-bg text-ok" : "border-edge text-mut"
                }`}
              >
                {autofix ? "On" : "Off"}
              </button>
              <span className="ml-2 text-[12px] text-faint">
                whether a repository with no explicit switch may be fixed
              </span>
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
            onClick={() => void preview()}
            className="rounded-lg bg-ink px-4 py-1.5 text-[13px] font-semibold text-white disabled:opacity-45"
          >
            {busy ? "Working…" : "Review changes"}
          </button>
          <button
            type="button"
            disabled={!dirty || busy}
            onClick={() => {
              setRuns(fleetRuns);
              setRequired(fleetRequired);
              setMinInterval(fleet.min_interval);
              setWeekly(String(fleet.weekly_limit));
              setAutofix(fleet.autofix_default);
            }}
            className="rounded-lg border border-edge px-4 py-1.5 text-[13px] font-semibold text-mut disabled:opacity-45"
          >
            Discard
          </button>
          {dirty && <span className="text-[12.5px] text-warn">unsaved changes</span>}
          {fleet.recorded && !dirty && (
            <button
              type="button"
              disabled={busy}
              onClick={() => void save(true)}
              className="ml-auto text-[12.5px] text-acc hover:underline disabled:opacity-45"
            >
              Drop the record — follow this host's env again
            </button>
          )}
        </div>
      </div>

      {impact && (
        <Confirm
          title="Save fleet defaults?"
          danger={impact.reopened > 0}
          confirmLabel={impact.reopened > 0 ? `Save and reopen ${impact.reopened}` : "Save"}
          busy={busy}
          error={error}
          body={
            <>
              <ul className="mb-2 list-disc pl-4">
                {impact.changes.map((c) => (
                  <li key={c}>{c}</li>
                ))}
              </ul>
              {impact.summary}
              {impact.reopened > 0 && (
                <p className="mt-2 text-warn">
                  Reopened rounds are reviewed again, and the metered ones spend the shared
                  allowance.
                </p>
              )}
            </>
          }
          onConfirm={() => void save()}
          onCancel={() => setImpact(null)}
        />
      )}
    </Card>
  );
}

export function fleetChange(
  fleet: FleetSettings,
  fleetRuns: string[],
  fleetRequired: string[],
  edited: {
    runs: string[];
    required: string[];
    minInterval: string;
    weekly: string;
    autofix: boolean;
  },
) {
  const change: {
    cobots?: string[];
    required?: string[];
    min_interval?: string;
    weekly_limit?: number;
    autofix_default?: boolean;
  } = {};
  if (!sameMembers(edited.runs, fleetRuns)) change.cobots = edited.runs;
  if (!sameMembers(edited.required, fleetRequired)) change.required = edited.required;
  if (edited.minInterval !== fleet.min_interval) change.min_interval = edited.minInterval;
  if (edited.weekly !== String(fleet.weekly_limit)) change.weekly_limit = Number(edited.weekly);
  if (edited.autofix !== fleet.autofix_default) change.autofix_default = edited.autofix;
  return change;
}

function Row({ label, source, children }: { label: string; source: string; children: ReactNode }) {
  return (
    <tr className="border-b border-[#EEF0F3] last:border-none">
      <td className="w-40 py-2.5 pr-3 align-top">
        <div className="text-[13px] font-[550]">{label}</div>
        <Pill tone={source === "fleet" ? "ok" : "mut"}>{source}</Pill>
      </td>
      <td className="py-2.5 align-middle">{children}</td>
    </tr>
  );
}

/** The short config name, which is what every control here is keyed by. */
function short(login: string) {
  return login.replace(/\[bot\]$/, "").toLowerCase();
}
