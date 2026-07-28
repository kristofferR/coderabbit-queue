import { useState, type ReactNode } from "react";
import type { BotCard, Check, HostInfo, HostTools, RepoRow, SetupView, Snapshot, Tool } from "./api";
import { refreshSetup } from "./actions";
import { BotIcon, Pill, RepoIcon, Td, Th } from "./ui";
import { ago, useNow } from "./time";

type Status = "ok" | "warn" | "bad" | "unknown";

const STATUS_TONE: Record<Status, "ok" | "warn" | "bad" | "mut"> = {
  ok: "ok",
  warn: "warn",
  bad: "bad",
  unknown: "mut",
};

const STATUS_WEIGHT: Record<Status, number> = { ok: 0, unknown: 1, warn: 2, bad: 3 };

function worst(...statuses: (string | undefined)[]): Status {
  return statuses.reduce<Status>((current, value) => {
    const next = (value === "ok" || value === "warn" || value === "bad" || value === "unknown")
      ? value
      : "unknown";
    return STATUS_WEIGHT[next] > STATUS_WEIGHT[current] ? next : current;
  }, "ok");
}

function check(setup: SetupView, key: string): Check | undefined {
  return setup.checks.find((item) => item.key === key);
}

function hostHealthStatus(health?: HostInfo["health"]): Status {
  return health === "healthy" ? "ok" : health === "unhealthy" ? "bad" : "unknown";
}

export function SetupPage({
  setup,
  bots,
  repos,
  onSnapshot,
}: {
  setup: SetupView;
  bots: BotCard[];
  repos: RepoRow[];
  onSnapshot?: (snapshot: Snapshot) => void;
}) {
  const now = useNow(5000);
  const [refreshing, setRefreshing] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const total = Math.max(1, setup.ready + setup.attention + setup.optional);

  const rerun = async () => {
    setRefreshing(true);
    setError(null);
    try {
      onSnapshot?.(await refreshSetup());
    } catch (cause) {
      setError((cause as Error).message);
    } finally {
      setRefreshing(false);
    }
  };

  return (
    <main className="mx-auto max-w-[1120px] px-6 pt-5 pb-16">
      <h1 className="text-xl font-[650] tracking-tight">Setup</h1>
      <p className="mt-1 max-w-[840px] text-[13.5px] text-mut">
        Everything crq needs to run, and whether this fleet actually has it. This stays useful after
        first run: come here when a host stops fixing, a credential expires, or a machine joins.
      </p>

      <section className="my-3.5 grid grid-cols-[minmax(0,1fr)_auto] items-center gap-5 rounded-[10px] border border-edge bg-card px-5 py-4 shadow-card max-[760px]:grid-cols-1">
        <div>
          <h2 className="text-[15px] font-[650]">
            {setup.attention === 0
              ? "Everything required is ready"
              : `${setup.attention} ${setup.attention === 1 ? "thing needs" : "things need"} attention`}
          </h2>
          <div
            className="mt-2.5 flex h-2 overflow-hidden rounded-full bg-[#EEF0F3]"
            aria-label={`${setup.ready} ready, ${setup.attention} need attention, ${setup.optional} optional missing`}
          >
            <span className="bg-ok" style={{ width: `${(setup.ready / total) * 100}%` }} />
            <span className="bg-warn-fg" style={{ width: `${(setup.attention / total) * 100}%` }} />
            <span className="bg-faint" style={{ width: `${(setup.optional / total) * 100}%` }} />
          </div>
          <div className="mt-2 flex flex-wrap items-center gap-2 text-[12.5px]">
            <Pill tone="ok">{setup.ready} ready</Pill>
            {setup.attention > 0 && <Pill tone="warn">{setup.attention} need attention</Pill>}
            {setup.optional > 0 && <Pill tone="mut">{setup.optional} optional missing</Pill>}
            <span className="text-faint">{setup.fleet?.length ?? 0} host(s) reporting</span>
          </div>
        </div>
        <div className="text-right max-[760px]:text-left">
          <button
            type="button"
            disabled={refreshing}
            onClick={() => void rerun()}
            className="rounded-lg bg-ink px-4 py-2 text-[13px] font-semibold text-white hover:bg-[#2E3C4E] disabled:cursor-wait disabled:opacity-55"
          >
            {refreshing ? "Running checks…" : "Re-run checks"}
          </button>
          <p className="mt-1 text-[11.5px] text-faint">re-probes this service PATH and every live state check</p>
        </div>
      </section>

      {error && (
        <div className="mb-3.5 rounded-[10px] border border-bad-edge bg-bad-bg px-4 py-2.5 text-[13px] text-bad">
          Could not re-run setup checks: {error}
        </div>
      )}

      <GitHubAccess setup={setup} />
      <ToolMatrix setup={setup} now={now} />
      <HostsAndServices setup={setup} now={now} />
      <QueueHome setup={setup} />
      <ReviewBots bots={bots} now={now} />
      <Repositories repos={repos} />
    </main>
  );
}

function SetupStep({
  id,
  title,
  subtitle,
  status,
  end,
  children,
}: {
  id?: string;
  title: string;
  subtitle: string;
  status: Status;
  end?: ReactNode;
  children: ReactNode;
}) {
  const mark = status === "ok" ? "✓" : status === "unknown" ? "?" : "!";
  const markClass =
    status === "ok"
      ? "bg-ok text-white"
      : status === "bad"
        ? "bg-bad text-white"
        : status === "warn"
          ? "bg-warn-fg text-white"
          : "bg-[#EEF0F3] text-mut";
  return (
    <section id={id} className="mb-3.5 rounded-[10px] border border-edge bg-card shadow-card">
      <header className="flex flex-wrap items-center gap-3 border-b border-[#EEF0F3] px-5 py-3">
        <span className={`flex size-6 shrink-0 items-center justify-center rounded-full text-[12.5px] font-semibold ${markClass}`}>
          {mark}
        </span>
        <h2 className="text-[15px] font-[650]">{title}</h2>
        <span className="text-[12.5px] text-faint">{subtitle}</span>
        {end && <span className="ml-auto flex flex-wrap items-center gap-2">{end}</span>}
      </header>
      {children}
    </section>
  );
}

function CheckRow({
  status,
  label,
  detail,
  end,
}: {
  status: Status;
  label: ReactNode;
  detail?: ReactNode;
  end?: ReactNode;
}) {
  const dot =
    status === "ok"
      ? "bg-ok"
      : status === "bad"
        ? "bg-bad"
        : status === "warn"
          ? "bg-warn-fg"
          : "bg-faint";
  return (
    <div className="grid grid-cols-[10px_minmax(0,1fr)_auto] items-start gap-3 border-b border-[#EEF0F3] py-2 last:border-none max-[760px]:grid-cols-[10px_minmax(0,1fr)]">
      <span className={`mt-[7px] size-2 rounded-full ${dot}`} />
      <div className="text-[13.5px]">
        <div className="font-[550]">{label}</div>
        {detail && <div className="mt-0.5 text-[12.5px] text-faint">{detail}</div>}
      </div>
      {end && <div className="text-right text-[12.5px] text-mut max-[760px]:col-start-2 max-[760px]:text-left">{end}</div>}
    </div>
  );
}

function GitHubAccess({ setup }: { setup: SetupView }) {
  const state = check(setup, "state");
  const gh = setup.tools.find((tool) => tool.name === "gh");
  const reports = setup.fleet ?? [];
  const visible = reports.filter((host) => host.tools.some((tool) => tool.name === "gh" && tool.path));
  const missing = reports.filter((host) => !host.tools.some((tool) => tool.name === "gh" && tool.path));
  const status = worst(state?.status, gh?.found ? "ok" : "bad", missing.length > 0 ? "bad" : "ok");

  return (
    <SetupStep
      title="GitHub access"
      subtitle="credentials and shared-state reachability"
      status={status}
      end={<Pill tone={STATUS_TONE[status]}>{status === "ok" ? "Ready" : "Needs attention"}</Pill>}
    >
      <div className="px-5 pb-3 pt-1">
        <CheckRow
          status={(state?.status as Status) ?? "unknown"}
          label="Shared queue state is authenticated and readable"
          detail="Loading this page proves the service can read the state ref; writes use the same credential path."
          end={<span className="font-mono">{state?.detail ?? "no state report"}</span>}
        />
        <CheckRow
          status={gh?.found ? "ok" : "bad"}
          label={`GitHub CLI is visible on ${setup.tools_host}`}
          detail="crq uses gh or an injected token to post triggers, resolve threads, and inspect pull requests."
          end={<span className="font-mono">{gh?.path ?? "missing from service PATH"}</span>}
        />
        {reports.length > 0 && (
          <CheckRow
            status={missing.length === 0 ? "ok" : "bad"}
            label={`GitHub CLI is visible to ${visible.length} of ${reports.length} reporting services`}
            detail={missing.length > 0 ? `Missing on ${missing.map((host) => host.host).join(", ")}` : "Every reporting host can reach gh from its service PATH."}
          />
        )}
      </div>
    </SetupStep>
  );
}

function ToolMatrix({ setup, now }: { setup: SetupView; now: number }) {
  const fleet: HostTools[] =
    (setup.fleet?.length ?? 0) > 0
      ? setup.fleet!
      : [
          {
            host: setup.tools_host,
            tools: setup.tools.map((tool) => ({ name: tool.name, path: tool.path })),
          },
        ];
  const known = new Map(setup.tools.map((tool) => [tool.name, tool]));
  for (const host of fleet) {
    for (const reported of host.tools) {
      if (!known.has(reported.name)) {
        known.set(reported.name, {
          name: reported.name,
          purpose: "reported by this host",
          required: false,
          found: false,
        });
      }
    }
  }
  const tools = [...known.values()];
  const missingRequired = tools.filter(
    (tool) => tool.required && fleet.some((host) => !host.tools.some((reported) => reported.name === tool.name && reported.path)),
  );
  const staleOrBehind = fleet.some((host) => host.stale || host.behind);
  const status: Status = missingRequired.length > 0 ? "bad" : staleOrBehind ? "warn" : "ok";

  return (
    <SetupStep
      id="tools"
      title="Command-line tools"
      subtitle="what each host's service can actually reach"
      status={status}
      end={
        <>
          {missingRequired.length > 0 && <Pill tone="bad">{missingRequired.length} blocking</Pill>}
          {setup.optional > 0 && <Pill tone="mut">{setup.optional} optional missing locally</Pill>}
        </>
      }
    >
      <div className="px-5 pb-4 pt-2">
        <p className="mb-2 max-w-[880px] text-[12.5px] text-mut">
          Required tools must exist on every daemon host. Recommended tools each unlock a feature.
          These reports come from the service PATH, not an interactive shell, which is the distinction
          behind most “installed but not found” failures.
        </p>
        <div className="overflow-x-auto">
          <table className="w-full min-w-[720px] border-collapse">
            <thead>
              <tr>
                <Th>Tool</Th>
                {fleet.map((host) => (
                  <Th key={host.host} className="min-w-[150px] text-center">
                    <span className="font-mono normal-case tracking-normal text-ink">{host.host}</span>
                    {host.stale && <span className="ml-1 text-warn">stale</span>}
                  </Th>
                ))}
              </tr>
            </thead>
            <tbody>
              <ToolGroup label="Required" note="crq cannot run its daemon work without these" tools={tools.filter((tool) => tool.required)} fleet={fleet} />
              <ToolGroup label="Recommended" note="each one turns on a feature" tools={tools.filter((tool) => !tool.required)} fleet={fleet} />
            </tbody>
          </table>
        </div>
        <p className="mt-2 text-[11.5px] text-faint">
          <b>These are CLIs, not review bots.</b> Codex and CodeRabbit reviews are GitHub Apps; their
          local tools only affect autofix or preflight. Last host report:{" "}
          {fleet.map((host) => `${host.host} ${host.at ? ago(host.at, now) : "unknown"}`).join(" · ")}.
        </p>
      </div>
    </SetupStep>
  );
}

function ToolGroup({
  label,
  note,
  tools,
  fleet,
}: {
  label: string;
  note: string;
  tools: Tool[];
  fleet: HostTools[];
}) {
  if (tools.length === 0) return null;
  return (
    <>
      <tr className="bg-[#FAFBFC]">
        <td colSpan={fleet.length + 1} className="px-0 py-1.5 text-[11px] font-semibold tracking-[0.07em] text-mut uppercase">
          {label} <span className="ml-2 font-normal tracking-normal text-faint normal-case">— {note}</span>
        </td>
      </tr>
      {tools.map((tool) => (
        <tr key={tool.name} className="border-b border-[#EEF0F3] align-top last:border-none">
          <Td>
            <div className="font-mono text-[13px] font-semibold">{tool.name}</div>
            <div className="mt-0.5 max-w-[420px] text-[12.5px] text-mut">{tool.purpose}</div>
            {(tool.fix?.length ?? 0) > 0 && (
              <details className="mt-1.5">
                <summary className="cursor-pointer text-[12px] font-[550] text-acc">Install or repair</summary>
                <div className="mt-1.5 grid gap-1.5">
                  {tool.fix!.map((command) => (
                    <CopyCommand key={command} command={command} />
                  ))}
                </div>
              </details>
            )}
          </Td>
          {fleet.map((host) => {
            const found = host.tools.find((reported) => reported.name === tool.name);
            const missing = !found?.path;
            return (
              <Td key={host.host} className="text-center">
                {missing ? (
                  <Pill tone={tool.required ? "bad" : "mut"}>{tool.required ? "missing" : "not installed"}</Pill>
                ) : (
                  <>
                    <Pill tone={host.behind && tool.name === "crq" ? "warn" : "ok"}>
                      {shortVersion(found.version) || "found"}
                      {host.behind && tool.name === "crq" ? " · behind" : ""}
                    </Pill>
                    <div className="mt-1 max-w-[180px] truncate font-mono text-[10.5px] text-faint" title={found.path}>
                      {found.path}
                    </div>
                  </>
                )}
              </Td>
            );
          })}
        </tr>
      ))}
    </>
  );
}

function shortVersion(version?: string): string {
  if (!version) return "";
  return version
    .replace(/^crq\s+/i, "")
    .replace(/^git version\s+/i, "")
    .replace(/^gh version\s+/i, "")
    .replace(/^codex-cli\s+/i, "")
    .split(/\s|\(/)[0] ?? version;
}

function CopyCommand({ command }: { command: string }) {
  const [copied, setCopied] = useState(false);
  const copy = async () => {
    try {
      await navigator.clipboard.writeText(command);
      setCopied(true);
      setTimeout(() => setCopied(false), 1400);
    } catch {
      setCopied(false);
    }
  };
  return (
    <div className="flex items-center gap-2 rounded-lg border border-edge bg-[#F5F6F8] px-2.5 py-1.5">
      <code className="min-w-0 flex-1 overflow-x-auto text-[11.5px] whitespace-nowrap">{command}</code>
      <button
        type="button"
        onClick={() => void copy()}
        className="shrink-0 rounded-md border border-acc-edge bg-acc-bg px-2 py-0.5 text-[11px] font-semibold text-acc"
      >
        {copied ? "copied" : "copy"}
      </button>
    </div>
  );
}

function HostsAndServices({ setup, now }: { setup: SetupView; now: number }) {
  const leader = check(setup, "leader");
  const autofix = check(setup, "autofix");
  const status = worst(
    leader?.status,
    autofix?.status,
    ...setup.hosts.map((host) => hostHealthStatus(host.health)),
  );
  const reports = new Map((setup.fleet ?? []).map((host) => [host.host, host]));
  const hosts = setup.hosts.length > 0
    ? setup.hosts
    : (setup.fleet ?? []).map<HostInfo>((host) => ({ name: host.host, roles: host.roles, last_seen: host.at }));

  return (
    <SetupStep
      id="hosts"
      title="Hosts & services"
      subtitle="who drives reviews and who may fix"
      status={status}
      end={<Pill tone={STATUS_TONE[status]}>{status === "ok" ? `${hosts.length} reporting` : "Needs attention"}</Pill>}
    >
      <div className="px-5 pb-4 pt-2">
        <p className="mb-2 text-[12.5px] text-mut">
          One host holds the leader lease and drives reviews for the fleet; any host may run autofix.
          “No recent signal” stays unknown rather than being painted healthy.
        </p>
        <div className="grid grid-cols-3 gap-3 max-[940px]:grid-cols-1">
          {hosts.map((host) => (
            <HostCard key={host.name} host={host} report={reports.get(host.name)} now={now} />
          ))}
        </div>
        <div className="mt-2">
          {leader && <CheckRow status={leader.status as Status} label={leader.label} detail={leader.detail} />}
          {autofix && <CheckRow status={autofix.status as Status} label={autofix.label} detail={autofix.detail} end={<a href="#/repos" className="text-acc hover:underline">Per-repo autofix →</a>} />}
        </div>
      </div>
    </SetupStep>
  );
}

function HostCard({ host, report, now }: { host: HostInfo; report?: HostTools; now: number }) {
  const roles = new Set([...(host.roles ?? []), ...(report?.roles ?? [])]);
  const health = host.health ?? (report?.stale ? "unknown" : "healthy");
  const reviewRole = roles.has("leader") ? "leader" : roles.has("autoreview") ? "standby" : "not reporting";
  const autofixRole = roles.has("autofix")
    ? host.health === "unhealthy"
      ? `${host.failures ?? 0} failures`
      : report?.agent
        ? `running · ${report.agent}`
        : "reporting"
    : "not installed";
  return (
    <article className={`rounded-lg border px-3.5 py-3 text-[12.5px] ${health === "unhealthy" ? "border-bad-edge bg-bad-bg" : "border-edge"}`}>
      <div className="flex items-center gap-2">
        <span className="font-mono text-[13.5px] font-[650]">{host.name}</span>
        <Pill tone={health === "healthy" ? "ok" : health === "unhealthy" ? "bad" : "mut"}>
          {health === "unknown" ? "no recent signal" : health}
        </Pill>
      </div>
      <HostFact label="Review daemon" value={reviewRole} />
      <HostFact label="Autofix service" value={autofixRole} />
      <HostFact label="crq" value={`${report?.version ?? "unknown"}${report?.behind ? " · behind" : ""}`} />
      <HostFact label="Last signal" value={host.last_seen ? ago(host.last_seen, now) : report?.at ? ago(report.at, now) : "unknown"} />
      {host.last_error && (
        <div className="mt-2 break-words font-mono text-[11px] text-bad">
          {host.last_error} <a href="#tools" className="font-sans font-semibold text-acc">tools ↑</a>
        </div>
      )}
    </article>
  );
}

function HostFact({ label, value }: { label: string; value: string }) {
  return (
    <div className="mt-1.5 flex justify-between gap-3 text-mut">
      <span>{label}</span>
      <b className="text-right font-medium text-ink">{value}</b>
    </div>
  );
}

function QueueHome({ setup }: { setup: SetupView }) {
  const checks = ["state", "dashboard", "calibration"]
    .map((key) => check(setup, key))
    .filter((item): item is Check => item !== undefined);
  const status = worst(...checks.map((item) => item.status));
  return (
    <SetupStep
      title="Queue home"
      subtitle="where shared state, calibration, and the legacy dashboard live"
      status={status}
      end={<Pill tone={STATUS_TONE[status]}>{status === "ok" ? "Ready" : "Needs attention"}</Pill>}
    >
      <div className="px-5 pb-3 pt-1">
        <p className="py-2 text-[12.5px] text-mut">
          One private repository holds the state every host reads and writes. <code>crq init</code> creates it.
        </p>
        {checks.map((item) => (
          <CheckRow key={item.key} status={item.status as Status} label={item.label} end={<span className="font-mono">{item.detail}</span>} />
        ))}
      </div>
    </SetupStep>
  );
}

function ReviewBots({ bots, now }: { bots: BotCard[]; now: number }) {
  const enabled = bots.filter((bot) => bot.enabled);
  const status: Status =
    enabled.length === 0
      ? "bad"
      : enabled.some((bot) => bot.status === "silent")
        ? "bad"
        : enabled.some((bot) => bot.status === "quiet" || bot.status === "unverified")
          ? "warn"
          : "ok";
  return (
    <SetupStep
      title="Review bots"
      subtitle="the GitHub Apps that do the reviewing"
      status={status}
      end={
        <>
          <Pill tone={STATUS_TONE[status]}>{enabled.length} active</Pill>
          <a href="#/bots" className="rounded-lg border border-edge px-3 py-1 text-[12px] font-semibold text-mut hover:bg-bg">
            Compare & set up
          </a>
        </>
      }
    >
      <div className="px-5 pb-4 pt-2">
        <p className="mb-2 text-[12.5px] text-mut">
          These run on GitHub, not on your machines. Status comes from answers crq has actually seen,
          so a new bot remains unverified until its first review.
        </p>
        <div className="grid grid-cols-4 gap-2.5 max-[940px]:grid-cols-2">
          {bots.map((bot) => {
            const tone = bot.status === "working" ? "ok" : bot.status === "silent" ? "bad" : bot.enabled ? "warn" : "mut";
            return (
              <article key={bot.login} className="flex gap-2.5 rounded-lg border border-edge px-3 py-2.5">
                <BotIcon login={bot.login} name={bot.name} size={22} />
                <div className="min-w-0">
                  <div className="font-[600]">{bot.name}</div>
                  <div className="mt-0.5 text-[11.5px] text-faint">
                    {bot.primary ? "primary" : "co-reviewer"}
                    {bot.required ? " · required" : ""}
                    {bot.last_seen ? ` · ${ago(bot.last_seen, now)}` : ""}
                  </div>
                  <span className="mt-1.5 inline-block">
                    <Pill tone={tone}>{bot.enabled ? bot.status : "off"}</Pill>
                  </span>
                </div>
              </article>
            );
          })}
        </div>
      </div>
    </SetupStep>
  );
}

function Repositories({ repos }: { repos: RepoRow[] }) {
  const reviewed = repos.filter((repo) => repo.reviewed);
  const env = reviewed.filter((repo) => repo.enrollment === "env");
  const status: Status = reviewed.length === 0 ? "bad" : env.length > 0 ? "warn" : "ok";
  return (
    <SetupStep
      title="Repositories"
      subtitle="what the fleet watches"
      status={status}
      end={
        <>
          <Pill tone={STATUS_TONE[status]}>{reviewed.length} enrolled</Pill>
          <a href="#/repos/add" className="rounded-lg bg-ink px-3 py-1 text-[12px] font-semibold text-white">
            + Add repository
          </a>
        </>
      }
    >
      <div className="px-5 pb-3 pt-1">
        <CheckRow
          status={reviewed.length > 0 ? "ok" : "bad"}
          label={reviewed.length > 0 ? `${reviewed.length} repositories enrolled` : "No repositories enrolled"}
          detail={
            reviewed.length > 0 ? (
              <span className="flex flex-wrap gap-x-3 gap-y-1 pt-0.5">
                {reviewed.map((repo) => (
                  <span key={repo.repo} className="inline-flex items-center gap-1.5 text-mut">
                    <RepoIcon repo={repo.repo} /> {repo.repo.split("/").pop()}
                  </span>
                ))}
              </span>
            ) : (
              "Add a repository to begin fleet review."
            )
          }
          end={<a href="#/repos" className="text-acc hover:underline">Manage →</a>}
        />
        {env.length > 0 && (
          <CheckRow
            status="warn"
            label={`${env.length} ${env.length === 1 ? "repository is" : "repositories are"} still enrolled by a host env file`}
            detail={`${env.map((repo) => repo.repo).join(", ")} may not be visible to every host until recorded in shared state.`}
            end={<a href="#/repos" className="text-acc hover:underline">Review →</a>}
          />
        )}
      </div>
    </SetupStep>
  );
}
