// @vitest-environment jsdom
//
// One editor tab, on screen, with its real Toolbar and its real Canvas. The
// canvas holds the focus and answers Ctrl+Z / Ctrl+Y itself; the Toolbar
// listens on the window. One key press is one step, not two (#1788).
import { act, cleanup, fireEvent, render, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { afterEach, beforeAll, beforeEach, describe, expect, it, vi } from "vitest";

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
vi.mock("@/components/icons/ProviderIcon", () => ({ ProviderIcon: () => null, ProviderLabel: () => null }));
vi.mock("@/components/icons/BackendBadge", () => ({
  effectiveBackend: (backend?: string, resolved?: string) => backend || resolved || "claw",
  BackendBadge: () => null,
}));
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
vi.mock("@/components/ui", async (importOriginal) => ({
  ...(await importOriginal<typeof import("@/components/ui")>()),
  DesktopOnlyNotice: ({ children }: { children: React.ReactNode }) => <>{children}</>,
}));

import { createEmptyDocument } from "@/lib/defaults";
import { getOrCreateDocumentStore, type DocumentStore } from "@/store/document";
import { useTabsStore } from "@/store/tabs";
import { useUIStore } from "@/store/ui";
import EditorTabHost from "@/components/shared/EditorTabHost";

beforeAll(() => {
  globalThis.ResizeObserver ??= class {
    observe() {}
    unobserve() {}
    disconnect() {}
  } as unknown as typeof ResizeObserver;
});

const marks = (s: DocumentStore) => (s.getState().document?.comments ?? []).map((c) => c.text).join(",");

beforeEach(() => {
  vi.resetAllMocks();
  window.localStorage.clear();
  useTabsStore.setState({ tabs: [], activeEditorTabId: null, activeRunTabId: null });
  useUIStore.setState({ toasts: [], filePickerOpen: false });
  api.listFiles.mockResolvedValue([]);
  api.listExampleEntries.mockResolvedValue([]);
});
afterEach(cleanup);

describe("Ctrl+Z and Ctrl+Y with the canvas focused", () => {
  it("take one step each", async () => {
    const id = useTabsStore.getState().openTab("editor", { file: "bots/a.bot" }, "A");
    useTabsStore.getState().setActive(id);
    const store = getOrCreateDocumentStore(id);
    store.getState().setDocument({ ...createEmptyDocument(), comments: [{ text: "A" }] });
    store.getState().setCurrentFilePath("bots/a.bot");
    store.getState().setCurrentSource("A\n");
    store.getState().markSaved();
    store.getState().addComment({ text: "e1" });
    store.getState().addComment({ text: "e2" });
    const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(
      <QueryClientProvider client={qc}>
        <EditorTabHost tabId={id} file="bots/a.bot" />
      </QueryClientProvider>,
    );
    // EditorView and React Flow load lazily: under a full run's load that takes
    // longer than waitFor's default second.
    await waitFor(() => expect(document.querySelector(".react-flow")).not.toBeNull(), { timeout: 10_000 });
    await waitFor(() => expect(document.querySelector('input[type="file"]')).not.toBeNull(), { timeout: 10_000 });
    const canvasRoot = document.querySelector(".react-flow")?.parentElement;
    if (!canvasRoot) throw new Error("no canvas");
    act(() => canvasRoot.focus());
    expect(document.activeElement).toBe(canvasRoot);

    act(() => {
      fireEvent.keyDown(canvasRoot, { key: "z", ctrlKey: true });
    });
    expect(marks(store)).toBe("A,e1");
    act(() => {
      fireEvent.keyDown(canvasRoot, { key: "y", ctrlKey: true });
    });
    expect(marks(store)).toBe("A,e1,e2");
  });
});
