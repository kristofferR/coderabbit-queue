import { useEffect, useRef, useState } from "react";

type Tail = {
  text: string;
  size: number;
  truncated: boolean;
  host?: string;
};

export function AutofixLog({ repo, pr }: { repo: string; pr: number }) {
  const [open, setOpen] = useState(false);
  const [tail, setTail] = useState<Tail | null>(null);
  const [error, setError] = useState<string | null>(null);
  const pane = useRef<HTMLPreElement>(null);

  useEffect(() => {
    if (!open) return;
    let stopped = false;
    const refresh = async () => {
      try {
        const response = await fetch(`/api/autofix-log/${repo}/${pr}`, {
          headers: { "X-CRQ-Dashboard": "1" },
        });
        const body = (await response.json()) as Tail & { error?: string };
        if (!response.ok) throw new Error(body.error || `HTTP ${response.status}`);
        if (!stopped) {
          setTail(body);
          setError(null);
        }
      } catch (cause) {
        if (!stopped) setError((cause as Error).message);
      }
    };
    void refresh();
    const timer = window.setInterval(() => void refresh(), 1000);
    return () => {
      stopped = true;
      window.clearInterval(timer);
    };
  }, [open, repo, pr]);

  useEffect(() => {
    if (pane.current) pane.current.scrollTop = pane.current.scrollHeight;
  }, [tail?.text]);

  return (
    <div className="w-full">
      <button
        type="button"
        aria-expanded={open}
        onClick={() => setOpen((value) => !value)}
        className="text-[12px] font-semibold text-acc hover:underline"
      >
        {open ? "Hide live log" : "Watch live log"}
      </button>
      {open && (
        <div className="mt-2 overflow-hidden rounded-lg border border-[#263447] bg-[#111827] text-[#D8E2F0] shadow-inner">
          <div className="flex items-center gap-2 border-b border-white/10 px-3 py-1.5 text-[10.5px] text-[#93A4B8]">
            <span className="size-1.5 animate-pulse rounded-full bg-[#5DDB9D]" />
            live tail · refreshes every second
            {tail?.host && <span>· {tail.host}</span>}
            {tail?.truncated && <span className="ml-auto">showing newest 128 KB</span>}
          </div>
          {error ? (
            <div className="px-3 py-3 text-[11.5px] text-[#FFB4B4]">{error}</div>
          ) : (
            <pre
              ref={pane}
              tabIndex={0}
              className="max-h-80 min-h-28 overflow-auto whitespace-pre-wrap break-words px-3 py-2.5 font-mono text-[11px] leading-relaxed"
            >
              {tail?.text || "Waiting for the session to write output…"}
            </pre>
          )}
        </div>
      )}
    </div>
  );
}
