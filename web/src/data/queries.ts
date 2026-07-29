import { act } from "../actions";
import { requestJson } from "../client";
import { DiscoverResponseSchema, EnrollImpactSchema, PRViewSchema } from "./contracts";

export const discover = (refresh = false) =>
  requestJson(DiscoverResponseSchema, `/api/discover${refresh ? "?refresh=1" : ""}`);

export const enrollmentImpact = (repo: string) =>
  requestJson(EnrollImpactSchema, `/api/enroll-preview?repo=${encodeURIComponent(repo)}`);

export const pullRequest = (repo: string, pr: number, refresh = false) =>
  requestJson(PRViewSchema, `/api/pr/${repo}/${pr}${refresh ? "?refresh=1" : ""}`);

export const fleetImpact = (fleet: {
  cobots?: string[];
  required?: string[];
  min_interval?: string;
  weekly_limit?: number;
  autofix_default?: boolean;
}) => act("fleet", { fleet, preview: true });
