import { DashboardLoading } from "../DashboardProvider";
import { useDashboard } from "../DashboardState";
import { BotsPage } from "../pages/BotsPage";

export function BotsRoute() {
  const { snapshot } = useDashboard();
  if (!snapshot) return <DashboardLoading />;
  return <BotsPage bots={snapshot.bots} />;
}
