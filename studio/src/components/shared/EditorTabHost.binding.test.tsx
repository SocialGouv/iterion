// @vitest-environment jsdom
//
// A tab that opened a file which does not parse still NAMES that file, and
// keeps it across a remount. `/editor` is a wouter route: leaving it
// unmounts EditorTabsView and every EditorTabHost, while the per-tab
// document store survives in the module registry. Coming back remounts the
// host with `tab.params.file` — whatever TabBindingSync last wrote there,
// and it only writes a non-null path. Taking the path away to stop a save
// would leave that param on the PREVIOUS file, and the remount would fetch
// it over the author's in-progress repair: a second loss by another road.
import { cleanup, render, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

const addToast = vi.fn();
vi.mock("@/store/ui", () => ({
  useUIStore: (sel: (s: unknown) => unknown) => sel({ addToast }),
}));
const findDraftBotSource = vi.fn();
vi.mock("@/api/runs/artifacts", () => ({
  findDraftBotSource: (...a: unknown[]) => findDraftBotSource(...a),
}));
const openFile = vi.fn();
const loadExample = vi.fn();
vi.mock("@/api/client", () => ({
  openFile: (...a: unknown[]) => openFile(...a),
  loadExample: (...a: unknown[]) => loadExample(...a),
  parseSource: vi.fn(),
}));
vi.mock("@/components/EditorView", () => ({ default: () => <div /> }));

import { getOrCreateDocumentStore } from "@/store/document";
import { useTabsStore } from "@/store/tabs";
import { useBotsStore } from "@/store/bots";
import { openExampleIntoStore } from "@/lib/openExample";

import EditorTabHost from "./EditorTabHost";

const document = { agents: [], judges: [], routers: [], humans: [], tools: [], edges: [] };

function mount(tabId: string, file: string | undefined) {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: 0 } } });
  return render(
    <QueryClientProvider client={qc}>
      <EditorTabHost tabId={tabId} file={file} />
    </QueryClientProvider>,
  );
}

const state = (tabId: string) => getOrCreateDocumentStore(tabId).getState();
const paramsFileOf = (tabId: string) =>
  useTabsStore.getState().tabs.find((t) => t.id === tabId)?.params.file;

beforeEach(() => {
  addToast.mockClear();
  findDraftBotSource.mockReset().mockResolvedValue(null);
  openFile.mockReset();
  loadExample.mockReset();
  // Seed the catalog so TabBindingSync's lazy persona fetch never fires.
  useBotsStore.setState({ bots: [], loading: false, error: null, discoveryErrors: [] });
  useTabsStore.setState({ tabs: [], activeEditorTabId: null, activeRunTabId: null });
});

afterEach(cleanup);

describe("a tab names the file it is about, salvage or not", () => {
  it("keeps the repair of an unparseable file across leaving the editor and coming back", async () => {
    const tabId = useTabsStore
      .getState()
      .openTab("editor", { file: "bots/alpha/main.bot" }, "alpha");

    openFile.mockResolvedValue({
      source: "ALPHA ON DISK",
      document,
      diagnostics: [],
      path: "bots/alpha/main.bot",
    });
    const view = mount(tabId, "bots/alpha/main.bot");
    await waitFor(() => expect(state(tabId).currentSource).toBe("ALPHA ON DISK"));

    // The author opens, in this tab, a workspace file that does not parse.
    // The server names it and says its document is a salvage.
    loadExample.mockResolvedValue({
      source: "SALVAGE",
      document,
      diagnostics: ["one/main.bot:3:1: error [E001]: unexpected character"],
      path: "catalog/one/main.bot",
      bindable: false,
    });
    await openExampleIntoStore("one/main.bot", state(tabId));
    expect(state(tabId).salvaged).toBe(true);
    await waitFor(() => expect(paramsFileOf(tabId)).toBe("catalog/one/main.bot"));

    // The author starts repairing it, then leaves /editor and comes back.
    state(tabId).setCurrentSource("THE AUTHOR'S REPAIR");
    view.unmount();
    mount(tabId, paramsFileOf(tabId));
    await new Promise((r) => setTimeout(r, 10));

    // No second fetch: the tab came back to the file it names, not to the
    // one it held before it.
    expect(openFile.mock.calls.map((c) => c[0])).toEqual(["bots/alpha/main.bot"]);
    expect(state(tabId).currentSource).toBe("THE AUTHOR'S REPAIR");
  });
});
