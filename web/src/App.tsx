import { useEffect, useState } from "react";
import type { Live, Snapshot } from "./api";
import { subscribe } from "./api";
import { OverviewPage } from "./Overview";
import { BotsPage, ReposPage, SettingsPage, SetupPage } from "./Pages";
import { PRDetailPage } from "./PRDetail";
import { FirstRun, isFirstRun } from "./FirstRun";
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
  const now = useNow(5000);

  useEffect(() => subscribe(setSnap, setLive), []);
  useEffect(() => {
    const onHash = () => setRoute(location.hash || "#/");
    addEventListener("hashchange", onHash);
    return () => removeEventListener("hashchange", onHash);
  }, []);

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
              className={`rounded-lg px-3 py-1.5 text-[13.5px] font-medium ${
                route === n.href ? "bg-bg text-ink" : "text-mut hover:bg-bg"
              }`}
            >
              {n.label}
            </a>
          ))}
        </nav>
        <span className="ml-auto flex items-center gap-2 text-xs text-mut">
          <span
            className={`size-2 rounded-full ${
              live === "live" ? "bg-ok" : live === "connecting" ? "bg-faint" : "bg-warn-fg"
            }`}
          />
          {live === "live" ? (
            <>live · rev {snap?.overview.rev ?? "—"}</>
          ) : live === "connecting" ? (
            "connecting…"
          ) : (
            <>reconnecting… showing state from {ago(snap?.overview.wrote_at, now)}</>
          )}
        </span>
      </header>

      {live === "reconnecting" && snap && (
        <div className="border-b border-warn-edge bg-warn-bg px-6 py-2 text-[12.5px] text-warn">
          Lost the connection to <span className="font-mono">crq serve</span>. This is the last state it
          sent — countdowns keep ticking against it, so treat times as approximate until it reconnects.
        </div>
      )}

      {prRoute(route) ? (
        <PRDetailPage repo={prRoute(route)!.repo} pr={prRoute(route)!.pr} rev={snap?.overview.rev} />
      ) : !snap ? (
        <Loading live={live} />
      ) : route === "#/repos" ? (
        <ReposPage repos={snap.repos} bots={snap.bots} held={snap.overview.held} onSnapshot={setSnap} />
      ) : route === "#/bots" ? (
        <BotsPage bots={snap.bots} />
      ) : route === "#/setup" ? (
        <SetupPage setup={snap.setup} bots={snap.bots} repos={snap.repos} />
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

function Loading({ live }: { live: Live }) {
  return (
    <main className="mx-auto max-w-[1400px] px-6 py-16 text-mut">
      {live === "reconnecting"
        ? "Cannot reach the server. Retrying…"
        : "Reading the state ref…"}
    </main>
  );
}
