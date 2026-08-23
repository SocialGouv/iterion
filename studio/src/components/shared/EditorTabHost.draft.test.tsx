// @vitest-environment jsdom
//
// The canvas must FOLLOW the conversation, not take one snapshot of it.
//
// Reported from a real session — "je n'ai pas mon graphe qui se met à jour
// sous mes yeux". The assistant redrafts across turns ("now add a judge"), but
// the tab seeded once and refused everything after, so the canvas sat frozen
// while the assistant announced an update the operator could not see. Worse
// than a missing feature: the assistant was asserting a state that was false.
//
// The counterweight is ownership. We may replace the buffer only while it
// still holds exactly what we last put there; the moment the operator edits
// it, the canvas is theirs.
//
// The refresh is INVALIDATION-driven, not a clock: the draft is a node output
// written when the turn ends, so the conversation is what knows there is
// something new. These tests drive that the way the dock does.
import { cleanup, render } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

const findDraftBotSource = vi.fn();
const parseSource = vi.fn();

vi.mock("@/api/runs/artifacts", () => ({
  findDraftBotSource: (...a: unknown[]) => findDraftBotSource(...a),
}));
vi.mock("@/api/client", () => ({
  openFile: vi.fn(),
  parseSource: (...a: unknown[]) => parseSource(...a),
}));
vi.mock("@/components/EditorView", () => ({ default: () => <div /> }));
vi.mock("@/store/ui", () => ({
  useUIStore: (sel: (s: unknown) => unknown) => sel({ addToast: vi.fn() }),
}));

import { editorDraftKey } from "@/hooks/useDraftBot";
import { getOrCreateDocumentStore } from "@/store/document";
import { useTabsStore } from "@/store/tabs";

import EditorTabHost from "./EditorTabHost";

let qc: QueryClient;

function mount(tabId: string, draft: string) {
  return render(
    <QueryClientProvider client={qc}>
      <EditorTabHost tabId={tabId} draft={draft} />
    </QueryClientProvider>,
  );
}

// What the dock does when a turn lands.
async function turnLanded(draft: string) {
  await qc.invalidateQueries({ queryKey: editorDraftKey(draft) });
  await settle();
}

function sourceOf(tabId: string) {
  return getOrCreateDocumentStore(tabId).getState().currentSource;
}

// waitFor polls on REAL timers, which fake timers stop — so settle the async
// apply by advancing the fake clock instead.
async function settle(ms = 0) {
  await vi.advanceTimersByTimeAsync(ms);
  await vi.advanceTimersByTimeAsync(0);
  await vi.advanceTimersByTimeAsync(0);
}

beforeEach(() => {
  vi.useFakeTimers();
  findDraftBotSource.mockReset();
  parseSource
    .mockReset()
    .mockImplementation(async (src: string) => ({
      document: { source: src },
      diagnostics: [],
    }));
  useTabsStore.setState({ tabs: [], activeEditorTabId: null, activeRunTabId: null });
  qc = new QueryClient({
    defaultOptions: { queries: { retry: false, gcTime: 0 } },
  });
});

afterEach(() => {
  cleanup();
  vi.useRealTimers();
});

describe("an open draft tab follows its conversation", () => {
  it("takes the first draft", async () => {
    findDraftBotSource.mockResolvedValue("v1");
    const id = useTabsStore.getState().openTab("editor", { draft: "run-1" }, "Draft");
    mount(id, "run-1");
    await settle();
    expect(sourceOf(id)).toBe("v1");
  });

  it("picks up the NEXT turn's draft without a reload", async () => {
    findDraftBotSource.mockResolvedValue("v1");
    const id = useTabsStore.getState().openTab("editor", { draft: "run-2" }, "Draft");
    mount(id, "run-2");
    await settle();
    expect(sourceOf(id)).toBe("v1");

    findDraftBotSource.mockResolvedValue("v2");
    await turnLanded("run-2");
    expect(sourceOf(id)).toBe("v2");
  });

  it("refuses to clobber the operator's own edits", async () => {
    findDraftBotSource.mockResolvedValue("v1");
    const id = useTabsStore.getState().openTab("editor", { draft: "run-3" }, "Draft");
    mount(id, "run-3");
    await settle();
    expect(sourceOf(id)).toBe("v1");

    // The operator types. From here the canvas is theirs.
    getOrCreateDocumentStore(id).getState().setCurrentSource("mine");

    findDraftBotSource.mockResolvedValue("v2");
    await turnLanded("run-3");
    expect(sourceOf(id)).toBe("mine");
  });

  it("keeps the canvas when a later poll finds nothing", async () => {
    findDraftBotSource.mockResolvedValue("v1");
    const id = useTabsStore.getState().openTab("editor", { draft: "run-4" }, "Draft");
    mount(id, "run-4");
    await settle();
    expect(sourceOf(id)).toBe("v1");

    findDraftBotSource.mockResolvedValue(null);
    await turnLanded("run-4");
    expect(sourceOf(id)).toBe("v1");
  });

  it("does not re-parse when the draft has not changed", async () => {
    findDraftBotSource.mockResolvedValue("v1");
    const id = useTabsStore.getState().openTab("editor", { draft: "run-5" }, "Draft");
    mount(id, "run-5");
    await settle();
    expect(sourceOf(id)).toBe("v1");

    await turnLanded("run-5");
    expect(parseSource).toHaveBeenCalledTimes(1);
  });

  it("does not poll — time alone fetches nothing new", async () => {
    findDraftBotSource.mockResolvedValue("v1");
    const id = useTabsStore.getState().openTab("editor", { draft: "run-6" }, "Draft");
    mount(id, "run-6");
    await settle();
    expect(findDraftBotSource).toHaveBeenCalledTimes(1);

    // Well short of the safety-net refetch: the conversation is what tells the
    // tab to look again, not a clock.
    await settle(10000);
    expect(findDraftBotSource).toHaveBeenCalledTimes(1);
  });
});
