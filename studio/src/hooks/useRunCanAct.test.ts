import { describe, expect, it } from "vitest";

import { runCanAct } from "./useRunCanAct";

describe("whether the run view acts", () => {
  it("acts for a member or an auth-less server while the socket holds", () => {
    expect(runCanAct({ role: "member" }, "open")).toBe(true);
    expect(runCanAct(null, "closed")).toBe(true);
    expect(runCanAct(undefined, "reconnecting")).toBe(true);
  });

  it("never acts for a viewer", () => {
    expect(runCanAct({ role: "viewer" }, "open")).toBe(false);
  });

  it("never acts once the socket is lost: the ID may name another run", () => {
    expect(runCanAct({ role: "member" }, "lost")).toBe(false);
    expect(runCanAct(null, "lost")).toBe(false);
  });
});
