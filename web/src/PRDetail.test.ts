import { describe, expect, test } from "bun:test";
import type { Finding } from "./api";
import { findingContent } from "./PRDetail";

function finding(bot: string, title: string, body: string): Finding {
  return { id: "finding", bot, severity: "minor", title, body };
}

describe("findingContent", () => {
  test("puts CodeRabbit prose first and collapses its implementation material", () => {
    const parsed = findingContent(
      finding(
        "coderabbitai",
        "Refresh the token before using cached misses.",
        `_🔒 Security & Privacy_ | _🟠 Major_ | _⚡ Quick win_

<details>
<summary>🧩 Analysis chain</summary>

\`\`\`shell
# This is not the finding title
rg LookupToken
\`\`\`
</details>

**Refresh the token before using cached misses.**

The startup token can expire.

<details>
<summary>🐛 Proposed fix</summary>

Retry with a fresh token.
</details>`,
      ),
    );

    expect(parsed.description).toBe("The startup token can expire.");
    expect(parsed.sections.map((section) => section.title)).toEqual([
      "🧩 Analysis chain",
      "🐛 Proposed fix",
    ]);
    expect(parsed.sections[0]?.body).toContain("This is not the finding title");
  });

  test("removes Codex's P badge and reaction footer without inventing sections", () => {
    const parsed = findingContent(
      finding(
        "chatgpt-codex-connector",
        "Preserve fallback ordering",
        `**<sub><sub>![P2 Badge](https://img.shields.io/badge/P2-yellow?style=flat)</sub></sub>  Preserve fallback ordering**

The fallback currently selects the wrong model.

Useful? React with 👍 / 👎.`,
      ),
    );

    expect(parsed.description).toBe("The fallback currently selects the wrong model.");
    expect(parsed.sections).toEqual([]);
  });

  test("handles Codex review-body findings without showing transport boilerplate", () => {
    const parsed = findingContent(
      finding(
        "chatgpt-codex-connector[bot]",
        "Query learning history by topic before taking",
        `### 💡 Codex Review

https://github.com/example/project/blob/abc123/query.ts#L20-L24
**<sub><sub>![P2 Badge](https://img.shields.io/badge/P2-yellow?style=flat)</sub></sub>  Query learning history by topic before taking**

Taking the newest sessions first can hide older matching sessions.

AGENTS.md reference: [AGENTS.md:L19-L23](https://github.com/example/project/blob/abc123/AGENTS.md#L19-L23)

<details><summary>ℹ️ About Codex in GitHub</summary>
This boilerplate must not become part of the finding.
</details>`,
      ),
    );

    expect(parsed.description).toBe(
      "Taking the newest sessions first can hide older matching sessions.",
    );
    expect(parsed.sections).toHaveLength(1);
    expect(parsed.sections[0]?.title).toBe("References");
    expect(parsed.sections[0]?.body).toContain("Source location");
    expect(parsed.sections[0]?.body).toContain("AGENTS.md reference");
  });
});
