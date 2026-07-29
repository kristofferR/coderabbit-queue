import { Effect, Schema } from "effect";
import { afterEach, describe, expect, it, vi } from "vitest";
import { decodeFrame, requestJson } from "./client";
import { EnrollImpactSchema, FindingSchema } from "./data/contracts";

const NameSchema = Schema.Struct({ name: Schema.String });

afterEach(() => {
  vi.unstubAllGlobals();
});

describe("dashboard data boundary", () => {
  it("decodes valid HTTP data through Effect Schema", async () => {
    const fetchMock = vi.fn(
      async (_url: string, _init?: RequestInit) => new Response('{"name":"crq"}', { status: 200 }),
    );
    vi.stubGlobal("fetch", fetchMock);

    await expect(Effect.runPromise(requestJson(NameSchema, "/api/test"))).resolves.toEqual({
      name: "crq",
    });
    const headers = new Headers(fetchMock.mock.calls[0]?.[1]?.headers);
    expect(headers.get("X-CRQ-Dashboard")).toBe("1");
  });

  it("turns malformed successful responses into typed decode failures", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(async () => new Response('{"name":42}', { status: 200 })),
    );

    const failure = await Effect.runPromise(Effect.flip(requestJson(NameSchema, "/api/test")));
    expect(failure.kind).toBe("decode");
    expect(failure.message).toContain("returned invalid data");
  });

  it("preserves the server's message for non-success responses", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(async () => new Response('{"error":"repository is held"}', { status: 409 })),
    );

    const failure = await Effect.runPromise(Effect.flip(requestJson(NameSchema, "/api/test")));
    expect(failure).toMatchObject({
      kind: "http",
      message: "repository is held",
      status: 409,
    });
  });

  it("validates event-stream frames with the endpoint contract", async () => {
    const valid = JSON.stringify({
      repo: "openai/example",
      open: 4,
      eligible: 2,
      low: 0.2,
      high: 0.8,
      summary: "2 reviews",
      prices_checked_at: "2026-07-29",
    });

    await expect(Effect.runPromise(decodeFrame(EnrollImpactSchema, valid))).resolves.toMatchObject({
      eligible: 2,
      repo: "openai/example",
    });

    const failure = await Effect.runPromise(
      Effect.flip(decodeFrame(EnrollImpactSchema, '{"repo":"openai/example"}')),
    );
    expect(failure.kind).toBe("decode");
  });

  it("preserves reviewer rubric metadata on findings", async () => {
    const finding = JSON.stringify({
      id: "finding-1",
      bot: "coderabbitai[bot]",
      severity: "potential",
      scale: "P2",
      category: "Bug",
      effort: "Quick win",
      title: "Keep the useful labels",
    });

    await expect(Effect.runPromise(decodeFrame(FindingSchema, finding))).resolves.toMatchObject({
      scale: "P2",
      category: "Bug",
      effort: "Quick win",
    });
  });

  it("propagates Effect interruption into the fetch AbortSignal", async () => {
    let aborted = false;
    vi.stubGlobal(
      "fetch",
      vi.fn(
        (_url: string, init?: RequestInit) =>
          new Promise<Response>((_resolve, reject) => {
            init?.signal?.addEventListener("abort", () => {
              aborted = true;
              reject(new DOMException("aborted", "AbortError"));
            });
          }),
      ),
    );

    const controller = new AbortController();
    const pending = Effect.runPromise(requestJson(NameSchema, "/api/slow"), {
      signal: controller.signal,
    });
    await Promise.resolve();
    controller.abort();

    await expect(pending).rejects.toBeDefined();
    expect(aborted).toBe(true);
  });
});
