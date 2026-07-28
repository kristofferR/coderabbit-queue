import { fireEvent, render, screen } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import { Confirm } from "./Confirm";

describe("Confirm", () => {
  it("has dialog semantics, requires a reason, and handles Escape", () => {
    const onCancel = vi.fn();
    const onConfirm = vi.fn();

    render(
      <Confirm
        title="Hold this round?"
        body="The round stays visible."
        confirmLabel="Hold"
        needsReason
        onCancel={onCancel}
        onConfirm={onConfirm}
      />,
    );

    expect(screen.getByRole("dialog", { name: "Hold this round?" })).toBeDefined();
    const confirm = screen.getByRole("button", { name: "Hold" }) as HTMLButtonElement;
    expect(confirm.disabled).toBe(true);

    fireEvent.change(screen.getByRole("textbox"), { target: { value: "maintenance" } });
    expect(confirm.disabled).toBe(false);
    fireEvent.click(confirm);
    expect(onConfirm).toHaveBeenCalledWith("maintenance");

    fireEvent.keyDown(document, { key: "Escape" });
    expect(onCancel).toHaveBeenCalledOnce();
  });
});
