import { useState } from "react";
import { act } from "./actions";
import type { EnvSetting, FleetImpact, Snapshot } from "./api";
import { Confirm } from "./Confirm";
import { Card, Pill } from "./ui";
import { useOperation } from "./useOperation";

const GROUPS: Record<string, { title: string; note: string }> = {
  pacing: { title: "Pacing and limits", note: "how fast crq spends the shared allowance" },
  review: { title: "Review", note: "who reviews, how they are asked, and what is waited for" },
  autofix: { title: "Autofix", note: "how fix sessions are found and bounded" },
  reporting: { title: "Reporting", note: "how crq presents itself" },
  identity: {
    title: "Identity and per-host",
    note: "not editable here — these say where the queue lives, or belong to one machine",
  },
};

const SOURCE: Record<string, { tone: "ok" | "acc" | "mut"; note: string }> = {
  fleet: { tone: "ok", note: "recorded for the whole fleet — every host reads this" },
  env: { tone: "acc", note: "this machine's env file; other hosts may have something else" },
  default: { tone: "mut", note: "nothing sets it, so crq's built-in default applies" },
};

/**
 * Every setting, with the layer that decided it.
 *
 * The point of showing a SOURCE per row is that "env" is not a neutral fact: it
 * means the value lives in a file on one machine, so the other hosts may be
 * running something else and you cannot tell from here. Changing a setting
 * records it for the fleet, which is what turns that row green.
 *
 * Identity and per-host settings are shown but not editable, with the reason on
 * the row. A disabled control that does not say why is worse than no control:
 * the honest answer is "this one cannot be a fleet setting", and that is
 * information.
 */
export function EnvEditor({
  env,
  onSnapshot,
}: {
  env: EnvSetting[];
  onSnapshot?: (s: Snapshot) => void;
}) {
  const [editing, setEditing] = useState<string | null>(null);
  const [draft, setDraft] = useState("");
  const [pending, setPending] = useState<{
    setting: EnvSetting;
    value: string;
    clear: boolean;
    impact: FleetImpact;
  } | null>(null);
  const { run: runOperation, running: busy, error } = useOperation();

  const groups = [...new Set(env.map((e) => e.group))];

  const commit = (setting: EnvSetting, value: string, clear: boolean, expectedRev?: number) =>
    runOperation(act("env", { key: setting.key, value, clear, expected_rev: expectedRev }), {
      onSuccess: ({ snapshot }) => {
        onSnapshot?.(snapshot);
        setEditing(null);
        setPending(null);
      },
    });

  const save = (setting: EnvSetting, value: string, clear = false) => {
    if (!setting.review_impact) {
      commit(setting, value, clear);
      return;
    }
    runOperation(act("env", { key: setting.key, value, clear, preview: true }), {
      onSuccess: (impact) => setPending({ setting, value, clear, impact }),
    });
  };

  return (
    <>
      {error && (
        <div className="mt-3 rounded-lg border border-bad-edge bg-bad-bg px-3 py-2 text-[12.5px] text-bad">
          {error}
        </div>
      )}
      {groups.map((g) => {
        const meta = GROUPS[g] ?? { title: g, note: "" };
        const rows = env.filter((e) => e.group === g);
        return (
          <Card key={g} title={meta.title} end={meta.note}>
            <table className="w-full border-collapse">
              <tbody>
                {rows.map((e) => {
                  const src = SOURCE[e.source] ?? SOURCE.default;
                  const locked = e.per_host || e.identity;
                  const open = editing === e.key;
                  return (
                    <tr
                      key={e.key}
                      className="border-b border-[#EEF0F3] align-top last:border-none"
                    >
                      <td className="w-[230px] py-2 pr-3 pl-[18px]">
                        <div className="text-[13px] font-[550]">{e.label}</div>
                        <div className="font-mono text-[11px] text-faint">{e.key}</div>
                      </td>
                      <td className="py-2 pr-3">
                        {open ? (
                          <div className="flex flex-wrap items-center gap-2">
                            {e.kind === "bool" ? (
                              <select
                                aria-label={`${e.key} value`}
                                value={draft}
                                onChange={(ev) => setDraft(ev.target.value)}
                                className="rounded-lg border border-edge bg-[#FBFBFC] px-2 py-1 text-[12.5px]"
                              >
                                <option value="1">on</option>
                                <option value="0">off</option>
                              </select>
                            ) : (
                              <input
                                aria-label={`${e.key} value`}
                                value={draft}
                                onChange={(ev) => setDraft(ev.target.value)}
                                className="w-64 rounded-lg border border-edge bg-[#FBFBFC] px-2 py-1 font-mono text-[12.5px]"
                              />
                            )}
                            <button
                              type="button"
                              disabled={busy}
                              onClick={() => save(e, draft)}
                              className="rounded-lg bg-ink px-3 py-1 text-[12.5px] font-semibold text-white disabled:opacity-45"
                            >
                              Save for the fleet
                            </button>
                            <button
                              type="button"
                              disabled={busy}
                              onClick={() => setEditing(null)}
                              className="rounded-lg border border-edge px-3 py-1 text-[12.5px] font-semibold text-mut"
                            >
                              Cancel
                            </button>
                          </div>
                        ) : (
                          <>
                            <span className="font-mono text-[12.5px]">
                              {e.value || <span className="text-faint">—</span>}
                            </span>
                            {e.host_value && e.host_value !== e.value && (
                              <span className="ml-2 text-[11.5px] text-faint">
                                (this host's env says{" "}
                                <span className="font-mono">{e.host_value}</span>, overridden)
                              </span>
                            )}
                            <div className="text-[11.5px] text-faint">{e.help}</div>
                          </>
                        )}
                      </td>
                      <td className="w-[150px] py-2 pr-[18px] text-right">
                        <span title={locked ? undefined : src.note}>
                          <Pill tone={locked ? "mut" : src.tone}>
                            {e.identity ? "identity" : e.per_host ? "this host" : e.source}
                          </Pill>
                        </span>
                        {!locked && !open && (
                          <div className="mt-1 flex justify-end gap-2 text-[12px]">
                            <button
                              type="button"
                              onClick={() => {
                                setEditing(e.key);
                                setPending(null);
                                setDraft(
                                  e.kind === "bool"
                                    ? ["1", "true", "yes", "on"].includes(e.value.toLowerCase())
                                      ? "1"
                                      : "0"
                                    : e.value,
                                );
                              }}
                              className="text-acc hover:underline"
                            >
                              Edit
                            </button>
                            {e.source === "fleet" && (
                              <button
                                type="button"
                                disabled={busy}
                                onClick={() => save(e, "", true)}
                                className="text-mut hover:underline disabled:opacity-45"
                              >
                                Unset
                              </button>
                            )}
                          </div>
                        )}
                      </td>
                    </tr>
                  );
                })}
              </tbody>
            </table>
          </Card>
        );
      })}
      {pending && (
        <Confirm
          title={
            pending.clear ? `Unset ${pending.setting.label}?` : `Save ${pending.setting.label}?`
          }
          danger={pending.impact.reopened > 0}
          confirmLabel={
            pending.impact.reopened > 0
              ? `${pending.clear ? "Unset" : "Save"} and reopen ${pending.impact.reopened}`
              : pending.clear
                ? "Unset"
                : "Save"
          }
          busy={busy}
          error={error}
          body={
            <>
              <ul className="mb-2 list-disc pl-4">
                {pending.impact.changes.map((change) => (
                  <li key={change}>{change}</li>
                ))}
              </ul>
              {pending.impact.summary}
              {pending.impact.reopened > 0 && (
                <p className="mt-2 text-warn">
                  Reopened rounds are reviewed again, and metered reviews spend the shared
                  allowance.
                </p>
              )}
            </>
          }
          onConfirm={() =>
            commit(pending.setting, pending.value, pending.clear, pending.impact.rev)
          }
          onCancel={() => setPending(null)}
        />
      )}
    </>
  );
}
