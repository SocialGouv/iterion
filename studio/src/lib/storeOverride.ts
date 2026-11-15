// The page's cross-store override: the `?store=` a run link appends when the
// run lives in a store other than the destination daemon's own (RunsPanel),
// so the daemon's cross-store routes read it (pkg/server/runs.go::
// resolveCrossStore). It addresses the page's run alone — the run of the
// page's /runs/<id> path, never another run the page shows (the chat dock's
// assistant run) — and only on the routes that read a foreign store
// (withStoreParam's callers and the run socket). Cross-store views are outside
// the expected-birth guarantee: the page sends no expectation for that run and
// adopts no birth for it (@/lib/lostRuns).

// InvalidStoreOverrideError is a `?store=` that is not a store path: a run
// view shows it as an error, never the primary store's run under the ID.
export class InvalidStoreOverrideError extends Error {
  readonly store: string;
  constructor(store: string) {
    super(`the page's ?store= is not a store path: ${JSON.stringify(store)}`);
    this.name = "InvalidStoreOverrideError";
    this.store = store;
  }
}

function hasControlCharacter(v: string): boolean {
  for (let i = 0; i < v.length; i++) {
    const c = v.charCodeAt(i);
    if (c < 0x20 || c === 0x7f) return true;
  }
  return false;
}

// isSafeStoreParam validates the override's shape before it is forwarded to
// the daemon: a filesystem path of at most 4096 characters with no control
// character and no `..` segment. Spaces, and any other character a directory
// name holds, are valid. The server-side resolveCrossStore guard rejects
// unknown stores; this is defence in depth against a hostile `?store=...` URL
// handed to a victim.
export function isSafeStoreParam(v: string | null): boolean {
  if (!v || v.length > 4096 || hasControlCharacter(v)) return false;
  return !v.split(/[\\/]/).includes("..");
}

// rawStoreOverride is the page's `?store=` as sent, null when the page names
// none (or there is no browser context) — "" is a present, empty one.
function rawStoreOverride(): string | null {
  if (typeof window === "undefined") return null;
  return new URLSearchParams(window.location.search).get("store");
}

// EMPTY_STORE_OVERRIDE stands for a present but empty `?store=` where a store
// key is needed: no store, yet not the server's own (a NUL is in no path).
const EMPTY_STORE_OVERRIDE = "\u0000";

// readStoreOverrideFromURL returns the page's `?store=`, "" when none is set
// or there is no browser context. An override that is not a store path — an
// empty one included — throws InvalidStoreOverrideError: it is never dropped
// to the primary store.
export function readStoreOverrideFromURL(): string {
  const v = rawStoreOverride();
  if (v === null) return "";
  if (!isSafeStoreParam(v)) throw new InvalidStoreOverrideError(v);
  return v;
}

// pageRunId is the run the page's path shows (/runs/<id>, behind any prefix:
// the studio base, a workspace pane's /x/<pane>), or null.
export function pageRunId(): string | null {
  if (typeof window === "undefined") return null;
  const segments = window.location.pathname.split("/");
  const at = segments.indexOf("runs");
  const raw = at >= 0 ? segments[at + 1] : undefined;
  if (!raw) return null;
  try {
    return decodeURIComponent(raw);
  } catch {
    return null;
  }
}

// storeOverrideFor is the page's override when runId is the page's run, ""
// for any other run (it lives in the daemon's own store). Throws
// InvalidStoreOverrideError as readStoreOverrideFromURL does.
export function storeOverrideFor(runId: string): string {
  if (!runId || pageRunId() !== runId) return "";
  return readStoreOverrideFromURL();
}

// pageStoreOf is the store the page shows runId from, without validating it:
// the page's raw override for the page's run, "" for any other run. An
// invalid override is still a foreign store — nothing is adopted for it — an
// empty one included (EMPTY_STORE_OVERRIDE).
export function pageStoreOf(runId: string): string {
  if (!runId || pageRunId() !== runId) return "";
  const v = rawStoreOverride();
  if (v === null) return "";
  return v === "" ? EMPTY_STORE_OVERRIDE : v;
}

// isForeignStoreRun reports whether runId is the page's run shown from a
// foreign store — an invalid override included: the page sends no expected
// birth for it and adopts none.
export function isForeignStoreRun(runId: string): boolean {
  return pageStoreOf(runId) !== "";
}

// storeParamOfURL is the `?store=` a request URL names, "" for none.
export function storeParamOfURL(url: string): string {
  try {
    return new URL(url, "http://studio.invalid").searchParams.get("store") ?? "";
  } catch {
    return "";
  }
}

// appendStoreParam adds `store=<override>` to url, before any fragment. A
// no-op for an empty override.
export function appendStoreParam(url: string, override: string): string {
  if (!override) return url;
  const hashAt = url.indexOf("#");
  const base = hashAt >= 0 ? url.slice(0, hashAt) : url;
  const hash = hashAt >= 0 ? url.slice(hashAt) : "";
  const sep = !base.includes("?") ? "?" : base.endsWith("?") || base.endsWith("&") ? "" : "&";
  return `${base}${sep}store=${encodeURIComponent(override)}${hash}`;
}
