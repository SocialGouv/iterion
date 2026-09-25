// @vitest-environment jsdom
import { beforeEach, describe, expect, it, vi } from "vitest";

import { selectEditorTabs, selectRunTabs, useTabsStore, type TabKind } from "./tabs";
import { disposeRunStore } from "./run";
import { disposeDocumentStore } from "./document";
import { disposeSelectionStore } from "./selection";

vi.mock("./run", () => ({ disposeRunStore: vi.fn() }));
vi.mock("./document", () => ({ disposeDocumentStore: vi.fn() }));
vi.mock("./selection", () => ({ disposeSelectionStore: vi.fn() }));

beforeEach(() => {
  vi.clearAllMocks();
  useTabsStore.setState({
    tabs: [], activeEditorTabId: null, activeRunTabId: null,
    currentProjectKey: null, runOpenNonce: {},
  });
});

function open(kind: TabKind, project: string, name: string) {
  const store = useTabsStore.getState();
  store.setCurrentProjectKey(project);
  return store.openTab(kind, kind === "editor" ? { file: `${name}.bot` } : { runId: name });
}

function active(kind: TabKind) {
  const state = useTabsStore.getState();
  return kind === "editor" ? state.activeEditorTabId : state.activeRunTabId;
}

describe.each(["editor", "run"] as const)("close %s tab within project scope", (kind) => {
  it("selects the previous visible tab across interleaved projects", () => {
    const b1 = open(kind, "B", "b1");
    const hidden = open(kind, "A", "a");
    const b2 = open(kind, "B", "b2");
    useTabsStore.getState().closeTab(b2);
    expect(active(kind)).toBe(b1);
    const state = useTabsStore.getState();
    expect((kind === "editor" ? selectEditorTabs : selectRunTabs)(state).map((t) => t.id)).toEqual([b1]);
    expect(state.tabs.map((t) => t.id)).toEqual([b1, hidden]);
    expect(disposeRunStore).toHaveBeenCalledTimes(kind === "run" ? 1 : 0);
    if (kind === "run") expect(disposeRunStore).toHaveBeenCalledWith("b2");
    else {
      expect(disposeDocumentStore).toHaveBeenCalledExactlyOnceWith(b2);
      expect(disposeSelectionStore).toHaveBeenCalledExactlyOnceWith(b2);
    }
  });

  it("selects the next visible tab when the first one closes", () => {
    const b1 = open(kind, "B", "b1");
    const hidden = open(kind, "A", "a");
    const b2 = open(kind, "B", "b2");
    useTabsStore.getState().setActive(b1);
    useTabsStore.getState().closeTab(b1);
    expect(active(kind)).toBe(b2);
    expect(useTabsStore.getState().tabs.map((t) => t.id)).toEqual([hidden, b2]);
  });

  it("uses the visible index when closing a middle tab", () => {
    const b1 = open(kind, "B", "b1");
    const hidden = open(kind, "A", "a");
    const b2 = open(kind, "B", "b2");
    const b3 = open(kind, "B", "b3");
    useTabsStore.getState().setActive(b2);
    useTabsStore.getState().closeTab(b2);
    expect(active(kind)).toBe(b1);
    expect(useTabsStore.getState().tabs.map((t) => t.id)).toEqual([b1, hidden, b3]);
  });

  it("clears only this kind when its last visible tab closes", () => {
    const hidden = open(kind, "A", "a");
    const visible = open(kind, "B", "b");
    const otherKind = kind === "editor" ? "run" : "editor";
    const other = open(otherKind, "B", "other");
    useTabsStore.getState().closeTab(visible);
    expect(active(kind)).toBeNull();
    expect(active(otherKind)).toBe(other);
    expect(useTabsStore.getState().tabs.some((t) => t.id === hidden)).toBe(true);
  });

  it("keeps the active tab when an inactive or unknown tab closes", () => {
    const hidden = open(kind, "A", "a");
    const b1 = open(kind, "B", "b1");
    const b2 = open(kind, "B", "b2");
    useTabsStore.getState().closeTab(hidden);
    useTabsStore.getState().closeTab(b1);
    useTabsStore.getState().closeTab("unknown");
    expect(active(kind)).toBe(b2);
  });

  it("preserves global neighbour selection when cloud scoping is off", () => {
    open(kind, "B", "b1");
    const a = open(kind, "A", "a");
    const b2 = open(kind, "B", "b2");
    useTabsStore.getState().setCurrentProjectKey(null);
    useTabsStore.getState().closeTab(b2);
    expect(active(kind)).toBe(a);
  });
});
