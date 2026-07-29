import { Effect } from "effect";
import { requestJson } from "../client";
import {
  DiscoverResponseSchema,
  EnrollImpactSchema,
  FleetImpactResponseSchema,
  PRViewSchema,
} from "./contracts";

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
}) =>
  requestJson(FleetImpactResponseSchema, "/api/action/fleet", {
    method: "POST",
    headers: { "Content-Type": "application/json", "X-CRQ-Dashboard": "1" },
    body: JSON.stringify({ fleet, preview: true }),
  }).pipe(Effect.map(({ impact }) => impact));
