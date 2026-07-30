import { describe, expect, it } from "vitest";
import { newestSnapshot } from "./DashboardProvider";

describe("dashboard snapshot ordering", () => {
  it("keeps the newest revision across SSE and action responses", () => {
    const current = { overview: { rev: 12 }, value: "sse" };

    expect(newestSnapshot(current, { overview: { rev: 11 }, value: "action" })).toBe(current);
    expect(newestSnapshot(current, { overview: { rev: 13 }, value: "next" }).value).toBe("next");
  });

  it("orders equal revisions by the snapshot render time", () => {
    const current = {
      overview: { rev: 12, now: "2026-07-29T21:00:02Z" },
      value: "expired claim removed",
    };

    expect(
      newestSnapshot(current, {
        overview: { rev: 12, now: "2026-07-29T21:00:01Z" },
        value: "stale action response",
      }),
    ).toBe(current);
    expect(
      newestSnapshot(current, {
        overview: { rev: 12, now: "2026-07-29T21:00:03Z" },
        value: "newer frame",
      }).value,
    ).toBe("newer frame");
  });
});
