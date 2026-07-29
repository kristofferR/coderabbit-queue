import { afterEach, describe, expect, it, vi } from "vitest";
import type { Live } from "./live";
import { subscribe } from "./live";

class FakeEventSource {
  static current: FakeEventSource | undefined;

  onopen: (() => void) | null = null;
  onmessage: ((event: MessageEvent) => void) | null = null;
  onerror: (() => void) | null = null;
  unavailable: EventListenerOrEventListenerObject | undefined;

  constructor() {
    FakeEventSource.current = this;
  }

  addEventListener(type: string, listener: EventListenerOrEventListenerObject) {
    if (type === "unavailable") this.unavailable = listener;
  }

  removeEventListener(type: string, listener: EventListenerOrEventListenerObject) {
    if (type === "unavailable" && this.unavailable === listener) this.unavailable = undefined;
  }

  close() {}

  emitUnavailable(data: string) {
    const event = { data } as MessageEvent;
    if (typeof this.unavailable === "function") {
      this.unavailable(event);
    } else {
      this.unavailable?.handleEvent(event);
    }
  }
}

afterEach(() => {
  vi.unstubAllGlobals();
  FakeEventSource.current = undefined;
});

describe("live state stream", () => {
  it("surfaces a named first-load unavailable event", async () => {
    vi.stubGlobal("EventSource", FakeEventSource);
    const states: Live[] = [];
    const stop = subscribe(
      () => undefined,
      (state) => states.push(state),
    );

    await vi.waitFor(() => expect(FakeEventSource.current).toBeDefined());
    FakeEventSource.current?.emitUnavailable('{"error":"state ref unreadable"}');

    await vi.waitFor(() =>
      expect(states.at(-1)).toEqual({ status: "reconnecting", error: "state ref unreadable" }),
    );
    stop();
  });
});
