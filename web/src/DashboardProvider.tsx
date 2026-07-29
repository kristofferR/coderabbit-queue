import { type ReactNode, useCallback, useEffect, useState } from "react";
import type { Live, Snapshot } from "./api";
import { subscribe } from "./api";
import { DashboardContext, useDashboard } from "./DashboardState";
import { ago, useNow } from "./time";

export function newestSnapshot<T extends { overview: { rev: number } }>(
  current: T | null,
  incoming: T,
): T {
  return current && incoming.overview.rev < current.overview.rev ? current : incoming;
}

export function DashboardProvider({ children }: { children: ReactNode }) {
  const [snapshot, setSnapshot] = useState<Snapshot | null>(null);
  const [live, setLive] = useState<Live>({ status: "connecting" });
  const applySnapshot = useCallback((incoming: Snapshot) => {
    setSnapshot((current) => newestSnapshot(current, incoming));
  }, []);

  useEffect(() => subscribe(applySnapshot, setLive), [applySnapshot]);

  return (
    <DashboardContext value={{ snapshot, setSnapshot: applySnapshot, live }}>
      {children}
    </DashboardContext>
  );
}

export function DashboardLoading() {
  const { live, snapshot } = useDashboard();
  const now = useNow(5000);
  return (
    <main className="mx-auto max-w-[1400px] px-4 py-16 text-mut sm:px-6">
      <div className="flex items-center gap-3 font-mono text-[12px] tracking-[0.04em] uppercase">
        <span className="size-2 animate-pulse rounded-full bg-faint" />
        {live.status === "reconnecting"
          ? `${live.error ?? "Cannot reach the server"} · last state ${ago(snapshot?.overview.wrote_at, now)}`
          : "Reading the shared state ref"}
      </div>
    </main>
  );
}
