// @vitest-environment jsdom
//
// A background tab must not raise a global toast.
//
// EditorTabsView mounts EVERY hydrated tab and hides the inactive ones with
// display:none, so a stale tab — a draft whose run is gone, a file that moved —
// still runs its load on mount and still fails. A toast is app-level, so that
// failure appeared over whatever the operator was doing and read as the answer
// to it: reported as "Open draft failed" popping up right after clicking a link
// that had, in fact, worked.
//
// The failure is not swallowed — the tab renders it inline with Retry/Close.
// These tests pin WHERE it is announced, not whether.
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { useTabsStore } from "@/store/tabs";

const addToast = vi.fn();
vi.mock("@/store/ui", () => ({
  useUIStore: (sel: (s: unknown) => unknown) => sel({ addToast }),
}));

// The draft lookup is the failure under test.
const findDraftBotSource = vi.fn();
vi.mock("@/api/runs/artifacts", () => ({
  findDraftBotSource: (...a: unknown[]) => findDraftBotSource(...a),
}));
vi.mock("@/api/client", () => ({
  openFile: vi.fn(),
  parseSource: vi.fn(),
}));
vi.mock("@/components/EditorView", () => ({ default: () => <div /> }));

import EditorTabHost from "./EditorTabHost";

beforeEach(() => {
  addToast.mockClear();
  findDraftBotSource.mockReset().mockResolvedValue(null);
  useTabsStore.setState({ tabs: [], activeEditorTabId: null, activeRunTabId: null });
});

afterEach(cleanup);

function openDraftTab(draft: string) {
  return useTabsStore.getState().openTab("editor", { draft }, "Draft");
}

// The host reads its draft through react-query now, so it needs a client.
function mount(tabId: string, draft: string) {
  const qc = new QueryClient({
    defaultOptions: { queries: { retry: false, gcTime: 0 } },
  });
  return render(
    <QueryClientProvider client={qc}>
      <EditorTabHost tabId={tabId} draft={draft} />
    </QueryClientProvider>,
  );
}

describe("where a failed tab load is announced", () => {
  it("stays silent when the failing tab is not the one on screen", async () => {
    const background = openDraftTab("run-old");
    const foreground = openDraftTab("run-new");
    expect(useTabsStore.getState().activeEditorTabId).toBe(foreground);

    mount(background, "run-old");

    // The inline state still reports it, in the tab it belongs to.
    await waitFor(() => expect(screen.getByText(/couldn't reload/i)).toBeTruthy());
    expect(addToast).not.toHaveBeenCalled();
  });

  it("toasts when the operator is looking at the tab that failed", async () => {
    const active = openDraftTab("run-new");
    expect(useTabsStore.getState().activeEditorTabId).toBe(active);

    mount(active, "run-new");

    await waitFor(() => expect(addToast).toHaveBeenCalled());
  });
});
