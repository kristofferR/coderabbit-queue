import { act } from "../actions";
import { requestJson } from "../client";
import { DiscoverResponseSchema, EnrollImpactSchema, PRViewSchema } from "./contracts";

export const discover = (refresh = false) =>
  requestJson(DiscoverResponseSchema, `/api/discover${refresh ? "?refresh=1" : ""}`);

export const enrollmentImpact = (repo: string) =>
  requestJson(EnrollImpactSchema, `/api/enroll-preview?repo=${encodeURIComponent(repo)}`);

export const pullRequest = (repo: string, pr: number, refresh = false, stateOnly = false) => {
  const params = new URLSearchParams();
  if (refresh) params.set("refresh", "1");
  if (stateOnly) params.set("state_only", "1");
  const query = params.size > 0 ? `?${params}` : "";
  return requestJson(PRViewSchema, `/api/pr/${repo}/${pr}${query}`);
};

export const fleetImpact = (fleet: {
  cobots?: string[];
  required?: string[];
  min_interval?: string;
  weekly_limit?: number;
  autofix_default?: boolean;
  clear?: boolean;
}) => act("fleet", { fleet, preview: true });
