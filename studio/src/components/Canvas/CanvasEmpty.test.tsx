// @vitest-environment jsdom
//
// Start blank is a replacement with no answer to wait for, and the author's
// latest request: an example still loading — picked, then its dialog closed —
// must not land on the blank canvas once it answers.
import { act, cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { afterEach, describe, expect, it, vi } from "vitest";

const api = vi.hoisted(() => ({ listExampleEntries: vi.fn(), loadExample: vi.fn() }));
vi.mock("@/api/client", async (importOriginal) => ({
  ...(await importOriginal<typeof import("@/api/client")>()),
  ...api,
}));

import { createEmptyDocument } from "@/lib/defaults";
import { DocumentStoreProvider, createDocumentStore } from "@/store/document";
import { useUIStore } from "@/store/ui";
import CanvasEmpty from "./CanvasEmpty";

afterEach(cleanup);

describe("Start blank, while an example is still loading", () => {
  it("is not replaced by the example's late answer", async () => {
    api.listExampleEntries.mockResolvedValue([{ name: "x/main.bot" }]);
    let land!: (v: unknown) => void;
    api.loadExample.mockReturnValue(new Promise((r) => (land = r)));
    const store = createDocumentStore();
    store.getState().setDocument({ ...createEmptyDocument(), comments: [{ text: "before" }] });
    store.getState().markSaved();
    render(
      <QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}>
        <DocumentStoreProvider store={store}>
          <CanvasEmpty />
        </DocumentStoreProvider>
      </QueryClientProvider>,
    );

    fireEvent.click(screen.getByText("Examples"));
    fireEvent.click(await screen.findByText("main.bot"));
    await waitFor(() => expect(api.loadExample).toHaveBeenCalledTimes(1));
    fireEvent.keyDown(document.activeElement ?? document.body, { key: "Escape" });
    fireEvent.click(screen.getByText("Start blank"));
    await waitFor(() => expect(store.getState().document?.comments?.[0]?.text).toBeUndefined());

    await act(async () =>
      land({
        source: "x\n",
        document: { ...createEmptyDocument(), comments: [{ text: "EXAMPLE" }] },
        diagnostics: [],
        path: "examples/x/main.bot",
      }),
    );
    expect(store.getState().document?.comments?.[0]?.text).toBeUndefined();
    expect(store.getState().currentFilePath).toBeNull();
    // Superseded, not refused: Start blank is the author's newer request.
    expect(useUIStore.getState().toasts.map((t) => t.message).join(" ")).not.toContain("was opening");
  });
});

describe("an example that fails to load from the empty canvas", () => {
  it("says why, and leaves the list open for another pick", async () => {
    useUIStore.setState({ toasts: [] });
    api.listExampleEntries.mockResolvedValue([{ name: "x/main.bot" }]);
    api.loadExample.mockRejectedValueOnce(new Error("the example is being rewritten by another process"));
    const store = createDocumentStore();
    store.getState().setDocument(createEmptyDocument());
    store.getState().markSaved();
    render(
      <QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}>
        <DocumentStoreProvider store={store}>
          <CanvasEmpty />
        </DocumentStoreProvider>
      </QueryClientProvider>,
    );
    fireEvent.click(screen.getByText("Examples"));
    fireEvent.click(await screen.findByText("main.bot"));
    await waitFor(() =>
      expect(useUIStore.getState().toasts.map((t) => t.message).join(" | ")).toContain(
        "Open failed: the example is being rewritten by another process",
      ),
    );
    expect(screen.getByText("main.bot")).toBeTruthy();
  });
});
