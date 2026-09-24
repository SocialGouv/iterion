// Whether the editor route is what the app shows. EditorTabsView raises it
// while it is mounted — it is mounted only on that route — so an app-level
// command aimed at "the editor" (the desktop's and the workspace shell's
// Edit → Undo) can tell the editor on screen from an editor tab left behind
// while the author is on another page.
let mounted = 0;

export function markEditorOnScreen(): () => void {
  mounted += 1;
  return () => {
    mounted -= 1;
  };
}

export function editorOnScreen(): boolean {
  return mounted > 0;
}
