import { Data, Effect, Schema } from "effect";

/**
 * The dashboard has one recoverable failure channel. Transport, HTTP and JSON
 * failures remain distinguishable by `kind`, while every React caller gets the
 * same display-safe message instead of inspecting `unknown`.
 */
export class DashboardError extends Data.TaggedError("DashboardError")<{
  readonly kind: "network" | "http" | "decode" | "stream";
  readonly message: string;
  readonly status?: number;
  readonly cause?: unknown;
}> {}

const decodeJson = <S extends Schema.Constraint>(
  schema: S,
  text: string,
  context: string,
): Effect.Effect<S["Type"], DashboardError, S["DecodingServices"]> =>
  Schema.decodeUnknownEffect(Schema.fromJsonString(schema))(text).pipe(
    Effect.mapError(
      (cause) =>
        new DashboardError({
          kind: "decode",
          message: `${context} returned invalid data: ${cause.message}`,
          cause,
        }),
    ),
  );

const errorMessage = (body: unknown, status: number, text: string): string => {
  if (
    typeof body === "object" &&
    body !== null &&
    "error" in body &&
    typeof body.error === "string"
  ) {
    return body.error;
  }
  return text.trim() || `HTTP ${status}`;
};

/**
 * The sole HTTP boundary for the dashboard. Fetch cancellation follows Effect
 * interruption, non-2xx responses enter the typed error channel, and response
 * bodies are decoded exactly once.
 */
export const requestJson = <S extends Schema.Constraint>(
  schema: S,
  url: string,
  init?: RequestInit,
): Effect.Effect<S["Type"], DashboardError, S["DecodingServices"]> =>
  Effect.gen(function* () {
    const headers = new Headers(init?.headers);
    // A custom same-origin header is the dashboard's CSRF boundary. Some
    // read-only endpoints (notably repository discovery) require it too.
    headers.set("X-CRQ-Dashboard", "1");
    const response = yield* Effect.tryPromise({
      try: (signal) => fetch(url, { ...init, headers, signal }),
      catch: (cause) =>
        new DashboardError({
          kind: "network",
          message: "Could not reach crq serve",
          cause,
        }),
    });

    const text = yield* Effect.tryPromise({
      try: () => response.text(),
      catch: (cause) =>
        new DashboardError({
          kind: "network",
          message: "The server response could not be read",
          cause,
        }),
    });

    if (!response.ok) {
      const body = yield* decodeJson(Schema.Unknown, text, "The server").pipe(
        Effect.orElseSucceed(() => undefined),
      );
      return yield* new DashboardError({
        kind: "http",
        status: response.status,
        message: errorMessage(body, response.status, text),
      });
    }

    return yield* decodeJson(schema, text, "The server");
  });

/** Decode an SSE data frame through the same typed JSON boundary as HTTP. */
export const decodeFrame = <S extends Schema.Constraint>(
  schema: S,
  text: string,
): Effect.Effect<S["Type"], DashboardError, S["DecodingServices"]> =>
  decodeJson(schema, text, "The event stream");

export const streamDisconnected = () =>
  new DashboardError({
    kind: "stream",
    message: "The live state stream disconnected",
  });
