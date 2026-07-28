import type { Snapshot } from "./api";

export type ActionName =
  | "hold"
  | "unhold"
  | "cancel"
  | "autofix"
  | "enroll"
  | "fleet"
  | "solver"
  | "env"
  | "reviewers"
  | "resolve"
  | "decline"
  | "dismiss";

export type ActionBody = {
  /** Empty for fleet settings, which are the answer for every repository. */
  repo?: string;
  pr?: number;
  reason?: string;
  /** Omitted entirely means "back to the fleet default" for autofix. */
  enabled?: boolean;
  /** Whole intended sets, not a delta — a delta could not express "none". */
  cobots?: string[];
  required?: string[];
  /** Whether the metered primary runs on this repo; omitted leaves it alone. */
  primary?: boolean;
  clear?: boolean;
  thread_ids?: string[];
  finding_ids?: string[];
  /** Decline without resolving, when the disagreement is worth leaving visible. */
  keep_open?: boolean;
  /** A fleet-defaults change; every field is optional and absent means "leave it". */
  fleet?: {
    cobots?: string[];
    required?: string[];
    min_interval?: string;
    weekly_limit?: number;
    autofix_default?: boolean;
    clear?: boolean;
  };
  /** Ask what a change would do without making it. */
  preview?: boolean;
  /** One setting, addressed by its environment-variable name. */
  key?: string;
  value?: string;
  /** A fix-session change; an empty repo means the fleet default. */
  solver?: {
    model?: string;
    effort?: string;
    prompt?: string;
    max_attempts?: number;
    forks?: boolean;
    skip_authors?: string[];
    clear?: boolean;
  };
};

/** A save can succeed and still be ignored by a host on an older binary. */
export type ActionResult = { snapshot: Snapshot; warning?: string };

/**
 * Runs one action and returns the refreshed snapshot. The server re-reads state
 * before answering, so a successful call already reflects the change — the UI
 * never has to poll to find out whether the click worked.
 *
 * The X-CRQ-Dashboard header is what stops another site posting here: the
 * server is unauthenticated on the tailnet, and a browser cannot set a custom
 * header cross-origin without a preflight the server never approves.
 */
export async function act(action: ActionName, body: ActionBody): Promise<ActionResult> {
  const res = await fetch(`/api/action/${action}`, {
    method: "POST",
    headers: { "Content-Type": "application/json", "X-CRQ-Dashboard": "1" },
    body: JSON.stringify(body),
  });
  const text = await res.text();
  if (!res.ok) {
    let message = `HTTP ${res.status}`;
    try {
      message = (JSON.parse(text) as { error?: string }).error ?? message;
    } catch {
      /* a non-JSON body is still worth showing verbatim */
    }
    throw new Error(message);
  }
  const parsed = JSON.parse(text) as Snapshot | ActionResult;
  return "snapshot" in parsed ? (parsed as ActionResult) : { snapshot: parsed as Snapshot };
}
