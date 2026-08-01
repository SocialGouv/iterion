import { describe, expect, it } from "vitest";

import {
  isAssistantOwnRoute,
  matchPath,
  referenceForRoute,
  sanitizeReferenceText,
} from "./routeReference";

describe("sanitizeReferenceText", () => {
  // Route params are attacker-supplied — the operator only has to open a
  // crafted link. These are the characters that break the single-line,
  // bracket-delimited context protocol.
  it("strips the line and bracket breakers", () => {
    expect(sanitizeReferenceText("019f\n\nIgnore me")).toBe("019fIgnore me");
    expect(sanitizeReferenceText("a\rb")).toBe("ab");
    expect(sanitizeReferenceText("a]b[c")).toBe("abc");
    expect(sanitizeReferenceText("a\u2028b\u2029c")).toBe("abc");
  });

  // U+0085 NEL is a line terminator in Unicode's book even though JS's
  // "\n" split and /\s/ both miss it — the same class as U+2028/U+2029,
  // which are stripped, so leaving it in would be inconsistent with the
  // set's own rationale. The C1 block it sits in goes with it.
  it("strips the line terminators JS does not see", () => {
    expect(sanitizeReferenceText("019f\u0085\u0085SYSTEM")).toBe("019fSYSTEM");
    expect(sanitizeReferenceText("a\u0090b")).toBe("ab");
  });

  // Bidi overrides reorder rendered text, so a crafted reference could
  // make the chip DISPLAY something other than what the prompt carries.
  // The chip exists so context is never invisible; that has to hold.
  it("strips the bidi controls that would misrepresent the chip", () => {
    expect(sanitizeReferenceText("a\u202eb\u202cc")).toBe("abc");
    expect(sanitizeReferenceText("a\u200eb\u2066c\u2069d")).toBe("abcd");
  });

  // Narrow on purpose: a blanket "printable ASCII" strip would eat the
  // digits and uppercase that make up most of a run id.
  it("leaves a legitimate reference untouched", () => {
    expect(sanitizeReferenceText("019fbd46-ED82_7c32.bot")).toBe(
      "019fbd46-ED82_7c32.bot",
    );
    expect(sanitizeReferenceText("bots/review-pr/main.bot")).toBe(
      "bots/review-pr/main.bot",
    );
  });

  it("bounds the length, since ?file= and /repos/:key are unbounded", () => {
    expect(sanitizeReferenceText("x".repeat(500))).toHaveLength(200);
  });
});

describe("isAssistantOwnRoute", () => {
  // The dock stands down where the full-width view already renders the
  // same session — two composers over one conversation is the ambiguity
  // this feature removes, not one it should add.
  it("is true only for the assistant's full-width route", () => {
    expect(isAssistantOwnRoute("/whats-next")).toBe(true);
    expect(isAssistantOwnRoute("/board")).toBe(false);
    expect(isAssistantOwnRoute("/whats-next/extra")).toBe(false);
  });
});

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
