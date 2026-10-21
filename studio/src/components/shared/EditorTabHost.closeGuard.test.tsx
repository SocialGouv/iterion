// @vitest-environment jsdom
//
// Closing an editor tab disposes its document store. Every close control
// asks first when that store holds unsaved work — including the one on the
// card a tab shows when its document fails to load.
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
  it("asks before disposing work already held when the first load fails, and waits for the answer", async () => {
    const id = useTabsStore.getState().openTab("editor", { file: "bots/gone.bot" }, "gone.bot");
    const store = getOrCreateDocumentStore(id);
    store.getState().addComment({ text: "the author's edit" });
    host(id, { file: "bots/gone.bot" });

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
