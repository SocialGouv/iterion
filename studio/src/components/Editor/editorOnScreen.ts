// The editor tab the app shows, or null. EditorTabsView publishes it: it
// alone decides what the editor route shows — the welcome pane (no tab), or
// the active tab when that tab is one of the current project's. An app-level
// command aimed at "the editor" (the desktop's and the workspace shell's
// Edit → Undo) acts on that tab, and on nothing when the author is on another
// page or the editor shows its welcome pane.
let onScreen: string | null = null;

export function markEditorTabOnScreen(tabId: string | null): () => void {
  onScreen = tabId;
  return () => {
    if (onScreen === tabId) onScreen = null;
  };
}

export function editorTabOnScreen(): string | null {
  return onScreen;
}
