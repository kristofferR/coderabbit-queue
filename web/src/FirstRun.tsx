import type { Snapshot } from "./api";

/**
 * What to show a fleet that has nothing in it yet.
 *
 * An empty Overview is technically correct and useless: it says the queue is
 * idle, which is indistinguishable from a queue that works and has nothing to
 * do. Someone arriving for the first time needs to know what crq is FOR, what
 * is already in place, and what the one next step is.
 *
 * It is derived, not a mode. There is no "first run" flag to get stuck on —
 * this renders whenever no repository is enrolled and nothing has ever been
 * queued, and stops the moment either becomes false.
 */
export function isFirstRun(snap: Snapshot): boolean {
  const enrolled = snap.repos.filter((r) => r.reviewed).length;
  const anyWork =
    snap.overview.counts.in_flight +
      snap.overview.counts.queued +
      snap.overview.counts.held +
      snap.overview.counts.fixing >
    0;
  return enrolled === 0 && !anyWork && snap.overview.finished.length === 0;
}

export function FirstRun({ snap }: { snap: Snapshot }) {
  const setup = snap.setup;
  const bots = snap.bots.filter((b) => b.enabled);
  const working = snap.bots.filter((b) => b.status === "working").length;
  const agent = snap.repos[0]?.solver?.agent ?? "";
  const hosts = setup.fleet?.length ?? 0;

  // The steps are the real ones, in order, each answering itself from live
  // state — a checklist that cannot tell you whether it is done is a to-do
  // list somebody else wrote.
  const steps: { title: string; done: boolean; body: React.ReactNode }[] = [
    {
      title: "A place for the queue to live",
      done: setup.checks.some((c) => c.key === "state" && c.status === "ok"),
      body: (
        <>
          crq keeps its whole state in a git ref on one repository, so every host reads the same
          answer and nothing needs a database. <code>crq init</code> sets it up.
        </>
      ),
    },
    {
      title: "A daemon to drive it",
      done: snap.overview.leader != null && !snap.overview.leader.expired,
      body: (
        <>
          One host holds a lease and does the firing, so two machines never ask for the same review.{" "}
          {hosts > 0 ? `${hosts} host(s) are reporting in.` : "No host is reporting yet."}
        </>
      ),
    },
    {
      title: "Reviewers that are actually set up",
      done: working > 0,
      body: (
        <>
          {bots.length} reviewer(s) enabled, {working} of which crq has seen working here. Enabled and
          working are different things — a bot nobody signed up for looks configured and reviews
          nothing.{" "}
          <a href="#/bots" className="text-acc hover:underline">
            Compare and set them up →
          </a>
        </>
      ),
    },
    {
      title: "An agent to fix what they find",
      done: agent !== "",
      body: (
        <>
          Optional, and the half most people skip: crq can run a coding agent over the findings and
          push the fixes. <code>crq autofix install</code> sets it up on a host.
        </>
      ),
    },
    {
      title: "A repository to review",
      done: false,
      body: (
        <>
          Nothing is enrolled yet, which is why this page is here instead of the queue.{" "}
          <a href="#/repos" className="font-semibold text-acc hover:underline">
            Add a repository →
          </a>
        </>
      ),
    },
  ];

  return (
    <main className="mx-auto max-w-[860px] px-6 pt-8 pb-16">
      <h1 className="text-[26px] font-[680] tracking-tight">Nothing is enrolled yet</h1>
      <p className="mt-2 text-[14.5px] text-mut">
        crq keeps automated reviewers off each other's toes. It runs three kinds of work, and knowing
        which is which explains most of what the rest of this dashboard says.
      </p>

      <div className="mt-4 grid grid-cols-3 gap-3 max-[760px]:grid-cols-1">
        {[
          {
            k: "Metered review",
            v: "The reviewer that costs an account allowance. One at a time across the whole fleet, which is what the queue is for.",
          },
          {
            k: "Co-review",
            v: "Reviewers covered by their own subscriptions. They cost nothing per review, so they never wait for the queue.",
          },
          {
            k: "Autofix",
            v: "A coding agent that fixes what the reviewers found and pushes. Separate from both, and off wherever you say so.",
          },
        ].map((c) => (
          <div key={c.k} className="rounded-[10px] border border-edge bg-card px-4 py-3 shadow-card">
            <div className="text-[13px] font-[650]">{c.k}</div>
            <div className="mt-1 text-[12.5px] text-mut">{c.v}</div>
          </div>
        ))}
      </div>

      <ol className="mt-6">
        {steps.map((s, i) => (
          <li key={s.title} className="flex gap-3 border-b border-[#EEF0F3] py-3 last:border-none">
            <span
              className={`mt-0.5 flex size-[22px] shrink-0 items-center justify-center rounded-full text-[12px] font-semibold ${
                s.done ? "bg-ok text-white" : "border border-edge text-mut"
              }`}
            >
              {s.done ? "✓" : i + 1}
            </span>
            <span>
              <div className="text-[14px] font-[650]">{s.title}</div>
              <div className="mt-0.5 text-[13px] text-mut">{s.body}</div>
            </span>
          </li>
        ))}
      </ol>

      <p className="mt-5 text-[12.5px] text-faint">
        This page is not a mode and there is no flag to get stuck on: it is showing because nothing is
        enrolled and nothing has ever been queued. Enrol one repository and the queue replaces it.{" "}
        <a href="#/setup" className="text-acc hover:underline">
          Setup
        </a>{" "}
        has the full check list either way.
      </p>
    </main>
  );
}
