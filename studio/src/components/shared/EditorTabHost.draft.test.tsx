// @vitest-environment jsdom
//
// The canvas must FOLLOW the conversation, not take one snapshot of it.
//
// Reported from a real session — "je n'ai pas mon graphe qui se met à jour
// sous mes yeux". The assistant redrafts across turns ("now add a judge"), but
// the tab seeded once and refused everything after, so the canvas sat frozen
// while the assistant announced an update the operator could not see. Worse
// than a missing feature: the assistant was asserting a state that was false.
//
// The counterweight is ownership. We may replace the buffer only while it
// still holds exactly what we last put there; the moment the operator edits
// it, the canvas is theirs.
//
// The refresh is INVALIDATION-driven, not a clock: the draft is a node output
// written when the turn ends, so the conversation is what knows there is
// something new. These tests drive that the way the dock does.
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

const findDraftBotSource = vi.fn();
const parseSource = vi.fn();

vi.mock("@/api/runs/artifacts", () => ({
  findDraftBotSource: (...a: unknown[]) => findDraftBotSource(...a),
}));
vi.mock("@/api/client", () => ({
  openFile: vi.fn(),
  parseSource: (...a: unknown[]) => parseSource(...a),
}));
vi.mock("@/components/EditorView", () => ({ default: () => <div /> }));
const ui = vi.hoisted(() => ({ addToast: vi.fn() }));
vi.mock("@/store/ui", () => ({
  useUIStore: Object.assign((sel: (s: unknown) => unknown) => sel({ addToast: ui.addToast }), {
    getState: () => ({ addToast: ui.addToast }),
  }),
}));

import { editorDraftKey } from "@/hooks/useDraftBot";
import { createEmptyDocument } from "@/lib/defaults";
import { applyOpenedFile } from "@/lib/openedFile";
import { replaceDocument } from "@/lib/replaceDocument";
import { getOrCreateDocumentStore, stampEditor, stampHolds } from "@/store/document";
import { useTabsStore } from "@/store/tabs";

import EditorTabHost from "./EditorTabHost";

let qc: QueryClient;

function mount(tabId: string, draft: string) {
  return render(
    <QueryClientProvider client={qc}>
      <EditorTabHost tabId={tabId} draft={draft} />
    </QueryClientProvider>,
  );
}

// What the dock does when a turn lands.
async function turnLanded(draft: string) {
  await qc.invalidateQueries({ queryKey: editorDraftKey(draft) });
  await settle();
}

function sourceOf(tabId: string) {
  return getOrCreateDocumentStore(tabId).getState().currentSource;
}

// waitFor polls on REAL timers, which fake timers stop — so settle the async
// apply by advancing the fake clock instead.
async function settle(ms = 0) {
  await vi.advanceTimersByTimeAsync(ms);
  await vi.advanceTimersByTimeAsync(0);
  await vi.advanceTimersByTimeAsync(0);
}

beforeEach(() => {
  vi.useFakeTimers();
  findDraftBotSource.mockReset();
  parseSource
    .mockReset()
    .mockImplementation(async (src: string) => ({
      document: { source: src },
      diagnostics: [],
    }));
  useTabsStore.setState({ tabs: [], activeEditorTabId: null, activeRunTabId: null });
  qc = new QueryClient({
    defaultOptions: { queries: { retry: false, gcTime: 0 } },
  });
});

afterEach(() => {
  cleanup();
  vi.useRealTimers();
});

describe("an open draft tab follows its conversation", () => {
  it("takes the first draft", async () => {
    findDraftBotSource.mockResolvedValue("v1");
    const id = useTabsStore.getState().openTab("editor", { draft: "run-1" }, "Draft");
    mount(id, "run-1");
    await settle();
    expect(sourceOf(id)).toBe("v1");
  });

  it("picks up the NEXT turn's draft without a reload", async () => {
    findDraftBotSource.mockResolvedValue("v1");
    const id = useTabsStore.getState().openTab("editor", { draft: "run-2" }, "Draft");
    mount(id, "run-2");
    await settle();
    expect(sourceOf(id)).toBe("v1");

    findDraftBotSource.mockResolvedValue("v2");
    await turnLanded("run-2");
    expect(sourceOf(id)).toBe("v2");
  });

  it("counts a draft that lands as a replacement of what the tab held", async () => {
    // A Save As still writing the previous draft must not bind its name to
    // this one: it reads the applied-replacement count, which this moves.
    findDraftBotSource.mockResolvedValue("v1");
    const id = useTabsStore.getState().openTab("editor", { draft: "run-2r" }, "Draft");
    mount(id, "run-2r");
    await settle();
    expect(sourceOf(id)).toBe("v1");
    const asked = stampEditor(getOrCreateDocumentStore(id).getState());

    findDraftBotSource.mockResolvedValue("v2");
    await turnLanded("run-2r");
    expect(sourceOf(id)).toBe("v2");
    expect(stampHolds(asked, getOrCreateDocumentStore(id).getState()).replaced).toBe(false);
  });

  it("refuses to clobber the operator's own edits", async () => {
    findDraftBotSource.mockResolvedValue("v1");
    const id = useTabsStore.getState().openTab("editor", { draft: "run-3" }, "Draft");
    mount(id, "run-3");
    await settle();
    expect(sourceOf(id)).toBe("v1");

    // The operator types. From here the canvas is theirs.
    getOrCreateDocumentStore(id).getState().setCurrentSource("mine");

    findDraftBotSource.mockResolvedValue("v2");
    await turnLanded("run-3");
    expect(sourceOf(id)).toBe("mine");
  });

  it("refuses to clobber an edit made on the CANVAS", async () => {
    // A canvas edit moves the document, not `currentSource`: a guard reading
    // the source alone let the next draft replace it.
    findDraftBotSource.mockResolvedValue("v1");
    const id = useTabsStore.getState().openTab("editor", { draft: "run-3c" }, "Draft");
    mount(id, "run-3c");
    await settle();
    expect(sourceOf(id)).toBe("v1");

    const store = getOrCreateDocumentStore(id);
    store.getState().addAgent({
      name: "mine",
      model: "m",
      input: "in",
      output: "out",
      system: "s",
      user: "u",
      session: "fresh",
    });
    const edited = store.getState().document;

    findDraftBotSource.mockResolvedValue("v2");
    await turnLanded("run-3c");
    expect(sourceOf(id)).toBe("v1");
    expect(store.getState().document).toBe(edited);
  });

  it("refuses to clobber text typed in the Source view", async () => {
    findDraftBotSource.mockResolvedValue("v1");
    const id = useTabsStore.getState().openTab("editor", { draft: "run-3s" }, "Draft");
    mount(id, "run-3s");
    await settle();
    const store = getOrCreateDocumentStore(id);
    store.getState().setSourceBuffer({
      path: null,
      rel: null,
      text: "typed",
      base: "v1",
      doc: store.getState().document,
      session: 1,
    });
    const before = store.getState().document;

    findDraftBotSource.mockResolvedValue("v2");
    await turnLanded("run-3s");
    expect(sourceOf(id)).toBe("v1");
    expect(store.getState().document).toBe(before);
  });

  it("waits for an Open the author asked for, instead of landing and getting it refused", async () => {
    findDraftBotSource.mockResolvedValue("v1");
    const id = useTabsStore.getState().openTab("editor", { draft: "run-p" }, "Draft");
    mount(id, "run-p");
    await settle();
    expect(sourceOf(id)).toBe("v1");
    const store = getOrCreateDocumentStore(id);
    let land!: (v: unknown) => void;
    const outcome = replaceDocument(
      store,
      "bots/q.bot",
      () => new Promise((r) => (land = r)),
      (r, st) => applyOpenedFile(r as never, st),
    );

    findDraftBotSource.mockResolvedValue("v2");
    await turnLanded("run-p");
    expect(sourceOf(id)).toBe("v1");
    // It waits: nothing of the new draft is even parsed while the Open loads.
    expect(parseSource).not.toHaveBeenCalledWith("v2");

    land({
      source: "Q\n",
      document: { ...createEmptyDocument(), comments: [{ text: "Q" }] },
      diagnostics: [],
      path: "bots/q.bot",
    });
    expect(await outcome).toBe("applied");
    await settle();
    expect(store.getState().currentFilePath).toBe("bots/q.bot");
    expect(ui.addToast).not.toHaveBeenCalled();
  });

  it("takes a draft that arrived while an Open loaded, once that Open has failed", async () => {
    findDraftBotSource.mockResolvedValue("v1");
    const id = useTabsStore.getState().openTab("editor", { draft: "run-pf" }, "Draft");
    mount(id, "run-pf");
    await settle();
    expect(sourceOf(id)).toBe("v1");
    const store = getOrCreateDocumentStore(id);
    let failLoad!: (e: unknown) => void;
    const outcome = replaceDocument(
      store,
      "bots/gone.bot",
      () => new Promise((_, reject) => (failLoad = reject)),
      () => {},
    );

    findDraftBotSource.mockResolvedValue("v2");
    await turnLanded("run-pf");
    expect(sourceOf(id)).toBe("v1");

    // Nothing replaced the draft: the conversation's newest one is due.
    failLoad(new Error("404: file not found"));
    await expect(outcome).rejects.toThrow("404");
    await settle();
    expect(sourceOf(id)).toBe("v2");
  });

  it("does not land a draft whose parse the author's Open overtook", async () => {
    findDraftBotSource.mockResolvedValue("v1");
    const id = useTabsStore.getState().openTab("editor", { draft: "run-pp" }, "Draft");
    mount(id, "run-pp");
    await settle();
    expect(sourceOf(id)).toBe("v1");
    const store = getOrCreateDocumentStore(id);

    let landParse!: (v: unknown) => void;
    parseSource.mockReturnValueOnce(new Promise((r) => (landParse = r)));
    findDraftBotSource.mockResolvedValue("v2");
    await turnLanded("run-pp");
    expect(parseSource).toHaveBeenLastCalledWith("v2");
    // The author asks for a file while that parse is in flight.
    let failLoad!: (e: unknown) => void;
    const outcome = replaceDocument(
      store,
      "bots/q.bot",
      () => new Promise((_, reject) => (failLoad = reject)),
      () => {},
    );
    landParse({ document: { source: "v2" }, diagnostics: [] });
    await settle();
    expect(sourceOf(id)).toBe("v1");
    failLoad(new Error("404: file not found"));
    await expect(outcome).rejects.toThrow("404");
  });

  it("does not land a draft whose parse answered in the tick the author's Open began", async () => {
    // The answer is already queued when the Open starts, so it runs before
    // the re-render that would cancel it: the check at the answer is what
    // stops it.
    findDraftBotSource.mockResolvedValue("v1");
    const id = useTabsStore.getState().openTab("editor", { draft: "run-pt" }, "Draft");
    mount(id, "run-pt");
    await settle();
    expect(sourceOf(id)).toBe("v1");
    const store = getOrCreateDocumentStore(id);

    let landParse!: (v: unknown) => void;
    parseSource.mockReturnValueOnce(new Promise((r) => (landParse = r)));
    findDraftBotSource.mockResolvedValue("v2");
    await turnLanded("run-pt");
    expect(parseSource).toHaveBeenLastCalledWith("v2");

    landParse({ document: { source: "v2" }, diagnostics: [] });
    let land!: (v: unknown) => void;
    const outcome = replaceDocument(
      store,
      "bots/q.bot",
      () => new Promise((r) => (land = r)),
      (r, st) => applyOpenedFile(r as never, st),
    );
    await settle();
    expect(sourceOf(id)).toBe("v1");

    land({
      source: "Q\n",
      document: { ...createEmptyDocument(), comments: [{ text: "Q" }] },
      diagnostics: [],
      path: "bots/q.bot",
    });
    expect(await outcome).toBe("applied");
    expect(store.getState().currentFilePath).toBe("bots/q.bot");
  });

  it("keeps the canvas when a later poll finds nothing", async () => {
    findDraftBotSource.mockResolvedValue("v1");
    const id = useTabsStore.getState().openTab("editor", { draft: "run-4" }, "Draft");
    mount(id, "run-4");
    await settle();
    expect(sourceOf(id)).toBe("v1");

    findDraftBotSource.mockResolvedValue(null);
    await turnLanded("run-4");
    expect(sourceOf(id)).toBe("v1");
  });

  it("does not re-parse when the draft has not changed", async () => {
    findDraftBotSource.mockResolvedValue("v1");
    const id = useTabsStore.getState().openTab("editor", { draft: "run-5" }, "Draft");
    mount(id, "run-5");
    await settle();
    expect(sourceOf(id)).toBe("v1");

    await turnLanded("run-5");
    expect(parseSource).toHaveBeenCalledTimes(1);
  });

  it("does not poll — time alone fetches nothing new", async () => {
    findDraftBotSource.mockResolvedValue("v1");
    const id = useTabsStore.getState().openTab("editor", { draft: "run-6" }, "Draft");
    mount(id, "run-6");
    await settle();
    expect(findDraftBotSource).toHaveBeenCalledTimes(1);

    // Well short of the safety-net refetch: the conversation is what tells the
    // tab to look again, not a clock.
    await settle(10000);
    expect(findDraftBotSource).toHaveBeenCalledTimes(1);
  });

  it("retries a failed draft lookup", async () => {
    findDraftBotSource
      .mockRejectedValueOnce(new Error("temporary outage"))
      .mockResolvedValueOnce("v1");
    const id = useTabsStore
      .getState()
      .openTab("editor", { draft: "run-retry" }, "Draft");
    mount(id, "run-retry");
    await settle();

    fireEvent.click(screen.getByRole("button", { name: "Retry" }));
    await settle();

    expect(findDraftBotSource).toHaveBeenCalledTimes(2);
    expect(sourceOf(id)).toBe("v1");
  });
});
