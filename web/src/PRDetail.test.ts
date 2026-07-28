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
});
