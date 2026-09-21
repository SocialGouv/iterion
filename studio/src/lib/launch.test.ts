import { describe, expect, it } from "vitest";

import { createEmptyDocument } from "@/lib/defaults";
import { createDocumentStore } from "@/store/document";

import { launchRefusal, pristineBuffer } from "./launch";

// The real store: its scaffold document is what makes "document != null" a
// wrong reading of "there is something to launch", and its saved mark is
// what tells a fresh buffer from an edited one.
describe("launchRefusal", () => {
  it("refuses a fresh buffer — the scaffold is not a workflow to launch", () => {
    const store = createDocumentStore();
    expect(pristineBuffer(store.getState())).toBe(true);
    expect(launchRefusal(store.getState())).toMatch(/write or open a workflow first/i);
  });

  it("refuses the buffer File → New and Start blank leave: unbound, saved", () => {
    const store = createDocumentStore();
    store.getState().setDocument(createEmptyDocument());
    store.getState().setCurrentFilePath(null);
    store.getState().markSaved();
    expect(launchRefusal(store.getState())).toMatch(/write or open a workflow first/i);
  });

  it("launches an unbound buffer once it is edited — the inline launch, the only one cloud has", () => {
    const store = createDocumentStore();
    store.getState().setDocument(createEmptyDocument());
    expect(pristineBuffer(store.getState())).toBe(false);
    expect(launchRefusal(store.getState())).toBeNull();
  });

  it("launches a bound file that parses and validates", () => {
    const store = createDocumentStore();
    store.getState().setDocument(createEmptyDocument());
    store.getState().setCurrentFilePath("bots/x/main.bot");
    store.getState().markSaved();
    expect(launchRefusal(store.getState())).toBeNull();
  });

  // The launch view would open the file from disk and the server would fail
  // to parse it: a Run that leads to a failed launch reads as working.
  it("refuses a salvage, bound or not, naming the way out", () => {
    const bound = createDocumentStore();
    bound.getState().setDocument(createEmptyDocument());
    bound.getState().setCurrentFilePath("bots/x/main.bot");
    bound.getState().setSalvaged(true);
    bound.getState().markSaved();
    expect(launchRefusal(bound.getState())).toMatch(/did not parse/i);

    const imported = createDocumentStore();
    imported.getState().setDocument(createEmptyDocument());
    imported.getState().setSalvaged(true);
    expect(launchRefusal(imported.getState())).toMatch(/did not parse/i);
  });

  // The salvage flag is the certain fact: auto-validation rewrites the
  // diagnostics with the verdict on the salvaged DOCUMENT, which may be clean.
  it("refuses a salvage even when its diagnostics have been cleared", () => {
    const store = createDocumentStore();
    store.getState().setDocument(createEmptyDocument());
    store.getState().setCurrentFilePath("bots/x/main.bot");
    store.getState().setSalvaged(true);
    store.getState().setDiagnostics([], [], []);
    store.getState().markSaved();
    expect(launchRefusal(store.getState())).toMatch(/did not parse/i);
  });

  it("refuses a document with error diagnostics, and counts them", () => {
    const store = createDocumentStore();
    store.getState().setDocument(createEmptyDocument());
    store.getState().setCurrentFilePath("bots/x/main.bot");
    store.getState().setDiagnostics(["e1", "e2"], ["w1"]);
    store.getState().markSaved();
    expect(launchRefusal(store.getState())).toBe("Fix the 2 errors in the Diagnostics panel before launching");

    store.getState().setDiagnostics(["e1"], ["w1"]);
    expect(launchRefusal(store.getState())).toBe("Fix the 1 error in the Diagnostics panel before launching");
  });

  it("does not refuse on warnings alone", () => {
    const store = createDocumentStore();
    store.getState().setDocument(createEmptyDocument());
    store.getState().setCurrentFilePath("bots/x/main.bot");
    store.getState().setDiagnostics([], ["w1", "w2"]);
    store.getState().markSaved();
    expect(launchRefusal(store.getState())).toBeNull();
  });

  // The reasons are ordered from the most fundamental: a buffer with nothing
  // in it is told so before it is told about its errors.
  it("names the empty buffer before its diagnostics", () => {
    const store = createDocumentStore();
    store.getState().setDiagnostics(["e1"]);
    expect(launchRefusal(store.getState())).toMatch(/write or open a workflow first/i);
  });
});
