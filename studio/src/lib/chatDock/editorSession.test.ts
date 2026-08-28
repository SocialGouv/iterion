// @vitest-environment jsdom

import { beforeEach, describe, expect, it, vi } from "vitest";

const api = vi.hoisted(() => ({ unparse: vi.fn() }));
vi.mock("@/api/client", () => api);

import { createEmptyDocument } from "@/lib/defaults";
import { getOrCreateDocumentStore } from "@/store/document";
import { useTabsStore } from "@/store/tabs";

import {
  MAX_ACTIVE_EDITOR_SOURCE,
  captureActiveEditorDocument,
  resolveEditorSession,
} from "./editorSession";

beforeEach(() => {
  vi.resetAllMocks();
  useTabsStore.setState({ tabs: [], activeEditorTabId: null, activeRunTabId: null });
});

describe("active editor session snapshots", () => {
  it("serialises the live AST and binds it to the active tab revision", async () => {
    api.unparse.mockResolvedValue("workflow live:\n  entry: a\n");
    const tabId = useTabsStore
      .getState()
      .openTab("editor", { file: "bots/live/main.bot" }, "live");
    const store = getOrCreateDocumentStore(tabId);
    store.getState().setDocument(createEmptyDocument());
    store.getState().setCurrentFilePath("bots/live/main.bot");

    const snapshot = await captureActiveEditorDocument();
    if (!snapshot) throw new Error("active editor snapshot was not captured");

    expect(snapshot).toMatchObject({
      revision: store.getState()._generation,
      file: "bots/live/main.bot",
      complete: true,
      source: "workflow live:\n  entry: a\n",
    });
    expect(resolveEditorSession(snapshot.sessionId)?.tabId).toBe(tabId);
  });

  it("withholds an oversized workflow instead of sending a misleading prefix", async () => {
    api.unparse.mockResolvedValue("x".repeat(MAX_ACTIVE_EDITOR_SOURCE + 1));
    const tabId = useTabsStore.getState().openTab("editor", {}, "large");
    getOrCreateDocumentStore(tabId).getState().setDocument(createEmptyDocument());

    const snapshot = await captureActiveEditorDocument();

    expect(snapshot?.complete).toBe(false);
    expect(snapshot?.sourceLength).toBe(MAX_ACTIVE_EDITOR_SOURCE + 1);
    expect(snapshot).not.toHaveProperty("source");
  });
});
