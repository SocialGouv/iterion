import { afterEach, describe, expect, it } from "vitest";

import { STUDIO_BASE, studioRoute } from "./scope";

// The scope is read per call from the global the demux proxy injects into a
// workspace pane's index.html, so a test can set it and clear it.
function setScope(prefix: string | undefined): void {
  const g = globalThis as { __ITERION_SCOPE__?: unknown };
  if (prefix === undefined) delete g.__ITERION_SCOPE__;
  else g.__ITERION_SCOPE__ = prefix;
}

afterEach(() => setScope(undefined));

describe("studioRoute", () => {
  // Called from inside a nested router (the /admin console), where a plain
  // "/orgs/x" would resolve against the nested base. "~" drops every base, so
  // the studio's has to be put back by hand.
  it("addresses a studio page from inside a nested router", () => {
    expect(studioRoute("/orgs/o1")).toBe(`~${STUDIO_BASE}/orgs/o1`);
  });

  // The reason it is studioBase() and not a bare "~": a pane's /x/<id> is not
  // part of the studio base, and "~" strips that too — a bare "~/orgs/o1"
  // would send the pane to the desktop shell's own origin root.
  it("keeps a workspace pane's scope prefix", () => {
    setScope("/x/conn-1");
    expect(studioRoute("/orgs/o1")).toBe(`~/x/conn-1${STUDIO_BASE}/orgs/o1`);
  });

  it("ignores a global that is not a pane scope", () => {
    setScope("not-a-scope");
    expect(studioRoute("/teams/t1")).toBe(`~${STUDIO_BASE}/teams/t1`);
  });
});
