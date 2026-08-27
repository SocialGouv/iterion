// @vitest-environment jsdom
//
// The invariant: if the URL names a document, ITS tab is the one on screen.
//
// Reported from a real session — clicking "Open this draft in the editor" put
// the operator on a DIFFERENT, older Draft tab, which showed its own
// "couldn't reload" state. The link read as broken when it had worked: the
// right tab existed, it just was not the active one.
//
// The cause is ordering. `openTab` activates what it opens, but the tabs store
// is persisted, and rehydration of `activeEditorTabId` can land after the URL
// effect and restore the previously-active tab over it. These tests pin the
// re-assert that makes the effect self-correcting.
import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { useTabsStore } from "@/store/tabs";

import EditorTabsView from "./EditorTabsView";

// The panes are heavy (React Flow, Monaco) and irrelevant: these tests are
// about WHICH tab is active, which the store answers directly.
vi.mock("@/components/shared/EditorTabHost", () => ({
  default: ({ tabId }: { tabId: string }) => (
    <div data-testid={`host-${tabId}`} />
  ),
}));
vi.mock("@/components/Home/RecentFilesPanel", () => ({
  default: () => <div />,
}));

let search = "";
const loc = vi.hoisted(() => ({ setLocation: vi.fn() }));
vi.mock("wouter", () => ({
  useSearch: () => search,
  useLocation: () => ["/editor", loc.setLocation],
  Link: ({ children }: { children: React.ReactNode }) => <span>{children}</span>,
}));

function tabsSnapshot() {
  const s = useTabsStore.getState();
  return {
    active: s.tabs.find((t) => t.id === s.activeEditorTabId)?.params ?? null,
    count: s.tabs.length,
  };
}

beforeEach(() => {
  useTabsStore.setState({ tabs: [], activeEditorTabId: null, activeRunTabId: null });
  search = "";
  loc.setLocation.mockClear();
});

afterEach(cleanup);

describe("the URL decides which editor tab is on screen", () => {
  it("activates the draft tab the URL names", () => {
    search = "?draft=run-new";
    render(<EditorTabsView />);
    expect(tabsSnapshot().active).toEqual({ draft: "run-new" });
  });

  it("takes the URL's draft over a stale tab left active by persistence", () => {
    // Exactly the reported shape: an older draft tab is the persisted active
    // one, and it points at a run with nothing to open.
    const staleId = useTabsStore
      .getState()
      .openTab("editor", { draft: "run-old" }, "Draft");
    expect(useTabsStore.getState().activeEditorTabId).toBe(staleId);

    search = "?draft=run-new";
    render(<EditorTabsView />);

    expect(tabsSnapshot().active).toEqual({ draft: "run-new" });
    // The old tab is kept — the operator may still want it — just not shown.
    expect(tabsSnapshot().count).toBe(2);
  });

  it("does not open a second tab for a draft already open", () => {
    useTabsStore.getState().openTab("editor", { draft: "run-new" }, "Draft");
    search = "?draft=run-new";
    render(<EditorTabsView />);
    expect(tabsSnapshot().count).toBe(1);
    expect(tabsSnapshot().active).toEqual({ draft: "run-new" });
  });

  it("gives a file in the URL the same guarantee", () => {
    useTabsStore.getState().openTab("editor", { draft: "run-old" }, "Draft");
    search = "?file=bots/demo/main.bot";
    render(<EditorTabsView />);
    expect(tabsSnapshot().active).toEqual({ file: "bots/demo/main.bot" });
  });

  it("lets a file win over a draft when the URL carries both", () => {
    search = "?file=bots/demo/main.bot&draft=run-new";
    render(<EditorTabsView />);
    expect(tabsSnapshot().active).toEqual({ file: "bots/demo/main.bot" });
    expect(screen.queryByText("Draft")).toBeNull();
  });

  // Saving a draft used to add `file` while leaving `draft` on the tab
  // AND on the URL. paramsEqual then missed `{draft, file}` vs `{draft}`
  // and the effect opened a second "Draft" tab that stole focus.
  it("does not spawn a second Draft tab when that draft is saved as a file", () => {
    search = "?draft=run-new";
    const { rerender } = render(<EditorTabsView />);
    const id = useTabsStore.getState().activeEditorTabId;
    expect(id).toBeTruthy();
    useTabsStore.getState().bindFile(id!, "bots/foo/main.bot");
    rerender(<EditorTabsView />);
    expect(tabsSnapshot().count).toBe(1);
    expect(useTabsStore.getState().tabs[0]?.params).toEqual({
      draft: "run-new",
      file: "bots/foo/main.bot",
    });
  });
});
