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
import { REPLACE_DEADLINE_MS, replaceDocument } from "@/lib/replaceDocument";
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

  it("starts a fresh undo history when installing the first draft", async () => {
    findDraftBotSource.mockResolvedValue("v1");
    const id = useTabsStore.getState().openTab("editor", { draft: "run-history" }, "Draft");
    const store = getOrCreateDocumentStore(id);
    store.getState().setDocument({ ...createEmptyDocument(), comments: [{ text: "previous" }] });
    mount(id, "run-history");
    await settle();

    expect(store.getState()._history).toEqual([]);
    expect(store.getState()._future).toEqual([]);
    const installed = store.getState().document;
    store.getState().undo();
    expect(store.getState().document).toBe(installed);
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

  it("takes a draft waiting behind an Open the server never answers, once its deadline has passed", async () => {
    findDraftBotSource.mockResolvedValue("v1");
    const id = useTabsStore.getState().openTab("editor", { draft: "run-dl" }, "Draft");
    mount(id, "run-dl");
    await settle();
    expect(sourceOf(id)).toBe("v1");
    const store = getOrCreateDocumentStore(id);
    const hung = replaceDocument(store, "bots/hung.bot", () => new Promise<never>(() => {}), () => {}).catch(
      (err: unknown) => err,
    );

    findDraftBotSource.mockResolvedValue("v2");
    await turnLanded("run-dl");
    expect(sourceOf(id)).toBe("v1");

    await vi.advanceTimersByTimeAsync(REPLACE_DEADLINE_MS);
    expect(await hung).toBeInstanceOf(Error);
    await settle();
    expect(sourceOf(id)).toBe("v2");
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

  it("takes a newer draft after a full remount when nothing was edited", async () => {
    findDraftBotSource.mockResolvedValue("v1");
    const id = useTabsStore.getState().openTab("editor", { draft: "run-remount" }, "Draft");
    const first = mount(id, "run-remount");
    await settle();
    expect(sourceOf(id)).toBe("v1");

    first.unmount();
    mount(id, "run-remount");
    await settle();
    findDraftBotSource.mockResolvedValue("v2");
    await turnLanded("run-remount");

    expect(sourceOf(id)).toBe("v2");
  });

  it("keeps an installed draft on screen when its refetch fails across a remount", async () => {
    findDraftBotSource.mockResolvedValue("v1");
    const id = useTabsStore.getState().openTab("editor", { draft: "run-remount-error" }, "Draft");
    const first = mount(id, "run-remount-error");
    await settle();
    expect(sourceOf(id)).toBe("v1");
    first.unmount();

    findDraftBotSource.mockRejectedValue(new Error("offline while away"));
    await qc.invalidateQueries({ queryKey: editorDraftKey("run-remount-error") });
    mount(id, "run-remount-error");
    await settle();

    expect(sourceOf(id)).toBe("v1");
    expect(screen.queryByText("Couldn't reload “Draft”")).toBeNull();
  });

  it.each([
    ["canvas", "before"],
    ["canvas", "after"],
    ["Source", "before"],
    ["Source", "after"],
    ["currentSource", "before"],
    ["currentSource", "after"],
  ] as const)("keeps %s edits made %s a remount as the author's, and offers the new draft", async (kind, timing) => {
    findDraftBotSource.mockResolvedValue("v1");
    const run = `run-remount-${kind}-${timing}`;
    const id = useTabsStore.getState().openTab("editor", { draft: run }, "Draft");
    const view = mount(id, run);
    await settle();
    const store = getOrCreateDocumentStore(id);

    if (timing === "after") {
      view.unmount();
      mount(id, run);
      await settle();
    }

    let expectedSource = "v1";
    if (kind === "canvas") {
      store.getState().addAgent({
        name: "mine",
        model: "m",
        input: "in",
        output: "out",
        system: "s",
        user: "u",
        session: "fresh",
      });
    } else if (kind === "Source") {
      store.getState().setSourceBuffer({
        path: null,
        rel: null,
        text: "typed",
        base: "v1",
        doc: store.getState().document,
        session: 1,
      });
    } else {
      expectedSource = "mine";
      store.getState().setCurrentSource(expectedSource);
    }
    const editedDocument = store.getState().document;
    const editedBuffer = store.getState().sourceBuffer;

    if (timing === "before") {
      view.unmount();
      mount(id, run);
      await settle();
    }

    findDraftBotSource.mockResolvedValue("v2");
    await turnLanded(run);

    expect(sourceOf(id)).toBe(expectedSource);
    expect(store.getState().document).toBe(editedDocument);
    if (kind === "Source") expect(store.getState().sourceBuffer).toBe(editedBuffer);
    expect(screen.getByRole("button", { name: "Load it" })).toBeTruthy();
  });
});

describe("a newer draft offered while the tab is the author's", () => {
  function deferred<T>() {
    let resolve!: (value: T) => void;
    const promise = new Promise<T>((res) => {
      resolve = res;
    });
    return { promise, resolve };
  }

  async function mountOffer(run: string) {
    findDraftBotSource.mockResolvedValue("v1");
    const id = useTabsStore.getState().openTab("editor", { draft: run }, "Draft");
    useTabsStore.getState().setActive(id);
    mount(id, run);
    await settle();
    const store = getOrCreateDocumentStore(id);
    store.getState().setCurrentSource("mine");

    findDraftBotSource.mockResolvedValue("v2");
    await turnLanded(run);
    expect(sourceOf(id)).toBe("mine");
    expect(screen.getByRole("button", { name: "Load it" })).toBeTruthy();
    return store;
  }

  async function mountSourceOffer(run: string, session: number) {
    const store = await mountOffer(run);
    const held = {
      path: null,
      rel: null,
      text: "typed but not applied",
      base: "mine",
      doc: store.getState().document,
      session,
    };
    store.getState().setSourceBuffer(held);
    return { store, held };
  }

  function confirmOffer() {
    fireEvent.click(screen.getByRole("button", { name: "Load it" }));
    expect(screen.getByRole("dialog", { name: "Load the latest draft?" })).toBeTruthy();
  }

  it("keeps the author's work and Source buffer, and parses nothing, when the confirmation is cancelled", async () => {
    const { store, held } = await mountSourceOffer("run-offer-cancel", 71);
    const before = store.getState().document;
    const replaced = store.getState()._replaced;
    confirmOffer();

    fireEvent.click(screen.getByRole("button", { name: "Cancel" }));
    await settle();

    expect(store.getState().currentSource).toBe("mine");
    expect(store.getState().document).toBe(before);
    expect(store.getState().sourceBuffer).toBe(held);
    expect(store.getState()._replaced).toBe(replaced);
    expect(parseSource).not.toHaveBeenCalledWith("v2");
    expect(screen.getByRole("button", { name: "Load it" })).toBeTruthy();
  });

  it("does not spend the replacement deadline while the confirmation is open", async () => {
    const store = await mountOffer("run-offer-deadline");
    confirmOffer();

    await settle(REPLACE_DEADLINE_MS);
    expect(store.getState()._pendingIntent).toBeNull();
    expect(screen.getByRole("dialog", { name: "Load the latest draft?" })).toBeTruthy();

    fireEvent.click(screen.getByRole("button", { name: "Load draft" }));
    await settle();
    expect(store.getState().currentSource).toBe("v2");
  });

  it("loads through the shared replacement path on confirmation, then follows again", async () => {
    const store = await mountOffer("run-offer-confirm");
    const replaced = store.getState()._replaced;
    confirmOffer();

    fireEvent.click(screen.getByRole("button", { name: "Load draft" }));
    await settle();

    expect(store.getState().currentSource).toBe("v2");
    expect(store.getState().document).toMatchObject({ source: "v2" });
    expect(store.getState()._replaced).toBe(replaced + 1);
    expect(store.getState()._pendingIntent).toBeNull();

    findDraftBotSource.mockResolvedValue("v3");
    await turnLanded("run-offer-confirm");
    expect(store.getState().currentSource).toBe("v3");
  });

  it("discards the dirty Source buffer once the confirmed draft lands, and follows without it", async () => {
    const { store } = await mountSourceOffer("run-offer-source", 72);
    confirmOffer();

    fireEvent.click(screen.getByRole("button", { name: "Load draft" }));
    await settle();

    expect(store.getState().currentSource).toBe("v2");
    expect(store.getState().sourceBuffer).toBeNull();

    findDraftBotSource.mockResolvedValue("v3");
    await turnLanded("run-offer-source");
    expect(store.getState().currentSource).toBe("v3");
    expect(store.getState().sourceBuffer).toBeNull();
  });

  it("does not overwrite an edit made during confirmation", async () => {
    const store = await mountOffer("run-offer-confirm-edit");
    confirmOffer();
    store.getState().setDocument({ ...createEmptyDocument(), comments: [{ text: "edited during confirmation" }] });
    const edited = store.getState().document;

    fireEvent.click(screen.getByRole("button", { name: "Load draft" }));
    await settle();

    expect(store.getState().document).toBe(edited);
    expect(store.getState().currentSource).toBe("mine");
  });

  it("does not overwrite a replacement asked for during confirmation", async () => {
    const store = await mountOffer("run-offer-confirm-replace");
    confirmOffer();
    const load = deferred<unknown>();
    const outcome = replaceDocument(
      store,
      "bots/q.bot",
      () => load.promise,
      (answer, st) => applyOpenedFile(answer as never, st),
    );

    fireEvent.click(screen.getByRole("button", { name: "Load draft" }));
    await settle();
    load.resolve({
      source: "Q\n",
      document: { ...createEmptyDocument(), comments: [{ text: "Q" }] },
      diagnostics: [],
      path: "bots/q.bot",
    });
    expect(await outcome).toBe("applied");
    await settle();

    expect(store.getState().currentFilePath).toBe("bots/q.bot");
    expect(store.getState().currentSource).toBe("Q\n");
  });

  it("does not overwrite a currentSource edit made while the manual load parses", async () => {
    const store = await mountOffer("run-offer-parse-edit");
    const parsed = deferred<unknown>();
    parseSource.mockImplementationOnce(() => parsed.promise);
    confirmOffer();
    fireEvent.click(screen.getByRole("button", { name: "Load draft" }));
    await settle();
    expect(parseSource).toHaveBeenLastCalledWith("v2", { signal: expect.any(AbortSignal) });

    store.getState().setCurrentSource("typed during parse");
    parsed.resolve({ document: { source: "v2" }, diagnostics: [] });
    await settle();

    expect(store.getState().currentSource).toBe("typed during parse");
  });

  it("keeps the dirty Source buffer when an edit during parse gets the replacement refused", async () => {
    const { store, held } = await mountSourceOffer("run-offer-parse-refused", 73);
    const parsed = deferred<unknown>();
    parseSource.mockImplementationOnce(() => parsed.promise);
    confirmOffer();
    fireEvent.click(screen.getByRole("button", { name: "Load draft" }));
    await settle();
    expect(parseSource).toHaveBeenLastCalledWith("v2", { signal: expect.any(AbortSignal) });

    store.getState().setDocument({ ...createEmptyDocument(), comments: [{ text: "edited during parse" }] });
    const edited = store.getState().document;
    parsed.resolve({ document: { source: "v2" }, diagnostics: [] });
    await settle();

    expect(store.getState().document).toBe(edited);
    expect(store.getState().currentSource).toBe("mine");
    expect(store.getState().sourceBuffer).toBe(held);
    expect(screen.getByRole("button", { name: "Load it" })).toBeTruthy();
  });

  it("does not overwrite a replacement asked for while the manual load parses", async () => {
    const store = await mountOffer("run-offer-parse-replace");
    const parsed = deferred<unknown>();
    parseSource.mockImplementationOnce(() => parsed.promise);
    confirmOffer();
    fireEvent.click(screen.getByRole("button", { name: "Load draft" }));
    await settle();
    expect(parseSource).toHaveBeenLastCalledWith("v2", { signal: expect.any(AbortSignal) });

    const load = deferred<unknown>();
    const outcome = replaceDocument(
      store,
      "bots/q.bot",
      () => load.promise,
      (answer, st) => applyOpenedFile(answer as never, st),
    );
    parsed.resolve({ document: { source: "v2" }, diagnostics: [] });
    await settle();
    load.resolve({
      source: "Q\n",
      document: { ...createEmptyDocument(), comments: [{ text: "Q" }] },
      diagnostics: [],
      path: "bots/q.bot",
    });
    expect(await outcome).toBe("applied");
    await settle();

    expect(store.getState().currentFilePath).toBe("bots/q.bot");
    expect(store.getState().currentSource).toBe("Q\n");
  });

  it("keeps the author's work, Source buffer and offer when the manual parse fails", async () => {
    const { store, held } = await mountSourceOffer("run-offer-failure", 74);
    const before = store.getState().document;
    parseSource.mockRejectedValueOnce(new Error("parse unavailable"));
    confirmOffer();

    fireEvent.click(screen.getByRole("button", { name: "Load draft" }));
    await settle();

    expect(store.getState().currentSource).toBe("mine");
    expect(store.getState().document).toBe(before);
    expect(store.getState().sourceBuffer).toBe(held);
    expect(store.getState()._pendingIntent).toBeNull();
    expect(screen.getByRole("button", { name: "Load it" })).toBeTruthy();
  });
});
