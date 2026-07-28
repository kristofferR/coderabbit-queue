import { render, screen } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import { AppErrorBoundary } from "./AppErrorBoundary";

function BrokenView(): never {
  throw new Error("render exploded");
}

describe("AppErrorBoundary", () => {
  it("renders an actionable defect surface instead of a blank page", () => {
    vi.spyOn(console, "error").mockImplementation(() => undefined);

    render(
      <AppErrorBoundary>
        <BrokenView />
      </AppErrorBoundary>,
    );

    expect(screen.getByText("The dashboard hit an unexpected error")).toBeDefined();
    expect(screen.getByText("render exploded")).toBeDefined();
    expect(screen.getByRole("button", { name: "Reload dashboard" })).toBeDefined();
  });
});
