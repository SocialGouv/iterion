import { useCallback } from "react";

import { useConfirm } from "@/hooks/useConfirm";
import { getOrCreateDocumentStore } from "@/store/document";
import { useTabsStore } from "@/store/tabs";

const DISCARD_TAB_PROMPT = {
  title: "Discard unsaved changes?",
  message:
    "This tab has changes that have not been saved — in the canvas, in the .bot source pane, or both. Closing it loses them.",
  confirmLabel: "Discard",
  confirmVariant: "danger",
} as const;

/** Closing an editor tab DISPOSES its document store (`disposeDocumentStore`),
 *  taking the document and the Source view's un-applied text with it. That
 *  is the one class the view cannot recover from: hiding the pane, expanding
 *  the canvas or leaving `/editor` only unmount the view, which keeps its
 *  buffer in the store and adopts it back on the next mount — so those need
 *  no prompt, and a prompt there would ask about something that is not lost.
 *
 *  The two close controls sit OUTSIDE the tab's `DocumentStoreProvider`, so
 *  the tab's store comes from the registry; reading the contextual one would
 *  silently guard the default store and never fire.
 *
 *  `hasUnsavedWork`, not `isSourceDirty`: the question a discard path asks
 *  cannot depend on WHICH kind of unsaved work is at stake. Disposing the
 *  store takes the document and the Source buffer alike, and asking about
 *  only one of them made `hasUnsavedWork`'s own promise — "a reload, a tab
 *  close or a File → New takes both" — untrue of the tab close. */
export function useDropEditorTab() {
  const { confirm, dialog } = useConfirm();

  /** Run `drop` — after asking, when the editor tab it disposes holds
   *  un-applied source text. `tabId` defaults to the active tab. */
  const guardDroppingEditorTab = useCallback(
    async (drop: () => void, tabId?: string | null) => {
      const id = tabId === undefined ? useTabsStore.getState().activeEditorTabId : tabId;
      if (id && getOrCreateDocumentStore(id).getState().hasUnsavedWork()) {
        if (!(await confirm(DISCARD_TAB_PROMPT))) return;
      }
      drop();
    },
    [confirm],
  );

  return { guardDroppingEditorTab, dialog };
}
