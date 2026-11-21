// The page's lost runs, keyed by the store the request or socket named (its
// ?store=, "" for the daemon's own store) and the run ID: runs whose run
// socket the server closed scope_lost, or whose request the server refused
// 409 run_origin_lost. The run the page was watching was replaced or deleted
// under its ID, or the caller's access to it changed, so anything this page
// sends by that ID to that store now would reach whatever run holds it — the
// successor's snapshot, events or logs, or an action (merge, answer, resume,
// delete…) meant for the original. A run of the same ID in another store is
// another run.
//
// The page also keeps the BIRTH of every run of the daemon's own store it
// learned (a snapshot's run.birth_id, a run row's birth_id). Every request
// addressing a run whose birth is known carries it as the expected birth —
// the X-Iterion-Expected-Birth header on a fetch, the expected_birth
// parameter on a URL the browser loads by itself (src=, a link, a socket) —
// and the server refuses one whose run was replaced (409 run_origin_lost, or
// a scope_lost socket close). That server refusal is the guarantee; this
// registry is defense in depth: every HTTP request (apiRequest, runFetch) and
// every run WebSocket addressing a lost run is refused before it leaves, with
// a LostRunError. A run of a foreign store (a cross-store view, ?store=) is
// outside the guarantee: no birth is adopted for it and no expectation sent
// (@/lib/storeOverride).
// The registry is module-level on purpose: it survives a view's unmount and
// a run store's reset, and only a full page reload clears it — the reload
// the lost-run banner offers, which starts every view of the run over.

import { isForeignStoreRun, pageRunId, storeParamOfURL } from "./storeOverride";

// lost: store → the run IDs lost in it. Nested, never a joined key: a store
// path and a run ID can both hold any separator.
const lost = new Map<string, Set<string>>();
const lostListeners = new Set<(store: string, runId: string) => void>();

// births: run ID of the daemon's own store → the birth learned ("" = a
// historical run with no birth) and where it was learned. A snapshot's birth
// is the run a view watches: a later, different one means the ID now names
// another run. A row's birth (a list, children or global-active poll) only
// pins the view a row opens: a row of a later-SENT request replaces it — a
// listing can see a run recreated no view watches — and never marks a run
// lost; a row of an earlier-sent request landing late changes nothing. A
// navigation's birth (the clicked row's, carried by the navigation:
// pinRunBirth) is the run the opened view means: no row replaces it, the
// view's first snapshot adopts or refuses it.
type BirthSource = "snapshot" | "row" | "navigation";
const births = new Map<string, { birth: string; source: BirthSource; seq: number }>();
let rowRequests = 0;
const birthListeners = new Set<() => void>();
let birthsVersion = 0;

// The server's wire names (pkg/server/run_origin_gate.go). HISTORICAL_BIRTH
// expects a run with no birth; RUN_ORIGIN_LOST is the error code of a
// request whose run was replaced.
export const EXPECTED_BIRTH_HEADER = "X-Iterion-Expected-Birth";
export const EXPECTED_BIRTH_PARAM = "expected_birth";
export const HISTORICAL_BIRTH = "historical";
export const RUN_ORIGIN_LOST = "run_origin_lost";

export class LostRunError extends Error {
  readonly runId: string;
  constructor(runId: string) {
    super(
      `run ${runId} was replaced or deleted, or your access changed, while this page watched it — reload to see the run that holds this ID now`,
    );
    this.name = "LostRunError";
    this.runId = runId;
  }
}

// markRunLost marks runId of store lost ("" for the daemon's own store).
export function markRunLost(store: string, runId: string): void {
  if (!runId || isRunLost(store, runId)) return;
  let ids = lost.get(store);
  if (ids === undefined) {
    ids = new Set<string>();
    lost.set(store, ids);
  }
  ids.add(runId);
  for (const listener of [...lostListeners]) listener(store, runId);
}

export function isRunLost(store: string, runId: string | null | undefined): boolean {
  return !!runId && (lost.get(store)?.has(runId) ?? false);
}

// onRunLost calls listener with every run marked lost from now on, and its
// store, whoever detected the loss (a socket close, a 409, a snapshot of
// another birth). Returns the unsubscribe.
export function onRunLost(listener: (store: string, runId: string) => void): () => void {
  lostListeners.add(listener);
  return () => {
    lostListeners.delete(listener);
  };
}

// Static routes of the run families (pkg/server route registration): the
// segment after `runs/` names the route, not a run, when it is the last one —
// GET /api/runs/global-active, GET /api/runs/stats, POST /api/runs/preview-cost,
// POST /api/runs/uploads.
const STATIC_RUN_ROUTES = new Set(["global-active", "stats", "preview-cost", "uploads"]);

// runAddressedBy returns the run ID a request URL addresses, or null. The run
// route families are /api/runs/<id>/… (REST) and /api/ws/runs/<id>/… (the run
// sockets), behind any prefix (a workspace pane's /x/<pane>/api, an absolute
// cloud origin). Only the FIRST such `runs` segment counts: a run-like path
// further in — a file path inside another run's workspace — is that run's
// data, not a second run. A static route (stats, global-active…) addresses
// no run, even when a run happens to bear that name.
export function runAddressedBy(url: string): string | null {
  let path: string;
  try {
    path = new URL(url, "http://studio.invalid").pathname;
  } catch {
    path = url.split(/[?#]/, 1)[0] ?? "";
  }
  const segments = path.split("/");
  for (let i = 1; i + 1 < segments.length; i++) {
    if (segments[i] !== "runs") continue;
    const family = segments[i - 1] === "api" || (segments[i - 1] === "ws" && segments[i - 2] === "api");
    if (!family) continue;
    const raw = segments[i + 1] ?? "";
    const last = i + 2 === segments.length;
    if (last && STATIC_RUN_ROUTES.has(raw)) return null;
    try {
      return decodeURIComponent(raw) || null;
    } catch {
      return null;
    }
  }
  return null;
}

// lostRunAddressedBy returns the lost run ID a request URL addresses in the
// store it names, or null.
export function lostRunAddressedBy(url: string): string | null {
  if (lost.size === 0) return null;
  const runId = runAddressedBy(url);
  return runId !== null && isRunLost(storeParamOfURL(url), runId) ? runId : null;
}

// assertRunReachable throws LostRunError when url addresses a lost run.
export function assertRunReachable(url: string): void {
  const runId = lostRunAddressedBy(url);
  if (runId !== null) throw new LostRunError(runId);
}

function setBirth(runId: string, birth: string, source: BirthSource, seq: number): void {
  const known = births.get(runId);
  births.set(runId, { birth, source, seq });
  if (known?.birth !== birth) {
    birthsVersion++;
    for (const listener of [...birthListeners]) listener();
  }
}

// noteRunBirth records the birth a run SNAPSHOT reported for runId in store
// (the ?store= of the request or socket that answered, "" for the daemon's
// own store). undefined — a server that predates birth exposure — records
// nothing, nor does a snapshot of a foreign store (outside the guarantee).
// The first birth is adopted, a row-learned one included; a different one
// later means the ID now names another run: the run is marked lost and false
// is returned, so the caller drops what came with it. false too for a run
// already lost.
export function noteRunBirth(store: string, runId: string, birth: string | null | undefined): boolean {
  if (!runId) return true;
  if (store === "" && typeof birth === "string") {
    const known = births.get(runId);
    if (known === undefined || known.birth === birth) {
      setBirth(runId, birth, "snapshot", 0);
    } else {
      markRunLost(store, runId);
    }
  }
  return !isRunLost(store, runId);
}

// beginRowRequest numbers a request that returns run rows, before it is
// sent: its rows are noted with that number (noteRunRowBirth).
export function beginRowRequest(): number {
  return ++rowRequests;
}

// noteRunRowBirth records the birth a run ROW reported, seq being its
// request's beginRowRequest number: it pins the view the row opens. It is
// adopted when none is known and replaces the row of an earlier-sent request;
// a row of an earlier-sent request landing late changes nothing. It never
// replaces a snapshot's or a navigation's, never marks a run lost — a listing
// can see a run recreated no view watches — and a foreign store's row records
// nothing.
export function noteRunRowBirth(store: string, runId: string, birth: string | null | undefined, seq: number): void {
  if (store !== "" || !runId || typeof birth !== "string") return;
  const known = births.get(runId);
  if (known?.source === "snapshot" || known?.source === "navigation" || (known !== undefined && known.seq > seq)) return;
  setBirth(runId, birth, "row", seq);
}

// pinRunBirth records the birth a navigation carried for runId of the
// daemon's own store — the clicked row's: the run the opened view means. It
// replaces nothing a snapshot taught; no row replaces it; the view's first
// snapshot adopts it or, for another birth, marks the run lost.
export function pinRunBirth(runId: string, birth: string): void {
  if (!runId || births.get(runId)?.source === "snapshot") return;
  setBirth(runId, birth, "navigation", 0);
}

// NAVIGATION_BIRTH_STATE is the history-state key an in-page navigation to a
// run view carries the clicked row's birth under: { runId, birth }.
export const NAVIGATION_BIRTH_STATE = "iterionExpectedBirth";

// navigationBirthState is the history state an in-page navigation to runId's
// view carries: the clicked row's birth, nothing for a row without one.
export function navigationBirthState(runId: string, birth: string | null | undefined): Record<string, unknown> | undefined {
  return typeof birth === "string" ? { [NAVIGATION_BIRTH_STATE]: { runId, birth } } : undefined;
}

// consumeNavigationBirth pins the birth the navigation to the page's run view
// carried — in its history state (an in-page link) or its expected_birth
// parameter (a full page load, the desktop's cross-daemon link) — once, before
// the view's first request, and removes it from the page: a reload starts
// from what the server says. A run shown from a foreign store adopts none.
function consumeNavigationBirth(): void {
  if (typeof window === "undefined") return;
  const runId = pageRunId();
  if (!runId) return;
  const state = window.history.state as Record<string, unknown> | null;
  const carried = state?.[NAVIGATION_BIRTH_STATE] as { runId?: unknown; birth?: unknown } | undefined;
  const url = new URL(window.location.href);
  const param = url.searchParams.get(EXPECTED_BIRTH_PARAM);
  if (carried === undefined && param === null) return;
  const rest = state ? { ...state } : null;
  if (rest) delete rest[NAVIGATION_BIRTH_STATE];
  url.searchParams.delete(EXPECTED_BIRTH_PARAM);
  window.history.replaceState(rest, "", url.pathname + url.search + url.hash);
  const birth = carried?.runId === runId && typeof carried.birth === "string" ? carried.birth : param;
  if (birth === null || isForeignStoreRun(runId)) return;
  pinRunBirth(runId, birth === HISTORICAL_BIRTH ? "" : birth);
}

// knownRunBirth is the birth the page learned for runId in store, or
// undefined — always for a foreign store's run: births are the daemon's own
// store's.
export function knownRunBirth(store: string, runId: string): string | undefined {
  consumeNavigationBirth();
  return store === "" ? births.get(runId)?.birth : undefined;
}

// expectedBirthWire is the wire value of a known birth ("" → the sentinel).
export function expectedBirthWire(birth: string): string {
  return birth === "" ? HISTORICAL_BIRTH : birth;
}

// knownRunBirthWire is the wire expectation for a run the page addresses in
// a request BODY (in store), or undefined when its birth is unknown — and for
// the page's run while the page shows it from a foreign store.
export function knownRunBirthWire(store: string, runId: string): string | undefined {
  if (isForeignStoreRun(runId)) return undefined;
  const birth = knownRunBirth(store, runId);
  return birth === undefined ? undefined : expectedBirthWire(birth);
}

function parsedURL(url: string): URL | null {
  try {
    return new URL(url, "http://studio.invalid");
  } catch {
    return null;
  }
}

// expectedBirthFor is the wire expectation for the run url addresses, or null
// when the page knows no birth for it — and always for a request naming a
// foreign store, or addressing the page's run while the page shows it from
// one: cross-store views are outside the guarantee.
export function expectedBirthFor(url: string): string | null {
  const runId = runAddressedBy(url);
  if (runId === null || isForeignStoreRun(runId)) return null;
  const birth = knownRunBirth(storeParamOfURL(url), runId);
  return birth === undefined ? null : expectedBirthWire(birth);
}

// snapshotStores remembers the store each snapshot was read from — the
// ?store= of the request or socket that answered, captured when it was sent —
// so its birth is keyed by that store, not by whatever the page shows when it
// lands.
const snapshotStores = new WeakMap<object, string>();

export function tagSnapshotStore<T extends object>(snapshot: T, store: string): T {
  snapshotStores.set(snapshot, store);
  return snapshot;
}

export function snapshotStoreOf(snapshot: object): string | undefined {
  return snapshotStores.get(snapshot);
}

function carriesExpectedBirth(url: string): boolean {
  return parsedURL(url)?.searchParams.has(EXPECTED_BIRTH_PARAM) ?? false;
}

// withExpectedBirth returns url carrying expected_birth when it addresses a
// run whose birth the page knows — for what the browser loads by itself (an
// <img>/<iframe>/<track> src, a link, a socket). Read at call time: a URL
// built before the birth was learned carries none (useRunBirth rebuilds a
// memoized one).
export function withExpectedBirth(url: string): string {
  const birth = expectedBirthFor(url);
  if (birth === null || carriesExpectedBirth(url)) return url;
  const hashAt = url.indexOf("#");
  const base = hashAt >= 0 ? url.slice(0, hashAt) : url;
  const hash = hashAt >= 0 ? url.slice(hashAt) : "";
  const sep = !base.includes("?") ? "?" : base.endsWith("?") || base.endsWith("&") ? "" : "&";
  return `${base}${sep}${EXPECTED_BIRTH_PARAM}=${encodeURIComponent(birth)}${hash}`;
}

// expectedBirthHeaders is the header a fetch of url carries: the expectation
// when the page knows the run's birth and url does not already name one.
export function expectedBirthHeaders(url: string): Record<string, string> {
  const birth = expectedBirthFor(url);
  if (birth === null || carriesExpectedBirth(url)) return {};
  return { [EXPECTED_BIRTH_HEADER]: birth };
}

// subscribeRunBirths / runBirthsVersion back useRunBirth: a memoized URL
// depends on the version, so it is rebuilt once a birth is learned.
export function subscribeRunBirths(listener: () => void): () => void {
  birthListeners.add(listener);
  return () => {
    birthListeners.delete(listener);
  };
}

export function runBirthsVersion(): number {
  return birthsVersion;
}

// lostRunFromResponse maps a 409 run_origin_lost answer to the run it lost:
// the run_id its body names (a run carried in a request body), else the run
// url addresses. null for any other answer. It reads a clone: the caller
// keeps the body.
export async function lostRunFromResponse(url: string, res: Response): Promise<string | null> {
  if (res.status !== 409) return null;
  try {
    const body = (await res.clone().json()) as { error_code?: unknown; run_id?: unknown };
    if (body?.error_code !== RUN_ORIGIN_LOST) return null;
    return (typeof body.run_id === "string" && body.run_id) || runAddressedBy(url);
  } catch {
    return null;
  }
}

// lostRunStore is the store of a run a 409 answering url lost: the store url
// names for the run it addresses, the daemon's own for a run its body carried.
export function lostRunStore(url: string, lostRun: string): string {
  return runAddressedBy(url) === lostRun ? storeParamOfURL(url) : "";
}

// runFetch is fetch for a URL that may address a run: a lost run is refused
// before the request leaves, the run's known birth rides as the expected
// birth, and a 409 run_origin_lost marks the run lost and rejects with a
// LostRunError. Every direct fetch of a run path goes through it (the shared
// apiRequest does the same).
export async function runFetch(url: string, init?: RequestInit): Promise<Response> {
  assertRunReachable(url);
  const expected = expectedBirthHeaders(url);
  let sent = init;
  if (Object.keys(expected).length > 0) {
    const headers = new Headers(init?.headers);
    for (const [name, value] of Object.entries(expected)) {
      if (!headers.has(name)) headers.set(name, value);
    }
    sent = { ...init, headers };
  }
  const res = await fetch(url, sent);
  const lostRun = await lostRunFromResponse(url, res);
  if (lostRun !== null) {
    markRunLost(lostRunStore(url, lostRun), lostRun);
    throw new LostRunError(lostRun);
  }
  return res;
}

// forgetLostRunsForTest empties the registry between tests; production
// clears it only by reloading the page.
export function forgetLostRunsForTest(): void {
  lost.clear();
  births.clear();
  rowRequests = 0;
  birthsVersion++;
}
