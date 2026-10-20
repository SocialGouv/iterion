// @vitest-environment jsdom
//
// The inline launch — the toolbar's Run on a tab with no path, and the only
// launch cloud has — renders the editor's document through /api/unparse to
// get the program it sends. That render is the LAUNCH's text, not the tab's:
// the route reads the ACTIVE editor tab's store (LaunchDocStoreProvider),
// and a draft tab follows its conversation only while its source is still
// the draft the assistant installed. Written there, a render that is not
// byte-identical to the draft reads as the author's edit: the assistant's
// next drafts stop landing, and nothing says so.
//
// The second describe replays that end to end over the real EditorTabHost,
// including the host remount caused by leaving the editor for the launch and
// coming back (#1790, #1791).
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import type { IterDocument } from "@/api/types";

const draftApi = vi.hoisted(() => ({ findDraftBotSource: vi.fn() }));
vi.mock("@/api/runs/artifacts", () => draftApi);

const api = vi.hoisted(() => ({
  openFile: vi.fn(),
  parseSource: vi.fn(),
  unparse: vi.fn(),
}));
vi.mock("@/api/client", () => api);
vi.mock("@/components/EditorView", () => ({ default: () => <div /> }));

import EditorTabHost from "@/components/shared/EditorTabHost";
import { editorDraftKey } from "@/hooks/useDraftBot";
import { createEmptyDocument } from "@/lib/defaults";
import { getOrCreateDocumentStore } from "@/store/document";
import { useTabsStore } from "@/store/tabs";

import LaunchDocStoreProvider from "./LaunchDocStoreProvider";
import { useLaunchDoc } from "./useLaunchDoc";

const DRAFT_V1 = "workflow v1:\n  entry: done\n";
const DRAFT_V2 = "workflow v2:\n  entry: done\n";

// The draft text a document carries, so the unparse mock below renders FROM
// the document it is given rather than from a constant.
const textOf = (doc: IterDocument | undefined) => doc?.comments?.[0]?.text ?? "";
const documentFor = (source: string): IterDocument => ({
  ...createEmptyDocument(),
  comments: [{ text: source }],
});
// The server's render of a document is not always byte-identical to the text
// it was parsed from — a declaration can move, blank lines can appear, a
// final newline can go missing — and one differing byte is all it takes for
// the tab to read it as the author's edit. This render drifts on purpose.
const driftedRender = (doc: IterDocument | undefined) => `${textOf(doc)}# rendered\n`;

function Launch() {
  const { currentSource } = useLaunchDoc("", () => {});
  return <output aria-label="launched">{currentSource ?? ""}</output>;
}
const launched = () => screen.getByLabelText("launched").textContent;

function sourceOf(tabId: string) {
  return getOrCreateDocumentStore(tabId).getState().currentSource;
}

// A draft tab as the assistant leaves it: no file path, the draft's text in
// the buffer, the document edited past the saved mark — a launch candidate,
// and the inline branch's only one.
function draftTab(source: string) {
  const id = useTabsStore.getState().openTab("editor", { draft: "run-x" }, "Draft");
  useTabsStore.getState().setActive(id);
  const store = getOrCreateDocumentStore(id);
  store.getState().setDocument(documentFor(source));
  store.getState().setCurrentSource(source);
  return { id, store };
}

beforeEach(() => {
  vi.resetAllMocks();
  api.unparse.mockImplementation(async (doc: IterDocument) => ({ source: driftedRender(doc) }));
  api.parseSource.mockImplementation(async (src: string) => ({
    document: documentFor(src),
    diagnostics: [],
  }));
  useTabsStore.setState({ tabs: [], activeEditorTabId: null, activeRunTabId: null });
});

afterEach(cleanup);

describe("an inline launch of the active editor tab's buffer", () => {
  it("carries its own render and leaves the tab's buffer untouched", async () => {
    const { store } = draftTab(DRAFT_V1);
    const before = store.getState();

    render(
      <LaunchDocStoreProvider>
        <Launch />
      </LaunchDocStoreProvider>,
    );

    await waitFor(() => expect(launched()).toBe(`${DRAFT_V1}# rendered\n`));
    const after = store.getState();
    expect(after.currentSource).toBe(DRAFT_V1);
    expect(after.document).toBe(before.document);
    expect(after._generation).toBe(before._generation);
    expect(after.sourceBuffer).toBe(before.sourceBuffer);
  });

  // The tab's store keeps living while the form is up — the chat dock is
  // app-level and can apply a proposal to it — and the render of a document
  // the tab has moved past is not this launch's program any more.
  it("stops reporting a render the document has moved past", async () => {
    const { store } = draftTab(DRAFT_V1);
    let answerSecond!: (rendered: { source: string }) => void;
    api.unparse
      .mockImplementationOnce(async (doc: IterDocument) => ({ source: driftedRender(doc) }))
      .mockImplementationOnce(() => new Promise((resolve) => (answerSecond = resolve)));

    render(
      <LaunchDocStoreProvider>
        <Launch />
      </LaunchDocStoreProvider>,
    );
    await waitFor(() => expect(launched()).toBe(`${DRAFT_V1}# rendered\n`));

    store.getState().setDocument(documentFor(DRAFT_V2));
    await waitFor(() => expect(launched()).toBe(""));

    answerSecond({ source: `${DRAFT_V2}# rendered\n` });
    await waitFor(() => expect(launched()).toBe(`${DRAFT_V2}# rendered\n`));
  });
});

describe("a draft tab the operator launched", () => {
  let qc: QueryClient;

  function mount(tabId: string, draft: string, launching: boolean) {
    return (
      <QueryClientProvider client={qc}>
        <EditorTabHost tabId={tabId} draft={draft} />
        {launching && (
          <LaunchDocStoreProvider>
            <Launch />
          </LaunchDocStoreProvider>
        )}
      </QueryClientProvider>
    );
  }

  // What the dock does when a turn lands: the draft is a node output, so the
  // conversation is what knows there is something new to read.
  async function turnLanded(draft: string) {
    await qc.invalidateQueries({ queryKey: editorDraftKey(draft) });
  }

  beforeEach(() => {
    qc = new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: 0 } } });
  });

  it("takes the next turn's draft when no launch happened", async () => {
    draftApi.findDraftBotSource.mockResolvedValue(DRAFT_V1);
    const id = useTabsStore.getState().openTab("editor", { draft: "run-a" }, "Draft");
    useTabsStore.getState().setActive(id);
    render(mount(id, "run-a", false));
    await waitFor(() => expect(sourceOf(id)).toBe(DRAFT_V1));

    draftApi.findDraftBotSource.mockResolvedValue(DRAFT_V2);
    await turnLanded("run-a");
    await waitFor(() => expect(sourceOf(id)).toBe(DRAFT_V2));
  });

  it("takes the next turn's draft after a launch whose render differs from it", async () => {
    draftApi.findDraftBotSource.mockResolvedValue(DRAFT_V1);
    const id = useTabsStore.getState().openTab("editor", { draft: "run-b" }, "Draft");
    useTabsStore.getState().setActive(id);
    const { rerender } = render(mount(id, "run-b", false));
    await waitFor(() => expect(sourceOf(id)).toBe(DRAFT_V1));

    rerender(mount(id, "run-b", true));
    await waitFor(() => expect(launched()).toBe(`${DRAFT_V1}# rendered\n`));

    draftApi.findDraftBotSource.mockResolvedValue(DRAFT_V2);
    await turnLanded("run-b");
    await waitFor(() => expect(sourceOf(id)).toBe(DRAFT_V2));
  });

  it("takes the next turn's draft after a launch whose render equals it", async () => {
    api.unparse.mockImplementation(async (doc: IterDocument) => ({ source: textOf(doc) }));
    draftApi.findDraftBotSource.mockResolvedValue(DRAFT_V1);
    const id = useTabsStore.getState().openTab("editor", { draft: "run-c" }, "Draft");
    useTabsStore.getState().setActive(id);
    const { rerender } = render(mount(id, "run-c", false));
    await waitFor(() => expect(sourceOf(id)).toBe(DRAFT_V1));

    rerender(mount(id, "run-c", true));
    await waitFor(() => expect(launched()).toBe(DRAFT_V1));

    draftApi.findDraftBotSource.mockResolvedValue(DRAFT_V2);
    await turnLanded("run-c");
    await waitFor(() => expect(sourceOf(id)).toBe(DRAFT_V2));
  });

  it("follows the conversation after the whole editor host remounts around a launch", async () => {
    draftApi.findDraftBotSource.mockResolvedValue(DRAFT_V1);
    const id = useTabsStore.getState().openTab("editor", { draft: "run-d" }, "Draft");
    useTabsStore.getState().setActive(id);
    const { rerender } = render(mount(id, "run-d", false));
    await waitFor(() => expect(sourceOf(id)).toBe(DRAFT_V1));

    // Leaving /editor for /runs/new unmounts the host, not the tab's store.
    rerender(
      <QueryClientProvider client={qc}>
        <LaunchDocStoreProvider>
          <Launch />
        </LaunchDocStoreProvider>
      </QueryClientProvider>,
    );
    await waitFor(() => expect(launched()).toBe(`${DRAFT_V1}# rendered\n`));

    rerender(mount(id, "run-d", false));
    await waitFor(() =>
      expect(qc.getQueryState(editorDraftKey("run-d"))?.fetchStatus).toBe("idle"),
    );
    draftApi.findDraftBotSource.mockResolvedValue(DRAFT_V2);
    await turnLanded("run-d");
    await waitFor(() => expect(sourceOf(id)).toBe(DRAFT_V2));
  });
});
