import { createContext, useContext } from "react";

/**
 * Whether the editor tab this subtree belongs to is the one on screen.
 *
 * Every hydrated tab stays mounted — the inactive ones are only hidden — so a
 * component that listens on `window`, opens a dialog from a global flag, or
 * fills a global slot does it once per open tab unless it asks this first: a
 * shortcut pressed on one tab would undo, save or open in the others.
 * EditorView provides it from its own `active` prop; outside any EditorView
 * the answer is no, so a component that finds no editor tab around it acts
 * on nothing global.
 */
export const EditorTabActiveContext = createContext(false);

export function useEditorTabActive(): boolean {
  return useContext(EditorTabActiveContext);
}
