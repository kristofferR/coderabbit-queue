import { Link, Outlet, useRouterState } from "@tanstack/react-router";
import { Activity, Bot, Boxes, Gauge, Settings, Wrench } from "lucide-react";
import { DashboardProvider } from "./DashboardProvider";
import { useDashboard } from "./DashboardState";
import { ago, useNow } from "./time";

const NAV = [
  { label: "Overview", to: "/", icon: Gauge },
  { label: "Repos", to: "/repos", icon: Boxes },
  { label: "Bots", to: "/bots", icon: Bot },
  { label: "Setup", to: "/setup", icon: Wrench },
  { label: "Settings", to: "/settings", icon: Settings },
] as const;

export function App() {
  return (
    <DashboardProvider>
      <DashboardShell />
    </DashboardProvider>
  );
}

function DashboardShell() {
  const { snapshot, live } = useDashboard();
  const now = useNow(5000);
  const pathname = useRouterState({ select: (state) => state.location.pathname });
  const activePath = pathname.startsWith("/pr/") ? "/" : pathname;

  return (
    <div className="min-h-screen">
      <header className="sticky top-0 z-40 border-b border-white/10 bg-ink text-white shadow-[0_8px_24px_rgb(14_24_36/0.16)]">
        <div className="mx-auto flex max-w-[1600px] flex-wrap items-center gap-x-5 gap-y-2 px-4 py-2.5 sm:px-6">
          <Link
            to="/"
            className="group flex items-center gap-3"
            aria-label="Code Review Queue overview"
          >
            <span className="rounded-[5px] bg-[#B9F4D2] px-1.5 py-0.5 font-mono text-[13px] font-semibold tracking-[-0.03em] text-[#10251A] shadow-[inset_0_0_0_1px_rgb(255_255_255/0.3)]">
              crq
            </span>
            <span className="flex flex-col leading-none">
              <span className="text-[14.5px] font-[650] tracking-[-0.015em]">
                Code Review Queue
              </span>
              <span className="mt-1 font-mono text-[9px] tracking-[0.14em] text-white/45 uppercase">
                fleet control
              </span>
            </span>
          </Link>

          <nav
            aria-label="Dashboard"
            className="order-3 flex w-full gap-1 overflow-x-auto sm:order-none sm:w-auto"
          >
            {NAV.map(({ icon: Icon, label, to }) => {
              const active = activePath === to;
              return (
                <Link
                  key={to}
                  to={to}
                  aria-current={active ? "page" : undefined}
                  className={`inline-flex shrink-0 items-center gap-1.5 rounded-md px-3 py-1.5 text-[13px] font-medium ${
                    active
                      ? "bg-white text-ink shadow-sm"
                      : "text-white/65 hover:bg-white/8 hover:text-white"
                  }`}
                >
                  <Icon aria-hidden className="size-3.5" strokeWidth={1.8} />
                  {label}
                </Link>
              );
            })}
          </nav>

          <span
            title={
              live.status === "live"
                ? `Live, revision ${snapshot?.overview.rev ?? "unknown"}`
                : live.status === "connecting"
                  ? "Connecting to the dashboard server"
                  : `${live.error ?? "Reconnecting"}; last state ${ago(snapshot?.overview.wrote_at, now)}`
            }
            className={`ml-auto flex items-center gap-2 rounded-full border px-2.5 py-1 font-mono text-[10.5px] ${
              live.status === "live"
                ? "border-[#68D99B]/30 bg-[#68D99B]/10 text-[#A7EDC5]"
                : "border-white/15 bg-white/5 text-white/60"
            }`}
          >
            <span
              className={`size-2 rounded-full ${
                live.status === "live"
                  ? "bg-[#68D99B] shadow-[0_0_0_3px_rgb(104_217_155/0.12)]"
                  : live.status === "connecting"
                    ? "animate-pulse bg-white/35"
                    : "bg-[#F3B66A]"
              }`}
            />
            {live.status === "live" ? (
              <>LIVE · REV {snapshot?.overview.rev ?? "—"}</>
            ) : live.status === "connecting" ? (
              "CONNECTING…"
            ) : (
              <>STALE · {ago(snapshot?.overview.wrote_at, now)}</>
            )}
          </span>
        </div>
      </header>

      {live.status === "reconnecting" && snapshot && (
        <div
          role="status"
          className="border-b border-warn-edge bg-warn-bg px-4 py-2 text-[12.5px] text-warn sm:px-6"
        >
          {snapshot.stale
            ? `The shared state ref is unavailable: ${snapshot.stale.error}.`
            : "Lost the connection to crq serve."}{" "}
          This is the last state loaded; actions are disabled until it becomes current again.
        </div>
      )}

      <div inert={live.status === "live" ? undefined : true}>
        <Outlet />
      </div>
    </div>
  );
}

export function RouteError({ error }: { error: Error }) {
  return (
    <main className="mx-auto max-w-[900px] px-6 py-16">
      <div className="rounded-xl border border-bad-edge bg-bad-bg p-5 text-bad">
        <Activity aria-hidden className="mb-3 size-5" />
        <h1 className="font-[650]">This dashboard view could not render</h1>
        <p className="mt-1 text-sm">{error.message}</p>
      </div>
    </main>
  );
}

export function NotFound() {
  return (
    <main className="mx-auto max-w-[900px] px-6 py-16 text-mut">
      <p className="font-mono text-xs tracking-wider text-faint uppercase">
        404 · unknown control surface
      </p>
      <h1 className="mt-2 text-xl font-[650] text-ink">That dashboard route does not exist.</h1>
      <Link to="/" className="mt-4 inline-block font-semibold text-acc hover:underline">
        Return to Overview
      </Link>
    </main>
  );
}
