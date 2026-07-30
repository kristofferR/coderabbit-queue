import { Effect } from "effect";
import { useCallback, useEffect, useRef, useState } from "react";
import type { DashboardError } from "./client";

type Options<A> = {
  onSuccess?: (value: A) => void;
  onFailure?: (error: DashboardError) => void;
  onFinally?: () => void;
};

/**
 * Runs one interruptible Effect at a time for a component.
 *
 * Starting a replacement operation aborts the old fetch, unmounting interrupts
 * the fiber, and typed failures become renderable state. Components are left
 * with their domain-specific success transitions rather than copies of the
 * same try/catch/finally ceremony.
 */
export function useOperation() {
  const active = useRef<AbortController | null>(null);
  const [running, setRunning] = useState(false);
  const [error, setError] = useState<string | null>(null);

  useEffect(
    () => () => {
      const current = active.current;
      active.current = null;
      current?.abort();
    },
    [],
  );

  const run = useCallback(function run<A>(
    program: Effect.Effect<A, DashboardError>,
    options: Options<A> = {},
  ): void {
    active.current?.abort();
    const controller = new AbortController();
    active.current = controller;
    setRunning(true);
    setError(null);

    const outcome = program.pipe(
      Effect.match({
        onFailure: (failure) => ({ ok: false as const, failure }),
        onSuccess: (value) => ({ ok: true as const, value }),
      }),
    );

    void Effect.runPromise(outcome, { signal: controller.signal })
      // Interruption bypasses `Effect.match`; it is expected on replacement or
      // unmount. Catch defects before callbacks run so a callback exception is
      // not mislabeled as a failed dashboard operation.
      .catch((defect: unknown) => {
        if (controller.signal.aborted) return undefined;
        console.error("Unexpected dashboard operation defect", defect);
        setError(
          defect instanceof Error
            ? `Unexpected dashboard failure: ${defect.message}`
            : "Unexpected dashboard failure",
        );
        return undefined;
      })
      .then((result) => {
        if (controller.signal.aborted || !result) return;
        if (result.ok) {
          options.onSuccess?.(result.value);
        } else {
          setError(result.failure.message);
          options.onFailure?.(result.failure);
        }
      })
      .finally(() => {
        if (active.current !== controller) return;
        active.current = null;
        setRunning(false);
        options.onFinally?.();
      });
  }, []);

  const clearError = useCallback(() => setError(null), []);

  return { run, running, error, clearError };
}
