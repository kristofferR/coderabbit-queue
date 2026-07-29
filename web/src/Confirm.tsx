import { type ReactNode, useState } from "react";
import { Dialog, DialogContent, DialogDescription, DialogTitle } from "./ui/dialog";

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

  return (
    <Dialog open onOpenChange={(open) => !open && onCancel()}>
      <DialogContent
        className="p-5"
        closeDisabled={busy}
        onInteractOutside={(event) => busy && event.preventDefault()}
        onPointerDownOutside={(event) => busy && event.preventDefault()}
        onEscapeKeyDown={(event) => busy && event.preventDefault()}
      >
        <DialogTitle className={danger ? "text-bad" : "text-ink"}>{title}</DialogTitle>
        <DialogDescription asChild>
          <div className="mt-2 text-[13px] text-mut">{body}</div>
        </DialogDescription>

        {needsReason && (
          <label className="mt-3 block">
            <span className="text-[12.5px] font-medium">{reasonLabel ?? "Reason"}</span>
            <input
              value={reason}
              onChange={(e) => setReason(e.target.value)}
              placeholder="why — this is what every screen will show"
              className="mt-1 w-full rounded-lg border border-edge bg-[#FBFBFC] px-2.5 py-1.5 text-[13px]"
            />
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
      </DialogContent>
    </Dialog>
  );
}
