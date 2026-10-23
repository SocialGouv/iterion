// @vitest-environment jsdom
//
// The save's answer refreshes the tab's write claim (the digest rule):
// what the NEXT save presents is what THIS save answered. Held stale, the
// second save of an edit round presents the open's digest over bytes the
// tab's own write replaced — a 409 against our own edit — and the first
// save after a Save As presents none over a file that now exists.
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";

const api = vi.hoisted(() => ({
  saveFile: vi.fn(),
}));
vi.mock("@/api/client", async (importOriginal) => ({
  ...(await importOriginal<typeof import("@/api/client")>()),
  ...api,
}));

import { createEmptyDocument } from "@/lib/defaults";
import { DocumentStoreProvider, createDocumentStore, type DocumentStore } from "@/store/document";
import { useUIStore } from "@/store/ui";
import { useDocumentFileOps } from "./useDocumentFileOps";
import { useDocumentSaveAs } from "@/components/DocumentSaveAs/useDocumentSaveAs";

function CanvasSaveButton() {
  const ops = useDocumentFileOps({ confirm: async () => true });
  return <button onClick={() => void ops.handleSave()}>Canvas Save</button>;
}

function SaveAsButtons({ store }: { store: DocumentStore }) {
  const saveAs = useDocumentSaveAs();
  return (
    <>
      <button onClick={() => saveAs.requestSaveAs({ store })}>Request Save As</button>
      <button onClick={() => void saveAs.confirmSaveAs()}>Confirm Save As</button>
      <input
        aria-label="file name"
        value={saveAs.fileName}
        onChange={(e) => saveAs.setFileName(e.target.value)}
      />
    </>
  );
}

function boundStore(digest: string | null): DocumentStore {
  const store = createDocumentStore();
  store.getState().setDocument(createEmptyDocument());
  store.getState().setCurrentFilePath("bots/a.bot");
  store.getState().setOpenedDigest(digest);
  store.getState().markSaved();
  return store;
}

function mount(store: DocumentStore, children: React.ReactNode) {
  return render(<DocumentStoreProvider store={store}>{children}</DocumentStoreProvider>);
}

const toasts = () => useUIStore.getState().toasts.map((t) => t.message);

afterEach(() => {
  cleanup();
  useUIStore.setState({ toasts: [] });
});

describe("the save response refreshes the write claim", () => {
  it("a canvas save sets the digest the answer carries, and the next save presents it", async () => {
    const store = boundStore("sha-open");
    api.saveFile.mockResolvedValueOnce({ path: "bots/a.bot", source: "saved\n", digest: "sha-saved" });
    mount(
      store,
      <CanvasSaveButton />,
    );
    fireEvent.click(screen.getByRole("button", { name: "Canvas Save" }));
    await waitFor(() => expect(toasts()).toContain("Saved successfully"));
    // The first save presented the open's digest; the store now holds what
    // the save answered, so the second save of the round presents that.
    expect(api.saveFile).toHaveBeenCalledWith("bots/a.bot", expect.anything(), { digest: "sha-open" });
    expect(store.getState().openedDigest).toBe("sha-saved");
  });

  it("a Save As binds the created file's digest to the tab", async () => {
    const store = boundStore(null);
    api.saveFile.mockResolvedValueOnce({ path: "new.bot", source: "saved\n", digest: "sha-created" });
    mount(
      store,
      <SaveAsButtons store={store} />,
    );
    fireEvent.click(screen.getByRole("button", { name: "Request Save As" }));
    fireEvent.change(screen.getByLabelText("file name"), { target: { value: "new" } });
    fireEvent.click(screen.getByRole("button", { name: "Confirm Save As" }));
    await waitFor(() => expect(store.getState().currentFilePath).toBe("new.bot"));
    // setCurrentFilePath cleared the open's digest — there is no open; the
    // file begins with THIS write, whose answer is the claim the next
    // Ctrl+S presents. Without it that save presents none over a file that
    // now exists, and is refused.
    expect(store.getState().openedDigest).toBe("sha-created");
  });
});
