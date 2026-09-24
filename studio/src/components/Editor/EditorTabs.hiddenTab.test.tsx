// @vitest-environment jsdom
//
// Every hydrated editor tab stays mounted — the inactive ones are only
// hidden — so every tab renders its own Toolbar. A key pressed on the tab on
// screen must act on that tab alone: the hidden ones were undoing, saving
// and opening files too (#1788). Driven through the real chain the app
// renders: two EditorTabHosts, the tabs store naming the active one.
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

const api = vi.hoisted(() => ({
  saveFile: vi.fn(),
  openFile: vi.fn(),
  listFiles: vi.fn(),
  listExampleEntries: vi.fn(),
  parseBotSourceEditorPath: () => null,
  inferCatalogBotId: () => null,
  botSourceEditorPath: () => "",
}));
vi.mock("@/api/client", async (importOriginal) => ({
  ...(await importOriginal<typeof import("@/api/client")>()),
  ...api,
}));
vi.mock("wouter", () => ({
  useLocation: () => ["/editor", vi.fn()],
  useSearch: () => "",
  Link: ({ children }: { children: React.ReactNode }) => <span>{children}</span>,
}));
vi.mock("@/auth/AuthContext", () => ({ useAuth: () => ({ activeTeamID: "t" }), useMaybeAuth: () => null }));
// The panes around the Toolbar are heavy (React Flow, Monaco) and not what
// this is about.
vi.mock("@/components/Canvas/Canvas", () => ({ default: () => <div /> }));
vi.mock("@/components/Inspector/Inspector", () => ({ default: () => <div /> }));
vi.mock("@/components/Diagnostics/DiagnosticsPanel", () => ({ default: () => <div /> }));
vi.mock("@/components/Library/LibraryPanel", () => ({ default: () => <div /> }));
vi.mock("@/components/Canvas/SubNodePalette", () => ({ default: () => <div /> }));
vi.mock("@/components/SourceView/SourceView", () => ({ default: () => <div /> }));
vi.mock("@/components/Editor/BundleFilesDrawer", () => ({ default: () => null }));
vi.mock("@/hooks/useAutoValidation", () => ({ useAutoValidation: () => {} }));
vi.mock("@/hooks/useAutoOpenDiagnosticsOnError", () => ({ useAutoOpenDiagnosticsOnError: () => {} }));
vi.mock("@/hooks/useFileWatcher", () => ({ useFileWatcher: () => {} }));
vi.mock("@/lib/chatDock/pageContext", () => ({ useAssistantPageContext: () => {} }));
vi.mock("@/components/shared/ShortcutsHelp", () => ({
  default: ({ open }: { open: boolean }) => (open ? <div data-testid="shortcuts-help" /> : null),
}));
vi.mock("@/components/ui", async (importOriginal) => ({
  ...(await importOriginal<typeof import("@/components/ui")>()),
  DesktopOnlyNotice: ({ children }: { children: React.ReactNode }) => <>{children}</>,
}));

import { createEmptyDocument } from "@/lib/defaults";
import { getOrCreateDocumentStore, type DocumentStore } from "@/store/document";
import { useRecentsStore } from "@/store/recents";
import { useTabsStore } from "@/store/tabs";
import { useUIStore } from "@/store/ui";
import EditorTabHost from "@/components/shared/EditorTabHost";

const marks = (s: DocumentStore) => (s.getState().document?.comments ?? []).map((c) => c.text).join(",");

// A tab on `file` with one unsaved edit, as EditorTabsView would host it.
function openTab(file: string, m: string) {
  const id = useTabsStore.getState().openTab("editor", { file }, m);
  const store = getOrCreateDocumentStore(id);
  store.getState().setDocument({ ...createEmptyDocument(), comments: [{ text: m }] });
  store.getState().setCurrentFilePath(file);
  store.getState().setCurrentSource(`${m}\n`);
  store.getState().markSaved();
  store.getState().addComment({ text: `${m}-edit` });
  return { id, store, file };
}

async function mountBoth() {
  const a = openTab("bots/a.bot", "A");
  const b = openTab("bots/b.bot", "B");
  useTabsStore.getState().setActive(a.id);
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(
    <QueryClientProvider client={qc}>
      <div data-testid="tab-a">
        <EditorTabHost tabId={a.id} file={a.file} />
      </div>
      <div data-testid="tab-b" className="hidden" aria-hidden>
        <EditorTabHost tabId={b.id} file={b.file} />
      </div>
    </QueryClientProvider>,
  );
  // EditorView is loaded lazily: wait until both tabs show their Toolbar.
  await waitFor(() => {
    for (const id of ["tab-a", "tab-b"]) {
      expect(screen.getByTestId(id).querySelector('input[type="file"]')).not.toBeNull();
    }
  });
  return { a, b };
}

// Where a key press lands when nothing inside a tab has the focus; it
// bubbles to the window listeners.
const key = (k: string, extra: Partial<KeyboardEventInit> = {}) =>
  act(() => {
    fireEvent.keyDown(document.body, { key: k, ctrlKey: true, ...extra });
  });

beforeEach(() => {
  vi.resetAllMocks();
  window.localStorage.clear();
  useTabsStore.setState({ tabs: [], activeEditorTabId: null, activeRunTabId: null });
  useUIStore.setState({ toasts: [], filePickerOpen: false });
  api.saveFile.mockImplementation(async (path: string) => ({ path, source: `${path} saved\n` }));
  api.listFiles.mockResolvedValue([]);
  api.listExampleEntries.mockResolvedValue([]);
});
afterEach(cleanup);

describe("a shortcut pressed with two editor tabs open", () => {
  it("undoes and redoes in the tab on screen only", async () => {
    const { a, b } = await mountBoth();
    key("z");
    expect(marks(a.store)).toBe("A");
    expect(marks(b.store)).toBe("B,B-edit");
    key("z", { shiftKey: true });
    expect(marks(a.store)).toBe("A,A-edit");
    key("z");
    key("y");
    expect(marks(a.store)).toBe("A,A-edit");
    expect(marks(b.store)).toBe("B,B-edit");
  });

  it("saves the tab on screen only", async () => {
    const { b } = await mountBoth();
    key("s");
    await waitFor(() => expect(api.saveFile).toHaveBeenCalledTimes(1));
    expect(api.saveFile.mock.calls[0]?.[0]).toBe("bots/a.bot");
    expect(b.store.getState().isDirty()).toBe(true);
  });

  it("opens the file chooser of the tab on screen only", async () => {
    await mountBoth();
    const clicked: Element[] = [];
    const click = vi.spyOn(HTMLInputElement.prototype, "click").mockImplementation(function (this: HTMLInputElement) {
      clicked.push(this);
    });
    key("o");
    click.mockRestore();
    expect(clicked).toHaveLength(1);
    expect(screen.getByTestId("tab-a").contains(clicked[0] ?? null)).toBe(true);
  });

  it("shows one shortcuts help, the one of the tab on screen", async () => {
    await mountBoth();
    act(() => {
      fireEvent.keyDown(document.body, { key: "?" });
    });
    const shown = screen.getAllByTestId("shortcuts-help");
    expect(shown).toHaveLength(1);
    expect(screen.getByTestId("tab-a").contains(shown[0] ?? null)).toBe(true);
  });

  it("follows the tab brought on screen", async () => {
    const { a, b } = await mountBoth();
    act(() => useTabsStore.getState().setActive(b.id));
    key("z");
    expect(marks(b.store)).toBe("B");
    expect(marks(a.store)).toBe("A,A-edit");
  });
});

describe("leaving the page with two editor tabs open", () => {
  // The one thing a hidden tab must still answer: its unsaved work is lost
  // with the page as surely as the visible tab's.
  it("is warned about by a hidden tab that holds unsaved work", async () => {
    const { a } = await mountBoth();
    act(() => a.store.getState().markSaved());
    const event = new Event("beforeunload", { cancelable: true });
    act(() => {
      window.dispatchEvent(event);
    });
    expect(event.defaultPrevented).toBe(true);
  });
});

describe("the file picker with two editor tabs open", () => {
  it("opens once, and a pick lands in the tab on screen", async () => {
    useRecentsStore.setState({ recents: ["bots/r.bot"] } as never);
    api.openFile.mockImplementation(async (path: string) => ({
      source: "R\n",
      document: { ...createEmptyDocument(), comments: [{ text: "R" }] },
      diagnostics: [],
      path,
    }));
    const { a, b } = await mountBoth();
    // Nothing to lose: the pick opens without a discard prompt.
    act(() => {
      a.store.getState().markSaved();
      b.store.getState().markSaved();
    });
    act(() => useUIStore.getState().setFilePickerOpen(true));
    await waitFor(() => expect(document.querySelectorAll('[role="dialog"]').length).toBeGreaterThan(0));
    const dialogs = Array.from(document.querySelectorAll('[role="dialog"]')) as HTMLElement[];
    expect(dialogs).toHaveLength(1);
    const dialog = dialogs[0];
    if (!dialog) throw new Error("no picker");
    fireEvent.click(within(dialog).getByText("bots/r.bot"));
    await waitFor(() => expect(a.store.getState().currentFilePath).toBe("bots/r.bot"));
    expect(b.store.getState().currentFilePath).toBe("bots/b.bot");
  });
});

describe("a Toolbar outside any editor tab", () => {
  it("answers no shortcut", async () => {
    const { DocumentStoreProvider } = await import("@/store/document");
    const { default: Toolbar } = await import("@/components/Toolbar/Toolbar");
    const store = getOrCreateDocumentStore("outside");
    store.getState().setDocument({ ...createEmptyDocument(), comments: [{ text: "O" }] });
    store.getState().addComment({ text: "O-edit" });
    const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(
      <QueryClientProvider client={qc}>
        <DocumentStoreProvider store={store}>
          <Toolbar />
        </DocumentStoreProvider>
      </QueryClientProvider>,
    );
    key("z");
    expect(marks(store)).toBe("O,O-edit");
  });
});
