// Shapes mirror internal/serve/overview.go. They are a view model, not the
// state schema: the server does the reducing so the browser never has to
// re-implement Queue()'s rules about what is knowable.

export type Headline = {
  kind: "stranded" | "blocked" | "reviewing" | "awaiting" | "queued" | "held" | "idle";
  text: string;
  detail?: string;
  subject?: string;
};

export type Quota = {
  scope?: string;
  remaining?: number | null;
  blocked_until?: string;
  source?: string;
  checked_at?: string;
  last_fired?: string;
  fair_use: {
    fires: number;
    limit?: number;
    complete: boolean;
    since?: string;
    level: "ok" | "warn" | "over";
    note: string;
  };
};

export type Slot = { held: boolean; key?: string; since?: string; hold_until?: string };
export type Leader = { owner: string; host: string; expires_at: string; expired: boolean };
export type Counts = { in_flight: number; queued: number; held: number; fixing: number };

export type Attention = {
  kind: string;
  level: "bad" | "warn";
  subject?: string;
  text: string;
  detail?: string;
};

export type Bot = {
  login: string;
  name: string;
  mark: "commanded" | "claimed" | "pending";
  required?: boolean;
  at?: string;
  primary?: boolean;
};

export type RoundRow = {
  key: string;
  repo: string;
  pr: number;
  head: string;
  phase: string;
  fired_at?: string;
  deadline?: string;
  bots: Bot[];
  host?: string;
  note?: string;
  next?: string;
  fixing?: boolean;
};

export type QueueRow = {
  key: string;
  repo: string;
  pr: number;
  head: string;
  position?: number;
  ready_at?: string;
  why?: string;
  attempts?: number;
  host?: string;
  co_only?: boolean;
  next?: string;
};

export type HeldRow = {
  key: string;
  repo: string;
  pr: number;
  head?: string;
  reason?: string;
  by?: string;
  at: string;
};

export type Session = {
  key: string;
  repo: string;
  pr: number;
  head?: string;
  host?: string;
  attempt?: number;
  since: string;
  heartbeat?: string;
};

export type Host = {
  name: string;
  health: "healthy" | "unhealthy" | "unknown";
  failures?: number;
  last_error?: string;
  last_failure?: string;
  last_success?: string;
};

export type DoneRow = {
  key: string;
  repo: string;
  pr: number;
  head: string;
  outcome: string;
  note?: string;
  at?: string;
};

export type Overview = {
  now: string;
  rev: number;
  wrote_at?: string;
  headline: Headline;
  quota: Quota;
  slot: Slot;
  leader?: Leader;
  counts: Counts;
  attention: Attention[];
  in_flight: RoundRow[];
  queue: QueueRow[];
  held: HeldRow[];
  autofix: { sessions: Session[]; hosts: Host[] };
  finished: DoneRow[];
};

export type RepoRow = {
  repo: string;
  enrollment: "state" | "env" | "excluded" | "scope" | "off";
  reviewed: boolean;
  env_conflict?: boolean;
  enroll_reason?: string;
  enroll_by?: string;
  enroll_at?: string;
  env_host?: string;
  reviewers: string[];
  required: string[];
  primary_off?: boolean;
  solver?: RepoSolver;
  override: boolean;
  override_by?: string;
  override_at?: string;
  autofix: "default" | "on" | "off";
  autofix_reason?: string;
  autofix_by?: string;
  autofix_at?: string;
  active_rounds: number;
  queued_rounds: number;
  held_prs: number;
  fixing: number;
};

export type BotCard = {
  login: string;
  name: string;
  primary: boolean;
  metered: boolean;
  enabled: boolean;
  required: boolean;
  command?: string;
  trigger?: string;
  grace?: string;
  last_seen?: string;
  seen_on?: string;
  repo_count: number;
  status: "working" | "quiet" | "unverified" | "off";
  site?: string;
  docs?: string;
  pitch?: string;
  cost?: string;
  setup?: string[];
  suited_to?: string;
};

export type Check = { key: string; label: string; status: "ok" | "warn" | "bad" | "unknown"; detail?: string };
export type Tool = { name: string; purpose: string; required: boolean; found: boolean; path?: string };
export type HostInfo = {
  name: string;
  roles?: string[];
  health?: "healthy" | "unhealthy" | "unknown";
  last_seen?: string;
  failures?: number;
  last_error?: string;
  caps?: number;
};
export type SetupView = { checks: Check[]; tools: Tool[]; hosts: HostInfo[]; tools_host: string };

export type ReviewerCfg = {
  login: string;
  name: string;
  primary: boolean;
  required: boolean;
  metered: boolean;
  command?: string;
  trigger?: string;
  grace?: string;
};

export type FleetConfig = {
  gate_repo: string;
  state_ref: string;
  dashboard_issue?: number;
  calibration_pr?: number;
  scope?: string[];
  allow_repos?: string[];
  exclude_repos?: string[];
  skip_authors?: string[];
  skip_marker?: string;
  min_interval: string;
  inflight_timeout: string;
  watch_interval: string;
  reviewers: ReviewerCfg[];
  autofix_command?: string[];
  autofix_max_attempts?: number;
  autofix_concurrency?: number;
  autofix_forks?: boolean;
  workspace_root?: string;
};

export type KV = { key: string; value: string; detail?: string };
/** How one repository's fix sessions run, resolved env → fleet → repo. */
export type RepoSolver = {
  overridden: boolean;
  agent?: string;
  model?: string;
  effort?: string;
  prompt?: string;
  max_attempts: number;
  forks: boolean;
  skip_authors: string[];
  sources: Record<string, string>;
  by?: string;
  lagging_hosts?: string[];
};

export type FleetSettings = {
  recorded: boolean;
  reviewers: { login: string; budget: string; required: boolean; trigger?: string }[];
  min_interval: string;
  weekly_limit: number;
  autofix_default: boolean;
  sources: Record<string, string>;
  by?: string;
  updated_at?: string;
  lagging_hosts?: string[];
};

export type SettingsView = {
  config: FleetConfig;
  quota: Quota;
  plumbing: KV[];
  fleet?: FleetSettings;
};

export type Finding = {
  id: string;
  bot: string;
  severity: string;
  path?: string;
  line?: number;
  title: string;
  body?: string;
  thread_id?: string;
  url?: string;
  source?: string;
  commit?: string;
  created_at?: string;
};

export type Observation = {
  head: string;
  converged: boolean;
  status?: string;
  reason?: string;
  reviewed_by?: Record<string, boolean>;
  findings: Finding[];
  dismissed?: number;
  checked_at: string;
};

export type RoundView = {
  head: string;
  phase: string;
  attempts?: number;
  enqueued_at: string;
  fired_at?: string;
  deadline?: string;
  retry_at?: string;
  note?: string;
  host?: string;
  co_only?: boolean;
  bots: Bot[];
  fixing?: Session;
  dismissed?: { id: string; reason: string }[];
  next?: string;
};

export type HistoryEntry = {
  head: string;
  outcome: string;
  note?: string;
  at?: string;
  current?: boolean;
};

/** The PR page: state renders instantly, the observation fills in behind it. */
export type PRView = {
  repo: string;
  pr: number;
  round?: RoundView;
  hold?: HeldRow;
  observed?: Observation;
  observe_error?: string;
  cost?: Cost;
  cost_error?: string;
  history: HistoryEntry[];
};

/** What one more round would cost. Ranges, never a single confident figure. */
export type Cost = {
  low: number;
  high: number;
  exact?: boolean;
  unpriced?: string[];
  summary: string;
  prices_checked_at: string;
  reviewers: {
    bot: string;
    low: number;
    high: number;
    exact?: boolean;
    unknown?: boolean;
    basis: string;
  }[];
  diff: { additions: number; deletions: number; changed_files: number };
};

export type Event = {
  at: string;
  kind: string;
  level: "ok" | "warn" | "bad" | "info";
  repo?: string;
  pr?: number;
  head?: string;
  text: string;
  detail?: string;
};

/** One payload for every page, so two views can never disagree about a revision. */
export type Snapshot = {
  overview: Overview;
  repos: RepoRow[];
  bots: BotCard[];
  setup: SetupView;
  settings: SettingsView;
  events: Event[];
};

/** Connection state, so the UI can say "stale" instead of quietly lying. */
export type Live = "connecting" | "live" | "reconnecting";

/**
 * Subscribes to whole snapshots. The server pushes only when the state ref's
 * revision moves; clocks tick locally, so nothing here polls for time.
 */
export function subscribe(
  onData: (snap: Snapshot) => void,
  onLive: (live: Live) => void,
): () => void {
  let source: EventSource | null = null;
  let closed = false;
  // Backs off so a server that is down for an hour is not asked every three
  // seconds for an hour. Reset on a successful open, so the common case — a
  // restart lasting a second or two — still reconnects immediately.
  let delay = 1000;
  const maxDelay = 30000;

  const open = () => {
    if (closed) return;
    source = new EventSource("/api/events");
    source.onopen = () => {
      delay = 1000;
      onLive("live");
    };
    source.onmessage = (e) => {
      try {
        onData(JSON.parse(e.data) as Snapshot);
        onLive("live");
      } catch {
        /* a malformed frame is not worth tearing the stream down for */
      }
    };
    source.onerror = () => {
      onLive("reconnecting");
      source?.close();
      // EventSource retries on its own, but only for transport errors; an
      // explicit reopen covers the server restarting underneath us.
      setTimeout(open, delay);
      delay = Math.min(delay * 2, maxDelay);
    };
  };

  onLive("connecting");
  open();
  return () => {
    closed = true;
    source?.close();
  };
}

/** One repository the picker offers, with what crq already knows about it. */
export type Candidate = {
  repo: string;
  private: boolean;
  archived: boolean;
  fork: boolean;
  issues: number;
  pushed_at?: string;
  language?: string;
  enrollment?: {
    source: string;
    enabled: boolean;
    env_conflict?: boolean;
    reason?: string;
    by?: string;
  };
};
