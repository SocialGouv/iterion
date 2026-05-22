import { describe, expect, it } from "vitest";

import { pickCopy } from "./PreFlightPanel";

describe("PreFlightPanel.pickCopy", () => {
  it("falls back to the launching copy when runStatus is still null", () => {
    const { title, body } = pickCopy("launching", null, 0);
    expect(title).toMatch(/Starting/i);
    expect(body).toMatch(/Loading the backend/i);
  });

  it("uses the queued copy and points at a runner", () => {
    const { title, body } = pickCopy("active", "queued", 0);
    expect(title).toBe("Queued");
    expect(body).toMatch(/runner/i);
  });

  it("distinguishes 'dispatched, no events' from 'running but warming up'", () => {
    const dispatched = pickCopy("active", "running", 0);
    expect(dispatched.title).toBe("Run dispatched");
    expect(dispatched.body).toMatch(/first event/i);

    const warming = pickCopy("active", "running", 4);
    expect(warming.title).toMatch(/Preparing the first step/);
    expect(warming.body).toMatch(/warming up/i);
  });

  it("explains the paused-waiting-human gate", () => {
    const { title, body } = pickCopy("active", "paused_waiting_human", 12);
    expect(title).toMatch(/Waiting for your input/i);
    expect(body).toMatch(/human gate/i);
  });

  it("uses a generic fallback for unhandled lifecycle states", () => {
    const { title } = pickCopy("active", "finished", 100);
    expect(title).toMatch(/Waiting/i);
  });
});
