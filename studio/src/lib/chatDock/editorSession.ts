// A capability bridge from the active editor TAB to a conversational bot.
//
// The model never receives a path it can write and never gets a filesystem
// tool. At send time the studio snapshots the live AST as .bot source and
// mints an opaque browser-session id bound to that tab. A later proposal must
// echo the id + generation; the studio resolves both back to the still-active
// store before it can apply anything.

import * as api from "@/api/client";
import {
  snapshotAssistantAuthoring,
  type AssistantAuthoringSnapshot,
} from "@/api/assistantAuthoring";
import { getBotSource } from "@/api/botSources";
import {
  getDocumentStore,
  type DocumentStore,
} from "@/store/document";
import { useTabsStore } from "@/store/tabs";

import type { ActiveEditorDocumentSnapshot } from "./contextMessage";
import type { TypedReference } from "./routeReference";

// Large bots belong behind a fetch/tool boundary, not copied into every LLM
// turn. Never send a prefix: a partial workflow looks editable but cannot be
// validated honestly. The marker tells the bot the document was withheld.
export const MAX_ACTIVE_EDITOR_SOURCE = 160_000;
export const MAX_ATTACHED_BOT_FILE_SOURCE = 64 * 1024;

const tokenByTab = new Map<string, string>();
const tabByToken = new Map<string, string>();
const authoringBySessionRevision = new Map<string, AssistantAuthoringSnapshot>();

function newToken(): string {
  if (typeof crypto !== "undefined" && "randomUUID" in crypto) {
    return crypto.randomUUID();
  }
  return `editor_${Date.now().toString(36)}_${Math.random().toString(36).slice(2)}`;
}

function tokenForTab(tabId: string): string {
  const current = tokenByTab.get(tabId);
  if (current) return current;
  const token = newToken();
  tokenByTab.set(tabId, token);
  tabByToken.set(token, tabId);
  return token;
}

/** Capture the authoritative live document, including unsaved canvas edits. */
export async function captureActiveEditorDocument(
  attached: readonly TypedReference[] = [],
): Promise<ActiveEditorDocumentSnapshot | null> {
  const tabs = useTabsStore.getState();
  const tabId = tabs.activeEditorTabId;
  if (!tabId || !tabs.tabs.some((tab) => tab.id === tabId && tab.kind === "editor")) {
    return null;
  }
  const store = getDocumentStore(tabId);
  if (!store) return null;
  const state = store.getState();
  if (!state.document) return null;

  // Unparse the AST snapshot, not currentSource: currentSource is a cache of
  // the last open/save/source-edit operation and can lag canvas mutations.
  const source = await api.unparse(state.document);
  const complete = source.length <= MAX_ACTIVE_EDITOR_SOURCE;
  const sessionId = tokenForTab(tabId);
  let authoring: AssistantAuthoringSnapshot | undefined;
  if (state.currentFilePath) {
    try {
      authoring = await snapshotAssistantAuthoring(state.currentFilePath);
      authoring = await enrichAttachedBotFiles(authoring, attached);
      for (const key of authoringBySessionRevision.keys()) {
        if (key.startsWith(`${sessionId}:`)) authoringBySessionRevision.delete(key);
      }
      authoringBySessionRevision.set(
        `${sessionId}:${state._generation}`,
        authoring,
      );
    } catch {
      // Most bots declare no companion-file perimeter. The active .bot bridge
      // remains useful on its own, so an unavailable authoring snapshot is not
      // allowed to suppress the editor marker.
    }
  }
  return {
    sessionId,
    revision: state._generation,
    file: state.currentFilePath,
    complete,
    sourceLength: source.length,
    ...(complete ? { source } : {}),
    ...(authoring ? { authoring } : {}),
  };
}

async function enrichAttachedBotFiles(
  snapshot: AssistantAuthoringSnapshot,
  attached: readonly TypedReference[],
): Promise<AssistantAuthoringSnapshot> {
  const active = api.parseBotSourceEditorPath(snapshot.editor_path);
  if (!active) return snapshot;
  const wanted = new Set<string>();
  for (const ref of attached) {
    if (ref.kind !== "bot-file") continue;
    const parts = ref.ref.slice("bot-file/".length).split("/");
    if (parts[0] !== active.teamID || parts[1] !== active.slug) continue;
    const rel = parts.slice(2).join("/");
    if (rel) wanted.add(rel);
  }
  if (wanted.size === 0) return snapshot;
  const source = await getBotSource(active.teamID, active.slug);
  let total = 0;
  return {
    ...snapshot,
    files: snapshot.files.map((file) => {
      if (file.scope !== "bundle" || !wanted.has(file.path) || !file.available) {
        return file;
      }
      const content = source.files[file.path];
      const bytes = content === undefined ? 0 : new TextEncoder().encode(content).byteLength;
      if (content === undefined || total + bytes > MAX_ATTACHED_BOT_FILE_SOURCE) {
        return { ...file, complete: false };
      }
      total += bytes;
      return { ...file, readable: true, complete: true, source: content };
    }),
  };
}

/** Snapshot captured with the exact editor generation sent to the model. */
export function resolveAuthoringSnapshot(
  sessionId: string,
  revision: number,
): AssistantAuthoringSnapshot | null {
  return authoringBySessionRevision.get(`${sessionId}:${revision}`) ?? null;
}

export interface ResolvedEditorSession {
  tabId: string;
  store: DocumentStore;
}

/** Resolve only a still-live tab. Callers separately require it to be active. */
export function resolveEditorSession(sessionId: string): ResolvedEditorSession | null {
  const tabId = tabByToken.get(sessionId);
  if (!tabId) return null;
  const tab = useTabsStore.getState().tabs.find((item) => item.id === tabId);
  const store = getDocumentStore(tabId);
  if (!tab || tab.kind !== "editor" || !store) return null;
  return { tabId, store };
}

export function isEditorSessionActive(sessionId: string): boolean {
  const resolved = resolveEditorSession(sessionId);
  return !!resolved && useTabsStore.getState().activeEditorTabId === resolved.tabId;
}
