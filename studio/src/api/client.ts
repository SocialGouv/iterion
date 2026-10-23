import type { IterDocument, FileEntry, ListFilesResponse, SaveFileResponse, ReadRefusal, UnitInfo, UnitFileInfo } from "./types";
import { apiBase, isScopedPane, scopePrefix } from "@/lib/scope";
import { isWorkflowFile } from "@/lib/workflowFile";
import {
  ensureLoop,
  fetchWithin,
  present as presentSignOut,
  recordSessionIdentity,
  renewedSince,
  SESSION_RENEWED_AT_KEY,
  signOutInProgress,
  signOutRequested,
  type PresentOutcome,
  type SignOutResult,
} from "./signOutMachine";

// The sign-out state machine (./signOutMachine) owns the pending record and
// every presentation of its logout; this module keeps the refresh machinery
// that has to stand still while a sign-out is owed. The keys are re-exported
// for the callers that have always imported them from here: the renewal stamp
// is the refresh path's evidence (written below, read by renewedSince), and
// the pending record is the machine's one key.
export { SESSION_RENEWED_AT_KEY, SIGN_OUT_PENDING_KEY } from "./signOutMachine";

const BASE_URL = apiBase();

// onUnauthorized fires when the studio server returns 401 on any
// /api/* call AND a token refresh couldn't recover the session. The
// AuthProvider registers a handler that flips its state to `anonymous`
// so the App swaps in the Login view.
let onUnauthorized: (() => void) | null = null;

export function setUnauthorizedHandler(fn: (() => void) | null) {
  onUnauthorized = fn;
}

// RefreshOutcome is what a silent refresh tells the request it was run for:
// "renewed" — replay the request; "rejected" — the server refused the
// session (401/403), it is over; "unavailable" — the server could not renew
// it right now (a 5xx, a rate limit, a network error), and the session the
// browser holds is still its own to present again.
export type RefreshOutcome =
  | { kind: "renewed" }
  | { kind: "rejected" }
  | { kind: "unavailable"; status: number | null };

export function refreshOutcomeOf(status: number): RefreshOutcome {
  if (status >= 200 && status < 300) return { kind: "renewed" };
  if (status === 401 || status === 403) return { kind: "rejected" };
  return { kind: "unavailable", status };
}

// browserStorage is localStorage, or null where there is none: storage the
// browser refuses (its access throws), or a runtime without it.
function browserStorage(): Storage | null {
  try {
    return globalThis.localStorage ?? null;
  } catch {
    return null;
  }
}

// recordRenewal stamps the moment the browser's session cookies were replaced
// — a refresh that renewed them, a session a credential route delivered — for
// every tab to compare its evidence with (renewedSince). A stamp that cannot
// be written is removed, so no tab takes an older one for the latest.
function recordRenewal(): void {
  const storage = browserStorage();
  if (!storage) return;
  try {
    storage.setItem(SESSION_RENEWED_AT_KEY, String(Date.now()));
  } catch (err) {
    console.warn(`auth: the session renewal could not be recorded for the other tabs: ${String(err)}`);
    try {
      storage.removeItem(SESSION_RENEWED_AT_KEY);
    } catch (removeErr) {
      console.warn(`auth: the previous renewal stamp could not be removed either: ${String(removeErr)}`);
    }
  }
}

// The pending sign-out is the machine's record (./signOutMachine): written
// BEFORE the logout it guards, read by every tab, kept as OWED until a
// presentation confirms it, the presented credential turns out dead, or the
// session it names is past its own expiry — and kept as ABANDONED, with its
// notice, when the user chooses to go on without it. This module delegates the
// record to the machine; the refresh discipline around a sign-out stays here.

// recordSessionLineage records which refresh session the browser's credential
// belongs to, from the identity read that named it — the machine's HELD/GONE
// evidence. Kept as the name the AuthProvider has always called.
export function recordSessionLineage(lineage: string | undefined): void {
  recordSessionIdentity({ lineage });
}

export type {
  Heldness,
  PresentOutcome,
  SignOutRecord,
  SignOutResult,
} from "./signOutMachine";
export {
  abandon,
  expireStale,
  signOutPending,
  externalChange,
  heldnessOf,
  readRecord,
  recordSessionIdentity,
  renewedSince,
  resolveDelivered,
  SignOutHeldError,
  subscribe,
  snapshotVersion,
  unnamedRecordWasReplaced,
} from "./signOutMachine";

// sessionGeneration numbers the session this tab acts for. A sign-out opens a
// new one when it STARTS, and so does a session a credential route delivered
// (sessionDelivered): a request sent under an older generation belongs to a
// session the tab has left, so its 401 renews nothing and signs nothing out —
// the session it would renew, or sign out, is the one that replaced it.
let sessionGeneration = 0;

export function currentSessionGeneration(): number {
  return sessionGeneration;
}

// signOutRun is the sign-out this tab is running, from its start until its
// logout settled; null outside one.
let signOutRun: Promise<SignOutResult> | null = null;

// signOutInProgress is the machine's window (a presentation on the wire, or an
// OWED record this browser can still present): every identity load, renewal
// and switch consults it when it starts and when its answer arrives. Kept on
// this module's surface for the callers that have always imported it here.
export { signOutInProgress };

// signOutOfServer is the USER's sign-out ask, wrapped in this module's refresh
// discipline: a session generation opens (a request sent under an older one
// renews nothing and signs nobody out), the retry series is dropped and its
// epoch ended (an answer already in flight re-arms nothing), a refresh in
// flight is waited out inside the bound and aborted past it (its late answer
// must not reach the cookie jar), and no new one starts until the logout
// settled. The machine writes the record BEFORE the logout goes on the wire,
// keeps its lineage even when a presentation fails (naming is not lost), and
// answers for the record's outcome. A sign-out already running is joined, not
// doubled.
export function signOutOfServer(): Promise<SignOutResult> {
  if (signOutRun) return signOutRun;
  sessionGeneration++;
  signOutRequested();
  cancelScheduledRefresh();
  const run = settleRefreshInFlight()
    .then(() => presentSignOut())
    .then(mapOutcome)
    .finally(() => {
      signOutRun = null;
    });
  signOutRun = run;
  return run;
}

// mapOutcome folds the machine's answer into the SignOutResult callers know: a
// credential the logout's own answer names dead (401/403) is a session that is
// over — as confirmed as a 2xx. An unconfirmed presentation arms the machine's
// retry loop.
function mapOutcome(outcome: PresentOutcome): SignOutResult {
  switch (outcome) {
    case "confirmed":
    case "dead-credential":
      return "confirmed";
    case "other-session":
      return "other-session";
    case "unconfirmed":
      ensureLoop();
      return "unconfirmed";
    default:
      return "unconfirmed";
  }
}

// settlePendingSignOut presents the logout again for a sign-out still pending
// — this tab's or another's, owed or abandoned — and answers what it came to,
// or "none" when no record stands or the record's session is no longer held
// here (a GONE record only warns). A sign-out running in this tab is joined;
// the record is never rewritten by a replay.
export function settlePendingSignOut(): Promise<SignOutResult | "none"> {
  if (signOutRun) return signOutRun;
  return presentSignOut().then((outcome) => (outcome === "none" ? "none" : mapOutcome(outcome)));
}

// beforeSignIn readies the browser for the session a sign-in is about to
// deliver: a pending sign-out is presented first — once the new cookies
// replace the old ones nothing here can end that session — and a refresh of
// the session being replaced is let go, so its late answer cannot set that
// session's cookies over the new ones. The caller holds the sign-in when the
// answer leaves an unconfirmed record standing (SignOutHeldError, the
// AuthProvider's gate).
export async function beforeSignIn(): Promise<SignOutResult | "none"> {
  const settled = await settlePendingSignOut();
  cancelScheduledRefresh();
  abortRefreshInFlight();
  return settled;
}

// sessionDelivered records that a credential route delivered a session: its
// cookies replaced the ones the browser held. Requests sent before it belong
// to the previous session, and the lineage this module held names the session
// that was replaced, so it is dropped until an identity read names the new
// one. The record is NOT touched here — resolving it against the delivery is
// the machine's resolveDelivered, which the AuthProvider runs with the kind of
// the delivery (a sign-in the user chose, or a credential change) BEFORE the
// new identity is read, so the read is not blocked by a record whose session
// is already gone from the jar.
export function sessionDelivered(): void {
  sessionGeneration++;
  cancelScheduledRefresh();
  recordRenewal();
}

// RefreshFlight is the POST /auth/refresh in flight. Concurrent callers share
// it: when several calls 401 at once (the access JWT just expired), they all
// await one presentation of the refresh token. Its abort settles every caller
// at once as "unavailable", and cancels the request, so its answer never
// reaches the cookie jar. sentAt is when it presented the token the jar held.
interface RefreshFlight {
  abort: AbortController;
  sentAt: number;
  answer: Promise<RefreshOutcome>;
}

let refreshInFlight: RefreshFlight | null = null;

// REFRESH_RETRY_DELAYS_MS is when a refresh the server could not serve is
// presented again on its own, counted from the previous attempt: the refresh
// may have rotated the token before failing. The server completes a successor
// it handed to nobody (its write or its claim failed) for a presentation
// within five minutes, but redelivers one it handed out — the answer was lost
// on the way — only inside its reuse window (30 s by default), and the user's
// next click may come after it. The series stops at the first answer the
// server gives (renewed or refused) or after its last delay; a request that
// fails its own refresh starts it again.
export const REFRESH_RETRY_DELAYS_MS: readonly number[] = [2000, 5000, 10000];

let refreshRetryTimer: ReturnType<typeof setTimeout> | null = null;
let refreshRetryAttempt = 0;

// refreshEpoch numbers the series a refresh belongs to. Every attempt carries
// the epoch it STARTED under, and an answer from an epoch that is over arms
// nothing and signs nobody out. Three things end one:
//   - cancelScheduledRefresh() from a sign-out. Without the epoch a refresh
//     already in flight when the user signed out schedules a re-presentation
//     that reaches the server AFTER /logout: it presents a token the sign-out
//     revoked, or — when the logout never reached the server, the same
//     incident that made the refresh fail — renews the very session the user
//     left; and its own 401 signs out whatever session the browser holds by
//     then.
//   - a session a credential route delivered: the series belongs to the
//     session it replaced.
//   - an answer (renewed or refused). The series exists for an answer the
//     server could not give; once it gave one there is nothing to re-present,
//     and an attempt still between its own steps must not present the token a
//     renewal has already retired.
let refreshEpoch = 0;

// cancelScheduledRefresh drops a pending re-presentation and ends the epoch —
// a signed-out browser presents nothing, including through an answer that was
// already in flight when it signed out.
export function cancelScheduledRefresh(): void {
  if (refreshRetryTimer !== null) clearTimeout(refreshRetryTimer);
  refreshRetryTimer = null;
  refreshRetryAttempt = 0;
  refreshEpoch++;
}

// SIGNOUT_REFRESH_SETTLE_MS bounds the wait for a refresh found already in
// flight when the sign-out starts. The wait is what makes the cookies the
// logout clears the ones the browser ends up with: a rotation that answers
// while the logout is on the wire sets the successor's cookies AFTER that
// clear, leaving a signed-out browser holding a session's credential. The
// bound is what keeps a refresh that never answers from holding the sign-out
// open, and the abort past it is what keeps that refresh's answer — a 401
// clearing the cookies of whoever signs in next, a 200 setting the previous
// session's over them — from landing after the sign-out.
const SIGNOUT_REFRESH_SETTLE_MS = 2000;

// settleRefreshInFlight waits out the refresh in flight, if any, up to
// SIGNOUT_REFRESH_SETTLE_MS, and aborts it past that. It never throws: the
// flight already turns a failure into an outcome.
async function settleRefreshInFlight(): Promise<void> {
  const flight = refreshInFlight;
  if (!flight) return;
  let cap: ReturnType<typeof setTimeout> | undefined;
  const answered = await Promise.race([
    flight.answer.then(() => true),
    new Promise<boolean>((resolve) => {
      cap = setTimeout(() => resolve(false), SIGNOUT_REFRESH_SETTLE_MS);
    }),
  ]);
  clearTimeout(cap);
  if (!answered) abortRefreshInFlight();
}

function abortRefreshInFlight(): void {
  const flight = refreshInFlight;
  if (!flight) return;
  refreshInFlight = null;
  flight.abort.abort();
}

// SessionLiveness is what an authenticated read says about the credential the
// browser holds: "live" — the access cookie still works; "expired" — the
// server refused it (401/403); "unknown" — it could not answer (5xx, rate
// limit, no answer).
type SessionLiveness = "live" | "expired" | "unknown";

// LIVENESS_READ_TIMEOUT_MS bounds that read. It exists to AVOID a rotation
// another tab already made, and a read that cannot answer in time says
// nothing ("unknown"), so waiting on it only pushes the re-presentation
// further from the rotation it has to reach inside the server's reuse window.
// Measured with every answer taking the server's 5 s store-op timeout: an
// unbounded read moves the series' last re-presentation from 27 s to 42 s
// after the first answer; bounded here it costs 1.5 s per attempt, and
// nothing at all when the read answers (the common case — it is a cheap
// authenticated read, not a rotation).
const LIVENESS_READ_TIMEOUT_MS = 1500;

// readSessionLiveness asks GET /auth/me, which presents the ACCESS cookie and
// no refresh token, so it rotates nothing.
//
// The refresh token is shared by every open tab of the browser while each
// tab's evidence is its own: each holds its own copy of this module, its own
// timers, and reads the one cookie jar. A tab presenting a token another tab
// has already rotated is reuse, which the server answers by revoking the
// token's lineage: every tab of this browser signs out. So before presenting,
// a tab checks its evidence is still current: the renewal stamp first
// (renewedSince — a renewal after the evidence makes it stale), and this read
// where the stamp cannot tell — a scheduled re-presentation, whose evidence is
// an answer the server could not give, and a request with no stamp to compare
// its 401 with. "unknown" presents the token anyway: a server that cannot
// serve this read served no sibling tab's rotation either, so the tabs are
// still on the same token — which the server answers with one shared
// successor — and skipping would lose the renewal the series exists for.
//
// A read rather than a BroadcastChannel announcement: it needs no API that
// jsdom and older browsers lack, and it answers about the credential the
// browser actually holds instead of relaying another document's claim, so it
// also covers a tab that was suspended while the renewal happened.
async function readSessionLiveness(): Promise<SessionLiveness> {
  try {
    const res = await fetchWithin(`${BASE_URL}/auth/me`, { credentials: "include" }, LIVENESS_READ_TIMEOUT_MS);
    if (res.ok) return "live";
    return res.status === 401 || res.status === 403 ? "expired" : "unknown";
  } catch {
    return "unknown";
  }
}

// scheduleRefreshRetry arms the series' next attempt for a presentation the
// server could not answer, sent at evidenceAt: what that presentation was
// about is the token the jar held THEN, so a renewal recorded since — even
// before its failure arrived — means the browser already holds a session at
// least as new as the one its lost answer carried.
function scheduleRefreshRetry(evidenceAt: number): void {
  if (refreshRetryTimer !== null) return;
  const delay = REFRESH_RETRY_DELAYS_MS[refreshRetryAttempt];
  if (delay === undefined) return;
  refreshRetryAttempt++;
  const epoch = refreshEpoch;
  refreshRetryTimer = setTimeout(() => {
    refreshRetryTimer = null;
    void runScheduledRefresh(epoch, evidenceAt);
  }, delay);
}

// runScheduledRefresh is the series' attempt: it re-presents the refresh token
// only while its epoch is the current one, no sign-out is in progress, and
// the session the browser holds was not renewed since the presentation that
// armed it was sent (evidenceAt).
async function runScheduledRefresh(epoch: number, evidenceAt: number): Promise<void> {
  if (epoch !== refreshEpoch || signOutInProgress()) return;
  if (
    renewedSince(evidenceAt) === true ||
    (await readSessionLiveness()) === "live" ||
    // A renewal recorded while the read was out: its "expired" is stale.
    renewedSince(evidenceAt) === true
  ) {
    // Renewed since this series was armed — by another tab, or by a request
    // of this one: the browser holds the successor, and presenting the token
    // the series kept would rotate a retired one.
    if (epoch === refreshEpoch) cancelScheduledRefresh();
    return;
  }
  if (epoch !== refreshEpoch) return;
  // A re-presentation the server refuses ends the session as a request's own
  // refusal does (origin "retry"); one it could not serve schedules the next.
  await presentRefreshToken("retry");
}

// RefreshOrigin is who presents the refresh token: a request's 401, the retry
// series, or an identity load (the bootstrap, the stale tree's re-read), which
// handles its own outcome — it arms no series and signs nobody out.
type RefreshOrigin = "request" | "retry" | "identity";

// presentRefreshToken presents the refresh token — joining the presentation
// in flight, if any — and acts on the answer: an answer the server could not
// give arms the series, any other ends it, and a refusal of the series' own
// re-presentation signs out. A presentation is refused while a sign-out is in
// progress (the logout is about to end this session, and a rotation would
// hand the browser a successor after the logout cleared the jar), and an
// answer from an epoch that is over — a sign-out ends the epoch when it starts
// — acts on nothing.
function presentRefreshToken(origin: RefreshOrigin): Promise<RefreshOutcome> {
  if (signOutInProgress()) return Promise.resolve({ kind: "unavailable", status: null });
  const epoch = refreshEpoch;
  const flight = refreshInFlight ?? startRefreshFlight();
  return flight.answer.then((outcome) => {
    if (epoch !== refreshEpoch) return outcome;
    if (outcome.kind === "unavailable") {
      if (origin === "identity") return outcome;
      if (origin === "request" && refreshRetryTimer === null) refreshRetryAttempt = 0;
      scheduleRefreshRetry(flight.sentAt);
      return outcome;
    }
    cancelScheduledRefresh();
    if (outcome.kind === "rejected" && origin === "retry" && onUnauthorized) onUnauthorized();
    return outcome;
  });
}

function startRefreshFlight(): RefreshFlight {
  const abort = new AbortController();
  const aborted = new Promise<RefreshOutcome>((resolve) => {
    abort.signal.addEventListener("abort", () => resolve({ kind: "unavailable", status: null }), { once: true });
  });
  const sentAt = Date.now();
  const answered = fetch(`${BASE_URL}/auth/refresh`, {
    method: "POST",
    credentials: "include",
    signal: abort.signal,
  })
    .then((r): RefreshOutcome => {
      const outcome = refreshOutcomeOf(r.status);
      if (outcome.kind === "renewed" && !abort.signal.aborted) recordRenewal();
      return outcome;
    })
    .catch((): RefreshOutcome => ({ kind: "unavailable", status: null }));
  const flight: RefreshFlight = {
    abort,
    sentAt,
    answer: Promise.race([answered, aborted]).finally(() => {
      if (refreshInFlight === flight) refreshInFlight = null;
    }),
  };
  refreshInFlight = flight;
  return flight;
}

// Renewal is what a refused credential (a 401) came to: a RefreshOutcome, or
// "superseded" — the session was renewed after the evidence was sent (by
// another tab, or by this one), so the evidence is stale and nothing was
// presented: the caller asks again with the cookie the browser holds now.
type Renewal = RefreshOutcome | { kind: "superseded" };

// renewAfterRefusal renews the session for a 401 sent at evidenceSentAt. A 401
// is evidence about the access cookie the request was SENT with, not about the
// one the browser holds when the 401 arrives: another tab may have renewed in
// between, and presenting the refresh token then rotates the successor that
// tab just received — which turns any presentation of the previous token still
// in flight into reuse, and the lineage is revoked. So a renewal recorded at or
// after the send supersedes the evidence; where no stamp can tell, a request
// checks the cookie it holds now (readSessionLiveness) — an identity read does
// not, being that very check. That read is evidence of the same kind — about
// the cookie it was sent with — so the stamp is asked again once it answered:
// a renewal recorded while it was out supersedes its "expired" too. And the
// session the read was for may be over by then (a sign-out began, a session
// was delivered): nothing is presented for it.
async function renewAfterRefusal(evidenceSentAt: number, origin: "request" | "identity"): Promise<Renewal> {
  if (signOutInProgress()) return { kind: "unavailable", status: null };
  const generation = sessionGeneration;
  let since = renewedSince(evidenceSentAt);
  if (since === null && origin === "request") {
    if ((await readSessionLiveness()) === "live") return { kind: "superseded" };
    if (generation !== sessionGeneration) return { kind: "unavailable", status: null };
    since = renewedSince(evidenceSentAt);
  }
  if (since === true) return { kind: "superseded" };
  return presentRefreshToken(origin);
}

// renewSession renews the session for an identity read the server refused
// (401) — the bootstrap's, the stale tree's re-read — through the same guarded
// path a request takes: deduped with every other presentation (so a sign-out
// settles it), refused while a sign-out is in progress, superseded by a
// renewal made after the read was sent. "renewed" covers that last case too:
// the caller reads again either way.
export async function renewSession(evidenceSentAt: number): Promise<RefreshOutcome> {
  const renewal = await renewAfterRefusal(evidenceSentAt, "identity");
  return renewal.kind === "superseded" ? { kind: "renewed" } : renewal;
}

// ApiError is the typed error thrown by request/apiRequest when the
// HTTP status is non-2xx. Domain clients use `err instanceof ApiError
// && err.status === N` to react to specific statuses (notably 404 for
// feature-gating via `guard404`). The message keeps the historical
// `API error <status>: <body>` shape so toasts read the same as
// before.
export class ApiError extends Error {
  status: number;
  errorCode?: string;
  // The server's own message, without the "API error <status>: " prefix the
  // transport puts on `message` — for a surface that quotes the reason.
  detail?: string;
  constructor(status: number, message: string, errorCode?: string, detail?: string) {
    super(message);
    this.status = status;
    this.errorCode = errorCode;
    this.detail = detail;
    this.name = "ApiError";
  }
}

// SessionRenewalUnavailableError is what a request throws when its access
// token expired and the silent refresh could not renew the session right
// now: a 5xx, a rate limit or a network error on POST /auth/refresh. It is
// transient — the browser still holds its session, nothing signed it out,
// and the next request tries the renewal again. status is 503 (the server
// could not serve the request now); refreshStatus is what the refresh
// answered, null when it never answered.
export class SessionRenewalUnavailableError extends ApiError {
  refreshStatus: number | null;
  constructor(refreshStatus: number | null) {
    const cause = refreshStatus === null ? "no answer" : `HTTP ${refreshStatus}`;
    super(503, `API error 503: the session could not be renewed (${cause}); retry shortly`, "session_renewal_unavailable");
    this.refreshStatus = refreshStatus;
    this.name = "SessionRenewalUnavailableError";
  }
}

// apiErrorFrom is the error every non-2xx response throws. Exported so a test
// can hand a caller the exact shape production produces, prefix included.
export function apiErrorFrom(status: number, detail: ExtractedErrorDetail): ApiError {
  return new ApiError(status, `API error ${status}: ${detail.message}`, detail.errorCode, detail.message);
}

// is404 reports whether a thrown value represents an HTTP 404. Prefers
// the typed ApiError.status check; the message-substring fallback exists
// because a few fetchers (fetchToolBlob's direct fetch, and any older
// path not yet routed through apiRequest) still throw plain Errors that
// only carry the historical "API error <status>: …" message shape.
// Remove the fallback once every fetcher throws ApiError.
export function is404(err: unknown): boolean {
  if (err instanceof ApiError) return err.status === 404;
  return err instanceof Error && err.message.includes("API error 404");
}

// FeatureUnavailableError marks a server endpoint that is not enabled
// on the current deployment (HTTP 404 on a domain route). Views catch
// it and render an EmptyState "Not enabled on this server" instead of
// crashing. Detection is class-based (instanceof) so the guard is
// robust against minified error messages.
export class FeatureUnavailableError extends Error {
  feature: string;
  constructor(feature: string, message?: string) {
    super(message ?? `${feature} not available on this server`);
    this.feature = feature;
    this.name = "FeatureUnavailableError";
  }
}

// guard404 wraps a domain request and converts an ApiError(404) into
// a typed FeatureUnavailableError so the calling view can show the
// "not enabled" empty state. Any other error type or status is
// rethrown unchanged.
export async function guard404<T>(feature: string, fn: () => Promise<T>): Promise<T> {
  try {
    return await fn();
  } catch (err) {
    if (err instanceof ApiError && err.status === 404) {
      throw new FeatureUnavailableError(feature, err.message);
    }
    throw err;
  }
}

// request is exported so other api/*.ts modules (api/projects.ts,
// future per-domain clients) share the same 401-handling and JSON-
// decoding semantics. It prefixes BASE_URL on the supplied path.
export async function request<T>(path: string, init?: RequestInit): Promise<T> {
  return apiRequest<T>(`${BASE_URL}${path}`, init);
}

// apiRequest is the same fetch wrapper but takes a fully-qualified
// path. Used by /api/v1/dispatcher/* and /api/v1/native/* clients that
// don't sit under the BASE_URL /api root. 204 No Content returns
// `undefined as T` so DELETE-style endpoints don't trip over an empty
// body. isRetry: the request is a replay after its session was renewed, so a
// 401 renews nothing more.
export async function apiRequest<T>(
  fullPath: string,
  init?: RequestInit,
  isRetry = false,
): Promise<T> {
  // Workspace pane: literal /api/... paths (native board, dispatcher, bots, …)
  // bypass BASE_URL, so scope them here — the single choke point every
  // apiRequest caller flows through. Paths already carrying the scope (built
  // from BASE_URL = apiBase(), i.e. "/x/<id>/api/…") don't start with "/api",
  // so they're never double-prefixed. No-op outside a pane (scopePrefix() "").
  if (fullPath.startsWith("/api")) fullPath = scopePrefix() + fullPath;
  return sendRequest<T>(fullPath, init, isRetry ? "refreshed" : "fresh");
}

// RenewalStage is what a request already did about a 401: nothing ("fresh"),
// replayed itself because the session was renewed after it was sent
// ("replayed"), or had the session renewed for it ("refreshed").
type RenewalStage = "fresh" | "replayed" | "refreshed";

async function sendRequest<T>(fullPath: string, init: RequestInit | undefined, stage: RenewalStage): Promise<T> {
  const generation = sessionGeneration;
  const sentAt = Date.now();
  const res = await fetch(fullPath, {
    credentials: "include",
    headers: { "Content-Type": "application/json", ...(init?.headers ?? {}) },
    ...init,
  });
  // 401 → the access JWT likely just expired. Renew the session and replay
  // the request: without that the first call after the 15-minute access-token
  // TTL bounces the user to login even though their refresh token is valid
  // for weeks.
  //   - A 401 is evidence about the access cookie the request was sent with:
  //     when the session was renewed after the send — by another tab, or by
  //     this one — the request is replayed on the renewed cookie instead
  //     (renewAfterRefusal), once; a 401 to that replay is current evidence.
  //   - Only a refresh the server REFUSES (401/403) signs out; one it could
  //     not serve (5xx, rate limit, network) fails this request as transient
  //     and leaves the session alone.
  //   - A request sent for a session this tab has since left (a sign-out
  //     began, or another session was delivered: sessionGeneration) renews
  //     nothing and signs nothing out, whenever its 401 or its renewal's
  //     answer arrives — what it would renew, or sign out, is the session
  //     that replaced it. It fails with its own 401.
  //   - Not for the refresh endpoint itself (no loop), nor after the session
  //     was already renewed for this request.
  // In a workspace pane the desktop owns the token lifecycle: the demux proxy
  // strips the request Cookie and injects a Bearer (refreshed off the OS-held
  // jar by the cloudRoundTripper + background loop), so a pane-side
  // POST /auth/refresh has no refresh cookie and can only fail. Skip it — a 401
  // reaching the pane means the desktop's refresh already failed, and recovery
  // is driven from the shell (cloud:auth-expired → re-login → reload). We still
  // fire onUnauthorized so the pane's AuthGate can surface the expired state.
  if (res.status === 401 && generation === sessionGeneration) {
    if (stage !== "refreshed" && !isScopedPane() && !fullPath.endsWith("/auth/refresh")) {
      const renewal = stage === "replayed" ? await presentRefreshToken("request") : await renewAfterRefusal(sentAt, "request");
      if (generation === sessionGeneration) {
        if (renewal.kind === "superseded") return sendRequest<T>(fullPath, init, "replayed");
        if (renewal.kind === "renewed") return sendRequest<T>(fullPath, init, "refreshed");
        if (renewal.kind === "unavailable") throw new SessionRenewalUnavailableError(renewal.status);
        if (onUnauthorized) onUnauthorized();
      }
    } else if (onUnauthorized) {
      onUnauthorized();
    }
  }
  if (!res.ok) {
    throw apiErrorFrom(res.status, await extractErrorDetail(res));
  }
  // 204 No Content (e.g. DELETE endpoints) has an empty body. Don't
  // try to parse it — return undefined and let the typed caller cast.
  if (res.status === 204) return undefined as unknown as T;
  // A 2xx with a non-JSON body — e.g. the SPA index.html served for an
  // unrouted /api path — would make res.json() throw a cryptic
  // "JSON.parse: unexpected character at line 1 column 1". Surface a clear,
  // actionable error instead. (The backend now returns a JSON 404 for such
  // paths; this is defense-in-depth against any other non-JSON 2xx.)
  try {
    return (await res.json()) as T;
  } catch {
    const ct = res.headers.get("content-type") ?? "unknown";
    throw new ApiError(
      res.status,
      `expected JSON from ${fullPath} but received ${ct} — the endpoint may be unavailable in this mode`,
    );
  }
}

interface ExtractedErrorDetail {
  message: string;
  errorCode?: string;
}

// extractErrorDetail decodes both the human-facing message and the optional
// stable machine code from a JSON error envelope. It consumes the response
// body once so apiRequest can preserve both fields on ApiError.
async function extractErrorDetail(res: Response): Promise<ExtractedErrorDetail> {
  const text = await res.text();
  if (!text) return { message: res.statusText || "" };
  try {
    const body = JSON.parse(text) as unknown;
    if (body && typeof body === "object") {
      const env = body as {
        error?: unknown;
        error_code?: unknown;
        message?: unknown;
        detail?: unknown;
        reset_at?: unknown;
      };
      const errorCode =
        typeof env.error_code === "string" && env.error_code
          ? env.error_code
          : undefined;
      // Quota/launch denial envelopes carry the human-readable copy in
      // `detail`; prefer it (with the short `error` token as a prefix)
      // so toasts read "monthly_run_quota_exceeded: 1000/1000 — resets …".
      if (typeof env.detail === "string" && env.detail) {
        const token = typeof env.error === "string" && env.error ? env.error : "";
        const tail = typeof env.reset_at === "string" ? ` (resets ${env.reset_at})` : "";
        return {
          message: token ? `${token}: ${env.detail}${tail}` : `${env.detail}${tail}`,
          errorCode,
        };
      }
      if (typeof env.error === "string" && env.error) {
        return { message: env.error, errorCode };
      }
      if (typeof env.message === "string" && env.message) {
        return { message: env.message, errorCode };
      }
    }
  } catch {
    // Not JSON — fall through to the raw text.
  }
  return { message: text };
}

// extractErrorMessage prefers a structured envelope field (`error` or
// `message`) over the raw body, so the toast shown to the user reads
// "forbidden" rather than `{"error":"forbidden"}` for the common Go
// `httpError` shape served by pkg/server. Exported so other api/*.ts
// modules that hit `fetch` directly (file blobs, backend detect, …)
// share the same error-shape rendering.
export async function extractErrorMessage(res: Response): Promise<string> {
  return (await extractErrorDetail(res)).message;
}

/**
 * Wire-format diagnostic mirror of `ir.DiagnosticDTO` on the Go side.
 * Code/NodeID/EdgeID/Hint may be empty for parser-only paths.
 */
export interface DiagnosticIssue {
  code?: string;
  severity: "error" | "warning";
  message: string;
  node_id?: string;
  edge_id?: string;
  hint?: string;
}

export async function parseSource(
  source: string,
  options?: { signal?: AbortSignal },
): Promise<{
  document: IterDocument;
  diagnostics: string[];
  issues?: DiagnosticIssue[];
  /** False when the parse left errors: the document is what the parser
   *  SALVAGED, so a caller that opens a file through this must bind no
   *  path to it. Absent from an older server. */
  bindable?: boolean;
  /** Set when the server's reader refused the text and made the document a
   *  salvage (ReadRefusal): the cause and its remedy, never the author's
   *  text. */
  read_refusal?: ReadRefusal;
}> {
  return request("/parse", {
    method: "POST",
    body: JSON.stringify({ source }),
    signal: options?.signal,
  });
}

/** UnparsedSource is a document rendered as .bot source, with the reason
 *  the writer cannot reproduce the file it came from when that is the
 *  case. */
export interface UnparsedSource {
  source: string;
  /** Set when a value the file writes over several lines would come back
   *  as one (#1612, pkg/dsl/canon). Folding is a before/after property, so
   *  it is answered only where the server holds the file's current text:
   *  the server READS that text itself, from `path` or from the unit's
   *  files, and the client never supplies it. `source` is then the file's
   *  own text, never the writer's. The paths that WRITE refuse outright;
   *  this is what the ones that DISPLAY get instead, so the author reads
   *  their file rather than nothing. */
  refused?: string;
  /** True when `source` is the bytes the server READ — the file as it is
   *  kept — and not a render of the document. It decides whether the
   *  answer may be handed to the author AS their file: the per-file and
   *  single-file refusals read a file, the merged (`flatten`) one renders
   *  a program that is no file at all. Reading `refused` as "so source is
   *  the file" is right three times out of four and hands over a
   *  collapsed render the fourth. */
  stored?: boolean;
}

/** unparse renders a document as .bot source. `flatten` renders the merged
 *  program of a bot in several files for DISPLAY only: the server refuses
 *  to render such a document as one file otherwise, since a save of that
 *  text would fold every file into the main.
 *
 *  `path` is the workspace file the document was opened from, and it is
 *  what lets the answer carry `refused`: a fold is a before/after property
 *  and the server reads that before itself. Passing the text instead would
 *  make the guard depend on `currentSource` — a cache the client writes. */
export async function unparse(
  document: IterDocument,
  options?: { flatten?: boolean; path?: string | null },
): Promise<UnparsedSource> {
  const bs = options?.path ? parseBotSourceEditorPath(options.path) : null;
  if (bs) {
    // A cloud bot has a before too — the bundle holds it — and the server
    // cannot read one off a `botsource://` path, which resolves to no file
    // on its disk. Both shapes carry the files map instead: a per-file
    // render for a bot in one file (a unit of one, whose single part is
    // the whole document), and the bundle's files beside the merged text
    // so a flatten can name what that text no longer carries.
    const bundle = await apiRequest<BotSourceFilesResponse>(
      `/api/teams/${encodeURIComponent(bs.teamID)}/bot-sources/${encodeURIComponent(bs.slug)}`,
    );
    const files = bundle.files ?? {};
    return request("/unparse", {
      method: "POST",
      body: JSON.stringify(
        options?.flatten
          ? { document, files, main: bs.rel, flatten: true }
          : { document, files, main: bs.rel, file: bs.rel },
      ),
    });
  }
  const res = await request<UnparsedSource>("/unparse", {
    method: "POST",
    body: JSON.stringify({
      document,
      ...(options?.flatten ? { flatten: true } : {}),
      ...(options?.path ? { path: options.path } : {}),
    }),
  });
  return { source: res.source, refused: res.refused, stored: res.stored };
}

/** unparseUnitFile renders ONE file of a bot in several files: what the
 *  Source view's picker shows for the file it is on. Read-only — it
 *  presents no revision and writes nothing — so a file the writer cannot
 *  reproduce still comes back, as ITS OWN text, with the reason. */
export async function unparseUnitFile(
  document: IterDocument,
  editorPath: string,
  rel: string,
  unitFiles?: UnitFileInfo[],
): Promise<UnparsedSource> {
  const bs = parseBotSourceEditorPath(editorPath);
  if (bs) {
    const bundle = await apiRequest<BotSourceFilesResponse>(
      `/api/teams/${encodeURIComponent(bs.teamID)}/bot-sources/${encodeURIComponent(bs.slug)}`,
    );
    return request("/unparse", {
      method: "POST",
      body: JSON.stringify({ document, files: bundle.files ?? {}, main: bs.rel, file: rel, ...(unitFiles ? { unit_files: unitFiles } : {}) }),
    });
  }
  return request("/unparse", {
    method: "POST",
    body: JSON.stringify({ document, path: editorPath, file: rel, ...(unitFiles ? { unit_files: unitFiles } : {}) }),
  });
}

/** parseUnitFile re-parses a bot in several files with ONE of them
 *  replaced by the text the Source view holds: the author edits a real
 *  file, and the merged document the canvas and the save work from is
 *  rebuilt from it.
 *
 *  It answers NO revision, and the caller keeps the one it opened at: a
 *  revision is a claim about the files at rest, and an overlay moved none.
 *  Taking the staged unit's digest would make the very next save a false
 *  conflict; taking the current one would adopt a colleague's edit. */
export async function parseUnitFile(
  editorPath: string,
  rel: string,
  source: string,
): Promise<{ document: IterDocument; diagnostics: string[]; unit?: UnitInfo; bindable?: boolean; read_refusal?: ReadRefusal }> {
  const bs = parseBotSourceEditorPath(editorPath);
  if (bs) {
    const bundle = await apiRequest<BotSourceFilesResponse>(
      `/api/teams/${encodeURIComponent(bs.teamID)}/bot-sources/${encodeURIComponent(bs.slug)}`,
    );
    return request("/parse", {
      method: "POST",
      body: JSON.stringify({ files: bundle.files ?? {}, main: bs.rel, file: rel, source }),
    });
  }
  return request("/parse", {
    method: "POST",
    body: JSON.stringify({ path: editorPath, file: rel, source }),
  });
}

/** A bot in several files has an `import "lib/x.bot"` line at the head of
 *  its main. */
export function importsFragments(source: string): boolean {
  return /^import\s+"/m.test(source);
}

/** parseUnit parses a bot in several files, given as a bundle's files map,
 *  as its unit: one document whose every declaration names its file, and
 *  the unit's revision. */
export async function parseUnit(
  files: Record<string, string>,
  main: string,
  options?: { signal?: AbortSignal },
): Promise<{
  document: IterDocument;
  diagnostics: string[];
  unit?: UnitInfo;
  /** False when the MAIN's own parse left errors: the merged document is
   *  then a salvage. A unit that merely fails to LOAD stays true — its
   *  write back is refused server-side, naming the fragment. */
  bindable?: boolean;
  /** Set when the server's reader refused the text and made the document a
   *  salvage (ReadRefusal): the cause and its remedy, never the author's
   *  text. */
  read_refusal?: ReadRefusal;
}> {
  return request("/parse", {
    method: "POST",
    body: JSON.stringify({ files, main }),
    signal: options?.signal,
  });
}

/** unparseUnit writes a document of a bot in several files back into them,
 *  by provenance, and returns the files whose program changed — only
 *  those, to patch into the bundle — and the revision the bundle has once
 *  they are. `revision` is the one the document was opened at: the server
 *  refuses (409) a bundle whose files moved since, so a fragment a
 *  colleague changed is never rewritten with this document's text. */
export async function unparseUnit(
  document: IterDocument,
  files: Record<string, string>,
  main: string,
  revision: string,
  unitFiles?: UnitFileInfo[],
): Promise<{ source: string; files: Record<string, string>; revision?: string }> {
  const res = await request<{ source: string; files?: Record<string, string>; revision?: string }>("/unparse", {
    method: "POST",
    body: JSON.stringify({ document, files, main, revision, ...(unitFiles ? { unit_files: unitFiles } : {}) }),
  });
  return { source: res.source, files: res.files ?? {}, revision: res.revision };
}

/** validate compiles the document server-side. `path` is the workspace
 *  file it was opened from, when known: a main.bot inside a bundle is then
 *  validated with the bundle's prompts/*.md in scope, the way a launch
 *  compiles it, instead of refusing every prompt the bundle ships. */
export async function validate(
  document: IterDocument,
  signal?: AbortSignal,
  path?: string | null,
): Promise<{
  diagnostics: string[];
  warnings: string[];
  issues?: DiagnosticIssue[];
}> {
  return request("/validate", {
    method: "POST",
    body: JSON.stringify(path ? { document, path } : { document }),
    signal,
  });
}

export interface ExampleEntry {
  /** Relative load name, e.g. "whats-next/main.bot". The technical bot
   *  id is its first path segment ("whats-next"). */
  name: string;
  /** Bundle persona (manifest display_name), e.g. "Nexie". Empty for
   *  embedded recipes that ship no on-disk manifest. */
  display_name?: string;
  /** One-line manifest description, condensed server-side. */
  description?: string;
}

// The /examples endpoint returns the first-class bots as
// {name, display_name} objects (the Home shows the persona).
export async function listExampleEntries(): Promise<ExampleEntry[]> {
  return request("/examples");
}

// listExamples keeps the legacy string[] shape (relative names) for the
// FilePicker + CanvasEmpty surfaces that don't render the persona.
export async function listExamples(): Promise<string[]> {
  return (await listExampleEntries()).map((e) => e.name);
}

export async function loadExample(
  name: string,
  options?: { signal?: AbortSignal },
): Promise<{
  source: string;
  document: IterDocument;
  diagnostics: string[];
  /** The path the studio opens and saves the example by: set for a file
   *  inside the workspace, in one file or several, whether or not it
   *  parses; absent for an embedded bot or one outside the workspace, which
   *  the studio binds as bots/<name>, where a save of the one program
   *  lands. */
  path?: string;
  /** The on-disk path the server read, when `path` is set — what
   *  /api/files/open confirms for the same path. */
  confirmed_disk_path?: string;
  /** False when the example does not parse: `document` is the salvage, and
   *  the sites that write a document refuse it. Absent (an older server, a
   *  unit response) reads as true. */
  bindable?: boolean;
  /** Set when the server's reader refused the text and made the document a
   *  salvage (ReadRefusal): the cause and its remedy, never the author's
   *  text. */
  read_refusal?: ReadRefusal;
  /** Set when the example is a bot in several files inside the workspace:
   *  the document is the merged unit, and the unit carries the files' parse
   *  and load verdict — what the launch gate refuses on, whether or not
   *  they load as one bot, since this door opens the same bots
   *  /api/files/open does. An embedded bot, or one outside the workspace,
   *  is served as one flat program and has none. */
  unit?: UnitInfo;
  /** The digest the open issued for the program's file (the digest rule):
   *  a save of it presents this. Absent for a refused open. */
  digest?: string;
}> {
  // Encode each path segment but keep the slashes so subdirectory
  // examples (e.g. "feature_dev/main.bot") route correctly.
  const encoded = name.split("/").map(encodeURIComponent).join("/");
  return request(`/examples/${encoded}`, { signal: options?.signal });
}

// File management

export async function listFiles(): Promise<FileEntry[]> {
  const res = await request<ListFilesResponse>("/files");
  return res.files;
}

// BOTSOURCE_SCHEME marks a virtual editor path that resolves to a team-authored
// bot bundle in the cloud store (pkg/botsource) instead of a local file. It lets
// the whole editor UI stay filesystem-shaped in cloud mode: a tab's file is
// "botsource://<teamId>/<slug>/<relpath>", and openFile/saveFile below route it
// to /api/teams/{id}/bot-sources/{slug} transparently.
export const BOTSOURCE_SCHEME = "botsource://";

export function botSourceEditorPath(teamID: string, slug: string, rel = "main.bot"): string {
  return `${BOTSOURCE_SCHEME}${teamID}/${slug}/${rel}`;
}

// inferCatalogBotId derives a catalog bot id from a workflow file path, mirroring
// the server's inferCatalogBotID: "bots/<id>/main.bot", ".../examples/<id>/main.bot",
// "<id>/main.bot" and "<id>.bot" all map to <id>. Used to fork the bot the editor
// currently shows read-only.
export function inferCatalogBotId(path: string): string {
  const parts = path.split("/").filter(Boolean);
  const i = parts.findIndex((p) => p === "bots" || p === "examples");
  if (i >= 0 && parts[i + 1]) return parts[i + 1]!;
  const last = parts[parts.length - 1] ?? "";
  if (parts.length >= 2 && last === "main.bot") {
    return parts[parts.length - 2] ?? "";
  }
  return last.replace(/\.bot$/, "");
}

export function parseBotSourceEditorPath(
  path: string,
): { teamID: string; slug: string; rel: string } | null {
  if (!path.startsWith(BOTSOURCE_SCHEME)) return null;
  const rest = path.slice(BOTSOURCE_SCHEME.length);
  const parts = rest.split("/");
  if (parts.length < 3 || !parts[0] || !parts[1]) return null;
  return { teamID: parts[0], slug: parts[1], rel: parts.slice(2).join("/") };
}

interface BotSourceFilesResponse {
  id: string;
  slug: string;
  files: Record<string, string>;
  origin?: string;
  version: number;
  /** sha256 of each file's content as answered: the digests a save presents
   *  back (the digest rule). */
  file_sha256?: Record<string, string>;
}

// openedBotSources holds, by editor path, the bundle read each cloud save is
// decided on — the digest rule: the write carries the id, the version and
// the per-file digests THE OPEN ISSUED, never a fresh fetch (a fetch at save
// time would turn a concurrent editor's write, and a bot deleted and
// re-created, into silent overwrites). openFile sets it, saveFile consumes
// it; a save with no snapshot on file is refused — the studio holds no read
// of that bot to save against, and one must be made (reopen) before writing.
const openedBotSources = new Map<
  string,
  { id: string; version: number; files: Record<string, string>; digests: Record<string, string> }
>();

export async function openFile(
  path: string,
  options?: { signal?: AbortSignal },
): Promise<{
  source: string;
  document: IterDocument;
  diagnostics: string[];
  /** The file this answer is about, whether or not it parses: the editor is
   *  ABOUT that file. */
  path?: string;
  confirmed_disk_path?: string;
  /** False when the file does not parse: `document` is then what the parser
   *  SALVAGED, so the three sites that write a document refuse it. Carried
   *  for a cloud bot source too, which this client fetches and parses itself
   *  below — /api/parse answers the same verdict, so the two surfaces refuse
   *  on the same grounds. A unit stays writable either way: its write back is
   *  refused server-side, naming the fragment at fault. */
  bindable?: boolean;
  /** Set when the server's reader refused the text and made the document a
   *  salvage (ReadRefusal): the cause and its remedy, never the author's
   *  text. */
  read_refusal?: ReadRefusal;
  /** Set when the file is the main of a bot in several files: the document
   *  is the merged unit, and a save must present `unit.revision` (its files
   *  carry their own digests). */
  unit?: UnitInfo;
  /** The digest the open issued for this file (the digest rule): a save of
   *  it presents this. Absent for a refused open — no whole read, no
   *  digest — and for a cloud bot source (the opened bundle snapshot holds
   *  the digests). */
  digest?: string;
}> {
  const bs = parseBotSourceEditorPath(path);
  if (bs) {
    const bundle = await apiRequest<BotSourceFilesResponse>(
      `/api/teams/${encodeURIComponent(bs.teamID)}/bot-sources/${encodeURIComponent(bs.slug)}`,
      { signal: options?.signal },
    );
    const files = bundle.files ?? {};
    const digests = bundle.file_sha256 ?? {};
    openedBotSources.set(path, { id: bundle.id, version: bundle.version, files, digests });
    const source = files[bs.rel] ?? "";
    if (isWorkflowFile(bs.rel) && importsFragments(source)) {
      // A workflow in several files — the bundle's main, or a companion
      // workflow of its own: the unit is parsed from the whole files map
      // with that file as its main, so the fragments its imports reach
      // are in the document.
      const parsed = await parseUnit(bundle.files ?? {}, bs.rel, options);
      // The server's verdict, not a hardcoded true: a unit that does not
      // LOAD stays writable — its write back is refused there, naming the
      // fragment — but a main that did not PARSE makes the merged document a
      // salvage, and the sites that export it have to be told.
      return {
        source,
        document: parsed.document,
        diagnostics: parsed.diagnostics,
        path,
        unit: parsed.unit,
        bindable: parsed.bindable !== false,
        read_refusal: parsed.read_refusal,
      };
    }
    const parsed = await parseSource(source, options);
    // The verdict travels with the answer, the way /api/files/open carries it
    // for a file on disk. It has to: this path's save is a versioned PUT of
    // `unparse(document)`, and the compile guard behind it PASSES a salvage —
    // a salvage compiles, it is simply missing what the parser could not read.
    return {
      source,
      document: parsed.document,
      diagnostics: parsed.diagnostics,
      path,
      bindable: parsed.bindable !== false,
      read_refusal: parsed.read_refusal,
    };
  }
  return request("/files/open", {
    method: "POST",
    body: JSON.stringify({ path }),
    signal: options?.signal,
  });
}

export async function saveFile(
  path: string,
  document: IterDocument,
  options?: { createOnly?: boolean; revision?: string; unitFiles?: UnitFileInfo[]; digest?: string },
): Promise<SaveFileResponse> {
  const bs = parseBotSourceEditorPath(path);
  if (bs) {
    if (options?.createOnly) {
      throw new Error("Save As is not available for a cloud bot source.");
    }
    // The digest rule: the save carries the read it was decided on — the
    // bot's id and version, and, per file it replaces, the digest the open
    // issued. The read is THIS tab's, taken at open (openedBotSources): a
    // fetch at save time would decide the write on bytes nobody this save
    // knows read whole, and turn a concurrent editor's write — or a bot
    // deleted and re-created — into a silent overwrite.
    const opened = openedBotSources.get(path);
    if (!opened) {
      throw new Error(
        `the studio holds no read of ${bs.slug} to save against: reopen the bot (a save presents the digest its open issued)`,
      );
    }
    if (options?.revision !== undefined) {
      // A bot in several files — the document was opened as its unit and
      // carries the revision it was opened at, which decides the path:
      // never the stored text at save time, which may have moved. It is
      // written back file by file
      // — only the files whose program changed come back — patched into
      // the whole bundle THE OPEN held, and written as ONE versioned PUT
      // carrying the opened digests, so manifest, prompts, skills and every
      // other file survive and a concurrent editor is a conflict, never a
      // silent overwrite. The file list the client holds (unit.files) goes
      // with it: a per-file edit may have changed a file's `import` lines
      // or its `dsl:` profile, which the document does not carry.
      const rewritten = await unparseUnit(document, opened.files, bs.rel, options.revision, options.unitFiles);
      const files = { ...opened.files, ...rewritten.files };
      const digests: Record<string, string> = {};
      for (const rel of Object.keys(rewritten.files)) {
        digests[rel] = opened.digests[rel] ?? "";
      }
      const updated = await apiRequest<BotSourceFilesResponse>(
        `/api/teams/${encodeURIComponent(bs.teamID)}/bot-sources/${encodeURIComponent(bs.slug)}`,
        {
          method: "PUT",
          body: JSON.stringify({
            files,
            id: opened.id,
            version: opened.version,
            stored_sha256: digests,
          }),
        },
      );
      // The write's answer is the bundle as it now stands — what the NEXT
      // save presents (the version CAS, the per-file digests). Held stale,
      // the second save of an edit round is a 409 against this tab's own
      // write. A concurrent editor's write after this one still conflicts:
      // the snapshot is this write's own answer, never a fresh fetch.
      openedBotSources.set(path, {
        id: updated.id,
        version: updated.version,
        files: updated.files ?? files,
        digests: updated.file_sha256 ?? {},
      });
      return { path, source: rewritten.source, files: Object.keys(rewritten.files).sort(), revision: rewritten.revision };
    }
    const { source } = await unparse(document);
    const updated = await apiRequest<BotSourceFilesResponse>(
      `/api/teams/${encodeURIComponent(bs.teamID)}/bot-sources/${encodeURIComponent(bs.slug)}/files/${bs.rel}`,
      {
        method: "PUT",
        body: JSON.stringify({
          content: source,
          id: opened.id,
          version: opened.version,
          stored_sha256: opened.digests[bs.rel] ?? "",
        }),
      },
    );
    // Same refresh: the answer carries the claim the next save of this
    // file presents.
    openedBotSources.set(path, {
      id: updated.id,
      version: updated.version,
      files: updated.files ?? {},
      digests: updated.file_sha256 ?? {},
    });
    return { path, source };
  }
  return request("/files/save", {
    method: "POST",
    body: JSON.stringify({
      path,
      document,
      ...(options?.createOnly ? { create_only: true } : {}),
      ...(options?.revision ? { revision: options.revision } : {}),
      ...(options?.unitFiles ? { unit_files: options.unitFiles } : {}),
      // The digest rule: what this save replaces, it holds the digest the
      // open issued — a concurrent edit is a 409, never an overwrite.
      ...(options?.digest ? { digest: options.digest } : {}),
    }),
  });
}

export interface SharedBundleFileMetadata {
  name: string;
  version?: string;
  workflow?: string;
  bundle_sha256?: string;
  verified: boolean;
}

export interface FileDependencyMetadata {
  read_only: boolean;
  shared_bundle?: SharedBundleFileMetadata;
}

export async function getFileDependencyMetadata(
  path: string,
): Promise<FileDependencyMetadata> {
  return request("/files/dependency", {
    method: "POST",
    body: JSON.stringify({ path }),
  });
}

// Reasoning effort capabilities

export interface EffortCapabilities {
  supported: string[] | null;
  default: string;
  source: "claw-registry" | "codex-cli" | "codex-fallback";
}

export async function fetchEffortCapabilities(
  backend: string,
  model: string,
  signal?: AbortSignal,
): Promise<EffortCapabilities> {
  const params = new URLSearchParams({ backend, model });
  return request<EffortCapabilities>(`/effort-capabilities?${params.toString()}`, { signal });
}

// Model capabilities — the limits + published price a picker captions
// its selection with.

export interface ModelCapabilities {
  spec: string;
  provider?: string;
  model: string;
  // "aggregator" is a settled answer; "curated" is what a cold lookup
  // returns while the background spec refresh is still in flight, so it
  // can still improve and must not be cached for the whole session.
  source: "aggregator" | "curated";
  context_window: number;
  max_output_tokens: number;
  // Per-million-token prices. Zero means the aggregator had no price —
  // "unknown", never "free".
  input_cost_per_m: number;
  output_cost_per_m: number;
  // Omitted for a bare model id: the flags resolve through a curated
  // per-provider branch, and a bare id names no provider.
  reasoning?: boolean;
  tool_call?: boolean;
  temperature?: boolean;
}

// fetchModelCapabilities accepts both a qualified "provider/model-id"
// spec and a bare model id; the bare form is served from the spec
// aggregator's consensus-filtered index rather than rejected.
export async function fetchModelCapabilities(
  spec: string,
  signal?: AbortSignal,
): Promise<ModelCapabilities> {
  const params = new URLSearchParams({ spec });
  return request<ModelCapabilities>(`/model-capabilities?${params.toString()}`, { signal });
}

// fetchResolvedEffort asks the server to env-substitute and validate
// a reasoning_effort literal (e.g. "${VIBE_EFFORT:-max}"). Returns the
// resolved enum value, or "" when the literal is empty / expansion
// produced something not in low/medium/high/xhigh/max.
export async function fetchResolvedEffort(
  literal: string,
  signal?: AbortSignal,
): Promise<string> {
  const params = new URLSearchParams({ literal });
  const r = await request<{ resolved: string }>(
    `/resolve-effort?${params.toString()}`,
    { signal },
  );
  return r.resolved;
}

// fetchResolvedModel asks the server to env-substitute a model literal
// (e.g. "${CODEX_MODEL:-openai-codex/gpt-5.6-sol}"). Returns the
// resolved spec, or "" when the literal is empty / expansion produced
// something that does not look like a model (so the canvas can fall
// back to the authored default instead of leaking process env).
export async function fetchResolvedModel(
  literal: string,
  signal?: AbortSignal,
): Promise<string> {
  const params = new URLSearchParams({ literal });
  const r = await request<{ resolved: string }>(
    `/resolve-model?${params.toString()}`,
    { signal },
  );
  return r.resolved;
}
