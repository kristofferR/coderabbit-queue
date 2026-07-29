import { Effect, Fiber, Schema } from "effect";
import { decodeFrame, streamDisconnected } from "../client";
import type { Snapshot } from "./contracts";
import { SnapshotSchema } from "./contracts";

/** Connection state, so the UI can say "stale" instead of quietly lying. */
export type Live =
  | { status: "connecting" }
  | { status: "live" }
  | { status: "reconnecting"; error?: string };

const UnavailableSchema = Schema.Struct({ error: Schema.String });

/**
 * Subscribes to whole snapshots. The server pushes only when the state ref's
 * revision moves; clocks tick locally, so nothing here polls for time.
 */
export function subscribe(
  onData: (snapshot: Snapshot) => void,
  onLive: (live: Live) => void,
): () => void {
  const connection = (resetDelay: () => void) =>
    Effect.scoped(
      Effect.gen(function* () {
        const source = yield* Effect.acquireRelease(
          Effect.sync(() => new EventSource("/api/events")),
          (current) => Effect.sync(() => current.close()),
        );

        return yield* Effect.callback<never, ReturnType<typeof streamDisconnected>>((resume) => {
          source.onopen = () => {
            resetDelay();
          };
          source.onmessage = (event) => {
            const frame = Effect.runSync(
              decodeFrame(SnapshotSchema, event.data).pipe(
                Effect.match({
                  onFailure: () => undefined,
                  onSuccess: (snapshot) => snapshot,
                }),
              ),
            );
            // A malformed frame is isolated to that frame; the last valid
            // snapshot remains more useful than tearing down the stream.
            if (frame) {
              onData(frame);
              onLive(
                frame.stale
                  ? { status: "reconnecting", error: frame.stale.error }
                  : { status: "live" },
              );
            }
          };
          const unavailable = (event: MessageEvent) => {
            const frame = Effect.runSync(
              decodeFrame(UnavailableSchema, event.data).pipe(
                Effect.match({
                  onFailure: () => undefined,
                  onSuccess: (decoded) => decoded,
                }),
              ),
            );
            onLive({
              status: "reconnecting",
              error: frame?.error ?? "The shared state ref is unavailable",
            });
          };
          source.addEventListener("unavailable", unavailable);
          source.onerror = () => {
            onLive({ status: "reconnecting", error: "Lost the connection to crq serve" });
            resume(Effect.fail(streamDisconnected()));
          };
          return Effect.sync(() => {
            source.removeEventListener("unavailable", unavailable);
            source.onopen = null;
            source.onmessage = null;
            source.onerror = null;
          });
        });
      }),
    );

  const program = Effect.gen(function* () {
    onLive({ status: "connecting" });
    let delay = 1000;
    while (true) {
      yield* connection(() => {
        delay = 1000;
      }).pipe(Effect.catch(() => Effect.void));
      yield* Effect.sleep(delay);
      delay = Math.min(delay * 2, 30000);
    }
  });

  const fiber = Effect.runFork(program);
  return () => {
    Effect.runFork(Fiber.interrupt(fiber));
  };
}
