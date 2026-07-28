import { DashboardLoading } from "../DashboardProvider";
import { useDashboard } from "../DashboardState";
import { SetupPage } from "../pages/SetupPage";

export function SetupRoute() {
  const { snapshot } = useDashboard();
  if (!snapshot) return <DashboardLoading />;
  return <SetupPage setup={snapshot.setup} bots={snapshot.bots} repos={snapshot.repos} />;
}
