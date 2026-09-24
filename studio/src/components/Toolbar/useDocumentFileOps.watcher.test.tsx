// @vitest-environment jsdom
//
// The watcher's AUTOMATIC reload is an async answer too. Its request is made
// on a clean tab; when the author edits and saves before it answers — or the
// assistant writes and reloads the tab — the tab is clean again at the
// answer, and a read that predates the save would land over it, marked saved.
// The server does not tell the studio about its own writes, so nothing would
// read the file again: the next save would write the stale reading back.
import { act, cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

const api = vi.hoisted(() => ({
  parseSource: vi.fn(),
  unparse: vi.fn(),
  unparseUnitFile: vi.fn(),
  saveFile: vi.fn(),
  openFile: vi.fn(),
  loadExample: vi.fn(),
  validate: vi.fn(),
}));
vi.mock("@/api/client", async (importOriginal) => ({
  ...(await importOriginal<typeof import("@/api/client")>()),
  ...api,
}));
const ws = vi.hoisted(() => {
  let handler: ((e: unknown) => void) | null = null;
  return {
    fileWatcher: {
      connect: vi.fn(),
      disconnect: vi.fn(),
      subscribe: vi.fn((fn: (e: unknown) => void) => {
        handler = fn;
        return () => {
          handler = null;
        };
      }),
    },
    emit: (e: unknown) => handler?.(e),
  };
});
vi.mock("@/api/ws", () => ({ fileWatcher: ws.fileWatcher }));

import { createEmptyDocument } from "@/lib/defaults";
import { applyOpenedFile } from "@/lib/openedFile";
import { DocumentStoreProvider, createDocumentStore } from "@/store/document";
import { useFileWatcher } from "@/hooks/useFileWatcher";
import { useUIStore } from "@/store/ui";
import { useDocumentFileOps } from "./useDocumentFileOps";

function deferred<T>() {
  let resolve!: (v: T) => void;
  let reject!: (e: unknown) => void;
  const promise = new Promise<T>((res, rej) => {
    resolve = res;
    reject = rej;
  });
  return { promise, resolve, reject };
}
const opened = (path: string, marker: string) => ({
  source: `${marker}\n`,
  document: { ...createEmptyDocument(), comments: [{ text: marker }] },
  diagnostics: [],
  path,
});

function Harness() {
  useFileWatcher();
  const ops = useDocumentFileOps({ confirm: async () => true });
  return <button onClick={() => void ops.handleSave()}>Save</button>;
}

function tab() {
  const store = createDocumentStore();
  store.getState().setDocument({ ...createEmptyDocument(), comments: [{ text: "D0" }] });
  store.getState().setCurrentFilePath("bots/start.bot");
  store.getState().setCurrentSource("D0\n");
  store.getState().markSaved();
  render(
    <DocumentStoreProvider store={store}>
      <Harness />
    </DocumentStoreProvider>,
  );
  return store;
}
const markers = (store: ReturnType<typeof createDocumentStore>) =>
  (store.getState().document?.comments ?? []).map((c) => c.text).join("+");
const toasts = () => useUIStore.getState().toasts.map((t) => t.message).join(" | ");

beforeEach(() => {
  vi.resetAllMocks();
  useUIStore.setState({ toasts: [] });
});
afterEach(cleanup);

describe("an automatic reload whose read predates what the tab shows", () => {
  it("lands on a tab nobody touched", async () => {
    const store = tab();
    const read = deferred<ReturnType<typeof opened>>();
    api.openFile.mockReturnValueOnce(read.promise);
    ws.emit({ type: "file_modified", path: "bots/start.bot" });
    await waitFor(() => expect(api.openFile).toHaveBeenCalledTimes(1), { timeout: 2000 });
    await act(async () => read.resolve(opened("bots/start.bot", "W")));
    expect(markers(store)).toBe("W");
    expect(toasts()).toContain("File reloaded");
  });

  it("does not land over an edit saved while it read, and reads the file again", async () => {
    const store = tab();
    const read = deferred<ReturnType<typeof opened>>();
    api.openFile.mockReturnValueOnce(read.promise);
    ws.emit({ type: "file_modified", path: "bots/start.bot" });
    await waitFor(() => expect(api.openFile).toHaveBeenCalledTimes(1), { timeout: 2000 });

    act(() => store.getState().addComment({ text: "EDIT" }));
    api.saveFile.mockResolvedValueOnce({ path: "bots/start.bot", source: "D0+EDIT\n" });
    fireEvent.click(screen.getByRole("button", { name: "Save" }));
    await waitFor(() => expect(store.getState().isDirty()).toBe(false));

    // The second read finds what the save wrote.
    api.openFile.mockResolvedValueOnce({
      source: "D0+EDIT\n",
      document: { ...createEmptyDocument(), comments: [{ text: "D0" }, { text: "EDIT" }] },
      diagnostics: [],
      path: "bots/start.bot",
    });
    await act(async () => read.resolve(opened("bots/start.bot", "W (read before the save)")));
    expect(markers(store)).toBe("D0+EDIT");
    await waitFor(() => expect(api.openFile).toHaveBeenCalledTimes(2), { timeout: 2000 });
    await waitFor(() => expect(store.getState().currentSource).toBe("D0+EDIT\n"));
    expect(markers(store)).toBe("D0+EDIT");

    act(() => store.getState().addComment({ text: "NEXT" }));
    api.saveFile.mockResolvedValueOnce({ path: "bots/start.bot", source: "whatever\n" });
    fireEvent.click(screen.getByRole("button", { name: "Save" }));
    await waitFor(() => expect(api.saveFile).toHaveBeenCalledTimes(2));
    const [, savedDoc] = api.saveFile.mock.calls[1] as [string, { comments: { text: string }[] }];
    expect(savedDoc.comments.map((c) => c.text).join("+")).toBe("D0+EDIT+NEXT");
  });

  it("does not land over the assistant's reload made while it read", async () => {
    const store = tab();
    const read = deferred<ReturnType<typeof opened>>();
    api.openFile.mockReturnValueOnce(read.promise);
    ws.emit({ type: "file_modified", path: "bots/start.bot" });
    await waitFor(() => expect(api.openFile).toHaveBeenCalledTimes(1), { timeout: 2000 });
    // What the assistant's reload-after-write does once its own write is on
    // disk — a write the server does not announce.
    act(() => applyOpenedFile(opened("bots/start.bot", "A (the assistant's write)"), store.getState()));
    api.openFile.mockResolvedValueOnce(opened("bots/start.bot", "A (the assistant's write)"));
    await act(async () => read.resolve(opened("bots/start.bot", "W (read before the assistant wrote)")));
    expect(markers(store)).toBe("A (the assistant's write)");
    await waitFor(() => expect(api.openFile).toHaveBeenCalledTimes(2), { timeout: 2000 });
    expect(markers(store)).toBe("A (the assistant's write)");
  });
});
