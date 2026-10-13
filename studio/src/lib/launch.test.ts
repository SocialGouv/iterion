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

  // The hole #1532 is about: the open answers `bindable: true` (the MAIN
  // parsed) and the fragment's parse errors as diagnostics, then
  // auto-validation rewrites those diagnostics with the verdict on the
  // MERGED document — built from what parsed, and clean. The unit's
  // per-file verdict is the fact that survives it.
  it("refuses a unit whose fragment does not parse, naming the file and counting its errors", () => {
    const store = createDocumentStore();
    store.getState().setDocument(createEmptyDocument());
    store.getState().setCurrentFilePath("bots/feature-dev/main.bot");
    store.getState().setUnit({
      root: "bots/feature-dev",
      main: "main.bot",
      revision: "r1",
      files: [
        { rel: "main.bot" },
        {
          rel: "lib/nodes.bot",
          parse_errors: 2,
          parse_error: "lib/nodes.bot:202:9: error [E002]: expected :, got String",
        },
      ],
    });
    // The open answers those same errors as diagnostics: the unit's
    // verdict is the reason shown, because it is the one that names the
    // file to repair.
    store.getState().setDiagnostics(
      ["lib/nodes.bot:202:9: error [E002]: expected :, got String", "lib/nodes.bot:202:54: error [E002]: expected string literal, got Newline"],
      [],
      [],
    );
    store.getState().markSaved();
    const refusal = launchRefusal(store.getState());
    expect(refusal).not.toBeNull();
    expect(refusal).toMatch(/lib\/nodes\.bot/);
    expect(refusal).toMatch(/\b2 errors\b/);
    expect(refusal).toContain("lib/nodes.bot:202:9: error [E002]: expected :, got String");
    expect(refusal).not.toMatch(/Diagnostics panel/);

    // The rewrite itself: the diagnostics are replaced by the merged
    // document's clean verdict, and the refusal holds.
    store.getState().setDiagnostics([], ["w1"], []);
    expect(launchRefusal(store.getState())).toBe(refusal);
  });

  // A count with no text — a server that answers the count alone: the
  // refusal still names the file, in the singular, with no empty bracket.
  it("refuses on the count alone when the answer carries no error text", () => {
    const store = createDocumentStore();
    store.getState().setDocument(createEmptyDocument());
    store.getState().setCurrentFilePath("bots/feature-dev/main.bot");
    store.getState().setUnit({
      root: "bots/feature-dev",
      main: "main.bot",
      revision: "r1",
      files: [{ rel: "main.bot" }, { rel: "lib/nodes.bot", parse_errors: 1 }],
    });
    store.getState().markSaved();
    expect(launchRefusal(store.getState())).toBe(
      "Fix the 1 error in lib/nodes.bot before launching: a bot in several files is launched from its files as they are stored, and that file does not parse.",
    );
  });

  // Several broken files, and the SAME message whichever order the answer
  // lists them in: the files are named in path order, so the error the
  // refusal quotes does not depend on which file the loader reached first.
  it("names every file of the unit that does not parse, counts them all, in path order", () => {
    const files = [
      { rel: "main.bot" },
      { rel: "lib/a.bot", parse_errors: 2, parse_error: "lib/a.bot:1:1: error [E002]: a" },
      { rel: "lib/b.bot", parse_errors: 1, parse_error: "lib/b.bot:1:1: error [E002]: b" },
    ];
    const refusalFor = (listed: typeof files) => {
      const store = createDocumentStore();
      store.getState().setDocument(createEmptyDocument());
      store.getState().setCurrentFilePath("bots/feature-dev/main.bot");
      store.getState().setUnit({ root: "bots/feature-dev", main: "main.bot", revision: "r1", files: listed });
      store.getState().markSaved();
      return launchRefusal(store.getState());
    };

    // The whole sentence, so the plural branch is asserted too: with the
    // parts alone, collapsing it to the singular left the suite green.
    expect(refusalFor(files)).toBe(
      "Fix the 3 errors in lib/a.bot, lib/b.bot before launching: a bot in several files is launched " +
        "from its files as they are stored, and those files do not parse (lib/a.bot:1:1: error [E002]: a).",
    );
    expect(refusalFor([...files].reverse())).toBe(refusalFor(files));
  });

  // Every file parses and the bot still does not load as ONE: an `import`
  // naming no fragment, a cycle, a declaration made in two files. The load
  // verdict holds those on top of every file's own parse errors, and the
  // merged document — built from what loaded — validates clean. The launch
  // refuses on that very list, so the gate does too.
  it("refuses a unit whose files do not load as one bot, quoting the load error", () => {
    const store = createDocumentStore();
    store.getState().setDocument(createEmptyDocument());
    store.getState().setCurrentFilePath("bots/feature-dev/main.bot");
    store.getState().setUnit({
      root: "bots/feature-dev",
      main: "main.bot",
      revision: "r1",
      files: [{ rel: "main.bot" }, { rel: "lib/a.bot" }],
      load_errors: 1,
      load_error: 'lib/a.bot:1:1: error [E047]: import cycle: main.bot → lib/a.bot → lib/b.bot → lib/a.bot',
    });
    store.getState().markSaved();
    const refusal = launchRefusal(store.getState());
    expect(refusal).toBe(
      "Fix the 1 error in this bot's files before launching: a bot in several files is launched from " +
        "its files as they are stored, and they do not load as one bot " +
        "(lib/a.bot:1:1: error [E047]: import cycle: main.bot → lib/a.bot → lib/b.bot → lib/a.bot).",
    );
    // And it survives the rewrite that takes the diagnostics away: that is
    // the whole reason the fact lives on the unit.
    store.getState().setDiagnostics([], ["w1"], []);
    expect(launchRefusal(store.getState())).toBe(refusal);
  });

  // The load sentence's other two shapes, whole: the plural count, and a
  // server that answers the count with no text — no empty bracket. Asserted
  // because the per-file sentence's plural branch had collapsed to the
  // singular under its parts-only assertions and the suite stayed green.
  it("counts the load errors in the plural, and brackets nothing when no text came with them", () => {
    const store = createDocumentStore();
    store.getState().setDocument(createEmptyDocument());
    store.getState().setCurrentFilePath("bots/feature-dev/main.bot");
    store.getState().setUnit({
      root: "bots/feature-dev",
      main: "main.bot",
      revision: "r1",
      files: [{ rel: "main.bot" }, { rel: "lib/a.bot" }],
      load_errors: 2,
    });
    store.getState().markSaved();
    expect(launchRefusal(store.getState())).toBe(
      "Fix the 2 errors in this bot's files before launching: a bot in several files is launched from " +
        "its files as they are stored, and they do not load as one bot.",
    );
  });

  // A file that does not parse makes BOTH verdicts true. The per-file one is
  // shown: it names the file to repair.
  it("names the broken file rather than the load when both refuse", () => {
    const store = createDocumentStore();
    store.getState().setDocument(createEmptyDocument());
    store.getState().setCurrentFilePath("bots/feature-dev/main.bot");
    store.getState().setUnit({
      root: "bots/feature-dev",
      main: "main.bot",
      revision: "r1",
      files: [{ rel: "main.bot" }, { rel: "lib/a.bot", parse_errors: 1, parse_error: "lib/a.bot:1:1: error [E002]: a" }],
      load_errors: 1,
      load_error: "lib/a.bot:1:1: error [E002]: a",
    });
    store.getState().markSaved();
    expect(launchRefusal(store.getState())).toContain("Fix the 1 error in lib/a.bot before launching");
  });

  // A unit every file of which parses, and which loads, launches: the
  // verdict refuses the broken fragment, not the shape.
  it("launches a unit whose every file parses and which loads", () => {
    const store = createDocumentStore();
    store.getState().setDocument(createEmptyDocument());
    store.getState().setCurrentFilePath("bots/feature-dev/main.bot");
    store.getState().setUnit({
      root: "bots/feature-dev",
      main: "main.bot",
      revision: "r1",
      files: [{ rel: "main.bot", parse_errors: 0 }, { rel: "lib/nodes.bot" }],
      load_errors: 0,
    });
    store.getState().markSaved();
    expect(launchRefusal(store.getState())).toBeNull();
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
