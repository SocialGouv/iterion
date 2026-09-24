// @vitest-environment jsdom
//
// A tab remembers which file it is about in `tab.params.file`, written by
// TabBindingSync. `/editor` is a wouter route: leaving it unmounts
// EditorTabsView and every EditorTabHost, while the per-tab document store
// survives in the module registry. Coming back remounts the host with
// `tab.params.file`, and fetches that file into the store when the store is
// not already bound to it.
//
// So the param has to be TRUE at every moment: a tab that follows a file
// names it (salvage or not), and a tab that stopped following anything —
// File → New, Import, Start blank, on a file tab or a draft tab — names
// nothing. A stale param is a second fetch over the author's work, with no
// prompt (#1334).
//
// The other side of the same coin is the load window: a tab just opened for
// a file has a store still bound to nothing while the fetch is in flight,
// and its param is what the fetch is FOR. Dropping it then would cancel the
// load and leave the scaffold on screen under the file's name.
//
// The hosts mount under a stand-in for the one line of EditorTabsView that
// matters here — `file={tab.params.file} draft={tab.params.draft}` — so a
// param the binding drops reaches the host the way it does in the app. The
// stores, TabBindingSync and the three unbinding sites are the real ones;
// only the API client is mocked.
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

const addToast = vi.fn();
const noop = () => {};
vi.mock("@/store/ui", () => ({
  useUIStore: (sel: (s: unknown) => unknown) =>
    sel({
      addToast,
      openDiagnosticsPanel: noop,
      toggleLibraryPanel: noop,
      libraryExpanded: false,
      setFilePickerOpen: noop,
      activeWorkflowName: null,
      setActiveWorkflowName: noop,
    }),
}));
const findDraftBotSource = vi.fn();
vi.mock("@/api/runs/artifacts", () => ({
  findDraftBotSource: (...a: unknown[]) => findDraftBotSource(...a),
}));
const openFile = vi.fn();
const loadExample = vi.fn();
const parseSource = vi.fn();
vi.mock("@/api/client", () => ({
  openFile: (...a: unknown[]) => openFile(...a),
  loadExample: (...a: unknown[]) => loadExample(...a),
  parseSource: (...a: unknown[]) => parseSource(...a),
  listExampleEntries: vi.fn(),
  parseBotSourceEditorPath: () => null,
}));
// The editor surface is reduced to the three sites that stop a tab following
// anything: the toolbar's New and Import (useDocumentFileOps) and the empty
// canvas's Start blank (CanvasEmpty). They read the per-tab store through the
// same Context the host provides in the app.
vi.mock("@/components/EditorView", () => ({ default: () => <EditorSurface /> }));

import CanvasEmpty from "@/components/Canvas/CanvasEmpty";
import { useDocumentFileOps } from "@/components/Toolbar/useDocumentFileOps";
import { createEmptyDocument } from "@/lib/defaults";
import { openExampleIntoStore } from "@/lib/openExample";
import { useBotsStore } from "@/store/bots";
import { getOrCreateDocumentStore } from "@/store/document";
import { UNTITLED_TAB_LABEL, useTabsStore } from "@/store/tabs";

import EditorTabHost from "./EditorTabHost";

function EditorSurface() {
  const ops = useDocumentFileOps({ confirm: async () => true });
  return (
    <>
      <button onClick={() => void ops.handleNew()}>New</button>
      <input aria-label="import" type="file" onChange={(e) => void ops.handleImport(e)} />
      <CanvasEmpty />
    </>
  );
}

// What EditorTabsView does for a hydrated tab: the host's props are the
// tab's params, read live from the tabs store.
function Host({ tabId }: { tabId: string }) {
  const tab = useTabsStore((s) => s.tabs.find((t) => t.id === tabId));
  return <EditorTabHost tabId={tabId} file={tab?.params.file} draft={tab?.params.draft} />;
}

const document = { agents: [], judges: [], routers: [], humans: [], tools: [], edges: [] };

let qc: QueryClient;

function mount(tabId: string) {
  return render(
    <QueryClientProvider client={qc}>
      <Host tabId={tabId} />
    </QueryClientProvider>,
  );
}

const state = (tabId: string) => getOrCreateDocumentStore(tabId).getState();
const tabOf = (tabId: string) => useTabsStore.getState().tabs.find((t) => t.id === tabId);
const paramsFileOf = (tabId: string) => tabOf(tabId)?.params.file;

// The author's work after the tab stopped following a file: the scaffold
// without its agent — a document no fetch and no draft would produce.
const authored = { ...createEmptyDocument(), agents: [] as never[] };

async function openAlpha() {
  const tabId = useTabsStore
    .getState()
    .openTab("editor", { file: "bots/alpha/main.bot" }, "alpha");
  openFile.mockResolvedValue({
    source: "ALPHA ON DISK",
    document,
    diagnostics: [],
    path: "bots/alpha/main.bot",
  });
  const view = mount(tabId);
  await waitFor(() => expect(state(tabId).currentSource).toBe("ALPHA ON DISK"));
  return { tabId, view };
}

// Leave /editor and come back: the host unmounts, the store survives, the
// host remounts with whatever the tab's params say.
async function leaveAndReturn(tabId: string, view: { unmount: () => void }) {
  view.unmount();
  mount(tabId);
  await new Promise((r) => setTimeout(r, 10));
}

beforeEach(() => {
  addToast.mockClear();
  findDraftBotSource.mockReset().mockResolvedValue(null);
  openFile.mockReset();
  loadExample.mockReset();
  parseSource.mockReset();
  qc = new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: 0 } } });
  // Seed the catalog so TabBindingSync's lazy persona fetch never fires.
  useBotsStore.setState({ bots: [], loading: false, error: null, discoveryErrors: [] });
  useTabsStore.setState({ tabs: [], activeEditorTabId: null, activeRunTabId: null });
});

afterEach(cleanup);

describe("a tab names the file it is about, salvage or not", () => {
  it("keeps the repair of an unparseable file across leaving the editor and coming back", async () => {
    const { tabId, view } = await openAlpha();

    // The author opens, in this tab, a workspace file that does not parse.
    // The server names it and says its document is a salvage.
    loadExample.mockResolvedValue({
      source: "SALVAGE",
      document,
      diagnostics: ["one/main.bot:3:1: error [E001]: unexpected character"],
      path: "catalog/one/main.bot",
      bindable: false,
    });
    await openExampleIntoStore("one/main.bot", getOrCreateDocumentStore(tabId));
    expect(state(tabId).salvaged).toBe(true);
    await waitFor(() => expect(paramsFileOf(tabId)).toBe("catalog/one/main.bot"));

    // The author starts repairing it, then leaves /editor and comes back.
    state(tabId).setCurrentSource("THE AUTHOR'S REPAIR");
    await leaveAndReturn(tabId, view);

    // No second fetch: the tab came back to the file it names, not to the
    // one it held before it.
    expect(openFile.mock.calls.map((c) => c[0])).toEqual(["bots/alpha/main.bot"]);
    expect(state(tabId).currentSource).toBe("THE AUTHOR'S REPAIR");
  });

  // An embedded bot, a catalog directory outside the working directory, a
  // symlinked bots/: /api/examples/{name} then names no path, since the file
  // is not reachable through /api/files/open either. The tab follows
  // bots/<name>, where a save of the program would land — not the file it
  // held before.
  it("follows an example the workspace does not hold as bots/<name>", async () => {
    const { tabId, view } = await openAlpha();

    loadExample.mockResolvedValue({
      source: "SALVAGE",
      document,
      diagnostics: ["one/main.bot:3:1: error [E001]: unexpected character"],
      bindable: false,
    });
    await openExampleIntoStore("one/main.bot", getOrCreateDocumentStore(tabId));
    await waitFor(() => expect(paramsFileOf(tabId)).toBe("bots/one/main.bot"));

    state(tabId).setCurrentSource("THE AUTHOR'S REPAIR");
    await leaveAndReturn(tabId, view);

    expect(openFile.mock.calls.map((c) => c[0])).toEqual(["bots/alpha/main.bot"]);
    expect(state(tabId).currentSource).toBe("THE AUTHOR'S REPAIR");
  });
});

describe("a tab that stops following a file names nothing", () => {
  it("File → New drops the file param, and coming back does not fetch the old file over the new document", async () => {
    const { tabId, view } = await openAlpha();

    fireEvent.click(screen.getByRole("button", { name: "New" }));
    await waitFor(() => expect(state(tabId).currentFilePath).toBeNull());
    await waitFor(() => expect(paramsFileOf(tabId)).toBeUndefined());
    expect(tabOf(tabId)?.label).toBe(UNTITLED_TAB_LABEL);

    // The author builds on the new canvas, leaves /editor, comes back.
    state(tabId).setDocument(authored);
    await leaveAndReturn(tabId, view);

    expect(openFile.mock.calls.map((c) => c[0])).toEqual(["bots/alpha/main.bot"]);
    expect(state(tabId).document?.agents).toEqual([]);
    expect(state(tabId).currentSource).toBeNull();
  });

  it("Import drops the file param, and coming back keeps the imported text", async () => {
    const { tabId, view } = await openAlpha();

    parseSource.mockResolvedValue({ document, diagnostics: [], bindable: true });
    const file = new File(["IMPORTED TEXT"], "imported.bot");
    fireEvent.change(screen.getByLabelText("import"), { target: { files: [file] } });
    await waitFor(() => expect(state(tabId).currentSource).toBe("IMPORTED TEXT"));
    await waitFor(() => expect(paramsFileOf(tabId)).toBeUndefined());

    await leaveAndReturn(tabId, view);

    expect(openFile.mock.calls.map((c) => c[0])).toEqual(["bots/alpha/main.bot"]);
    expect(state(tabId).currentSource).toBe("IMPORTED TEXT");
  });

  it("Start blank drops the file param, and coming back keeps what was built on the blank canvas", async () => {
    const { tabId, view } = await openAlpha();

    fireEvent.click(screen.getByRole("button", { name: /Start blank/ }));
    await waitFor(() => expect(state(tabId).currentFilePath).toBeNull());
    await waitFor(() => expect(paramsFileOf(tabId)).toBeUndefined());

    state(tabId).setDocument(authored);
    await leaveAndReturn(tabId, view);

    expect(openFile.mock.calls.map((c) => c[0])).toEqual(["bots/alpha/main.bot"]);
    expect(state(tabId).document?.agents).toEqual([]);
  });

  // A draft tab re-hydrates from its conversation on every mount, and owns
  // the buffer only while the buffer holds what it last put there. File → New
  // empties the buffer — so a tab still naming the draft would take it again
  // on return, over whatever the author built since.
  it("File → New on a draft tab drops the draft param, and coming back does not re-apply the draft", async () => {
    findDraftBotSource.mockResolvedValue("THE DRAFT");
    parseSource.mockResolvedValue({ document, diagnostics: [], bindable: true });
    const tabId = useTabsStore.getState().openTab("editor", { draft: "run-1" }, "Draft");
    const view = mount(tabId);
    await waitFor(() => expect(state(tabId).currentSource).toBe("THE DRAFT"));

    fireEvent.click(screen.getByRole("button", { name: "New" }));
    await waitFor(() => expect(state(tabId).currentSource).toBeNull());
    await waitFor(() => expect(tabOf(tabId)?.params).toEqual({}));

    state(tabId).setDocument(authored);
    await leaveAndReturn(tabId, view);

    expect(parseSource).toHaveBeenCalledTimes(1);
    expect(state(tabId).document?.agents).toEqual([]);
    expect(state(tabId).currentSource).toBeNull();
  });
});

describe("a tab keeps its param through its own load window", () => {
  // The reason the binding must not drop a param on a bare null path: the
  // store of a tab just opened for a file is bound to nothing until the fetch
  // lands, and the param is what the fetch is FOR. Dropping it cancels the
  // load and leaves the scaffold on screen under the file's name.
  it("does not drop the file param while the file is still being fetched", async () => {
    let resolve: (v: unknown) => void = () => {};
    openFile.mockReturnValue(new Promise((r) => (resolve = r)));
    const tabId = useTabsStore
      .getState()
      .openTab("editor", { file: "bots/beta/main.bot" }, "beta");
    mount(tabId);
    await new Promise((r) => setTimeout(r, 10));

    // In flight: the store is bound to nothing, the tab still names its file.
    expect(state(tabId).currentFilePath).toBeNull();
    expect(paramsFileOf(tabId)).toBe("bots/beta/main.bot");
    expect(tabOf(tabId)?.label).toBe("beta");

    resolve({ source: "BETA ON DISK", document, diagnostics: [], path: "bots/beta/main.bot" });
    await waitFor(() => expect(state(tabId).currentSource).toBe("BETA ON DISK"));
    expect(paramsFileOf(tabId)).toBe("bots/beta/main.bot");
    expect(openFile).toHaveBeenCalledTimes(1);
  });

  it("does not drop the draft param while the draft is still being fetched", async () => {
    let resolve: (v: unknown) => void = () => {};
    findDraftBotSource.mockReturnValue(new Promise((r) => (resolve = r)));
    parseSource.mockResolvedValue({ document, diagnostics: [], bindable: true });
    const tabId = useTabsStore.getState().openTab("editor", { draft: "run-2" }, "Draft");
    mount(tabId);
    await new Promise((r) => setTimeout(r, 10));

    expect(tabOf(tabId)?.params).toEqual({ draft: "run-2" });
    expect(tabOf(tabId)?.label).toBe("Draft");

    resolve("THE DRAFT");
    await waitFor(() => expect(state(tabId).currentSource).toBe("THE DRAFT"));
    expect(tabOf(tabId)?.params).toEqual({ draft: "run-2" });
  });
});
