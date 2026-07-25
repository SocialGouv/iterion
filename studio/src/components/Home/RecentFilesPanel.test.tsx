// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";

import { createEmptyDocument } from "@/lib/defaults";
import { getOrCreateDocumentStore } from "@/store/document";
import { useTabsStore } from "@/store/tabs";

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
