import { create } from "zustand";

import { registerUnsavedBuffer } from "@/lib/unsavedBuffers";

/**
 * Editable buffers held OUT of component state, keyed by what they edit.
 *
 * A buffer in a component dies with it: any ancestor's conditional render —
 * the toolbar's drawer mounted on a `botsource://` path, a viewport notice,
 * a route change — takes the author's only copy, past every gate the
 * component owns, because none of them can reach an unmounting ancestor
 * (#1755). Held here instead, the buffer outlives the unmount and is
 * adopted back when the surface returns — the way the Source view's buffer
 * has lived in the document store since #1662. These two cannot live in a
 * document store: they answer to a bot bundle and a run worktree, not to a
 * tab, and they must survive the tab itself going away.
 *
 * One dirtiness rule for both: `value !== original`, exactly what the
 * surfaces' own save buttons and discard gates compare.
 */

export interface BundleBuffer {
  /** The bundle file the text belongs to. */
  rel: string;
  value: string;
  original: string;
  /** A file that does not exist yet: Save is offered on it even empty. */
  created: boolean;
}

export interface RunFileBuffer {
  value: string;
  original: string;
}

export const bundleBufferKey = (teamID: string, slug: string): string => `${teamID}\u0000${slug}`;
export const runFileBufferKey = (runId: string, path: string): string => `${runId}\u0000${path}`;

interface EditBuffersState {
  bundle: Record<string, BundleBuffer>;
  runFiles: Record<string, RunFileBuffer>;
  /** `null` releases the buffer — through a discard gate the surface asked,
   *  or the clean-release an unmount performs. Never called to drop DIRTY
   *  text without the author's answer: that is the loss this store exists
   *  to make impossible. */
  setBundle: (key: string, buffer: BundleBuffer | null) => void;
  setRunFile: (key: string, buffer: RunFileBuffer | null) => void;
}

export const useEditBuffersStore = create<EditBuffersState>((set) => ({
  bundle: {},
  runFiles: {},
  setBundle: (key, buffer) =>
    set((s) => {
      const next = { ...s.bundle };
      if (buffer) next[key] = buffer;
      else delete next[key];
      return { bundle: next };
    }),
  setRunFile: (key, buffer) =>
    set((s) => {
      const next = { ...s.runFiles };
      if (buffer) next[key] = buffer;
      else delete next[key];
      return { runFiles: next };
    }),
}));

// The store is the app-wide answer to "is anything held outside the document
// stores" — registered once, read at ask time by the unload warning.
registerUnsavedBuffer(() => {
  const s = useEditBuffersStore.getState();
  const dirty = (b: { value: string; original: string }) => b.value !== b.original;
  return Object.values(s.bundle).some(dirty) || Object.values(s.runFiles).some(dirty);
});
