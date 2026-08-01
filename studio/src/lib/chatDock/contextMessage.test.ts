import { describe, expect, it } from "vitest";

import { CONTEXT_PREFIX, withPageContext } from "./contextMessage";
import { referenceForRoute } from "./routeReference";
import { activeReference } from "./useRouteReference";

const runRef = referenceForRoute("/runs/019fbd46ed82");

describe("withPageContext", () => {
  it("prefixes the pointer, not the page's content", () => {
    const out = withPageContext("why is this red?", runRef);
    expect(out).toBe("[page context: run/019fbd46ed82]\n\nwhy is this red?");
    expect(out.startsWith(CONTEXT_PREFIX)).toBe(true);
  });

  it("leaves the message alone when there is no reference", () => {
    expect(withPageContext("hello", null)).toBe("hello");
  });

  it("never turns whitespace into a context-only message", () => {
    expect(withPageContext("   ", runRef)).toBe("");
  });
});

describe("activeReference", () => {
  const boardRef = referenceForRoute("/board");

  it("passes the reference through when nothing was dismissed", () => {
    expect(activeReference(runRef, null)).toBe(runRef);
  });

  // Dismissal is keyed on the reference, so navigating to a different
  // thing re-arms the chip without the operator asking.
  it("suppresses only the dismissed reference", () => {
    expect(activeReference(runRef, "run/019fbd46ed82")).toBeNull();
    expect(activeReference(boardRef, "run/019fbd46ed82")).toBe(boardRef);
  });

  it("stays null when the route points at nothing", () => {
    expect(activeReference(null, null)).toBeNull();
  });
});
