import { PRDetailPage } from "../PRDetail";
import { prRoute } from "../router";

export function PRRoute() {
  const { name, owner, pr } = prRoute.useParams();
  return <PRDetailPage repo={`${owner}/${name}`} pr={pr} />;
}
