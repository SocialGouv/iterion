// @vitest-environment jsdom
//
// A file-scoped deep link — /editor?file=P&node=n&from=r, "Open in editor"
// from the run console — is consumed by the visible tab that was opened FOR
// P: the key EditorTabsView matched the URL against is `tab.params.file`,
// and this view has to read the same one. Compared against the document's
// binding instead, the link was swallowed whenever the two differed: the
// node never focused, the banner never showed (#1326).
import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

let search = "";
const setLocation = vi.fn();
vi.mock("wouter", () => ({
  useSearch: () => search,
  useLocation: () => ["/editor", setLocation],
}));
// The panes are heavy (React Flow, Monaco) and irrelevant: this is about the
// effect that reads the URL, which the selection store and the banner answer.
vi.mock("@/components/Canvas/Canvas", () => ({ default: () => <div /> }));
vi.mock("@/components/Inspector/Inspector", () => ({ default: () => <div /> }));
vi.mock("@/components/Toolbar/Toolbar", () => ({ default: () => <div /> }));
vi.mock("@/components/Diagnostics/DiagnosticsPanel", () => ({ default: () => <div /> }));
vi.mock("@/components/Library/LibraryPanel", () => ({ default: () => <div /> }));
vi.mock("@/components/Canvas/SubNodePalette", () => ({ default: () => <div /> }));
vi.mock("@/components/SourceView/SourceView", () => ({ default: () => <div /> }));
vi.mock("@/hooks/useAutoValidation", () => ({ useAutoValidation: () => {} }));
vi.mock("@/hooks/useAutoOpenDiagnosticsOnError", () => ({ useAutoOpenDiagnosticsOnError: () => {} }));
vi.mock("@/hooks/useFileWatcher", () => ({ useFileWatcher: () => {} }));
vi.mock("@/lib/chatDock/pageContext", () => ({ useAssistantPageContext: () => {} }));
vi.mock("@/components/ui", async (importOriginal) => ({
  ...(await importOriginal<typeof import("@/components/ui")>()),
  DesktopOnlyNotice: ({ children }: { children: React.ReactNode }) => <>{children}</>,
}));

import { DocumentStoreProvider, getOrCreateDocumentStore } from "@/store/document";
import { SelectionStoreProvider, getOrCreateSelectionStore } from "@/store/selection";
import { useTabsStore } from "@/store/tabs";

import EditorView from "./EditorView";

function mount(tabId: string) {
  return render(
    <DocumentStoreProvider store={getOrCreateDocumentStore(tabId)}>
      <SelectionStoreProvider store={getOrCreateSelectionStore(tabId)}>
        <EditorView active />
      </SelectionStoreProvider>
    </DocumentStoreProvider>,
  );
}

const selectedNodeOf = (tabId: string) => getOrCreateSelectionStore(tabId).getState().selectedNodeId;

beforeEach(() => {
  search = "";
  setLocation.mockClear();
  useTabsStore.setState({ tabs: [], activeEditorTabId: null, activeRunTabId: null });
});

afterEach(cleanup);

describe("the editor deep link", () => {
  it("is consumed by the tab opened for the requested file, whatever its document is bound to", () => {
    const tabId = useTabsStore.getState().openTab("editor", { file: "bots/x/main.bot" }, "x");
    // The tab's document is bound to nothing — not the file the tab was
    // opened for.
    expect(getOrCreateDocumentStore(tabId).getState().currentFilePath).toBeNull();
    search = "?file=bots%2Fx%2Fmain.bot&node=agent_1&from=run-9";

    mount(tabId);

    expect(selectedNodeOf(tabId)).toBe("agent_1");
    expect(screen.getByText(/Opened from run/)).toBeTruthy();
  });

  it("is left alone by a tab opened for another file", () => {
    const tabId = useTabsStore.getState().openTab("editor", { file: "bots/y/main.bot" }, "y");
    search = "?file=bots%2Fx%2Fmain.bot&node=agent_1&from=run-9";

    mount(tabId);

    expect(selectedNodeOf(tabId)).toBeNull();
    expect(screen.queryByText(/Opened from run/)).toBeNull();
  });

  it("is consumed by the visible tab when it names no file", () => {
    const tabId = useTabsStore.getState().openTab("editor", { file: "bots/x/main.bot" }, "x");
    search = "?node=agent_1";

    mount(tabId);

    expect(selectedNodeOf(tabId)).toBe("agent_1");
  });
});
