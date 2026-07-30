import { DashboardLoading } from "../DashboardProvider";
import { useDashboard } from "../DashboardState";
import { FirstRun, isFirstRun } from "../FirstRun";
import { OverviewPage } from "../Overview";

export function OverviewRoute() {
  const { snapshot, setSnapshot } = useDashboard();
  if (!snapshot) return <DashboardLoading />;
  if (isFirstRun(snapshot)) return <FirstRun snap={snapshot} />;
  return (
    <OverviewPage
      ov={snapshot.overview}
      events={snapshot.events}
      repos={snapshot.repos}
      bots={snapshot.bots}
      onSnapshot={setSnapshot}
    />
  );
}
