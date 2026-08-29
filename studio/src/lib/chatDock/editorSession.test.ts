// @vitest-environment jsdom

import { beforeEach, describe, expect, it, vi } from "vitest";

const api = vi.hoisted(() => ({ unparse: vi.fn(), parseBotSourceEditorPath: vi.fn() }));
const authoring = vi.hoisted(() => ({ snapshotAssistantAuthoring: vi.fn() }));
const botSources = vi.hoisted(() => ({ getBotSource: vi.fn() }));
vi.mock("@/api/client", () => api);
vi.mock("@/api/assistantAuthoring", () => authoring);
vi.mock("@/api/botSources", () => botSources);

import { createEmptyDocument } from "@/lib/defaults";
import { getOrCreateDocumentStore } from "@/store/document";
import { useTabsStore } from "@/store/tabs";

import {
  MAX_ACTIVE_EDITOR_SOURCE,
  captureActiveEditorDocument,
  resolveAuthoringSnapshot,
  resolveEditorSession,
} from "./editorSession";

beforeEach(() => {
  vi.resetAllMocks();
  authoring.snapshotAssistantAuthoring.mockRejectedValue(new Error("no authoring perimeter"));
  api.parseBotSourceEditorPath.mockReturnValue(null);
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

  it("keeps the server-owned file hashes bound to the sent editor revision", async () => {
    api.unparse.mockResolvedValue("workflow live:\n  entry: a\n");
    authoring.snapshotAssistantAuthoring.mockResolvedValue({
      editor_path: "bots/live/main.bot",
      files: [
        {
          scope: "workspace",
          path: "scripts/helper.py",
          size: 8,
          sha256: "host-hash",
          available: true,
          readable: true,
        },
      ],
    });
    const tabId = useTabsStore.getState().openTab("editor", { file: "bots/live/main.bot" }, "live");
    const store = getOrCreateDocumentStore(tabId);
    store.getState().setDocument(createEmptyDocument());
    store.getState().setCurrentFilePath("bots/live/main.bot");

    const snapshot = await captureActiveEditorDocument();
    if (!snapshot) throw new Error("missing snapshot");
    expect(snapshot.authoring?.files[0]?.sha256).toBe("host-hash");
    expect(resolveAuthoringSnapshot(snapshot.sessionId, snapshot.revision)).toEqual(
      snapshot.authoring,
    );
  });

  it("inlines only an explicitly attached declared cloud bundle file", async () => {
    const path = "botsource://team/demo/main.bot";
    api.unparse.mockResolvedValue("workflow live:\n  entry: a\n");
    api.parseBotSourceEditorPath.mockReturnValue({ teamID: "team", slug: "demo", rel: "main.bot" });
    authoring.snapshotAssistantAuthoring.mockResolvedValue({
      editor_path: path,
      version: 3,
      files: [
        { scope: "bundle", path: "helper.py", size: 10, sha256: "hash", available: true, readable: false },
      ],
    });
    botSources.getBotSource.mockResolvedValue({ files: { "helper.py": "value = 1\n" } });
    const tabId = useTabsStore.getState().openTab("editor", { file: path }, "cloud");
    const store = getOrCreateDocumentStore(tabId);
    store.getState().setDocument(createEmptyDocument());
    store.getState().setCurrentFilePath(path);

    const snapshot = await captureActiveEditorDocument([
      { kind: "bot-file", ref: "bot-file/team/demo/helper.py", label: "helper.py" },
    ]);
    expect(snapshot?.authoring?.files[0]).toMatchObject({
      complete: true,
      readable: true,
      source: "value = 1\n",
    });
  });
});
