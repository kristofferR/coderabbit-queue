import { Link } from "@tanstack/react-router";
import { useEffect, useState } from "react";
import { act } from "./actions";
import type { BotCard, RepoRow, Snapshot } from "./api";
import { BotIcon, Pill, Toggle } from "./ui";
import { Dialog, DialogContent, DialogDescription, DialogTitle } from "./ui/dialog";
import { sameMembers } from "./ui/utils";
import { useOperation } from "./useOperation";

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
  const { run: runOperation, running: busy, error } = useOperation();
  const serverRuns = repo.reviewers.join();
  const serverRequired = repo.required.join();

  useEffect(() => {
    setRuns(repo.reviewers);
    setRequired(repo.required);
  }, [serverRuns, serverRequired]);

  const primary = bots.find((b) => b.primary);
  const dirty = !sameMembers(runs, repo.reviewers) || !sameMembers(required, repo.required);
  const newlyOn = runs.filter((n) => !repo.reviewers.includes(n));
  const configurableNames = new Set(bots.filter((bot) => bot.configurable).map((bot) => bot.name));
  const retiredOn = [
    ...new Set(
      runs.filter(
        (name) =>
          name !== primary?.name && !configurableNames.has(name) && !required.includes(name),
      ),
    ),
  ];

  const save = () => {
    runOperation(
      act("reviewers", {
        repo: repo.repo,
        cobots: runs.filter((name) => name !== primary?.name && configurableNames.has(name)),
        required,
        primary: primary ? runs.includes(primary.name) : undefined,
      }),
      {
        onSuccess: ({ snapshot }) => {
          onSnapshot?.(snapshot);
          onClose();
        },
      },
    );
  };

  return (
    <Dialog open onOpenChange={(open) => !open && onClose()}>
      <DialogContent variant="sheet" closeLabel={`Close reviewers for ${repo.repo}`}>
        <div className="flex items-center gap-2 border-b border-edge px-5 py-3.5">
          <DialogTitle className="min-w-0 truncate">{repo.repo}</DialogTitle>
          <Pill tone={repo.override ? "warn" : "mut"}>
            {repo.override ? "override" : "fleet default"}
          </Pill>
          <DialogDescription className="sr-only">
            Choose which review bots run and gate convergence for this repository.
          </DialogDescription>
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
                      label={`${b.name} runs`}
                      locked={!b.primary && !b.configurable}
                      title={
                        b.primary
                          ? "the metered reviewer — off here spends no quota on this repo"
                          : b.configurable
                            ? undefined
                            : "this retired reviewer is shown for history and cannot be configured"
                      }
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
                      label={`${b.name} required`}
                      locked={!b.primary && !b.configurable}
                      title={
                        !b.primary && !b.configurable
                          ? "this retired reviewer is shown for history and cannot be configured"
                          : undefined
                      }
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
              Saving would
              {newlyOn.length > 0 ? ` enable ${newlyOn.join(", ")} here` : " change who gates"}
              {retiredOn.length > 0
                ? ` and remove retired ${retiredOn.join(", ")} from the saved reviewer sets`
                : ""}
              {repo.active_rounds > 0
                ? `, and reconsider ${repo.active_rounds} round(s) already in flight at their current heads.`
                : ", taking effect on the next round."}
            </p>
          )}

          {error && (
            <div className="mt-3 rounded-lg border border-bad-edge bg-bad-bg px-3 py-2 text-[12.5px] text-bad">
              {error}
            </div>
          )}

          <Link to="/repos" className="mt-3 inline-block text-[12.5px] text-acc hover:underline">
            Everything else for this repository →
          </Link>
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
      </DialogContent>
    </Dialog>
  );
}
