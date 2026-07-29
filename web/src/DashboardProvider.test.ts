import { describe, expect, it } from "vitest";
import { newestSnapshot } from "./DashboardProvider";

describe("dashboard snapshot ordering", () => {
  it("keeps the newest revision across SSE and action responses", () => {
    const current = { overview: { rev: 12 }, value: "sse" };

    expect(newestSnapshot(current, { overview: { rev: 11 }, value: "action" })).toBe(current);
    expect(newestSnapshot(current, { overview: { rev: 13 }, value: "next" }).value).toBe("next");
  });
});
