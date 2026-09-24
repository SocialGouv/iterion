import { createContext, useContext } from "react";

/**
 * True inside a subtree that is mounted and not shown — an editor tab behind
 * another. The kit's portals (dialogs, drawers, menus, popovers, tooltips,
 * popups) render nothing while it holds: rendered into the body, they would
 * show over whatever is on screen and act on the hidden subtree. Their state
 * stays where it lives, so they come back as they were when the subtree is
 * shown again.
 */
export const HiddenSubtreeContext = createContext(false);

export function useHiddenSubtree(): boolean {
  return useContext(HiddenSubtreeContext);
}
