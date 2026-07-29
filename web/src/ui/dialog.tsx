import * as DialogPrimitive from "@radix-ui/react-dialog";
import { X } from "lucide-react";
import type { ComponentProps } from "react";
import { cn } from "./utils";

export const Dialog = DialogPrimitive.Root;
export const DialogTrigger = DialogPrimitive.Trigger;
export const DialogClose = DialogPrimitive.Close;

export function DialogOverlay({
  className,
  ...props
}: ComponentProps<typeof DialogPrimitive.Overlay>) {
  return (
    <DialogPrimitive.Overlay
      className={cn(
        "fixed inset-0 z-50 bg-[rgb(23_33_45/0.34)] backdrop-blur-[1px]",
        "data-[state=open]:animate-in data-[state=closed]:animate-out",
        "data-[state=closed]:fade-out-0 data-[state=open]:fade-in-0",
        className,
      )}
      {...props}
    />
  );
}

export function DialogContent({
  children,
  className,
  closeDisabled = false,
  closeLabel = "Close dialog",
  variant = "modal",
  ...props
}: ComponentProps<typeof DialogPrimitive.Content> & {
  closeDisabled?: boolean;
  closeLabel?: string;
  variant?: "modal" | "sheet";
}) {
  return (
    <DialogPrimitive.Portal>
      <DialogOverlay />
      <DialogPrimitive.Content
        className={cn(
          variant === "sheet"
            ? "fixed inset-y-0 right-0 z-50 flex h-full w-[420px] max-w-full flex-col border-l border-edge bg-card shadow-[0_0_48px_rgb(23_33_45/0.18)]"
            : "fixed top-[12vh] left-1/2 z-50 w-[calc(100%-2rem)] max-w-[520px] -translate-x-1/2 rounded-[10px] border border-edge bg-card shadow-[0_16px_48px_rgb(23_33_45/0.24)]",
          "focus:outline-none data-[state=open]:animate-in data-[state=closed]:animate-out",
          variant === "sheet"
            ? "data-[state=closed]:slide-out-to-right data-[state=open]:slide-in-from-right"
            : "data-[state=closed]:fade-out-0 data-[state=open]:fade-in-0 data-[state=closed]:zoom-out-95 data-[state=open]:zoom-in-95",
          className,
        )}
        {...props}
      >
        {children}
        <DialogPrimitive.Close
          aria-label={closeLabel}
          disabled={closeDisabled}
          className="absolute top-3 right-3 inline-flex size-7 items-center justify-center rounded-md text-faint hover:bg-bg hover:text-ink disabled:pointer-events-none disabled:opacity-45"
        >
          <X aria-hidden className="size-4" />
        </DialogPrimitive.Close>
      </DialogPrimitive.Content>
    </DialogPrimitive.Portal>
  );
}

export function DialogTitle({ className, ...props }: ComponentProps<typeof DialogPrimitive.Title>) {
  return (
    <DialogPrimitive.Title
      className={cn("text-[15px] font-[650] text-ink", className)}
      {...props}
    />
  );
}

export function DialogDescription({
  className,
  ...props
}: ComponentProps<typeof DialogPrimitive.Description>) {
  return (
    <DialogPrimitive.Description className={cn("text-[13px] text-mut", className)} {...props} />
  );
}
