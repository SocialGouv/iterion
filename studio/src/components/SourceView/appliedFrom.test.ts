import { describe, expect, it } from "vitest";

import type { IterDocument } from "@/api/types";

import { appliedFromDocument } from "./appliedFrom";

// Two documents, told apart by IDENTITY and nothing else: that is what
// `moved()` compares and what the badge reports, and a deep-equal pair of
// objects is exactly the case a value comparison would get wrong.
const opened = { workflows: [] } as unknown as IterDocument;
const changed = { workflows: [] } as unknown as IterDocument;

// The chokepoint both readers of the stale-content window go through: the
// header's badge and `handleApply`'s `was.document`. Exercised here as the
// function the view imports — the component witnesses then only have to
// prove that the two readers are wired to it (#1662).
describe("appliedFromDocument", () => {
  it("presents the document on the store while no render has landed", () => {
    // There is no provenance to present yet, and an Apply of what is on
    // screen is written against the document as it is.
    expect(appliedFromDocument({ salvaged: false, document: opened, rendered: null })).toBe(opened);
  });

  it("presents the rendered document once a render has landed", () => {
    expect(
      appliedFromDocument({ salvaged: false, document: opened, rendered: { doc: opened } }),
    ).toBe(opened);
  });

  it("presents the EARLIER document when one moved under the render", () => {
    // The window: the text came from `opened`, the store holds `changed`.
    // The value differs from the store's document, which is what makes
    // `moved()` refuse and the badge show.
    const presented = appliedFromDocument({
      salvaged: false,
      document: changed,
      rendered: { doc: opened },
    });
    expect(presented).toBe(opened);
    expect(presented).not.toBe(changed);
  });

  it("presents the current document while salvaged, whatever a render claims", () => {
    // The forbidden alternative, named: `rendered?.doc ?? document` without
    // the salvage term, which would label and refuse the way OUT of a
    // salvage — where the text is the FILE's and no render speaks for it.
    expect(
      appliedFromDocument({ salvaged: true, document: changed, rendered: { doc: opened } }),
    ).toBe(changed);
    expect(appliedFromDocument({ salvaged: true, document: changed, rendered: null })).toBe(changed);
  });

  it("presents nothing when there is no document to present", () => {
    expect(appliedFromDocument({ salvaged: false, document: null, rendered: null })).toBeNull();
  });
});
