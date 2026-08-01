import { describe, expect, it } from "vitest";

import { matchPath, referenceForRoute } from "./routeReference";

describe("matchPath", () => {
  it("matches segment-wise, not by prefix", () => {
    expect(matchPath("/board", "/board")).toEqual({});
    expect(matchPath("/board", "/board/labels")).toBeNull();
    expect(matchPath("/board/labels", "/board")).toBeNull();
  });

  it("captures :params and decodes them", () => {
    expect(matchPath("/bots/:name", "/bots/review-pr")).toEqual({
      name: "review-pr",
    });
    expect(matchPath("/repos/:key", "/repos/acme%2Fwidgets")).toEqual({
      key: "acme/widgets",
    });
  });

  // A hand-mangled URL must degrade to the raw segment, never throw into
  // the render.
  it("survives an undecodable segment", () => {
    expect(matchPath("/bots/:name", "/bots/100%")).toEqual({ name: "100%" });
  });

  it("matches the rest of the path on a trailing wildcard", () => {
    expect(matchPath("/admin/*", "/admin/users")).toEqual({});
    expect(matchPath("/admin/*", "/admin/orgs/deep")).toEqual({});
    expect(matchPath("/admin/*", "/board")).toBeNull();
  });
});

describe("referenceForRoute", () => {
  it("points at the run on /runs/:id", () => {
    expect(referenceForRoute("/runs/019fbd46ed82")).toEqual({
      kind: "run",
      ref: "run/019fbd46ed82",
      label: "Run 019fbd46",
    });
  });

  // Literal-before-param ordering, same rule as App.tsx's <Switch>.
  it("prefers the literal /runs/new over /runs/:id", () => {
    expect(referenceForRoute("/runs/new")?.ref).toBe("view/launch");
    expect(referenceForRoute("/bots/new")?.ref).toBe("view/bot-builder");
  });

  it("reads the pipeline card route's own key kinds", () => {
    expect(referenceForRoute("/pipelines/cards/issue/native%3Aabc")?.ref).toBe(
      "card/native:abc",
    );
    // A card keyed on a run IS the run — same reference the /runs/:id
    // route produces, so the assistant isn't handed two names for it.
    expect(referenceForRoute("/pipelines/cards/run/run-42")?.ref).toBe("run/run-42");
  });

  it("addresses the edited file on /editor, and the picker when bare", () => {
    expect(referenceForRoute("/editor", "?file=bots/review-pr/main.bot")).toEqual({
      kind: "bot",
      ref: "bot/bots/review-pr/main.bot",
      label: "main.bot",
    });
    expect(referenceForRoute("/editor")?.ref).toBe("view/editor");
  });

  it("reports a view when the route has no single entity behind it", () => {
    expect(referenceForRoute("/board")?.label).toBe("Board");
    expect(referenceForRoute("/board/labels")?.label).toBe("Board labels");
    expect(referenceForRoute("/pipelines")?.label).toBe("Pipelines");
    expect(referenceForRoute("/skills")?.label).toBe("Skills");
    expect(referenceForRoute("/admin/users")?.label).toBe("Admin");
  });

  // No chip on the assistant's own route (you're already in it), none on
  // home, and — importantly — none for a route nobody mapped: wrong
  // context is worse than no context.
  it("yields no reference where there is nothing to point at", () => {
    expect(referenceForRoute("/whats-next")).toBeNull();
    expect(referenceForRoute("/")).toBeNull();
    expect(referenceForRoute("/some/route/nobody/mapped")).toBeNull();
  });
});
