/**
 * A file-scoped editor deep-link belongs only to the visible tab that was
 * opened FOR that file — `tab.params.file`, the key EditorTabsView matched
 * the URL against — never to the path the document is bound to. The binding
 * is another fact (what the store holds), and a link compared against it was
 * swallowed whenever the two differed: `?node=` never focused, `?from=`
 * never showed, and the search was never marked handled. EditorTabsView and
 * EditorTabHost own creating and hydrating the matching tab; sibling
 * EditorViews stay mounted for fast tab switching and must ignore the shared
 * browser URL.
 */
export function editorDeepLinkTargetsDocument(
  active: boolean,
  tabFile: string | null,
  requestedFile: string | null,
): boolean {
  if (!active) return false;
  return requestedFile === null || requestedFile === tabFile;
}
