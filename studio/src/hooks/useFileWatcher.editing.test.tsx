// @vitest-environment jsdom
//
// The Source view's buffer is local to that component, so the store's
// _generation never moves and isDirty() cannot see an open edit. The
// watcher reads that to decide whether a file changed on disk may be
// reloaded automatically — and an auto-reload swaps the document AND the
// unit's revision, so the Apply that follows lands on top of whoever wrote
// the file meanwhile. The flag has to hold at BOTH sites that decide: the
// branch, and the 500 ms debounce that fires after it.
import { act, cleanup, render, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

const api = vi.hoisted(() => ({ openFile: vi.fn() }));
vi.mock("@/api/client", () => api);

// A set of handlers, as the real client keeps: every mounted tab listens.
const ws = vi.hoisted(() => {
  const handlers = new Set<(e: unknown) => void>();
  return {
    fileWatcher: {
      connect: vi.fn(),
      disconnect: vi.fn(),
      subscribe: vi.fn((fn: (e: unknown) => void) => {
        handlers.add(fn);
        return () => {
          handlers.delete(fn);
        };
      }),
    },
    emit: (e: unknown) => {
      for (const h of [...handlers]) h(e);
    },
  };
});
vi.mock("@/api/ws", () => ({ fileWatcher: ws.fileWatcher }));

import { createEmptyDocument } from "@/lib/defaults";
import { DocumentStoreProvider, createDocumentStore, type DocumentStore } from "@/store/document";
import { useFileWatcher } from "./useFileWatcher";
import { REPLACE_DEADLINE_MS, replaceDocument } from "@/lib/replaceDocument";
import { applyOpenedFile } from "@/lib/openedFile";
import { useUIStore } from "@/store/ui";

function Watcher() {
  useFileWatcher();
  return null;
}

function boundStore(): DocumentStore {
  const store = createDocumentStore();
  store.getState().setDocument(createEmptyDocument());
  store.getState().setCurrentFilePath("bots/demo/main.bot");
  store.getState().markSaved();
  return store;
}

function mount(store: DocumentStore) {
  return render(
    <DocumentStoreProvider store={store}>
      <Watcher />
    </DocumentStoreProvider>,
  );
}

// Longer than the watcher's 500 ms debounce.
const RELOAD_WINDOW_MS = 700;

beforeEach(() => {
  vi.resetAllMocks();
  useUIStore.setState({ toasts: [] });
  api.openFile.mockResolvedValue({
    source: "",
    document: createEmptyDocument(),
    diagnostics: [],
    path: "bots/demo/main.bot",
  });
});

afterEach(cleanup);

describe("the watcher against an open Source-view edit", () => {
  // The control, and it has to be here: without it every assertion below
  // would pass on a watcher that never reloads anything.
  it("reloads a clean buffer when the file changes on disk", async () => {
    mount(boundStore());
    ws.emit({ type: "file_modified", path: "bots/demo/main.bot" });
    await waitFor(() => expect(api.openFile).toHaveBeenCalledTimes(1), { timeout: 2000 });
  });

  it("does not reload when an edit is already open", async () => {
    const store = boundStore();
    store.getState().setSourceBuffer({ path: "bots/demo/main.bot", rel: null, text: "x", base: "x",
      doc: null,
      session: 1,
    });
    mount(store);
    ws.emit({ type: "file_modified", path: "bots/demo/main.bot" });
    await new Promise((r) => setTimeout(r, 900));
    expect(api.openFile).not.toHaveBeenCalled();
  });

  // The site the first fix missed: the author opens the edit AFTER the
  // event arrives and before the debounce fires.
  it("does not reload when the edit opens inside the debounce window, and offers the change", async () => {
    const store = boundStore();
    mount(store);
    ws.emit({ type: "file_modified", path: "bots/demo/main.bot" });
    await new Promise((r) => setTimeout(r, 100));
    store.getState().setSourceBuffer({ path: "bots/demo/main.bot", rel: null, text: "x", base: "x",
      doc: null,
      session: 1,
    });
    await new Promise((r) => setTimeout(r, 900));
    expect(api.openFile).not.toHaveBeenCalled();
    expect(useUIStore.getState().toasts.map((t) => t.message)).toContain("bots/demo/main.bot changed externally");
  });

  it("offers the change when a canvas edit lands inside the debounce window", async () => {
    const store = boundStore();
    mount(store);
    ws.emit({ type: "file_modified", path: "bots/demo/main.bot" });
    await new Promise((r) => setTimeout(r, 100));
    act(() => store.getState().addComment({ text: "canvas edit" }));
    await new Promise((r) => setTimeout(r, 900));
    expect(api.openFile).not.toHaveBeenCalled();
    const shown = useUIStore.getState().toasts;
    expect(shown.map((t) => t.message)).toContain("bots/demo/main.bot changed externally");
    expect(shown[shown.length - 1]?.action?.label).toBe("Reload and discard");
  });
});

// The third window, after the branch and the debounce: the author opens the
// edit while `openFile` is in flight. Applying the answer then swaps the
// document and the revision under what they are typing, and
// `applyOpenedFile` → `setCurrentFilePath` drops the buffer — so the text
// goes invisible to every discard path AND the next Apply is refused as
// stale, with the repair stranded.
describe("an edit opened while the reload is in flight", () => {
  it("is not applied over, and the change is offered instead", async () => {
    const store = boundStore();
    let release!: (v: unknown) => void;
    api.openFile.mockReturnValue(new Promise((r) => { release = r; }));
    mount(store);

    ws.emit({ type: "file_modified", path: "bots/demo/main.bot" });
    await waitFor(() => expect(api.openFile).toHaveBeenCalledTimes(1));

    // The author starts typing while the answer is on the wire.
    store.getState().setSourceBuffer({
      path: "bots/demo/main.bot",
      rel: null,
      text: "a repair the author typed",
      base: "workflow w:\n  entry: a\n",
      doc: null,
      session: 1,
    });
    release({ path: "bots/demo/main.bot", document: createEmptyDocument(), source: "" });
    await new Promise((r) => setTimeout(r, 50));

    // The buffer is still there, and still visible to every discard path.
    expect(store.getState().sourceBuffer).not.toBeNull();
    expect(store.getState().hasUnsavedWork()).toBe(true);
    // …and the author is told, with a way to take the change when ready.
    const toasts = useUIStore.getState().toasts;
    expect(toasts.map((t) => t.message)).toContain("bots/demo/main.bot changed externally");
    // The label says what the action takes: it applies the reload without a
    // second prompt, so a neutral "Reload" would be one click from the loss.
    expect(toasts[toasts.length - 1]?.action?.label).toBe("Reload and discard");
  });

  it("still applies a reload that lands on a clean buffer", async () => {
    const store = boundStore();
    let release!: (v: unknown) => void;
    api.openFile.mockReturnValue(new Promise((r) => { release = r; }));
    mount(store);

    ws.emit({ type: "file_modified", path: "bots/demo/main.bot" });
    await waitFor(() => expect(api.openFile).toHaveBeenCalledTimes(1));
    release({ path: "bots/demo/main.bot", document: createEmptyDocument(), source: "" });

    await waitFor(() =>
      expect(useUIStore.getState().toasts.map((t) => t.message)).toContain("File reloaded"),
    );
  });
});

// The FIRST window — the edit was already open when the external write
// landed — offers the same one-click reload. It applies without a second
// prompt, so its label owes the same sentence as the third window's.
describe("the toast raised when the edit was already open", () => {
  it("names what its action takes", async () => {
    const store = boundStore();
    store.getState().setSourceBuffer({
      path: "bots/demo/main.bot",
      rel: null,
      text: "a repair the author typed",
      base: "workflow w:\n  entry: a\n",
      doc: null,
      session: 1,
    });
    mount(store);
    ws.emit({ type: "file_modified", path: "bots/demo/main.bot" });

    await waitFor(() =>
      expect(useUIStore.getState().toasts.map((t) => t.message)).toContain(
        "bots/demo/main.bot changed externally",
      ),
    );
    const toasts = useUIStore.getState().toasts;
    expect(toasts[toasts.length - 1]?.action?.label).toBe("Reload and discard");
    expect(api.openFile).not.toHaveBeenCalled();
  });

  it("says a reload discards unsaved canvas edits too", async () => {
    // Clicking it replaces the document with the file on disk, which takes
    // the canvas's unsaved edits as surely as the Source view's text: a
    // neutral "Reload" was one click from a loss it did not name.
    const store = boundStore();
    store.getState().addAgent({
      name: "a",
      model: "m",
      input: "in",
      output: "out",
      system: "s",
      user: "u",
      session: "fresh",
    });
    mount(store);
    ws.emit({ type: "file_modified", path: "bots/demo/main.bot" });

    await waitFor(() =>
      expect(useUIStore.getState().toasts.map((t) => t.message)).toContain(
        "bots/demo/main.bot changed externally",
      ),
    );
    const toasts = useUIStore.getState().toasts;
    expect(toasts[toasts.length - 1]?.action?.label).toBe("Reload and discard");
  });

  it("keeps the neutral label when a reload would take nothing", async () => {
    // An open Source edit nobody has typed in holds the automatic reload
    // back (the gate is its presence), but reloading loses nothing.
    const store = boundStore();
    store.getState().setSourceBuffer({
      path: "bots/demo/main.bot",
      rel: null,
      text: "rendered",
      base: "rendered",
      doc: null,
      session: 1,
    });
    mount(store);
    ws.emit({ type: "file_modified", path: "bots/demo/main.bot" });

    await waitFor(() =>
      expect(useUIStore.getState().toasts.map((t) => t.message)).toContain(
        "bots/demo/main.bot changed externally",
      ),
    );
    const toasts = useUIStore.getState().toasts;
    expect(toasts[toasts.length - 1]?.action?.label).toBe("Reload");
  });
});

describe("the manual reload keeps what it was offered for", () => {
  const cleanEdit = {
    path: "bots/demo/main.bot",
    rel: null,
    text: "rendered",
    base: "rendered",
    doc: null,
    session: 1,
  };
  const offered = async () => {
    await waitFor(() =>
      expect(useUIStore.getState().toasts.map((t) => t.message)).toContain("bots/demo/main.bot changed externally"),
    );
    const toasts = useUIStore.getState().toasts;
    const action = toasts[toasts.length - 1]?.action;
    if (!action) throw new Error("no reload was offered");
    return action;
  };

  it("does not reload a file the tab has left since", async () => {
    const store = boundStore();
    store.getState().setSourceBuffer({ ...cleanEdit, text: "typed" });
    mount(store);
    ws.emit({ type: "file_modified", path: "bots/demo/main.bot" });
    const reload = await offered();

    act(() => store.getState().setCurrentFilePath("bots/other.bot"));
    api.openFile.mockClear();
    act(() => reload.onClick());
    expect(api.openFile).not.toHaveBeenCalled();
    expect(useUIStore.getState().toasts.map((t) => t.message)).toContain(
      "main.bot is no longer open in this tab, so it was not reloaded.",
    );
  });

  it("offers again with an accurate label when more work appeared since it was shown", async () => {
    const store = boundStore();
    store.getState().setSourceBuffer(cleanEdit);
    mount(store);
    ws.emit({ type: "file_modified", path: "bots/demo/main.bot" });
    const reload = await offered();
    expect(reload.label).toBe("Reload");

    act(() => store.getState().setSourceBuffer({ ...cleanEdit, text: "typed after the toast" }));
    act(() => reload.onClick());
    expect(api.openFile).not.toHaveBeenCalled();
    const again = useUIStore.getState().toasts;
    expect(again[again.length - 1]?.action?.label).toBe("Reload and discard");
    expect(store.getState().sourceBuffer?.text).toBe("typed after the toast");
  });

  it("offers again with an accurate label when the work it named has gone", async () => {
    const store = boundStore();
    store.getState().setSourceBuffer({ ...cleanEdit, text: "typed" });
    mount(store);
    ws.emit({ type: "file_modified", path: "bots/demo/main.bot" });
    const reload = await offered();
    expect(reload.label).toBe("Reload and discard");

    // The author discards what they typed before clicking.
    act(() => store.getState().setSourceBuffer(null));
    act(() => reload.onClick());
    expect(api.openFile).not.toHaveBeenCalled();
    const again = useUIStore.getState().toasts;
    expect(again[again.length - 1]?.action?.label).toBe("Reload");
  });

  it("offers again, naming the failure, when its read fails", async () => {
    const store = boundStore();
    store.getState().setSourceBuffer(cleanEdit);
    mount(store);
    ws.emit({ type: "file_modified", path: "bots/demo/main.bot" });
    const reload = await offered();

    api.openFile.mockRejectedValueOnce(new Error("503: the server is busy"));
    await act(async () => reload.onClick());
    const shown = useUIStore.getState().toasts;
    const failure = shown.find((t) => t.message.startsWith("Failed to reload main.bot"));
    expect(failure?.message).toContain("503");
    expect(failure?.action?.label).toBe("Reload");
  });

  it("refuses its answer when the author types while it fetches", async () => {
    const store = boundStore();
    store.getState().setSourceBuffer(cleanEdit);
    mount(store);
    ws.emit({ type: "file_modified", path: "bots/demo/main.bot" });
    const reload = await offered();

    let land!: (v: unknown) => void;
    api.openFile.mockReturnValue(new Promise((r) => (land = r)));
    act(() => reload.onClick());
    await waitFor(() => expect(api.openFile).toHaveBeenCalledTimes(1));
    expect(api.openFile).toHaveBeenCalledWith("bots/demo/main.bot", { signal: expect.any(AbortSignal) });
    act(() => store.getState().setSourceBuffer({ ...cleanEdit, text: "typed during the fetch" }));

    await act(async () =>
      land({
        source: "",
        document: { ...createEmptyDocument(), comments: [{ text: "reloaded" }] },
        diagnostics: [],
        path: "bots/demo/main.bot",
      }),
    );
    expect(store.getState().sourceBuffer?.text).toBe("typed during the fetch");
    expect(store.getState().document?.comments?.[0]?.text).toBeUndefined();
  });
});

describe("the automatic reload, while the author's own replacement is pending", () => {
  it("does not start", async () => {
    const store = boundStore();
    mount(store);
    const intent = store.getState().beginReplace();
    ws.emit({ type: "file_modified", path: "bots/demo/main.bot" });
    await new Promise((r) => setTimeout(r, 700));
    expect(api.openFile).not.toHaveBeenCalled();
    store.getState().endReplace(intent);
  });

  it("offers the reload once the replacement has ended, not while it is pending", async () => {
    // Dirty, so the event is offered rather than reloaded — after the
    // author's replacement ends, against the tab as it left it.
    const store = boundStore();
    store.getState().setSourceBuffer({
      path: "bots/demo/main.bot",
      rel: null,
      text: "typed",
      base: "rendered",
      doc: null,
      session: 1,
    });
    mount(store);
    const intent = store.getState().beginReplace();
    ws.emit({ type: "file_modified", path: "bots/demo/main.bot" });
    await new Promise((r) => setTimeout(r, 50));
    expect(useUIStore.getState().toasts.map((t) => t.message)).not.toContain("bots/demo/main.bot changed externally");
    act(() => store.getState().endReplace(intent));
    expect(useUIStore.getState().toasts.map((t) => t.message)).toContain("bots/demo/main.bot changed externally");
  });

  it("does not land an answer that arrives once one became pending", async () => {
    const store = boundStore();
    let land!: (v: unknown) => void;
    api.openFile.mockReturnValue(new Promise((r) => (land = r)));
    mount(store);
    ws.emit({ type: "file_modified", path: "bots/demo/main.bot" });
    await waitFor(() => expect(api.openFile).toHaveBeenCalledTimes(1), { timeout: 2000 });
    const before = store.getState().document;
    const intent = store.getState().beginReplace();

    await act(async () =>
      land({
        source: "",
        document: { ...createEmptyDocument(), comments: [{ text: "background" }] },
        diagnostics: [],
        path: "bots/demo/main.bot",
      }),
    );
    expect(store.getState().document).toBe(before);
    expect(useUIStore.getState().toasts.map((t) => t.message)).not.toContain("File reloaded");
    store.getState().endReplace(intent);
  });
});

describe("an event that arrives while the author's replacement is pending", () => {
  // It waits for that replacement to end and is then read against the tab
  // as it was left: a replacement that failed, was refused, or read the file
  // before this write leaves the tab on the file the event is about.
  function deferred<T>() {
    let resolve!: (v: T) => void;
    let reject!: (e: unknown) => void;
    const promise = new Promise<T>((res, rej) => {
      resolve = res;
      reject = rej;
    });
    return { promise, resolve, reject };
  }
  const onScreen = (store: DocumentStore) => store.getState().document?.comments?.[0]?.text;
  const sleep = (ms: number) => new Promise((r) => setTimeout(r, ms));
  function shownStore() {
    const store = boundStore();
    store.getState().setDocument({ ...createEmptyDocument(), comments: [{ text: "v0 on screen" }] });
    store.getState().markSaved();
    return store;
  }
  beforeEach(() => {
    api.openFile.mockResolvedValue({
      source: "v1\n",
      document: { ...createEmptyDocument(), comments: [{ text: "v1 on disk" }] },
      diagnostics: [],
      path: "bots/demo/main.bot",
    });
  });

  it("reloads the write once an Open that FAILED has ended", async () => {
    const store = shownStore();
    mount(store);
    const load = deferred<never>();
    const outcome = replaceDocument(store, "bots/other.bot", () => load.promise, () => {});
    ws.emit({ type: "file_modified", path: "bots/demo/main.bot" });
    await sleep(700);
    expect(api.openFile).not.toHaveBeenCalled();
    load.reject(new Error("404: file not found"));
    await expect(outcome).rejects.toThrow("404");
    await waitFor(() => expect(onScreen(store)).toBe("v1 on disk"), { timeout: 2000 });
  });

  it("tells a dirty tab once an Open that was REFUSED has ended", async () => {
    const store = shownStore();
    mount(store);
    const load = deferred<unknown>();
    const outcome = replaceDocument(store, "bots/other.bot", () => load.promise, () => {});
    act(() => store.getState().addComment({ text: "an edit during the load" }));
    ws.emit({ type: "file_modified", path: "bots/demo/main.bot" });
    load.resolve({});
    expect(await outcome).toBe("refused");
    await waitFor(() =>
      expect(useUIStore.getState().toasts.map((t) => t.message)).toContain("bots/demo/main.bot changed externally"),
    );
  });

  it("reloads a write that landed during a manual reload of the same file, after it", async () => {
    const store = shownStore();
    store.getState().setSourceBuffer({ path: "bots/demo/main.bot", rel: null, text: "r", base: "r", doc: null, session: 1 });
    mount(store);
    ws.emit({ type: "file_modified", path: "bots/demo/main.bot" });
    await waitFor(() =>
      expect(useUIStore.getState().toasts.map((t) => t.message)).toContain("bots/demo/main.bot changed externally"),
    );
    const shown = useUIStore.getState().toasts;
    const action = shown[shown.length - 1]?.action;
    if (!action) throw new Error("no reload was offered");
    const stale = deferred<unknown>();
    api.openFile.mockReturnValueOnce(stale.promise);
    act(() => action.onClick());
    await waitFor(() => expect(api.openFile).toHaveBeenCalledTimes(1));
    // A second write lands while the reload is still reading the first.
    ws.emit({ type: "file_modified", path: "bots/demo/main.bot" });
    await act(async () =>
      stale.resolve({
        source: "v1\n",
        document: { ...createEmptyDocument(), comments: [{ text: "v1 (read before the second write)" }] },
        diagnostics: [],
        path: "bots/demo/main.bot",
      }),
    );
    await waitFor(() => expect(onScreen(store)).toBe("v1 on disk"), { timeout: 2000 });
    expect(api.openFile).toHaveBeenCalledTimes(2);
  });

  it("still does not get in the way of an Open that lands (why it waits at all)", async () => {
    const store = shownStore();
    mount(store);
    const load = deferred<unknown>();
    const outcome = replaceDocument(store, "bots/other.bot", () => load.promise, (r, st) =>
      applyOpenedFile(r as never, st),
    );
    ws.emit({ type: "file_modified", path: "bots/demo/main.bot" });
    await sleep(700);
    load.resolve({
      source: "O\n",
      document: { ...createEmptyDocument(), comments: [{ text: "other" }] },
      diagnostics: [],
      path: "bots/other.bot",
    });
    expect(await outcome).toBe("applied");
    await sleep(900);
    expect(api.openFile).not.toHaveBeenCalled();
    expect(store.getState().currentFilePath).toBe("bots/other.bot");
    expect(useUIStore.getState().toasts.map((t) => t.message)).toEqual([]);
  });
});

describe("an event whose reload the author's replacement overtook", () => {
  function deferred<T>() {
    let resolve!: (v: T) => void;
    let reject!: (e: unknown) => void;
    const promise = new Promise<T>((res, rej) => {
      resolve = res;
      reject = rej;
    });
    return { promise, resolve, reject };
  }
  const onScreen = (store: DocumentStore) => store.getState().document?.comments?.[0]?.text;
  const v1 = {
    source: "v1\n",
    document: { ...createEmptyDocument(), comments: [{ text: "v1 on disk" }] },
    diagnostics: [],
    path: "bots/demo/main.bot",
  };

  it("inside the debounce: reloads once a replacement that FAILED has ended", async () => {
    const store = boundStore();
    api.openFile.mockResolvedValue(v1);
    mount(store);
    ws.emit({ type: "file_modified", path: "bots/demo/main.bot" });
    // The author asks for another file before the 500 ms debounce fires.
    const load = deferred<never>();
    const outcome = replaceDocument(store, "bots/other.bot", () => load.promise, () => {});
    await new Promise((r) => setTimeout(r, 700));
    expect(api.openFile).not.toHaveBeenCalled();
    load.reject(new Error("404: file not found"));
    await expect(outcome).rejects.toThrow("404");
    await waitFor(() => expect(onScreen(store)).toBe("v1 on disk"), { timeout: 2000 });
  });

  it("after its request: reloads once a replacement that FAILED has ended", async () => {
    const store = boundStore();
    const first = deferred<unknown>();
    api.openFile.mockReturnValueOnce(first.promise).mockResolvedValue(v1);
    mount(store);
    ws.emit({ type: "file_modified", path: "bots/demo/main.bot" });
    await waitFor(() => expect(api.openFile).toHaveBeenCalledTimes(1), { timeout: 2000 });
    const load = deferred<never>();
    const outcome = replaceDocument(store, "bots/other.bot", () => load.promise, () => {});
    await act(async () => first.resolve(v1));
    expect(onScreen(store)).toBeUndefined();
    load.reject(new Error("404: file not found"));
    await expect(outcome).rejects.toThrow("404");
    await waitFor(() => expect(onScreen(store)).toBe("v1 on disk"), { timeout: 2000 });
  });

  it("does nothing once the watcher that deferred it has gone away", async () => {
    const store = boundStore();
    api.openFile.mockResolvedValue(v1);
    const view = mount(store);
    const load = deferred<never>();
    const outcome = replaceDocument(store, "bots/other.bot", () => load.promise, () => {});
    ws.emit({ type: "file_modified", path: "bots/demo/main.bot" });
    view.unmount();
    load.reject(new Error("404: file not found"));
    await expect(outcome).rejects.toThrow("404");
    await new Promise((r) => setTimeout(r, 900));
    expect(api.openFile).not.toHaveBeenCalled();
  });
});

describe("the manual reload's label", () => {
  it("proceeds when more was typed under a label that already said it discards", async () => {
    const store = boundStore();
    store.getState().setSourceBuffer({
      path: "bots/demo/main.bot",
      rel: null,
      text: "typed",
      base: "rendered",
      doc: null,
      session: 1,
    });
    mount(store);
    ws.emit({ type: "file_modified", path: "bots/demo/main.bot" });
    await waitFor(() =>
      expect(useUIStore.getState().toasts.map((t) => t.message)).toContain("bots/demo/main.bot changed externally"),
    );
    const shown = useUIStore.getState().toasts;
    const action = shown[shown.length - 1]?.action;
    if (!action) throw new Error("no reload was offered");
    expect(action.label).toBe("Reload and discard");

    const held = store.getState().sourceBuffer;
    if (!held) throw new Error("the buffer went away");
    act(() => store.getState().setSourceBuffer({ ...held, text: "typed more" }));
    act(() => action.onClick());
    await waitFor(() => expect(api.openFile).toHaveBeenCalledTimes(1));
  });
});

describe("an event about the very file an Open is bringing in", () => {
  // The tab still shows A while B loads, so B is not yet the file its path
  // names: the event waits for the Open, then reads the tab it left.
  const onA = () => {
    const store = createDocumentStore();
    store.getState().setDocument(createEmptyDocument());
    store.getState().setCurrentFilePath("bots/a.bot");
    store.getState().markSaved();
    return store;
  };
  const b = (marker: string) => ({
    source: `${marker}\n`,
    document: { ...createEmptyDocument(), comments: [{ text: marker }] },
    diagnostics: [],
    path: "bots/b.bot",
  });

  it("reloads B once its Open lands, when B changed while it was read", async () => {
    const store = onA();
    mount(store);
    let landOpen!: (v: unknown) => void;
    const outcome = replaceDocument(
      store,
      "b.bot",
      () => new Promise((r) => (landOpen = r)),
      (r, st) => applyOpenedFile(r as never, st),
    );
    ws.emit({ type: "file_modified", path: "bots/b.bot" });
    api.openFile.mockResolvedValueOnce(b("B-fresh"));
    await act(async () => landOpen(b("B-stale")));
    expect(await outcome).toBe("applied");
    await waitFor(() => expect(store.getState().document?.comments?.[0]?.text).toBe("B-fresh"), { timeout: 2000 });
    expect(api.openFile).toHaveBeenCalledWith("bots/b.bot");
  });

  it("says B was deleted once its Open lands, when B was deleted while it was read", async () => {
    const store = onA();
    mount(store);
    let landOpen!: (v: unknown) => void;
    const outcome = replaceDocument(
      store,
      "b.bot",
      () => new Promise((r) => (landOpen = r)),
      (r, st) => applyOpenedFile(r as never, st),
    );
    ws.emit({ type: "file_deleted", path: "bots/b.bot" });
    expect(useUIStore.getState().toasts.map((t) => t.message)).not.toContain("bots/b.bot was deleted externally");
    await act(async () => landOpen(b("B")));
    expect(await outcome).toBe("applied");
    expect(useUIStore.getState().toasts.map((t) => t.message)).toContain("bots/b.bot was deleted externally");
  });
});

describe("a manual reload refused because the author typed while it read", () => {
  it("offers the reload again, with a label that says it discards, and nothing about opening", async () => {
    const store = boundStore();
    store.getState().setSourceBuffer({
      path: "bots/demo/main.bot",
      rel: null,
      text: "rendered",
      base: "rendered",
      doc: null,
      session: 1,
    });
    mount(store);
    ws.emit({ type: "file_modified", path: "bots/demo/main.bot" });
    await waitFor(() =>
      expect(useUIStore.getState().toasts.map((t) => t.message)).toContain("bots/demo/main.bot changed externally"),
    );
    const first = useUIStore.getState().toasts;
    const reload = first[first.length - 1]?.action;
    if (!reload) throw new Error("no reload was offered");
    expect(reload.label).toBe("Reload");

    let land!: (v: unknown) => void;
    api.openFile.mockReturnValueOnce(new Promise((r) => (land = r)));
    act(() => reload.onClick());
    await waitFor(() => expect(api.openFile).toHaveBeenCalledTimes(1));
    const held = store.getState().sourceBuffer;
    if (!held) throw new Error("the buffer went away");
    act(() => store.getState().setSourceBuffer({ ...held, text: "typed during the read" }));
    await act(async () =>
      land({ source: "", document: createEmptyDocument(), diagnostics: [], path: "bots/demo/main.bot" }),
    );

    const after = useUIStore.getState().toasts;
    expect(after.map((t) => t.message).join(" | ")).not.toContain("was opening");
    const again = after.filter((t) => t.message === "bots/demo/main.bot changed externally");
    expect(again).toHaveLength(1);
    expect(again[0]?.action?.label).toBe("Reload and discard");
    expect(store.getState().sourceBuffer?.text).toBe("typed during the read");
  });
});

describe("offers from two tabs", () => {
  const dirtyOn = (path: string) => {
    const store = createDocumentStore();
    store.getState().setDocument(createEmptyDocument());
    store.getState().setCurrentFilePath(path);
    store.getState().markSaved();
    store.getState().addComment({ text: "unsaved" });
    return store;
  };
  const opened = (path: string) => ({
    source: "FRESH\n",
    document: { ...createEmptyDocument(), comments: [{ text: `${path} FRESH` }] },
    diagnostics: [],
    path,
  });

  it("each keep their own, and each reloads its own tab", async () => {
    const a = dirtyOn("bots/a.bot");
    const b = dirtyOn("bots/b.bot");
    mount(a);
    mount(b);
    ws.emit({ type: "file_modified", path: "bots/a.bot" });
    ws.emit({ type: "file_modified", path: "bots/b.bot" });
    const offers = useUIStore.getState().toasts.filter((t) => t.action?.label === "Reload and discard");
    expect(offers.map((t) => t.message).sort()).toEqual([
      "bots/a.bot changed externally",
      "bots/b.bot changed externally",
    ]);

    api.openFile.mockImplementation(async (path: string) => opened(path));
    for (const offer of offers) await act(async () => offer.action?.onClick());
    expect(a.getState().document?.comments?.[0]?.text).toBe("bots/a.bot FRESH");
    expect(b.getState().document?.comments?.[0]?.text).toBe("bots/b.bot FRESH");
  });

  it("each keep their own when both tabs show the same file", async () => {
    const one = dirtyOn("bots/x.bot");
    const two = dirtyOn("bots/x.bot");
    mount(one);
    mount(two);
    ws.emit({ type: "file_modified", path: "bots/x.bot" });
    const offers = useUIStore.getState().toasts.filter((t) => t.message === "bots/x.bot changed externally");
    expect(offers).toHaveLength(2);

    api.openFile.mockImplementation(async (path: string) => opened(path));
    for (const offer of offers) await act(async () => offer.action?.onClick());
    expect(one.getState().document?.comments?.[0]?.text).toBe("bots/x.bot FRESH");
    expect(two.getState().document?.comments?.[0]?.text).toBe("bots/x.bot FRESH");
  });
});

describe("an event waiting behind an Open the server never answers", () => {
  afterEach(() => vi.useRealTimers());

  it("is reloaded once that Open's deadline has passed", async () => {
    vi.useFakeTimers();
    const store = boundStore();
    mount(store);
    const hung = replaceDocument(store, "bots/other.bot", () => new Promise<never>(() => {}), () => {}).catch(
      (err: unknown) => err,
    );
    ws.emit({ type: "file_modified", path: "bots/demo/main.bot" });
    await vi.advanceTimersByTimeAsync(RELOAD_WINDOW_MS);
    expect(api.openFile).not.toHaveBeenCalled();

    await vi.advanceTimersByTimeAsync(REPLACE_DEADLINE_MS);
    expect(await hung).toBeInstanceOf(Error);
    await vi.advanceTimersByTimeAsync(RELOAD_WINDOW_MS);
    expect(api.openFile).toHaveBeenCalledWith("bots/demo/main.bot");
  });
});

describe("a manual reload that never answers, superseded by an Open of the same file", () => {
  afterEach(() => vi.useRealTimers());

  it("offers nothing when its deadline passes: the Open spoke for the author", async () => {
    const store = boundStore();
    store.getState().setSourceBuffer({
      path: "bots/demo/main.bot",
      rel: null,
      text: "rendered",
      base: "rendered",
      doc: null,
      session: 1,
    });
    mount(store);
    ws.emit({ type: "file_modified", path: "bots/demo/main.bot" });
    await waitFor(() =>
      expect(useUIStore.getState().toasts.map((t) => t.message)).toContain("bots/demo/main.bot changed externally"),
    );
    const shown = useUIStore.getState().toasts;
    const reload = shown[shown.length - 1]?.action;
    if (!reload) throw new Error("no reload was offered");

    vi.useFakeTimers();
    api.openFile.mockReturnValueOnce(new Promise(() => {}));
    act(() => reload.onClick());
    act(() => useUIStore.setState({ toasts: [] }));
    const fresh = {
      source: "FRESH\n",
      document: { ...createEmptyDocument(), comments: [{ text: "FRESH" }] },
      diagnostics: [],
      path: "bots/demo/main.bot",
    };
    expect(
      await replaceDocument(store, "main.bot", async () => fresh, (r, st) => applyOpenedFile(r as never, st)),
    ).toBe("applied");
    act(() => store.getState().addComment({ text: "the author's edit" }));

    await vi.advanceTimersByTimeAsync(REPLACE_DEADLINE_MS);
    expect(useUIStore.getState().toasts.map((t) => t.message).join(" | ")).not.toContain("Failed to reload");
  });
});

describe("a deletion seen by two tabs", () => {
  it("names the file in each tab's message", async () => {
    const onFile = (path: string) => {
      const store = createDocumentStore();
      store.getState().setDocument(createEmptyDocument());
      store.getState().setCurrentFilePath(path);
      store.getState().markSaved();
      return store;
    };
    mount(onFile("bots/a.bot"));
    mount(onFile("bots/b.bot"));
    ws.emit({ type: "file_deleted", path: "bots/a.bot" });
    ws.emit({ type: "file_deleted", path: "bots/b.bot" });
    const shown = useUIStore.getState().toasts.map((t) => t.message).sort();
    expect(shown).toEqual(["bots/a.bot was deleted externally", "bots/b.bot was deleted externally"]);
  });
});
