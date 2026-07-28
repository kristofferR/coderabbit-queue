import { DashboardLoading } from "../DashboardProvider";
import { useDashboard } from "../DashboardState";
import { ReposPage } from "../pages/ReposPage";

export function ReposRoute() {
  const { snapshot, setSnapshot } = useDashboard();
  if (!snapshot) return <DashboardLoading />;
  return (
    <ReposPage
      repos={snapshot.repos}
      bots={snapshot.bots}
      held={snapshot.overview.held}
      onSnapshot={setSnapshot}
    />
  );
}
