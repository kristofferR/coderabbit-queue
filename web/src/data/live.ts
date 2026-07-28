import { Effect, Fiber } from "effect";
import { decodeFrame, streamDisconnected } from "../client";
import type { Snapshot } from "./contracts";
import { SnapshotSchema } from "./contracts";

/** Connection state, so the UI can say "stale" instead of quietly lying. */
export type Live = "connecting" | "live" | "reconnecting";

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
            onLive("live");
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
              onLive("live");
            }
          };
          source.onerror = () => {
            onLive("reconnecting");
            resume(Effect.fail(streamDisconnected()));
          };
          return Effect.sync(() => {
            source.onopen = null;
            source.onmessage = null;
            source.onerror = null;
          });
        });
      }),
    );

  const program = Effect.gen(function* () {
    onLive("connecting");
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
