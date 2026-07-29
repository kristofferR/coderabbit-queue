import { describe, expect, it } from "vitest";
import type { FleetSettings, RepoSolver, Snapshot } from "./api";
import { isFirstRun } from "./FirstRun";
import { fleetChange } from "./FleetEditor";
import { solverChange } from "./SolverEditor";

const fleet: FleetSettings = {
  recorded: false,
  reviewers: [],
  min_interval: "90s",
  weekly_limit: 60,
  autofix_default: true,
  sources: {},
};

describe("settings deltas", () => {
  it("sends only edited fleet fields", () => {
    expect(
      fleetChange(fleet, ["codex"], ["coderabbitai"], {
        runs: ["codex"],
        required: ["coderabbitai"],
        minInterval: "2m",
        weekly: "60",
        autofix: true,
      }),
    ).toEqual({ min_interval: "2m" });
  });

  it("keeps solver model fallbacks and omits inherited fields", () => {
    const solver: RepoSolver = {
      overridden: false,
      models: ["gpt-5.6-sol", "gpt-5.6-terra", "codex-auto-review"],
      model_choices: [],
      model: "gpt-5.6-sol",
      max_attempts: 3,
      forks: false,
      skip_authors: ["dependabot[bot]"],
      sources: {},
    };

    expect(
      solverChange(solver, {
        model: "gpt-5.6-terra",
        effort: "",
        prompt: "",
        attempts: "4",
        forks: false,
        authors: "dependabot[bot]",
      }),
    ).toEqual({
      models: ["gpt-5.6-terra", "codex-auto-review"],
      max_attempts: 4,
    });
  });
});

describe("first-run enrollment", () => {
  it("does not call an empty scope-wide fleet unenrolled", () => {
    const snapshot = {
      repos: [],
      settings: { config: { scope: ["owner"], allow_repos: [] } },
      overview: {
        counts: { in_flight: 0, queued: 0, held: 0, fixing: 0 },
        finished: [],
      },
    } as unknown as Snapshot;

    expect(isFirstRun(snapshot)).toBe(false);
  });
});
