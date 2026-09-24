// @vitest-environment jsdom
//
// Closing an editor tab disposes its document store. Every close control
// asks first when that store holds unsaved work — including the one on the
// card a tab shows when its document fails to load: a draft tab shown again
// after its draft stopped being readable still holds what the author was
// editing.
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

const findDraftBotSource = vi.fn();
vi.mock("@/api/runs/artifacts", () => ({
  findDraftBotSource: (...a: unknown[]) => findDraftBotSource(...a),
}));
vi.mock("@/api/client", () => ({
  openFile: vi.fn(),
  parseSource: vi.fn(async (src: string) => ({ document: { comments: [{ text: src }] }, diagnostics: [] })),
}));
vi.mock("@/components/EditorView", () => ({ default: () => <div /> }));

import * as client from "@/api/client";
import { getOrCreateDocumentStore } from "@/store/document";
import { useTabsStore } from "@/store/tabs";
import EditorTabHost from "./EditorTabHost";

beforeEach(() => {
  useTabsStore.setState({ tabs: [], activeEditorTabId: null, activeRunTabId: null });
  vi.mocked(client.openFile).mockRejectedValue(new Error("the file is gone"));
  findDraftBotSource.mockReset();
});

afterEach(cleanup);

function host(tabId: string, params: { file?: string; draft?: string }) {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: 0 } } });
  return render(
    <QueryClientProvider client={qc}>
      <EditorTabHost tabId={tabId} {...params} />
    </QueryClientProvider>,
  );
}

const tabExists = (id: string) => useTabsStore.getState().tabs.some((t) => t.id === id);

describe("the failed-load card's Close tab", () => {
  it("asks before disposing a draft the author was editing, and waits for the answer", async () => {
    findDraftBotSource.mockResolvedValue("v1");
    const id = useTabsStore.getState().openTab("editor", { draft: "run-g" }, "Draft");
    const first = host(id, { draft: "run-g" });
    const store = getOrCreateDocumentStore(id);
    await waitFor(() => expect(store.getState().currentSource).toBe("v1"));
    store.getState().addComment({ text: "the author's edit" });
    first.unmount();

    // Shown again, and the draft can no longer be read.
    findDraftBotSource.mockRejectedValue(new Error("the run was pruned"));
    host(id, { draft: "run-g" });
    fireEvent.click(await screen.findByRole("button", { name: "Close tab" }));
    expect(await screen.findByText("Discard unsaved changes?")).toBeTruthy();
    expect(tabExists(id)).toBe(true);

    fireEvent.click(screen.getByRole("button", { name: "Discard" }));
    await waitFor(() => expect(tabExists(id)).toBe(false));
  });

  it("closes a tab that holds nothing without asking", async () => {
    const id = useTabsStore.getState().openTab("editor", { file: "bots/gone.bot" }, "gone.bot");
    host(id, { file: "bots/gone.bot" });
    fireEvent.click(await screen.findByRole("button", { name: "Close tab" }));
    await waitFor(() => expect(tabExists(id)).toBe(false));
    expect(screen.queryByText("Discard unsaved changes?")).toBeNull();
  });
});
