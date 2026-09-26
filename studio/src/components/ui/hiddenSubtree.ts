import { createContext, useContext } from "react";

/**
 * True inside a subtree that is mounted and not shown — an editor tab behind
 * another. The kit's portals (dialogs, drawers, menus, popovers, tooltips,
 * popups) render nothing while it holds: rendered into the body, they would
 * show over whatever is on screen and act on the hidden subtree. What their
 * owners hold — an open flag, a pending question — stays, so a dialog comes
 * back with its subtree; what their content holds (a field not yet committed,
 * an editor's own undo) starts afresh.
 */
export const HiddenSubtreeContext = createContext(false);

export function useHiddenSubtree(): boolean {
  return useContext(HiddenSubtreeContext);
}
