// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";

import { createEmptyDocument } from "@/lib/defaults";
import { getOrCreateDocumentStore } from "@/store/document";
import { replaceDocumentNow } from "@/lib/replaceDocument";
import { useTabsStore } from "@/store/tabs";
import { useUIStore } from "@/store/ui";
import * as client from "@/api/client";

import RecentFilesPanel from "./RecentFilesPanel";

// Mock only the two network calls RecentFilesPanel makes; keep every other
// real export so transitive imports still resolve. loadExample returns a
// real (normalized-safe) empty document so setDocument doesn't throw.
vi.mock("@/api/client", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/api/client")>();
  return {
    ...actual,
    listExampleEntries: vi.fn(async () => [
      {
        name: "feature-dev/main.bot",
        display_name: "Featurly",
        description: "Ship a feature end to end",
      },
    ]),
    loadExample: vi.fn(async () => ({
      source: "agent a:\n  system: hi\n",
      document: createEmptyDocument(),
      diagnostics: [],
    })),
  };
});

// Not under test; rendering it bare would be harmless but we keep the test
// hermetic (no catalog fetch on mount).
vi.mock("@/components/Catalog/BotCatalogDialog", () => ({
  BotCatalogDialog: () => null,
}));

afterEach(cleanup);
beforeEach(() => {
  window.history.replaceState({}, "", "/");
  useUIStore.setState({ toasts: [] });
});
const toasts = () => useUIStore.getState().toasts.map((t) => t.message).join(" | ");

describe("RecentFilesPanel — launching a first-class bot from Home", () => {
  // Regression guard for the "can't Run a bot until you save" friction:
  // opening a bot must bind currentFilePath = bots/<name> so the Toolbar
  // Run button (disabled while currentFilePath is null) enables immediately.
  // Goes through the shared openExampleIntoStore helper (also used by
  // Toolbar.handlePickFile and CanvasEmpty).
  it("binds bots/<name> path + keeps source and stays non-dirty", async () => {
    const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(
      <QueryClientProvider client={qc}>
        <RecentFilesPanel />
      </QueryClientProvider>,
    );

    // The Bots section auto-opens when there are no recents; click Featurly.
    const botButton = await screen.findByText("Featurly");
    fireEvent.click(botButton);

    await waitFor(() => {
      const tabId = useTabsStore.getState().activeEditorTabId;
      expect(tabId).toBeTruthy();
      const doc = getOrCreateDocumentStore(tabId!).getState();
      expect(doc.currentFilePath).toBe("bots/feature-dev/main.bot");
      // The helper also keeps the example's source (so Save / cloud-mode
      // resume work without a re-open) — this is the behaviour Toolbar's
      // old "setCurrentSource(null)" branch diverged from before the merge.
      expect(doc.currentSource).toBe("agent a:\n  system: hi\n");
      // markSaved() ran after setCurrentFilePath, so the freshly-opened bot
      // is the clean baseline: no phantom unsaved-changes prompt, and the
      // file badge reads as a named file rather than "Unsaved".
      expect(doc.isDirty()).toBe(false);
    });
  });
});

describe("RecentFilesPanel — an example that fails to load", () => {
  // The tab is created before the load and is active and editable at once:
  // from the editor's home pane the author can be typing into it while the
  // example loads.
  async function openFailing() {
    let fail!: (e: unknown) => void;
    vi.mocked(client.loadExample).mockReturnValueOnce(
      new Promise((_, reject) => {
        fail = reject;
      }),
    );
    const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(
      <QueryClientProvider client={qc}>
        <RecentFilesPanel />
      </QueryClientProvider>,
    );
    const before = useTabsStore.getState().activeEditorTabId;
    fireEvent.click(await screen.findByText("Featurly"));
    await waitFor(() => expect(useTabsStore.getState().activeEditorTabId).not.toBe(before));
    const tabId = useTabsStore.getState().activeEditorTabId;
    if (!tabId) throw new Error("the example opened no tab");
    return { tabId, fail };
  }
  const tabExists = (id: string) => useTabsStore.getState().tabs.some((t) => t.id === id);

  it("closes the new tab when nobody touched it, and says why", async () => {
    const { tabId, fail } = await openFailing();
    await act(async () => fail(new Error("boom")));
    expect(tabExists(tabId)).toBe(false);
    expect(toasts()).toContain("Failed to open example: boom");
    expect(window.location.pathname).toBe("/");
  });

  it("keeps the new tab the author had started editing", async () => {
    const { tabId, fail } = await openFailing();
    const store = getOrCreateDocumentStore(tabId);
    act(() => {
      store.getState().setDocument({ ...createEmptyDocument(), comments: [{ text: "typed meanwhile" }] });
    });
    await act(async () => fail(new Error("boom")));
    expect(tabExists(tabId)).toBe(true);
    expect(store.getState().document?.comments?.[0]?.text).toBe("typed meanwhile");
    expect(toasts()).toContain("Failed to open example — the tab you had started editing is kept: boom");
    // The toast speaks of that tab: the author is taken to it.
    expect(window.location.pathname).toBe("/editor");
  });
});

describe("RecentFilesPanel — an example whose answer the author's edits refused", () => {
  // Edits need the editor: the tab kept with them, and the toast that says
  // the example was not opened, are both about the editor — the author is
  // taken there, wherever the panel was.
  it("keeps the tab the author edited, and takes the author to it", async () => {
    type Example = Awaited<ReturnType<typeof client.loadExample>>;
    let land!: (v: Example) => void;
    vi.mocked(client.loadExample).mockReturnValueOnce(new Promise<Example>((r) => (land = r)));
    const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(
      <QueryClientProvider client={qc}>
        <RecentFilesPanel />
      </QueryClientProvider>,
    );
    const before = useTabsStore.getState().activeEditorTabId;
    fireEvent.click(await screen.findByText("Featurly"));
    await waitFor(() => expect(useTabsStore.getState().activeEditorTabId).not.toBe(before));
    const tabId = useTabsStore.getState().activeEditorTabId;
    if (!tabId) throw new Error("the example opened no tab");
    const store = getOrCreateDocumentStore(tabId);
    act(() => {
      store.getState().setDocument({ ...createEmptyDocument(), comments: [{ text: "typed meanwhile" }] });
    });

    await act(async () =>
      land({ source: "agent a:\n  system: hi\n", document: createEmptyDocument(), diagnostics: [] }),
    );
    expect(store.getState().document?.comments?.[0]?.text).toBe("typed meanwhile");
    expect(toasts()).toContain("was not opened");
    expect(window.location.pathname).toBe("/editor");
  });
});

describe("RecentFilesPanel — an example a newer request in its tab superseded", () => {
  // That request is the author's, made from wherever they are: the panel
  // leaves them there, and says nothing about the dropped answer.
  it("does not take the author anywhere", async () => {
    type Example = Awaited<ReturnType<typeof client.loadExample>>;
    let land!: (v: Example) => void;
    vi.mocked(client.loadExample).mockReturnValueOnce(new Promise<Example>((r) => (land = r)));
    const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(
      <QueryClientProvider client={qc}>
        <RecentFilesPanel />
      </QueryClientProvider>,
    );
    const before = useTabsStore.getState().activeEditorTabId;
    fireEvent.click(await screen.findByText("Featurly"));
    await waitFor(() => expect(useTabsStore.getState().activeEditorTabId).not.toBe(before));
    const tabId = useTabsStore.getState().activeEditorTabId;
    if (!tabId) throw new Error("the example opened no tab");
    const store = getOrCreateDocumentStore(tabId);
    act(() =>
      replaceDocumentNow(store, (s) => {
        s.setDocument(createEmptyDocument());
        s.setCurrentFilePath(null);
        s.markSaved();
      }),
    );
    window.history.pushState({}, "", "/runs");

    await act(async () =>
      land({ source: "agent a:\n  system: hi\n", document: createEmptyDocument(), diagnostics: [] }),
    );
    expect(window.location.pathname).toBe("/runs");
    expect(toasts()).toBe("");
  });
});
