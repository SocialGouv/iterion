import { describe, expect, it } from "vitest";

import { dispatcherActionState } from "./dispatcherActionState";

describe("dispatcherActionState", () => {
  it("allows Poll now only while the dispatcher is running", () => {
    expect(dispatcherActionState("running").canPollDispatches).toBe(true);
    expect(dispatcherActionState("paused").canPollDispatches).toBe(false);
    expect(dispatcherActionState("idle").canPollDispatches).toBe(false);
    expect(dispatcherActionState("error").canPollDispatches).toBe(false);
    expect(dispatcherActionState(null).canPollDispatches).toBe(false);
  });

  it("uses paused-specific Poll now copy that does not promise retry dispatch", () => {
    const meta = dispatcherActionState("paused");

    expect(meta.pollTitle).toMatch(/Resume the dispatcher/i);
    expect(meta.pollTitle).toMatch(/due retries/i);
    expect(meta.pollTitle).not.toMatch(/next tick/i);
  });

  it("treats a paused snapshot as not pollable even before status catches up", () => {
    const meta = dispatcherActionState("running", true);

    expect(meta.canPollDispatches).toBe(false);
    expect(meta.pollTitle).toMatch(/Resume the dispatcher/i);
  });

  it("keeps Reload config available for running and paused dispatchers", () => {
    expect(dispatcherActionState("running").canReloadConfig).toBe(true);
    expect(dispatcherActionState("paused").canReloadConfig).toBe(true);
    expect(dispatcherActionState("idle").canReloadConfig).toBe(false);
  });
});
