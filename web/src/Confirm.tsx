import { useEffect, useRef, useState, type ReactNode } from "react";

/**
 * A single-step confirmation. Consequences come from live state and are stated
 * plainly — this is a single-user tool, so typed-confirmation ceremony would be
 * theatre, but a click that spends quota or archives a round should still say
 * what it will do before it does it.
 */
export function Confirm({
  title,
  body,
  confirmLabel,
  danger,
  needsReason,
  reasonLabel,
  busy,
  error,
  onConfirm,
  onCancel,
}: {
  title: string;
  body: ReactNode;
  confirmLabel: string;
  danger?: boolean;
  needsReason?: boolean;
  reasonLabel?: string;
  busy?: boolean;
  error?: string | null;
  onConfirm: (reason: string) => void;
  onCancel: () => void;
}) {
  const [reason, setReason] = useState("");
  const blocked = needsReason && reason.trim() === "";
  const panel = useRef<HTMLDivElement>(null);
  const reasonInput = useRef<HTMLInputElement>(null);
  const returnTo = useRef<Element | null>(null);
  const onCancelRef = useRef(onCancel);
  onCancelRef.current = onCancel;

  // A confirmation that spends quota or archives a round must not leave the
  // row behind it reachable by keyboard: Tab stays inside, Escape cancels, and
  // focus goes back where it came from so the list does not jump to the top.
  useEffect(() => {
    returnTo.current = document.activeElement;
    const focusable = () =>
      Array.from(
        panel.current?.querySelectorAll<HTMLElement>(
          'button:not([disabled]), input:not([disabled]), [href], select, textarea, [tabindex]:not([tabindex="-1"])',
        ) ?? [],
      );
    (needsReason ? reasonInput.current : null)?.focus();
    if (!panel.current?.contains(document.activeElement)) focusable()[0]?.focus();

    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape") {
        e.preventDefault();
        onCancelRef.current();
        return;
      }
      if (e.key !== "Tab") return;
      const items = focusable();
      if (items.length === 0) return;
      const first = items[0];
      const last = items[items.length - 1];
      const active = document.activeElement;
      if (e.shiftKey && (active === first || !panel.current?.contains(active))) {
        e.preventDefault();
        last.focus();
      } else if (!e.shiftKey && active === last) {
        e.preventDefault();
        first.focus();
      }
    };
    document.addEventListener("keydown", onKey);
    return () => {
      document.removeEventListener("keydown", onKey);
      (returnTo.current as HTMLElement | null)?.focus?.();
    };
  }, []);

  return (
    <div className="fixed inset-0 z-50 flex items-start justify-center bg-[rgb(27_36_48/0.28)] px-4 pt-[12vh]">
      <div
        ref={panel}
        role="dialog"
        aria-modal="true"
        aria-label={title}
        className="w-full max-w-[520px] rounded-[10px] border border-edge bg-card p-5 shadow-[0_16px_48px_rgb(27_36_48/0.24)]"
      >
        <h2 className={`text-[15px] font-[650] ${danger ? "text-bad" : "text-ink"}`}>{title}</h2>
        <div className="mt-2 text-[13px] text-mut">{body}</div>

        {needsReason && (
          <label className="mt-3 block">
            <span className="text-[12.5px] font-medium">{reasonLabel ?? "Reason"}</span>
            <input
              ref={reasonInput}
              required
              aria-invalid={blocked}
              aria-describedby="confirm-reason-hint"
              value={reason}
              onChange={(e) => setReason(e.target.value)}
              placeholder="why — this is what every screen will show"
              className="mt-1 w-full rounded-lg border border-edge bg-[#FBFBFC] px-2.5 py-1.5 text-[13px]"
            />
            <span id="confirm-reason-hint" className="mt-1 block text-[11.5px] text-faint">
              A reason is required and will be shown with this decision.
            </span>
          </label>
        )}

        {error && (
          <div className="mt-3 rounded-lg border border-bad-edge bg-bad-bg px-3 py-2 text-[12.5px] text-bad">
            {error}
          </div>
        )}

        <div className="mt-4 flex items-center gap-2.5">
          <button
            type="button"
            disabled={busy || blocked}
            onClick={() => onConfirm(reason.trim())}
            className={`rounded-lg px-4 py-1.5 text-[13px] font-semibold text-white disabled:opacity-45 ${
              danger ? "bg-bad hover:brightness-110" : "bg-ink hover:bg-[#2E3C4E]"
            }`}
          >
            {busy ? "Working…" : confirmLabel}
          </button>
          <button
            type="button"
            disabled={busy}
            onClick={onCancel}
            className="rounded-lg border border-edge px-4 py-1.5 text-[13px] font-semibold text-mut disabled:opacity-45"
          >
            Cancel
          </button>
        </div>
      </div>
    </div>
  );
}
