// @vitest-environment jsdom
//
// The invariant EditorTabsView keeps: if the URL names a document, ITS tab is
// the one on screen — and it re-asserts it on every change of the tabs, so a
// URL naming a file no tab names opens a tab for it. That makes the URL part
// of a tab's identity: when the active tab's file binding changes under a
// `?file=` URL — it stops following its file (File → New, Import, Start
// blank), or follows another (Save As) — the URL has to follow, or the
// re-assert opens a second tab for the file the URL still names and puts it
// on screen.
//
// The route, the host, the binding sync and the stores are the real ones;
// the panes are a surface with the actions, and the API client is mocked.
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

// wouter, reduced to what the route reads: the search string, and a
// setLocation that rewrites it the way the browser would.
let search = "";
const setLocation = vi.fn((target: string) => {
  const q = target.indexOf("?");
  search = q >= 0 ? target.slice(q) : "";
});
vi.mock("wouter", () => ({
  useSearch: () => search,
  useLocation: () => ["/editor", setLocation],
  Link: ({ children }: { children: React.ReactNode }) => <span>{children}</span>,
}));
const noop = () => {};
vi.mock("@/store/ui", () => ({
  useUIStore: (sel: (s: unknown) => unknown) =>
    sel({
      addToast: noop,
      openDiagnosticsPanel: noop,
      activeWorkflowName: null,
      setActiveWorkflowName: noop,
    }),
}));
const { findDraftBotSource, parseSource } = vi.hoisted(() => ({
  findDraftBotSource: vi.fn(),
  parseSource: vi.fn(),
}));
vi.mock("@/api/runs/artifacts", () => ({ findDraftBotSource }));
const openFile = vi.fn();
vi.mock("@/api/client", () => ({
  openFile: (...a: unknown[]) => openFile(...a),
  loadExample: vi.fn(),
  parseSource: (...a: unknown[]) => parseSource(...a),
  parseBotSourceEditorPath: () => null,
}));
vi.mock("@/components/Home/RecentFilesPanel", () => ({ default: () => <div /> }));
vi.mock("@/components/EditorView", () => ({ default: () => <EditorSurface /> }));

import { useDocumentFileOps } from "@/components/Toolbar/useDocumentFileOps";
import { applyOpenedFile } from "@/lib/openedFile";
import { useBotsStore } from "@/store/bots";
import {
  getOrCreateDocumentStore,
  useDocumentStore,
  useDocumentStoreInstance,
} from "@/store/document";
import { useTabsStore } from "@/store/tabs";

import EditorTabsView from "./EditorTabsView";

function EditorSurface() {
  const ops = useDocumentFileOps({ confirm: async () => true });
  // What Save As does to the store once the write landed.
  const setCurrentFilePath = useDocumentStore((s) => s.setCurrentFilePath);
  // What the picker's File → Open does to THIS tab's store.
  const store = useDocumentStoreInstance();
  return (
    <>
      <button onClick={() => void ops.handleNew()}>New</button>
      <button onClick={() => setCurrentFilePath("bots/beta/main.bot")}>Save As beta</button>
      <button
        onClick={() =>
          applyOpenedFile(
            {
              source: "alpha ON DISK",
              document: emptyDoc(),
              diagnostics: [],
              path: "bots/alpha/main.bot",
              bindable: true,
            },
            store.getState(),
          )
        }
      >
        Open alpha here
      </button>
    </>
  );
}

const document = { agents: [], judges: [], routers: [], humans: [], tools: [], edges: [] };
// applyOpenedFile wants a full IterDocument; the empty shell is enough here.
const emptyDoc = () => document as never;

function mount() {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: 0 } } });
  return render(
    <QueryClientProvider client={qc}>
      <EditorTabsView />
    </QueryClientProvider>,
  );
}

const tabs = () => useTabsStore.getState().tabs;
const activeTab = () => tabs().find((t) => t.id === useTabsStore.getState().activeEditorTabId);

beforeEach(() => {
  search = "";
  setLocation.mockClear();
  findDraftBotSource.mockReset().mockResolvedValue(null);
  parseSource.mockReset();
  // The server names the file it was asked for.
  openFile.mockReset().mockImplementation(async (path: string) => ({
    source: `${path} ON DISK`,
    document,
    diagnostics: [],
    path,
  }));
  useBotsStore.setState({ bots: [], loading: false, error: null, discoveryErrors: [] });
  useTabsStore.setState({ tabs: [], activeEditorTabId: null, activeRunTabId: null });
});

afterEach(cleanup);

describe("the URL follows the active tab's file binding", () => {
  it("File → New under ?file= drops the param from the URL, and no second tab opens for the old file", async () => {
    search = "?file=bots%2Falpha%2Fmain.bot";
    mount();
    await screen.findByRole("button", { name: "New" });
    const id = activeTab()?.id;
    expect(id).toBeTruthy();

    fireEvent.click(screen.getByRole("button", { name: "New" }));
    await waitFor(() => expect(activeTab()?.params).toEqual({}));
    await new Promise((r) => setTimeout(r, 20));

    expect(tabs()).toHaveLength(1);
    expect(activeTab()?.id).toBe(id);
    expect(search).toBe("");
    expect(openFile).toHaveBeenCalledTimes(1);
  });

  it("Save As under ?file= renames the param in the URL, and no second tab opens for the old file", async () => {
    search = "?file=bots%2Falpha%2Fmain.bot";
    mount();
    await screen.findByRole("button", { name: "Save As beta" });
    const id = activeTab()?.id;

    fireEvent.click(screen.getByRole("button", { name: "Save As beta" }));
    await waitFor(() => expect(activeTab()?.params).toEqual({ file: "bots/beta/main.bot" }));
    await new Promise((r) => setTimeout(r, 20));

    expect(tabs()).toHaveLength(1);
    expect(activeTab()?.id).toBe(id);
    expect(search).toBe("?file=bots%2Fbeta%2Fmain.bot");
    expect(openFile).toHaveBeenCalledTimes(1);
  });

  // The initial bind is the one that must NOT touch the URL: the tab was
  // opened for the file the URL names, and the link's other params are for
  // EditorView, which reads them once the document is on screen.
  it("keeps the deep link's node and run params through the tab's own load", async () => {
    search = "?file=bots%2Falpha%2Fmain.bot&node=agent_1&from=run-9";
    mount();
    await screen.findByRole("button", { name: "New" });
    await waitFor(() => expect(activeTab()?.params).toEqual({ file: "bots/alpha/main.bot" }));
    await new Promise((r) => setTimeout(r, 20));

    expect(search).toBe("?file=bots%2Falpha%2Fmain.bot&node=agent_1&from=run-9");
    expect(setLocation).not.toHaveBeenCalled();
    expect(tabs()).toHaveLength(1);
  });

  it("leaves the URL alone when a background tab's binding changes", async () => {
    // Two tabs; the URL names the second, which is on screen.
    const first = useTabsStore.getState().openTab("editor", { file: "bots/alpha/main.bot" }, "alpha");
    search = "?file=bots%2Fgamma%2Fmain.bot";
    mount();
    await waitFor(() => expect(tabs()).toHaveLength(2));
    await waitFor(() => expect(openFile).toHaveBeenCalledTimes(2));
    setLocation.mockClear();

    // The first tab's document, off screen, follows another file.
    const { getOrCreateDocumentStore } = await import("@/store/document");
    getOrCreateDocumentStore(first).getState().setCurrentFilePath("bots/beta/main.bot");
    await waitFor(() =>
      expect(tabs().find((t) => t.id === first)?.params).toEqual({ file: "bots/beta/main.bot" }),
    );

    expect(setLocation).not.toHaveBeenCalled();
    expect(search).toBe("?file=bots%2Fgamma%2Fmain.bot");
    expect(tabs()).toHaveLength(2);
  });
});

describe("the URL follows the active tab's unbind, draft included", () => {
  // A draft tab is named by ?draft= the same way a file tab is named by
  // ?file=: once the tab stops following anything, the URL has to drop the
  // param, or the re-assert opens a second Draft tab for the run the URL
  // still names — and re-applies the draft over the author's new canvas.
  it("File → New under ?draft= drops the param from the URL, and no second tab opens for the old draft", async () => {
    findDraftBotSource.mockResolvedValue("THE DRAFT");
    parseSource.mockResolvedValue({ document: emptyDoc(), diagnostics: [], bindable: true });
    search = "?draft=run-1";
    mount();
    await waitFor(() => expect(activeTab()?.params).toEqual({ draft: "run-1" }));
    const id = activeTab()?.id;
    expect(id).toBeTruthy();
    const draftStore = getOrCreateDocumentStore(id ?? "");
    await waitFor(() => expect(draftStore.getState().currentSource).toBe("THE DRAFT"));

    fireEvent.click(screen.getByRole("button", { name: "New" }));
    await waitFor(() => expect(tabs().find((t) => t.id === id)?.params).toEqual({}));
    await new Promise((r) => setTimeout(r, 20));

    expect(tabs()).toHaveLength(1);
    expect(tabs()[0]?.id).toBe(id);
    expect(search).toBe("");
  });
});

describe("the URL re-assert keeps the tab the author is on", () => {
  // Two tabs naming the same file is legal (the picker opens a tab per
  // pick). When the tab ON SCREEN already names the URL's file, it IS the
  // URL's tab: re-asserting by first match would switch to an OLDER tab
  // naming the same file and yank the author off the tab they opened the
  // file in.
  it("opening, in a second tab, a file an older tab already holds does not switch tabs", async () => {
    const a = useTabsStore.getState().openTab("editor", { file: "bots/alpha/main.bot" }, "alpha");
    mount();
    await waitFor(() => expect(openFile).toHaveBeenCalledTimes(1));

    // "+" — a fresh untitled tab, bare URL (what handleNewTab does).
    fireEvent.click(screen.getByRole("button", { name: "New editor tab" }));
    await waitFor(() => expect(tabs()).toHaveLength(2));
    const b = activeTab()?.id;
    expect(b).toBeTruthy();
    expect(b).not.toBe(a);
    expect(search).toBe("");

    // In the new tab, File → Open alpha.
    fireEvent.click(screen.getByRole("button", { name: "Open alpha here" }));
    await waitFor(() =>
      expect(tabs().find((t) => t.id === b)?.params).toEqual({ file: "bots/alpha/main.bot" }),
    );
    await new Promise((r) => setTimeout(r, 20));

    expect(tabs()).toHaveLength(2);
    expect(activeTab()?.id).toBe(b);
    expect(search).toBe("?file=bots%2Falpha%2Fmain.bot");
    expect(openFile).toHaveBeenCalledTimes(1);
  });
});
