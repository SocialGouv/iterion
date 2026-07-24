/**
 * A file-scoped editor deep-link belongs only to the visible tab whose
 * document is already bound to that file. EditorTabsView/EditorTabHost own
 * creating and hydrating the matching tab; sibling EditorViews stay mounted
 * for fast tab switching and must ignore the shared browser URL.
 */
export function editorDeepLinkTargetsDocument(
  active: boolean,
  currentFilePath: string | null,
  requestedFile: string | null,
): boolean {
  if (!active) return false;
  return requestedFile === null || requestedFile === currentFilePath;
}
