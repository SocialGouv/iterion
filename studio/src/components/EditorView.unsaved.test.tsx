// @vitest-environment jsdom
//
// The editor's `beforeunload` is the app's only browser-close warning. It
// asks the tab's document store — and, since #1755, the registry of buffers
// no document store owns: the bundle drawer's and the run file dialog's text
// must light it wherever in the app their surface currently is, including
// while none is mounted at all.
import { cleanup, render } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

let search = "";
const setLocation = vi.fn();
vi.mock("wouter", () => ({
  useSearch: () => search,
  useLocation: () => ["/editor", setLocation],
}));
// The panes are heavy (React Flow, Monaco) and irrelevant: this is about the
// unload handler, which the document store and the buffer registry answer.
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
import { bundleBufferKey, useEditBuffersStore } from "@/store/editBuffers";
import { createEmptyDocument } from "@/lib/defaults";

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

// Dispatch through the real window listener the effect registered: what is
// under test is the wiring, not a handler function of our own choosing.
function fireUnload(): boolean {
  const event = new Event("beforeunload", { cancelable: true });
  window.dispatchEvent(event);
  return event.defaultPrevented;
}

beforeEach(() => {
  search = "";
  setLocation.mockClear();
  useTabsStore.setState({ tabs: [], activeEditorTabId: null, activeRunTabId: null });
  useEditBuffersStore.setState({ bundle: {}, runFiles: {} });
});

afterEach(cleanup);

describe("the editor's unload warning", () => {
  it("stays quiet when nothing holds work", () => {
    const tabId = useTabsStore.getState().openTab("editor", { file: "bots/x/main.bot" }, "x");
    mount(tabId);
    expect(fireUnload()).toBe(false);
  });

  it("fires for the tab's own dirty document", () => {
    const tabId = useTabsStore.getState().openTab("editor", { file: "bots/x/main.bot" }, "x");
    const store = getOrCreateDocumentStore(tabId);
    store.getState().setDocument(createEmptyDocument());
    expect(store.getState().isDirty()).toBe(true);
    mount(tabId);
    expect(fireUnload()).toBe(true);
  });

  it("fires for a buffer no document store owns, though the tab itself is clean", () => {
    const tabId = useTabsStore.getState().openTab("editor", { file: "bots/x/main.bot" }, "x");
    mount(tabId);
    expect(getOrCreateDocumentStore(tabId).getState().hasUnsavedWork()).toBe(false);

    // The bundle drawer is not even mounted; its buffer is.
    useEditBuffersStore.getState().setBundle(bundleBufferKey("team-1", "demo"), {
      rel: "skills/notes.md",
      value: "# typed\n",
      original: "# notes\n",
      created: false,
    });

    expect(fireUnload()).toBe(true);
  });
});
