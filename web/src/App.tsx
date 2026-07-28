import { useEffect, useState } from "react";
import type { Live, Snapshot } from "./api";
import { subscribe } from "./api";
import { OverviewPage } from "./Overview";
import { BotsPage, ReposPage, SettingsPage } from "./Pages";
import { PRDetailPage } from "./PRDetail";
import { FirstRun, isFirstRun } from "./FirstRun";
import { SetupPage } from "./Setup";
import { ago, useNow } from "./time";

const NAV = [
  { label: "Overview", href: "#/" },
  { label: "Repos", href: "#/repos" },
  { label: "Bots", href: "#/bots" },
  { label: "Setup", href: "#/setup" },
  { label: "Settings", href: "#/settings" },
];

export function App() {
  const [snap, setSnap] = useState<Snapshot | null>(null);
  const [live, setLive] = useState<Live>("connecting");
  const [route, setRoute] = useState(() => location.hash || "#/");
  // Why there is no state at all, when the server can say. A read that fails
  // before the first one ever succeeds leaves the stream healthy and empty, so
  // this is the only thing that distinguishes "still loading" from "broken".
  const [unavailable, setUnavailable] = useState<string | null>(null);
  const now = useNow(5000);

  useEffect(() => subscribe(setSnap, setLive, setUnavailable), []);
  useEffect(() => {
    const onHash = () => setRoute(location.hash || "#/");
    addEventListener("hashchange", onHash);
    return () => removeEventListener("hashchange", onHash);
  }, []);
  const pr = prRoute(route);

  return (
    <div className="min-w-[820px]">
      <header className="sticky top-0 z-10 flex flex-wrap items-center gap-4 border-b border-edge bg-card px-6 py-2.5">
        <span className="flex items-baseline gap-2.5">
          <span className="rounded-md bg-ink px-1.5 py-0.5 font-mono text-[13px] font-medium text-white">crq</span>
          <span className="text-base font-[650] tracking-tight">Code Review Queue</span>
        </span>
        <nav className="ml-2 flex gap-1">
          {NAV.map((n) => (
            <a
              key={n.href}
              href={n.href}
              aria-current={route === n.href || (n.href === "#/repos" && route.startsWith("#/repos/")) ? "page" : undefined}
              className={`rounded-lg px-3 py-1.5 text-[13.5px] font-medium ${
                route === n.href || (n.href === "#/repos" && route.startsWith("#/repos/"))
                  ? "bg-bg text-ink"
                  : "text-mut hover:bg-bg"
              }`}
            >
              {n.label}
            </a>
          ))}
        </nav>
        <span className="ml-auto flex items-center gap-2 text-xs text-mut">
          <span
            className={`size-2 rounded-full ${
              snap?.stale
                ? "bg-bad"
                : live === "live"
                  ? "bg-ok"
                  : live === "connecting"
                    ? "bg-faint"
                    : "bg-warn-fg"
            }`}
          />
          {live === "live" && snap?.stale ? (
            <>stale · rev {snap.overview.rev}</>
          ) : live === "live" ? (
            <>live · rev {snap?.overview.rev ?? "—"}</>
          ) : live === "connecting" ? (
            "connecting…"
          ) : (
            <>reconnecting… showing state from {ago(snap?.overview.wrote_at, now)}</>
          )}
        </span>
      </header>

      {snap?.stale && (
        <div className="border-b border-bad-edge bg-bad-bg px-6 py-2 text-[12.5px] text-bad">
          <span className="font-mono">crq serve</span> has not been able to read the state ref since{" "}
          {ago(snap.stale.since, now)} — this page is the last state it loaded, and an action taken
          here may already be acting on a queue that has moved. ({snap.stale.error})
        </div>
      )}

      {live === "reconnecting" && snap && (
        <div className="border-b border-warn-edge bg-warn-bg px-6 py-2 text-[12.5px] text-warn">
          Lost the connection to <span className="font-mono">crq serve</span>. This is the last state it
          sent — countdowns keep ticking against it, so treat times as approximate until it reconnects.
        </div>
      )}

      {!snap ? (
        <Loading live={live} error={unavailable} />
      ) : pr ? (
        <PRDetailPage repo={pr.repo} pr={pr.pr} rev={snap.overview.rev} />
      ) : route === "#/repos" || route === "#/repos/add" ? (
        <ReposPage
          repos={snap.repos}
          bots={snap.bots}
          held={snap.overview.held}
          startAdding={route === "#/repos/add"}
          onSnapshot={setSnap}
        />
      ) : route === "#/bots" ? (
        <BotsPage bots={snap.bots} />
      ) : route === "#/setup" ? (
        <SetupPage setup={snap.setup} bots={snap.bots} repos={snap.repos} onSnapshot={setSnap} />
      ) : route === "#/settings" ? (
        <SettingsPage settings={snap.settings} bots={snap.bots} onSnapshot={setSnap} />
      ) : isFirstRun(snap) ? (
        <FirstRun snap={snap} />
      ) : (
        <OverviewPage
          ov={snap.overview}
          events={snap.events}
          repos={snap.repos}
          bots={snap.bots}
          onSnapshot={setSnap}
        />
      )}
    </div>
  );
}

/** #/pr/owner/name/123 — the only route that carries parameters. */
function prRoute(route: string): { repo: string; pr: number } | null {
  const parts = route.replace(/^#\//, "").split("/");
  if (parts.length !== 4 || parts[0] !== "pr") return null;
  const pr = Number(parts[3]);
  if (!Number.isFinite(pr) || pr <= 0) return null;
  return { repo: `${parts[1]}/${parts[2]}`, pr };
}

function Loading({ live, error }: { live: Live; error: string | null }) {
  if (live !== "reconnecting" && error) {
    // The server is there and answering; it is the state ref it cannot read.
    // Saying "Reading the state ref…" here waits on something that is not going
    // to happen, and hides the one sentence that says what to fix.
    return (
      <main className="mx-auto max-w-[1400px] px-6 py-16">
        <div className="rounded-[10px] border border-bad-edge bg-bad-bg px-5 py-4 text-[13px] text-bad">
          <span className="font-mono">crq serve</span> cannot read the state ref, so there is nothing
          to show yet. It keeps retrying.
          <div className="mt-2 font-mono text-[12.5px]">{error}</div>
        </div>
      </main>
    );
  }
  return (
    <main className="mx-auto max-w-[1400px] px-6 py-16 text-mut">
      {live === "reconnecting"
        ? "Cannot reach the server. Retrying…"
        : "Reading the state ref…"}
    </main>
  );
}
