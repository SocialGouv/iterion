import { useRef } from "react";

export interface BundleRef {
  teamID: string;
  slug: string;
  rel: string;
}

/**
 * Keep a bundle reference alive while its drawer is open.
 *
 * The bundle-files drawer is mounted conditionally on the editor's file being
 * a `botsource://` path. Losing that path UNMOUNTS the drawer, past its own
 * discard gate — and that gate is the only thing that can ask about the
 * buffer it holds, which is component state and therefore invisible to
 * `hasUnsavedWork()`. File → New, Import and "Start blank" all reach there
 * after their own guard answered "nothing to lose", because none of them can
 * see it.
 *
 * Latching the last reference while the drawer is OPEN keeps the drawer
 * mounted so its gate is reachable. It also keeps `onOpenChange` firing:
 * without it the parent still believes the drawer is open and pops it back
 * up on the next bundle opened.
 */
export function useLatchedBundleRef(parsed: BundleRef | null, open: boolean): BundleRef | null {
  const last = useRef(parsed);
  if (parsed) last.current = parsed;
  return open ? (parsed ?? last.current) : parsed;
}
