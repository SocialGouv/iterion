// @vitest-environment jsdom
import { beforeEach, describe, expect, it } from "vitest";

import { UNTITLED_TAB_LABEL, useTabsStore } from "./tabs";

const tabOf = (id: string) => useTabsStore.getState().tabs.find((t) => t.id === id);

beforeEach(() => {
  useTabsStore.setState({ tabs: [], activeEditorTabId: null, activeRunTabId: null });
});

describe("tabs store: unbindFile", () => {
  it("makes a file tab name nothing, and take the untitled label back", () => {
    const id = useTabsStore.getState().openTab("editor", { file: "bots/x/main.bot" }, "x");
    useTabsStore.getState().unbindFile(id);
    expect(tabOf(id)?.params).toEqual({});
    expect(tabOf(id)?.label).toBe(UNTITLED_TAB_LABEL);
  });

  // A draft tab that was saved as a file names both; after File → New it
  // names neither — a kept draft would be re-applied on the next mount.
  it("drops the draft with the file", () => {
    const id = useTabsStore.getState().openTab("editor", { draft: "run-1" }, "Draft");
    useTabsStore.getState().bindFile(id, "bots/x/main.bot");
    expect(tabOf(id)?.params).toEqual({ draft: "run-1", file: "bots/x/main.bot" });
    useTabsStore.getState().unbindFile(id);
    expect(tabOf(id)?.params).toEqual({});
  });

  it("leaves the other tabs, a run tab and an unknown id alone", () => {
    const other = useTabsStore.getState().openTab("editor", { file: "bots/y/main.bot" }, "y");
    const run = useTabsStore.getState().openTab("run", { runId: "r1" });
    const before = useTabsStore.getState().tabs;
    useTabsStore.getState().unbindFile(run);
    useTabsStore.getState().unbindFile("nope");
    expect(useTabsStore.getState().tabs).toBe(before);
    expect(tabOf(other)?.params).toEqual({ file: "bots/y/main.bot" });
    expect(tabOf(run)?.params).toEqual({ runId: "r1" });
  });

  it("is a no-op on a tab that already names nothing", () => {
    const id = useTabsStore.getState().newEditorTab();
    const before = useTabsStore.getState().tabs;
    useTabsStore.getState().unbindFile(id);
    expect(useTabsStore.getState().tabs).toBe(before);
  });
});
