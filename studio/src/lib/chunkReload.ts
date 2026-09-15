/**
 * Recovery for a lazy chunk the server no longer carries.
 *
 * Every lazy route is addressed by a content hash baked into the document that
 * loaded it. When the server stops serving that build — a deploy landed under
 * an open tab, or the request reached a replica on another build — the fetch
 * fails and the view never renders. The tab cannot heal itself: every hash it
 * still holds names the same vanished build, so retrying the import fails
 * identically. Re-fetching the entry document is the only way back to a
 * coherent set of hashes.
 *
 * The reload is rate-limited per tab. If the fresh document is itself served by
 * a replica that cannot serve its own chunks, an unguarded reload spins forever
 * and the user never sees the error explaining why — so once the allowance is
 * spent the failure is left to surface.
 */

/**
 * Keyed per DOCUMENT, not per tab. The workspace shell hosts one same-origin
 * `<iframe src="/x/<id>/">` per open connection, and same-origin frames share
 * the tab's sessionStorage — so a single key would let whichever document fails
 * first spend the allowance for the shell and every pane, leaving the others on
 * the vanished build with no reload AND no cancelled event.
 */
function reloadMarker(): string {
  return "iterion:chunk-reload-at:" + window.location.pathname;
}

/** Long enough to cover a rolling deploy, short enough that a later, unrelated
 *  stale chunk still gets its own recovery. */
export const RELOAD_COOLDOWN_MS = 30_000;

/**
 * Reload the document unless it already did so within the cooldown. Returns
 * whether the reload was issued, so the caller can decide whether it owns the
 * failure or must let it propagate.
 *
 * A storage engine that refuses access (blocked-cookies contexts throw on the
 * property itself, Safari private mode throws on write) declines the recovery
 * rather than the error: returning false leaves the event uncancelled, so Vite
 * still rethrows and the failure stays visible. Swallowing the throw here would
 * replace one legible error with two.
 */
export function reloadForStaleChunk(now: number = Date.now()): boolean {
  let last: number;
  try {
    last = Number(window.sessionStorage.getItem(reloadMarker()));
  } catch {
    return false;
  }
  if (Number.isFinite(last) && last > 0 && now - last < RELOAD_COOLDOWN_MS) {
    return false;
  }
  try {
    window.sessionStorage.setItem(reloadMarker(), String(now));
  } catch {
    return false;
  }
  window.location.reload();
  return true;
}

/**
 * Listen for Vite's preload failures. The event is emitted by the preload
 * helper that wraps dynamic `import()`, which is the shape every React.lazy
 * route takes — so this covers a stale LAZY chunk.
 *
 * It does NOT cover a stale ENTRY chunk: index.html loads that one as a plain
 * `<script type="module" src>`, whose failure emits no such event, and the
 * recovery code is itself inside the file that failed to load. Only markup in
 * index.html could catch that, and an inline script is exactly what this app's
 * `script-src 'self'` refuses — the directive the CSP treats as load-bearing.
 * A fleet where the entry chunk 404s is a fleet serving several builds; that is
 * fixed by pinning the image, not from inside the page.
 *
 * Cancelling the event suppresses Vite's rethrow, which is only correct when we
 * actually took the recovery over.
 */
export function installChunkReload(): void {
  window.addEventListener("vite:preloadError", (event) => {
    if (reloadForStaleChunk()) {
      event.preventDefault();
    }
  });
}
