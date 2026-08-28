// A capability bridge from the active editor TAB to a conversational bot.
//
// The model never receives a path it can write and never gets a filesystem
// tool. At send time the studio snapshots the live AST as .bot source and
// mints an opaque browser-session id bound to that tab. A later proposal must
// echo the id + generation; the studio resolves both back to the still-active
// store before it can apply anything.

import * as api from "@/api/client";
import {
  getDocumentStore,
  type DocumentStore,
} from "@/store/document";
import { useTabsStore } from "@/store/tabs";

import type { ActiveEditorDocumentSnapshot } from "./contextMessage";

// Large bots belong behind a fetch/tool boundary, not copied into every LLM
// turn. Never send a prefix: a partial workflow looks editable but cannot be
// validated honestly. The marker tells the bot the document was withheld.
export const MAX_ACTIVE_EDITOR_SOURCE = 160_000;

const tokenByTab = new Map<string, string>();
const tabByToken = new Map<string, string>();

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
export async function captureActiveEditorDocument(): Promise<ActiveEditorDocumentSnapshot | null> {
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
  return {
    sessionId: tokenForTab(tabId),
    revision: state._generation,
    file: state.currentFilePath,
    complete,
    sourceLength: source.length,
    ...(complete ? { source } : {}),
  };
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

