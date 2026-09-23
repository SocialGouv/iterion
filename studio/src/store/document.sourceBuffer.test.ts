import { describe, expect, it } from "vitest";

import { createEmptyDocument } from "@/lib/defaults";
import { createDocumentStore } from "./document";

// The Source view's text buffer used to be component-local `useState`:
// nothing outside could see it, `_generation` did not move while an author
// typed there, and every discard path took the text with no prompt (#1662).
// It is store state now, and these are the questions the rest of the studio
// asks of it.

function store() {
  const s = createDocumentStore();
  s.getState().setDocument(createEmptyDocument());
  s.getState().setCurrentFilePath("bots/demo/main.bot");
  s.getState().markSaved();
  return s;
}

const buffer = (text: string, base: string) => ({
  path: "bots/demo/main.bot",
  rel: null,
  text,
  base,
});

describe("the Source view's buffer, seen from outside", () => {
  it("is not dirty while it holds what its render produced", () => {
    const s = store();
    s.getState().setSourceBuffer(buffer("same", "same"));
    expect(s.getState().isSourceDirty()).toBe(false);
    expect(s.getState().hasUnsavedWork()).toBe(false);
  });

  it("is unsaved work as soon as the author types, with no document change", () => {
    const s = store();
    s.getState().setSourceBuffer(buffer("typed", "rendered"));
    // The document has not moved — this is exactly the window in which
    // `isDirty()` reported false and every prompt stayed silent.
    expect(s.getState().isDirty()).toBe(false);
    expect(s.getState().isSourceDirty()).toBe(true);
    expect(s.getState().hasUnsavedWork()).toBe(true);
  });

  it("still reports unsaved work when only the document moved", () => {
    const s = store();
    s.getState().addAgent({
      name: "a",
      model: "m",
      input: "in",
      output: "out",
      system: "s",
      user: "u",
      session: "fresh",
    });
    expect(s.getState().isSourceDirty()).toBe(false);
    expect(s.getState().hasUnsavedWork()).toBe(true);
  });

  it("holds nothing once the buffer is closed", () => {
    const s = store();
    s.getState().setSourceBuffer(buffer("typed", "rendered"));
    s.getState().setSourceBuffer(null);
    expect(s.getState().isSourceDirty()).toBe(false);
    expect(s.getState().hasUnsavedWork()).toBe(false);
  });

  it("is dropped with the file it belonged to", () => {
    const s = store();
    s.getState().setSourceBuffer(buffer("typed", "rendered"));
    s.getState().setCurrentFilePath("bots/other/main.bot");
    // A buffer surviving the file it was about would make the next tab
    // refuse a reload for text that is no longer on screen.
    expect(s.getState().sourceBuffer).toBeNull();
    expect(s.getState().hasUnsavedWork()).toBe(false);
  });
});
