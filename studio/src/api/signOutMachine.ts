// The sign-out state machine (studio half) — one record per browser, written
// BEFORE the action it guards, read by every tab. A sign-out the server did
// not confirm is OWED until it is confirmed (2xx for the session it names),
// the credential it was presented from turns out dead (401/403), the session
// it names is past its own expiry, or the user explicitly abandons it — and
// an abandonment keeps the record, as ABANDONED, with a visible notice (I2):
// the session the user asked to end stays active until its expiry.
//
// Every tab derives what it shows from the record (I6): OWED warns, ABANDONED
// notices, in the authenticated shell and over the signed-out page alike (I1 —
// the warning stands while the session may still be live). Sub-states are
// derived, never stored, with one exception: the delivery's own evidence —
// a credential route replaced the jar with the record's session gone — is
// persisted in the record (resolved_gone_at), because a resolution that lived
// only in the tab that ran the delivery would leave the others holding the
// record presentable forever. HELD (this browser still holds the named
// session — it can present), GONE (a delivery replaced the jar, or a
// presentation answered that the named lineage is not what this browser
// presents — it only warns), UNKNOWN (no identity read has named this
// browser's lineage yet).
import { apiBase, isScopedPane } from "@/lib/scope";

const BASE_URL = apiBase();

// SIGN_OUT_PENDING_KEY is the one record. There is no second key: the machine
// writes and reads only this.
export const SIGN_OUT_PENDING_KEY = `iterion.auth.signOutPending:${BASE_URL}`;

// SESSION_RENEWED_AT_KEY is NOT machine state: it is the refresh path's
// evidence about when the cookies were last replaced (recordRenewal in
// @/api/client). The machine reads it for exactly one decision — whether an
// UNNAMED record may still be presented at a reload (an unnamed logout ends
// whatever the jar holds, so a delivery since the ask must stop it). No
// listener reacts to its storage events (the phantom listener is gone).
export const SESSION_RENEWED_AT_KEY = `iterion.auth.renewedAt:${BASE_URL}`;

export const LOGOUT_OUTCOME_HEADER = "X-Iterion-Logout";
export const LOGOUT_OTHER_SESSION = "other-session";

// LOGOUT_TIMEOUT_MS bounds a logout. The user waits on it, and a logout that
// never answers would otherwise hold the sign-out open for as long as the
// connection hangs. A logout abandoned at the bound stays pending and is
// presented again: ending a lineage that is already ended answers 204, so a
// repeat costs nothing.
export const LOGOUT_TIMEOUT_MS = 5000;

// The one record (design §II). lineage "" names no session (UNNAMED): that
// sign-out can only be presented with the credential it was asked from.
export interface SignOutRecord {
  state: "owed" | "abandoned";
  lineage: string;
  account: string;
  expires_at: string | null;
  since: number;
  abandoned_at: number | null;
  // resolved_gone_at is when a credential route delivered a session over this
  // record's — the jar no longer holds the session it names. Persisted IN the
  // record so the resolution reaches every tab with the write's storage
  // event: a tab that missed the delivery resolves the record GONE at its
  // next read, instead of holding it presentable forever. null while the
  // record's session may still be held here; absent in records written by an
  // older build, which parseRecord reads as null.
  resolved_gone_at: number | null;
}

// SignOutResult is what a sign-out came to: "confirmed" — the logout answered
// 2xx (or the presented credential was refused as dead: the session is over
// either way); "unconfirmed" — it did not, and the sign-out stays; and
// "other-session" — the logout revoked NOTHING (the presented token belongs to
// another lineage than the record's): the record's session is not held in this
// browser, and the record stands, resolved GONE, with its notice.
export type SignOutResult = "confirmed" | "unconfirmed" | "other-session";
export type PresentOutcome = SignOutResult | "none" | "dead-credential";

// Heldness is the derived sub-state of a record: can THIS browser still
// present the logout it names?
export type Heldness = "held" | "gone" | "unknown";

function browserStorage(): Storage | null {
  try {
    return globalThis.localStorage ?? null;
  } catch {
    return null;
  }
}

// recordStorage is where the record goes: localStorage, shared by every tab of
// the origin like the cookie jar it describes — never in a workspace pane,
// whose session belongs to the desktop. Where storage cannot hold it, this
// document holds it alone, for its own lifetime (degraded, never shown as a
// normal sign-in).
function recordStorage(): Storage | null {
  return isScopedPane() ? null : browserStorage();
}

// The record this document holds where storage cannot: it dies with the
// document, which is what the warning's "tab" scope says.
let recordHere: SignOutRecord | null = null;

export function readRecord(): SignOutRecord | null {
  if (recordHere) return recordHere;
  const storage = browserStorage();
  if (!storage) return null;
  let raw: string | null;
  try {
    raw = storage.getItem(SIGN_OUT_PENDING_KEY);
  } catch (err) {
    console.warn(`auth: the pending sign-out could not be read: ${String(err)}`);
    return null;
  }
  return raw === null ? null : parseRecord(raw);
}

// parseRecord reads the machine's record and the older marks: a bare
// timestamp, or {at, lineage} — both owed sign-outs about a session whose
// expiry the older build did not know. Naming survives the upgrade.
function parseRecord(raw: string): SignOutRecord {
  const owed = (over: Partial<SignOutRecord>): SignOutRecord => ({
    state: "owed",
    lineage: "",
    account: "",
    expires_at: null,
    since: Date.now(),
    abandoned_at: null,
    resolved_gone_at: null,
    ...over,
  });
  try {
    const parsed = JSON.parse(raw) as unknown;
    if (parsed && typeof parsed === "object") {
      const mark = parsed as Record<string, unknown>;
      if (mark.state === "owed" || mark.state === "abandoned") {
        return {
          state: mark.state,
          lineage: typeof mark.lineage === "string" ? mark.lineage : "",
          account: typeof mark.account === "string" ? mark.account : "",
          expires_at: typeof mark.expires_at === "string" ? mark.expires_at : null,
          since: typeof mark.since === "number" ? mark.since : Date.now(),
          abandoned_at: typeof mark.abandoned_at === "number" ? mark.abandoned_at : null,
          resolved_gone_at: typeof mark.resolved_gone_at === "number" ? mark.resolved_gone_at : null,
        };
      }
      return owed({
        lineage: typeof mark.lineage === "string" ? mark.lineage : "",
        since: typeof mark.at === "number" ? mark.at : Date.now(),
      });
    }
  } catch {
    // Not JSON — a bare timestamp.
  }
  const at = Number(raw);
  return owed({ since: Number.isFinite(at) ? at : Date.now() });
}

function writeRecord(record: SignOutRecord): void {
  const storage = recordStorage();
  if (storage) {
    try {
      storage.setItem(SIGN_OUT_PENDING_KEY, JSON.stringify(record));
      recordHere = null;
      return;
    } catch (err) {
      console.warn(`auth: the pending sign-out could not be recorded for the other tabs (${String(err)}); this document keeps it`);
    }
  }
  recordHere = record;
}

// clearRecord is confirmation, a dead credential, or the record's expiry —
// never an event, never an abandonment.
function clearRecord(): void {
  recordHere = null;
  stopLoop();
  unnamedBaseline = null;
  unnamedSuperseded = false;
  try {
    browserStorage()?.removeItem(SIGN_OUT_PENDING_KEY);
  } catch (err) {
    console.warn(`auth: the pending sign-out could not be cleared for the other tabs: ${String(err)}`);
  }
  notify();
}

export function signOutPending(): boolean {
  return readRecord() !== null;
}

// SignOutMarkScope is how far a standing record reaches, and so what the
// warning shown for it may promise: "browser" — the record is in localStorage,
// where every tab reads it and a reload finds it; "tab" — storage could not
// hold it (a browser that refuses site data), so it dies with this document.
export type SignOutMarkScope = "browser" | "tab";

export function signOutMarkScope(): SignOutMarkScope {
  return recordHere !== null ? "tab" : "browser";
}

// ---- The jar's identity, as this document last read it ----

// heldLineage is the lineage of the session this browser holds, from the last
// adopted identity read. undefined until one does.
let heldLineage: string | undefined;
let heldAccount: string | undefined;
let heldExpiresAt: string | null = null;
// notHeldLineage: a lineage a presentation PROVED this browser does not hold
// (the logout answered other-session for it). The next identity read naming it
// contradicts the answer and clears it.
let notHeldLineage: string | null = null;
// An UNNAMED record names no lineage, so the jar's session cannot be compared
// with the ask. Once an identity read names the jar (the record's session
// finally has a name), that name is the baseline; a later read naming a
// DIFFERENT lineage is a delivery — the unnamed sign-out can no longer be
// presented (it would end the delivered session, I3).
let unnamedBaseline: string | null = null;
let unnamedSuperseded = false;
// replacedAt: when a credential route replaced this browser's cookies (a
// sign-in, a password change). A record written before that moment names a
// session the jar no longer holds.
let replacedAt: number | null = null;

export function recordSessionIdentity(identity: { lineage?: string | null; account?: string; expiresAt?: string | null }): void {
  // lineage null means the read named NO session (a personal access token, an
  // access token minted before sessions carried a lineage): the browser's
  // lineage is unknown again — not "unchanged".
  if (identity.lineage !== undefined) heldLineage = identity.lineage ?? undefined;
  if (identity.account !== undefined) heldAccount = identity.account ?? undefined;
  if (identity.expiresAt !== undefined) heldExpiresAt = identity.expiresAt ?? null;
  if (identity.lineage && notHeldLineage !== null && notHeldLineage === identity.lineage) {
    // The read contradicts the presentation's other-session answer: trust the
    // read and let the sign-out be presented again.
    notHeldLineage = null;
  }
  const record = readRecord();
  if (record && record.lineage === "" && identity.lineage) {
    if (unnamedBaseline === null) {
      unnamedBaseline = identity.lineage;
    } else if (identity.lineage !== unnamedBaseline) {
      unnamedSuperseded = true;
    }
  }
  notify();
}

export function currentHeldIdentity(): { lineage?: string; account?: string; expiresAt: string | null } {
  return { lineage: heldLineage, account: heldAccount, expiresAt: heldExpiresAt };
}

export function heldnessOf(record: SignOutRecord): Heldness {
  // The resolution persisted in the record travels with it: a delivery
  // replaced the jar with the record's session gone, in whichever tab it
  // ran — every tab reading the record resolves it GONE, this one included.
  // The stamp's presence decides: it is only ever written against the record
  // it found standing, so no comparison with `since` is needed — and a tie
  // (ask and delivery inside one millisecond) must not resurrect a session
  // the jar demonstrably lost.
  if (record.resolved_gone_at !== null) return "gone";
  // This tab's own evidence: a delivery this tab processed. The comparison
  // counts here — the stamp is this tab's moment, and a record written after
  // it names a session the jar still holds.
  if (replacedAt !== null && record.since < replacedAt) return "gone";
  if (!record.lineage) return unnamedSuperseded ? "gone" : "unknown";
  if (notHeldLineage === record.lineage) return "gone";
  if (heldLineage === undefined) return "unknown";
  return heldLineage === record.lineage ? "held" : "gone";
}

// ---- React surface ----

type Listener = () => void;
const listeners = new Set<Listener>();
let version = 0;

export function subscribe(fn: Listener): () => void {
  listeners.add(fn);
  return () => listeners.delete(fn);
}

export function snapshotVersion(): number {
  return version;
}

function notify(): void {
  version++;
  for (const fn of [...listeners]) fn();
}

// externalChange re-reads the record after another tab moved it (a storage
// event) and re-renders every subscriber (I6) — no tab edits the record
// because of an event alone.
export function externalChange(): void {
  notify();
}

// ---- Transitions ----

// signOutRequested is the SIGN-OUT REQUESTED transition: the record is written
// BEFORE the logout is issued (the caller presents next). It names the session
// this browser holds as far as the last identity read knows, with the expiry
// the credential answers gave. A record already standing is superseded: the
// last ask wins (there is one record, and it is this user's latest).
export function signOutRequested(): void {
  writeRecord({
    state: "owed",
    lineage: heldLineage ?? "",
    account: heldAccount ?? "",
    expires_at: heldExpiresAt,
    since: Date.now(),
    abandoned_at: null,
    resolved_gone_at: null,
  });
  notHeldLineage = null;
  unnamedBaseline = null;
  unnamedSuperseded = false;
  notify();
}

// abandon is the user's explicit choice to go on without the sign-out (the
// "sign in anyway" action): the record moves to ABANDONED and stays — with its
// lineage, account and expiry — so every tab shows the notice (I2) and a
// reload refuses to adopt the still-held session as a plain sign-in.
export function abandon(): SignOutRecord | null {
  const record = readRecord();
  if (!record) return null;
  const next: SignOutRecord = { ...record, state: "abandoned", abandoned_at: Date.now() };
  writeRecord(next);
  notify();
  return next;
}

// resolveDelivered is what a credential route that REPLACED this browser's
// cookies does to the record, before the new identity is read: the record's
// session is no longer held here, so it resolves GONE. A sign-in the user
// chose (a login, an SSO landing, a password reset) moves an owed record to
// ABANDONED first — the sign-in proceeds, the notice stays (I2, I4). An in-app
// password change is not a sign-in: the record stays OWED (the change does
// not confirm a sign-out) and resolves GONE IN the record, so the write's
// storage event carries the resolution to every tab — one that missed the
// delivery resolves the record GONE at its next read (I6), instead of
// standing OWED+HELD with its renewals blocked and nothing left to present.
export function resolveDelivered(kind: "sign-in" | "delivery"): void {
  replacedAt = Date.now();
  // The delivery replaced the cookies: the lineage this document held names
  // the session that is gone, so it is dropped until an identity read names
  // the new one.
  heldLineage = undefined;
  heldAccount = undefined;
  heldExpiresAt = null;
  const record = readRecord();
  if (record && record.state === "owed") {
    if (kind === "sign-in") {
      writeRecord({ ...record, state: "abandoned", abandoned_at: Date.now() });
    } else if (record.resolved_gone_at === null) {
      writeRecord({ ...record, resolved_gone_at: Date.now() });
    }
  }
  notify();
}

// expireStale removes a record whose session is past its own bound: nothing
// live is being hidden (I1's exception, documented). Run at bootstrap. The
// bound alone decides: a record whose expiry is known expires at it whether
// or not a delivery already resolved it GONE.
export function expireStale(): void {
  const record = readRecord();
  if (!record?.expires_at) return;
  const at = Date.parse(record.expires_at);
  if (Number.isFinite(at) && Date.now() > at) {
    console.warn(`auth: the sign-out of ${record.lineage || "an unnamed session"} names a session past its ${record.expires_at} expiry; the record is dropped`);
    clearRecord();
  }
}

// presentationDue answers whether THIS browser can still present the record's
// logout: a GONE record only warns. An unnamed record presents until an
// identity read proves the jar moved (unnamedSuperseded) — a settled renewal
// of the SAME session rotates the credential the ask was made from without
// replacing it, so the renewal stamp alone is not refusal evidence here; the
// cross-document delivery case is the bootstrap's (unnamedRecordWasReplaced).
function presentationDue(record: SignOutRecord): boolean {
  return heldnessOf(record) !== "gone";
}

// canPresent is presentationDue for the callers outside the module (the
// sign-in gates, the SSO start): the record they are about to present must
// still be presentable from this browser.
export function canPresent(record: SignOutRecord): boolean {
  return presentationDue(record);
}

// The presentation in flight, joined by every caller in this document: a
// retry button, the loop, a sign-in gate and a storage event share one wire.
let presentation: Promise<PresentOutcome> | null = null;

// present asks the server to end the session the record names. Only a 2xx
// ends it. A 2xx carrying LOGOUT_OUTCOME_HEADER: other-session revoked
// nothing — the record's session is not what this browser presents, so the
// record resolves GONE and stands with its notice. A 401/403 names the
// presented credential itself dead (revoked/expired): the session is gone,
// and the record goes with it (I1's dead-credential exception). Any other
// answer, and a logout with no answer at all, leave the record OWED.
export function present(): Promise<PresentOutcome> {
  if (presentation) return presentation;
  const task = doPresent().finally(() => {
    presentation = null;
  });
  presentation = task;
  return task;
}

async function doPresent(): Promise<PresentOutcome> {
  const record = readRecord();
  if (!record || !presentationDue(record)) return "none";
  const url = record.lineage
    ? `${BASE_URL}/auth/logout?lineage=${encodeURIComponent(record.lineage)}`
    : `${BASE_URL}/auth/logout`;
  let res: Response;
  try {
    res = await fetchWithin(url, { method: "POST", credentials: "include" }, LOGOUT_TIMEOUT_MS);
  } catch (err) {
    console.warn(`auth: the logout got no answer (${String(err)}); the sign-out stays pending`);
    return "unconfirmed";
  }
  if (res.ok) {
    if (res.headers.get(LOGOUT_OUTCOME_HEADER) === LOGOUT_OTHER_SESSION) {
      if (!record.lineage) {
        // A logout that named no lineage ends the session presented or there
        // was none: other-session on that answer is the server contradicting
        // itself. Nothing is signed out and the record stands.
        console.warn("auth: the logout revoked nothing although it named no session; nothing is signed out and the sign-out stays pending");
        return "unconfirmed";
      }
      console.warn(`auth: the logout revoked nothing — the session it named (${record.lineage}) is not the one this browser holds; the sign-out stands, resolved gone`);
      notHeldLineage = record.lineage;
      notify();
      return "other-session";
    }
    clearRecord();
    return "confirmed";
  }
  if (res.status === 401 || res.status === 403) {
    console.warn(`auth: the logout answered HTTP ${res.status}, which names the presented credential dead; the session is over`);
    clearRecord();
    return "dead-credential";
  }
  console.warn(`auth: the logout answered HTTP ${res.status}; the sign-out stays pending`);
  return "unconfirmed";
}

// ---- The presentation loop ----

// LOOP_RETRY_DELAYS_MS is when an unconfirmed presentation is presented again
// on its own: quickly while the incident is young (a settled server confirms
// within seconds), then once a minute — the record stands for as long as it
// takes. The loop runs while the record is OWED and presentable; every tab may
// run it, presentations are joined within a tab, and a confirmation anywhere
// clears the record for all of them. A GONE record is never presented.
export const LOOP_RETRY_DELAYS_MS: readonly number[] = [2000, 5000, 10000];
export const LOOP_STEADY_MS = 60000;

let loopTimer: ReturnType<typeof setTimeout> | null = null;
let loopAttempt = 0;

export function ensureLoop(): void {
  if (loopTimer !== null) return;
  const record = readRecord();
  if (!record || record.state !== "owed" || !presentationDue(record)) return;
  const delay = LOOP_RETRY_DELAYS_MS[loopAttempt] ?? LOOP_STEADY_MS;
  loopAttempt++;
  loopTimer = setTimeout(() => {
    loopTimer = null;
    void present().then((outcome) => {
      if (outcome === "unconfirmed") {
        ensureLoop();
        return;
      }
      loopAttempt = 0;
    });
  }, delay);
}

export function stopLoop(): void {
  if (loopTimer !== null) clearTimeout(loopTimer);
  loopTimer = null;
  loopAttempt = 0;
}

// signOutInProgress is the sign-out window every identity load, renewal and
// switch consults: open while a presentation is on the wire, and while a record
// is OWED and still presentable from this browser (nothing renews the session
// its logout is about). A GONE record (the jar holds another session) and an
// ABANDONED one (the user went on) let the held session renew, or a sign-in
// land.
export function signOutInProgress(): boolean {
  if (presentation !== null) return true;
  const record = readRecord();
  return record !== null && record.state === "owed" && presentationDue(record);
}

// SignOutHeldError is what a sign-in attempt raises while an unconfirmed
// sign-out still stands presentable: the sign-in does not land except through
// the explicit "sign in anyway" action, which abandons the record (I2) and
// retries the sign-in past this gate.
export class SignOutHeldError extends Error {
  record: SignOutRecord;
  constructor(record: SignOutRecord) {
    const until = record.expires_at ? ` until ${record.expires_at}` : " until its expiry";
    super(`the sign-out of ${record.lineage || "your session"} is not confirmed; the sign-in is held${until}`);
    this.record = record;
    this.name = "SignOutHeldError";
  }
}

// fetchWithin is fetch bounded by ms. The signal cancels the request — the
// browser then drops its answer, Set-Cookie included — and the race settles
// the caller at the bound whatever the fetch does with the signal.
export async function fetchWithin(url: string, init: RequestInit, ms: number): Promise<Response> {
  const abort = new AbortController();
  let timer: ReturnType<typeof setTimeout> | undefined;
  const expired = new Promise<never>((_resolve, reject) => {
    timer = setTimeout(() => {
      abort.abort();
      reject(new DOMException(`no answer within ${ms} ms`, "TimeoutError"));
    }, ms);
  });
  try {
    return await Promise.race([fetch(url, { ...init, signal: abort.signal }), expired]);
  } finally {
    clearTimeout(timer);
  }
}

// renewedSince answers whether the session cookies were replaced — by any tab
// — at or after t, from the stamp recordRenewal writes; null when there is no
// stamp to read. A tie counts as after: it costs a replay, where taking a
// stale 401 for current evidence costs a rotation of the token another tab
// just renewed. A stamp later than now (the clock moved back) reads as none.
export function renewedSince(t: number): boolean | null {
  const storage = browserStorage();
  if (!storage) return null;
  let raw: string | null;
  try {
    raw = storage.getItem(SESSION_RENEWED_AT_KEY);
  } catch {
    return null;
  }
  const at = raw === null ? Number.NaN : Number(raw);
  if (!Number.isFinite(at) || at > Date.now()) return null;
  return at >= t;
}

// unnamedRecordWasReplaced answers whether the cookies were replaced after an
// UNNAMED record was written. An unnamed logout ends whatever the jar holds,
// so a delivery since the ask must stop the presentation at a reload. While
// the record is OWED nothing renews the session, so a stamp after `since` can
// only be a credential route's delivery. For an ABANDONED record the stamp is
// coarser (renewals run beside an abandonment); reading it GONE there only
// trades a re-presentation for the notice, which I2 keeps either way.
export function unnamedRecordWasReplaced(record: SignOutRecord): boolean {
  return !record.lineage && renewedSince(record.since) === true;
}
