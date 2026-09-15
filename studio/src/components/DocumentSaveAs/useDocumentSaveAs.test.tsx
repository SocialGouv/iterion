// @vitest-environment jsdom

import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

const api = vi.hoisted(() => ({ saveFile: vi.fn() }));
vi.mock("@/api/client", () => api);

import { createEmptyDocument } from "@/lib/defaults";
import { createDocumentStore, type DocumentStore } from "@/store/document";
import { useRecentsStore } from "@/store/recents";
import { useServerInfoStore } from "@/store/serverInfo";
import { useUIStore } from "@/store/ui";

import DocumentSaveAsDialog from "./DocumentSaveAsDialog";
import { useDocumentSaveAs } from "./useDocumentSaveAs";

function Harness({
  store,
  strict = false,
  current = true,
}: {
  store: DocumentStore;
  strict?: boolean;
  current?: boolean;
}) {
  const controller = useDocumentSaveAs();
  return (
    <>
      <button
        onClick={() =>
          controller.requestSaveAs({
            store,
            ...(strict ? { expectedGeneration: store.getState()._generation } : {}),
            isTargetCurrent: () => current,
          })
        }
      >
        Open Save As
      </button>
      <DocumentSaveAsDialog controller={controller} />
    </>
  );
}

function documentStore() {
  const store = createDocumentStore();
  store.getState().setDocument(createEmptyDocument());
  store.getState().markSaved();
  return store;
}

beforeEach(() => {
  vi.resetAllMocks();
  window.localStorage.clear();
  useRecentsStore.setState({ recents: [] });
  useServerInfoStore.setState({ info: null });
  useUIStore.setState({ toasts: [] });
  api.saveFile.mockResolvedValue({
    path: "main.bot",
    source: "workflow main:\n  entry: agent_1\n",
  });
});

afterEach(cleanup);

describe("useDocumentSaveAs", () => {
  it("confirms a host-derived filename and binds only the successful response", async () => {
    const store = documentStore();
    render(<Harness store={store} />);

    fireEvent.click(screen.getByRole("button", { name: "Open Save As" }));
    expect(await screen.findByDisplayValue("main.bot")).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Save" }));

    await waitFor(() => expect(store.getState().currentFilePath).toBe("main.bot"));
    expect(api.saveFile).toHaveBeenCalledWith(
      "main.bot",
      store.getState().document,
      { createOnly: true },
    );
    expect(store.getState().isDirty()).toBe(false);
    expect(useRecentsStore.getState().recents).toEqual(["main.bot"]);
  });

  it("keeps the dialog open after an atomic collision", async () => {
    const store = documentStore();
    api.saveFile.mockRejectedValue(new Error("API error 409: file already exists"));
    render(<Harness store={store} />);

    fireEvent.click(screen.getByRole("button", { name: "Open Save As" }));
    fireEvent.click(await screen.findByRole("button", { name: "Save" }));

    expect((await screen.findByRole("alert")).textContent).toMatch(/already exists/i);
    expect(screen.getByRole("dialog")).toBeTruthy();
    expect(store.getState().currentFilePath).toBeNull();
  });

  it("cancels without persisting", async () => {
    const store = documentStore();
    render(<Harness store={store} />);

    fireEvent.click(screen.getByRole("button", { name: "Open Save As" }));
    fireEvent.click(await screen.findByRole("button", { name: "Cancel" }));

    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    expect(api.saveFile).not.toHaveBeenCalled();
  });

  it("rejects a changed assistant-bound generation at confirmation", async () => {
    const store = documentStore();
    render(<Harness store={store} strict />);

    fireEvent.click(screen.getByRole("button", { name: "Open Save As" }));
    await screen.findByRole("dialog");
    store.getState().setDocument(createEmptyDocument());
    fireEvent.click(screen.getByRole("button", { name: "Save" }));

    expect((await screen.findByRole("alert")).textContent).toMatch(/editor changed/i);
    expect(api.saveFile).not.toHaveBeenCalled();
  });

  it("does not open a filesystem Save As dialog in cloud mode", async () => {
    const store = documentStore();
    useServerInfoStore.setState({ info: { mode: "cloud" } as never });
    render(<Harness store={store} />);

    fireEvent.click(screen.getByRole("button", { name: "Open Save As" }));

    expect(screen.queryByRole("dialog")).toBeNull();
    expect(api.saveFile).not.toHaveBeenCalled();
    const toasts = useUIStore.getState().toasts;
    expect(toasts[toasts.length - 1]?.message).toMatch(/isn't available in cloud/i);
  });
});
