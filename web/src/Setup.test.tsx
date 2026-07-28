import { describe, expect, test } from "bun:test";
import { renderToStaticMarkup } from "react-dom/server";
import type { BotCard, RepoRow, SetupView } from "./api";
import { SetupPage } from "./Setup";

const setup: SetupView = {
  checks: [
    { key: "state", label: "Queue home", status: "ok", detail: "owner/state · rev 42" },
    { key: "dashboard", label: "Markdown dashboard", status: "ok", detail: "issue #2" },
    { key: "calibration", label: "Quota calibration", status: "ok", detail: "PR #1" },
    { key: "leader", label: "Review daemon", status: "ok", detail: "leader atlas" },
    { key: "tools", label: "Required tools", status: "ok", detail: "present on atlas" },
    { key: "autofix", label: "Autofix", status: "ok", detail: "1 host reporting" },
  ],
  tools: [
    { name: "crq", purpose: "the queue binary", required: true, found: true, path: "/bin/crq" },
    {
      name: "codex",
      purpose: "optional fix agent",
      required: false,
      found: false,
      fix: ["install codex"],
    },
  ],
  hosts: [{ name: "atlas", roles: ["leader", "autofix"], health: "healthy" }],
  tools_host: "atlas",
  fleet: [
    {
      host: "atlas",
      version: "2.0.0",
      agent: "codex",
      roles: ["autoreview", "autofix"],
      tools: [{ name: "crq", path: "/bin/crq", version: "crq 2.0.0" }, { name: "codex" }],
    },
  ],
  ready: 7,
  attention: 0,
  optional: 1,
};

const bots: BotCard[] = [
  {
    login: "coderabbitai[bot]",
    name: "CodeRabbit",
    primary: true,
    metered: true,
    enabled: true,
    required: true,
    repo_count: 1,
    status: "working",
  },
];

const repos: RepoRow[] = [
  {
    repo: "owner/repo",
    enrollment: "state",
    reviewed: true,
    reviewers: ["CodeRabbit"],
    required: ["CodeRabbit"],
    override: false,
    autofix: "default",
    active_rounds: 0,
    queued_rounds: 0,
    held_prs: 0,
    fixing: 0,
  },
];

describe("SetupPage", () => {
  test("keeps the mockup's diagnostic order and working controls", () => {
    const html = renderToStaticMarkup(<SetupPage setup={setup} bots={bots} repos={repos} />);
    const headings = [
      "GitHub access",
      "Command-line tools",
      "Hosts &amp; services",
      "Queue home",
      "Review bots",
      "Repositories",
    ].map((heading) => html.indexOf(heading));

    expect(headings.every((index) => index >= 0)).toBe(true);
    expect([...headings].sort((a, b) => a - b)).toEqual(headings);
    expect(html).toContain("Re-run checks");
    expect(html).toContain("Install or repair");
    expect(html).toContain(">copy<");
    expect(html).toContain("+ Add repository");
    expect(html).toContain("atlas");
  });
});
