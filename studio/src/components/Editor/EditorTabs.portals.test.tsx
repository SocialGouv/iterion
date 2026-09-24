// @vitest-environment jsdom
//
// Every open editor tab stays mounted — the inactive ones only hidden — and
// Back / Forward can put another tab on screen while a dialog of the one on
// screen is open. What a hidden tab renders into the page body (its dialogs)
// and what it opens app-wide (the diagnostics panel) must wait for it to be
// shown again (#1788). Driven through the real EditorTabsView, the URL
// deciding which tab is on screen.
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
// wouter, reduced to what the route reads: a search string the test moves
// the way popstate (browser Back) would.
const loc = vi.hoisted(() => ({ search: "" }));
vi.mock("wouter", () => ({
  useSearch: () => loc.search,
  useLocation: () => [
    "/editor",
    (target: string) => {
      const q = target.indexOf("?");
      loc.search = q >= 0 ? target.slice(q) : "";
    },
  ],
  Link: ({ children }: { children: React.ReactNode }) => <span>{children}</span>,
}));
const ws = vi.hoisted(() => ({ subs: [] as ((e: unknown) => void)[] }));
vi.mock("@/api/ws", () => ({
  fileWatcher: {
    connect: () => {},
    disconnect: () => {},
    subscribe: (cb: (e: unknown) => void) => {
      ws.subs.push(cb);
      return () => {
        ws.subs = ws.subs.filter((s) => s !== cb);
      };
    },
  },
}));
vi.mock("@/auth/AuthContext", () => ({ useAuth: () => ({ activeTeamID: "t" }), useMaybeAuth: () => null }));
vi.mock("@/components/Canvas/Canvas", () => ({ default: () => <div /> }));
vi.mock("@/components/Inspector/Inspector", () => ({ default: () => <div /> }));
vi.mock("@/components/Diagnostics/DiagnosticsPanel", () => ({ default: () => <div /> }));
vi.mock("@/components/Library/LibraryPanel", () => ({ default: () => <div /> }));
vi.mock("@/components/Canvas/SubNodePalette", () => ({ default: () => <div /> }));
vi.mock("@/components/SourceView/SourceView", () => ({ default: () => <div /> }));
vi.mock("@/components/Editor/BundleFilesDrawer", () => ({ default: () => null }));
vi.mock("@/components/Home/RecentFilesPanel", () => ({ default: () => <div /> }));
vi.mock("@/components/shared/InnerTabBar", () => ({ default: () => <div data-testid="strip" /> }));
vi.mock("@/hooks/useAutoValidation", () => ({ useAutoValidation: () => {} }));
vi.mock("@/lib/chatDock/pageContext", () => ({ useAssistantPageContext: () => {} }));
vi.mock("@/components/ui", async (importOriginal) => ({
  ...(await importOriginal<typeof import("@/components/ui")>()),
  DesktopOnlyNotice: ({ children }: { children: React.ReactNode }) => <>{children}</>,
}));

import { createEmptyDocument } from "@/lib/defaults";
import { getOrCreateDocumentStore, type DocumentStore } from "@/store/document";
import { useRecentsStore } from "@/store/recents";
import { useTabsStore } from "@/store/tabs";
import { useUIStore } from "@/store/ui";
import EditorTabsView from "@/components/Editor/EditorTabsView";

const marks = (s: DocumentStore) => (s.getState().document?.comments ?? []).map((c) => c.text).join(",");

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

const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
const tree = () => (
  <QueryClientProvider client={qc}>
    <EditorTabsView />
  </QueryClientProvider>
);

async function mountBoth() {
  const a = openTab("bots/a.bot", "A");
  const b = openTab("bots/b.bot", "B");
  loc.search = "?file=bots%2Fa.bot";
  const view = render(tree());
  await waitFor(() => expect(useTabsStore.getState().activeEditorTabId).toBe(a.id));
  // EditorView loads lazily: under a full run's load that takes longer than
  // waitFor's default second.
  await waitFor(() => expect(document.querySelectorAll('input[type="file"]').length).toBe(2), { timeout: 10_000 });
  return { a, b, view };
}

beforeEach(() => {
  vi.resetAllMocks();
  window.localStorage.clear();
  ws.subs = [];
  useTabsStore.setState({ tabs: [], activeEditorTabId: null, activeRunTabId: null });
  useUIStore.setState({ toasts: [], filePickerOpen: false, diagnosticsPanelOpen: false });
  api.saveFile.mockImplementation(async (path: string) => ({ path, source: `${path} saved\n` }));
  api.listFiles.mockResolvedValue([]);
  api.listExampleEntries.mockResolvedValue([]);
});
afterEach(cleanup);

describe("a dialog of the tab on screen, when Back puts another tab on screen", () => {
  it("leaves the screen with its tab, and comes back with it; nothing is discarded meanwhile", async () => {
    useRecentsStore.setState({ recents: ["bots/r.bot"] } as never);
    api.openFile.mockImplementation(async (path: string) => ({
      source: "R\n",
      document: { ...createEmptyDocument(), comments: [{ text: "R" }] },
      diagnostics: [],
      path,
    }));
    const { a, b, view } = await mountBoth();
    // Tab A (on screen) holds unsaved work; the picker's pick asks to discard it.
    act(() => useUIStore.getState().setFilePickerOpen(true));
    const picker = await screen.findByRole("dialog");
    fireEvent.click(within(picker).getByText("bots/r.bot"));
    await screen.findByText("Discard unsaved changes?");

    // Back: the URL names tab B's file, and B comes on screen.
    loc.search = "?file=bots%2Fb.bot";
    view.rerender(tree());
    await waitFor(() => expect(useTabsStore.getState().activeEditorTabId).toBe(b.id));
    await waitFor(() => expect(screen.queryByText("Discard unsaved changes?")).toBeNull());
    expect(marks(a.store)).toBe("A,A-edit");
    expect(marks(b.store)).toBe("B,B-edit");

    // Forward: A is on screen again, and so is its question.
    loc.search = "?file=bots%2Fa.bot";
    view.rerender(tree());
    await waitFor(() => expect(useTabsStore.getState().activeEditorTabId).toBe(a.id));
    const ask = await screen.findByText("Discard unsaved changes?");
    const dialog = ask.closest('[role="alertdialog"],[role="dialog"]') as HTMLElement;
    fireEvent.click(within(dialog).getByRole("button", { name: "Discard" }));
    await waitFor(() => expect(a.store.getState().currentFilePath).toBe("bots/r.bot"));
    expect(marks(b.store)).toBe("B,B-edit");
  });
});

describe("a tab whose validation turns red", () => {
  it("opens the diagnostics panel when it is the tab on screen", async () => {
    const { a } = await mountBoth();
    act(() => a.store.getState().setDiagnostics(["e1"]));
    await waitFor(() => expect(useUIStore.getState().diagnosticsPanelOpen).toBe(true));
  });

  it("waits while its tab is hidden, and opens it when the tab is shown", async () => {
    const { b, view } = await mountBoth();
    act(() => b.store.getState().setDiagnostics(["e1"]));
    await new Promise((r) => setTimeout(r, 50));
    expect(useUIStore.getState().diagnosticsPanelOpen).toBe(false);

    loc.search = "?file=bots%2Fb.bot";
    view.rerender(tree());
    await waitFor(() => expect(useTabsStore.getState().activeEditorTabId).toBe(b.id));
    await waitFor(() => expect(useUIStore.getState().diagnosticsPanelOpen).toBe(true));
  });
});
