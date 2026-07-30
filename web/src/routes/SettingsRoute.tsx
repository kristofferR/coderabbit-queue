import { DashboardLoading } from "../DashboardProvider";
import { useDashboard } from "../DashboardState";
import { SettingsPage } from "../pages/SettingsPage";

export function SettingsRoute() {
  const { snapshot, setSnapshot } = useDashboard();
  if (!snapshot) return <DashboardLoading />;
  return (
    <SettingsPage settings={snapshot.settings} bots={snapshot.bots} onSnapshot={setSnapshot} />
  );
}
