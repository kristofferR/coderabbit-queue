import { useEffect, useRef, useState } from "react";
import type { BotCard, RepoRow, Snapshot } from "./api";
import { act } from "./actions";
import { BotIcon, Pill, Toggle } from "./ui";
import { sameSet, setKey } from "./sets";

/**
 * A repository's reviewers, reachable from any row that mentions it.
 *
 * The Repos page owns the full settings; this is the same decision made where
 * you noticed you wanted to make it. Deliberately only the reviewers and the
 * autofix switch — a slide-over that grows into a second settings page is two
 * places to look for one answer, which is the mistake the Bots page already
 * made once.
 *
 * It says what saving would DO before you save, for the same reason the fleet
 * form does: enabling a bot triggers it on every open round at its current
 * head, and that is a consequence, not a preference.
 */
export function QuickSettings({
  repo,
  bots,
  onClose,
  onSnapshot,
}: {
  repo: RepoRow;
  bots: BotCard[];
  onClose: () => void;
  onSnapshot?: (s: Snapshot) => void;
}) {
  const [runs, setRuns] = useState<string[]>(repo.reviewers);
  const [required, setRequired] = useState<string[]>(repo.required);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  // The Repos page keeps this and so must the shortcut: a save that succeeded
  // can still name hosts too old to read the record, and closing on it reports a
  // fleet-wide decision that part of the fleet will go on ignoring.
  const [warning, setWarning] = useState<string | null>(null);
  const panel = useRef<HTMLDivElement>(null);
  const returnTo = useRef<Element | null>(null);
  const onCloseRef = useRef(onClose);
  onCloseRef.current = onClose;

  useEffect(() => {
    returnTo.current = document.activeElement;
    const focusable = () =>
      Array.from(
        panel.current?.querySelectorAll<HTMLElement>(
          'button:not([disabled]), input:not([disabled]), [href], select, textarea, [tabindex]:not([tabindex="-1"])',
        ) ?? [],
      );
    focusable()[0]?.focus();

    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape") {
        e.preventDefault();
        onCloseRef.current();
        return;
      }
      if (e.key !== "Tab") return;
      const items = focusable();
      if (items.length === 0) return;
      const first = items[0];
      const last = items[items.length - 1];
      const active = document.activeElement;
      if (e.shiftKey && (active === first || !panel.current?.contains(active))) {
        e.preventDefault();
        last.focus();
      } else if (!e.shiftKey && active === last) {
        e.preventDefault();
        first.focus();
      }
    };
    document.addEventListener("keydown", onKey);
    return () => {
      document.removeEventListener("keydown", onKey);
      (returnTo.current as HTMLElement | null)?.focus?.();
    };
  }, []);

  const runsRev = setKey(repo.reviewers);
  const requiredRev = setKey(repo.required);
  useEffect(() => {
    setRuns(repo.reviewers);
    setRequired(repo.required);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [repo.repo, runsRev, requiredRev]);

  const primary = bots.find((b) => b.primary);
  const dirty = !sameSet(runs, repo.reviewers) || !sameSet(required, repo.required);
  const newlyOn = runs.filter((n) => !repo.reviewers.includes(n));

  const save = async () => {
    setBusy(true);
    setError(null);
    setWarning(null);
    try {
      const res = await act("reviewers", {
        repo: repo.repo,
        cobots: runs.filter((n) => n !== primary?.name),
        required,
        primary: primary ? runs.includes(primary.name) : undefined,
      });
      onSnapshot?.(res.snapshot);
      if (res.warning) {
        setWarning(res.warning);
        return; // stay open: the warning is the only place this is said
      }
      onClose();
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setBusy(false);
    }
  };

  return (
    <div className="fixed inset-0 z-50 flex justify-end bg-[rgb(27_36_48/0.24)]">
      <div
        ref={panel}
        role="dialog"
        aria-modal="true"
        aria-label={`Reviewers for ${repo.repo}`}
        className="flex h-full w-[420px] max-w-full flex-col border-l border-edge bg-card shadow-[0_0_48px_rgb(27_36_48/0.18)]"
      >
        <div className="flex items-center gap-2 border-b border-edge px-5 py-3.5">
          <h2 className="min-w-0 truncate text-[15px] font-[650]">{repo.repo}</h2>
          <Pill tone={repo.override ? "warn" : "mut"}>
            {repo.override ? "override" : "fleet default"}
          </Pill>
          <button
            type="button"
            onClick={onClose}
            className="ml-auto rounded-lg border border-edge px-2.5 py-1 text-[12.5px] font-semibold text-mut"
          >
            Close
          </button>
        </div>

        <div className="min-h-0 flex-1 overflow-auto px-5 py-3">
          <p className="text-[12.5px] text-faint">
            <b>Runs</b> — the bot reviews this repo. <b>Required</b> — convergence waits for it.
            {repo.override_by && ` Set by ${repo.override_by}.`}
          </p>
          <table className="mt-2 w-full border-collapse">
            <tbody>
              {bots.map((b) => (
                <tr key={b.login} className="border-b border-[#EEF0F3] last:border-none">
                  <td className="py-2 pr-2">
                    <span className="flex items-center gap-2 text-[13px]">
                      <BotIcon login={b.login} name={b.name} size={18} />
                      <span className="font-[550]">{b.name}</span>
                    </span>
                  </td>
                  <td className="py-2 text-center">
                    <Toggle
                      on={runs.includes(b.name)}
                      label={`Runs ${b.name}`}
                      title={b.primary ? "the metered reviewer — off here spends no quota on this repo" : undefined}
                      onClick={() =>
                        setRuns((cur) => {
                          const on = cur.includes(b.name);
                          if (on) setRequired((r) => r.filter((n) => n !== b.name));
                          return on ? cur.filter((n) => n !== b.name) : [...cur, b.name];
                        })
                      }
                    />
                    <div className="text-[11px] text-faint">runs</div>
                  </td>
                  <td className="py-2 text-center">
                    <Toggle
                      on={required.includes(b.name)}
                      label={`Requires ${b.name}`}
                      onClick={() =>
                        setRequired((cur) => {
                          const on = cur.includes(b.name);
                          if (!on) setRuns((r) => (r.includes(b.name) ? r : [...r, b.name]));
                          return on ? cur.filter((n) => n !== b.name) : [...cur, b.name];
                        })
                      }
                    />
                    <div className="text-[11px] text-faint">required</div>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>

          {dirty && (
            <p className="mt-3 rounded-lg border border-warn-edge bg-warn-bg px-2.5 py-1.5 text-[12.5px] text-warn">
              Saving would{newlyOn.length > 0 ? ` enable ${newlyOn.join(", ")} here` : " change who gates"}
              {repo.active_rounds > 0
                ? `, and reconsider ${repo.active_rounds} round(s) already in flight at their current heads.`
                : ", taking effect on the next round."}
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

          <a href="#/repos" className="mt-3 inline-block text-[12.5px] text-acc hover:underline">
            Everything else for this repository →
          </a>
        </div>

        <div className="flex items-center gap-2.5 border-t border-edge px-5 py-3">
          <button
            type="button"
            disabled={!dirty || busy}
            onClick={() => void save()}
            className="rounded-lg bg-ink px-4 py-1.5 text-[13px] font-semibold text-white disabled:opacity-45"
          >
            {busy ? "Saving…" : "Save"}
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
          {dirty && <span className="text-[12.5px] text-warn">1 change pending</span>}
        </div>
      </div>
    </div>
  );
}
