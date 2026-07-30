import { createContext, use } from "react";
import type { Live, Snapshot } from "./api";

export type DashboardContextValue = {
  snapshot: Snapshot | null;
  setSnapshot: (snapshot: Snapshot) => void;
  live: Live;
};

export const DashboardContext = createContext<DashboardContextValue | null>(null);

export function useDashboard(): DashboardContextValue {
  const dashboard = use(DashboardContext);
  if (!dashboard) {
    throw new Error("useDashboard must be used inside DashboardProvider");
  }
  return dashboard;
}
