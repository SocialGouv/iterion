import { describe, expect, it } from "vitest";

import { touchesOpenUnit, unitAwaitsFragment } from "./unitFiles";

const unit = {
  root: "bots/demo",
  main: "main.bot",
  revision: "r1",
  files: [{ rel: "main.bot" }, { rel: "lib/nodes.bot" }, { rel: "lib/deep/schemas.bot" }],
};

// An edit to a fragment of the open unit is an edit to the open document:
// the merged canvas and the revision a save presents follow it, instead of
// drifting until a 409 at save time.
describe("touchesOpenUnit", () => {
  it("matches the open file, and every fragment of its unit under the root", () => {
    expect(touchesOpenUnit("bots/demo/main.bot", "bots/demo/main.bot", unit)).toBe(true);
    expect(touchesOpenUnit("bots/demo/lib/nodes.bot", "bots/demo/main.bot", unit)).toBe(true);
    expect(touchesOpenUnit("bots/demo/lib/deep/schemas.bot", "bots/demo/main.bot", unit)).toBe(true);
  });

  it("ignores another bot's files, a fragment the unit does not reach, and a closed tab", () => {
    expect(touchesOpenUnit("bots/other/lib/nodes.bot", "bots/demo/main.bot", unit)).toBe(false);
    expect(touchesOpenUnit("bots/demo/lib/unused.bot", "bots/demo/main.bot", unit)).toBe(false);
    expect(touchesOpenUnit("bots/demo/lib/nodes.bot", null, unit)).toBe(false);
    expect(touchesOpenUnit("bots/demo/lib/nodes.bot", "bots/demo/main.bot", null)).toBe(false);
  });

  it("names a cloud bundle's files from its root", () => {
    const cloud = { ...unit, root: "" };
    expect(touchesOpenUnit("lib/nodes.bot", "main.bot", cloud)).toBe(true);
    expect(touchesOpenUnit("bots/demo/lib/nodes.bot", "main.bot", cloud)).toBe(false);
  });
});

// The fragment an `import` names and the load could not read is a file of NO
// list — the loader stops at the failed read — while the gate refuses on that
// load (#1532). Membership alone therefore cannot recognise the one repair
// the refusal asks for: writing that fragment. The answer names it in
// `unread`, beside the verdict it is about: the import lines of the file list
// are no substitute — an apply that drops the broken `import` answers the
// STORED verdict over the STAGED list — and the resolution of an import is
// the loader's, asserted against it (pkg/dsl/unit
// TestTheLoadNamesTheFragmentsItCouldNotRead).
describe("unitAwaitsFragment", () => {
  const awaiting = {
    root: "bots/demo",
    main: "main.bot",
    revision: "r1",
    files: [{ rel: "main.bot", imports: ["lib/missing.bot", "lib/nodes.bot"] }, { rel: "lib/nodes.bot" }],
    unread: ["lib/missing.bot"],
    load_errors: 1,
    load_error: 'main.bot:1:1: error [E046]: import "lib/missing.bot": no such fragment (lib/missing.bot)',
  };

  it("matches the fragment the answer says the load could not read", () => {
    expect(unitAwaitsFragment("bots/demo/lib/missing.bot", awaiting)).toBe(true);
    // Several, each of them, however deep under the root.
    const two = { ...awaiting, unread: ["lib/deep/schemas.bot", "lib/missing.bot"] };
    expect(unitAwaitsFragment("bots/demo/lib/deep/schemas.bot", two)).toBe(true);
    expect(unitAwaitsFragment("bots/demo/lib/missing.bot", two)).toBe(true);
  });

  it("leaves alone a file the answer does not name, however close its path", () => {
    // A file written beside the bot, awaited by nothing: reading the main
    // again on it would reload the tab on every write under the bot's
    // directory.
    expect(unitAwaitsFragment("bots/demo/lib/unused.bot", awaiting)).toBe(false);
    // Another bot's fragment, of the same name as the one awaited.
    expect(unitAwaitsFragment("bots/other/lib/missing.bot", awaiting)).toBe(false);
    // A path the awaited name is a prefix of.
    expect(unitAwaitsFragment("bots/demo/lib/missing.bot.bak", awaiting)).toBe(false);
    // A file the unit HOLDS: membership's answer, not this one's.
    expect(unitAwaitsFragment("bots/demo/lib/nodes.bot", awaiting)).toBe(false);
  });

  it("leaves alone a file an import NAMES and the answer does not await", () => {
    // The load that refuses an import by its PATH — one outside the bot's
    // `lib/` — never read the file, so it is in no list AND in no `unread`:
    // writing it lifts nothing, and the directory beside a main is where
    // another bot's main lives. A predicate re-deriving the awaited name
    // from `imports` reloads this tab on every write to that neighbour.
    // The answer is the server's, verbatim (pkg/server
    // TestAnImportTheLoaderRefusesByPathIsAwaitedByNothing).
    const refusedByPath = {
      ...awaiting,
      files: [{ rel: "main.bot", imports: ["other.bot"] }],
      unread: undefined,
      load_error:
        "main.bot:1:1: error [E045]: import \"other.bot\" resolves to other.bot, outside the bot's `lib/` directory where every fragment lives",
    };
    expect(unitAwaitsFragment("bots/demo/other.bot", refusedByPath)).toBe(false);
    expect(unitAwaitsFragment("other.bot", refusedByPath)).toBe(false);
  });

  it("awaits nothing for a closed tab, or a unit whose imports all resolved", () => {
    expect(unitAwaitsFragment("bots/demo/lib/missing.bot", null)).toBe(false);
    // `unread` absent reads as "awaits nothing", which is what the server
    // omits for a unit that loads.
    expect(unitAwaitsFragment("bots/demo/lib/nodes.bot", unit)).toBe(false);
    expect(unitAwaitsFragment("bots/demo/lib/missing.bot", { ...awaiting, unread: [] })).toBe(false);
  });

  it("names a cloud bundle's awaited fragment from its root", () => {
    expect(unitAwaitsFragment("lib/missing.bot", { ...awaiting, root: "" })).toBe(true);
    expect(unitAwaitsFragment("bots/demo/lib/missing.bot", { ...awaiting, root: "" })).toBe(false);
  });
});
