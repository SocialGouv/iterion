import {
  createContext,
  useCallback,
  useContext,
  useEffect,
  useMemo,
  useRef,
  useState,
  useSyncExternalStore,
  type ReactNode,
} from "react";
import { apiBase } from "@/lib/scope";
import {
  beforeSignIn,
  cancelScheduledRefresh,
  currentSessionGeneration,
  publishExplicitTeam,
  renewSession,
  sessionDelivered,
  settlePendingSignOut,
  signOutInProgress,
  signOutOfServer,
  type SignOutResult,
} from "@/api/client";
import {
  abandon,
  canPresent,
  ensureLoop,
  expireStale,
  externalChange,
  heldnessOf,
  readRecord,
  recordSessionIdentity,
  resolveDelivered,
  SignOutHeldError,
  SIGN_OUT_PENDING_KEY,
  snapshotVersion,
  subscribe,
  unnamedRecordWasReplaced,
  type Heldness,
  type SignOutRecord,
} from "@/api/signOutMachine";
import { startBackoffLoop, type Backoff, type BackoffLoop } from "@/auth/backoffLoop";
import { useActiveRepoStore } from "@/store/activeRepo";
import { useServerInfoStore } from "@/store/serverInfo";
import type { ServerInfo } from "@/api/types";
import {
  ApiError,
  errorCodeOf,
  getMe,
  KEYCHAIN_LOCKED_CODE,
  login as apiLogin,
  register as apiRegister,
  switchOrg as apiSwitchOrg,
  switchTeam as apiSwitchTeam,
  type AuthIdentity,
  type AuthSession,
  type MembershipView,
  type OrgRole,
  type OrgTreeView,
  type Role,
  type UserView,
} from "@/api/auth";

interface AuthState {
  // "unreachable" = the server itself can't be reached (network error /
  // 5xx on the public /server/info probe) — distinct from "anonymous"
  // (server reachable, no session) so the gate can show a retry screen
  // instead of bouncing a local-mode operator to a sign-in form.
  // "identity_unavailable" = the browser holds a session, but the server
  // could not say who it belongs to right now (/auth/me answered 5xx, or the
  // session's renewal could not be served) — distinct from "anonymous" (no
  // session) and from an authenticated account that belongs to nothing.
  // "signing_out" = a sign-out is on the wire, from the click until its
  // logout settled: nothing of the account is shown, and no identity loads.
  // "keychain_locked" = a desktop connection could not read this pane's token
  // because the system keychain is locked (503 keychain_locked) — neither the
  // server nor the account is at fault, and nothing retries on its own: every
  // read raises an OS unlock prompt, so the retry is the user's.
  status: "loading" | "anonymous" | "authenticated" | "unreachable" | "identity_unavailable" | "signing_out" | "keychain_locked";
  user: UserView | null;
  // orgs is the user's full org→teams tree. teams/activeTeam are derived
  // from the ACTIVE org so existing consumers keep working unchanged.
  orgs: OrgTreeView[];
  activeOrgID: string;
  activeOrgRole: OrgRole | null;
  activeTeamID: string;
  activeRole: Role | null;
  // treeStale = the tree above is behind what the server would answer: a
  // /auth/me after a step the server COMMITTED (an org created, an invitation
  // accepted, a rename) or after a switch could not be read, so the org
  // created is missing from the switcher and the rename shows the old name. A
  // re-read is scheduled on TREE_REREAD_BACKOFF until one lands, which clears
  // this. Non-blocking: the step itself succeeded, and the surfaces that read
  // the tree stay usable — it is a caption, never a screen.
  treeStale: boolean;
  // signedOutNotice = why this browser is signed out, when the reason is one
  // the user has to read before signing in again: a password change the server
  // COMMITTED without opening a session for it (409 password_changed_sign_in),
  // whose message says to sign in with the new password. Pinned over the
  // signed-out page, where the form that learned it has already unmounted.
  // A sign-out the server did not confirm is not here: it is the machine's
  // record (signOutRecord below), which every tab reads and re-renders.
  signedOutNotice: string | null;
}

interface AuthCtx extends AuthState {
  // signOutRecord is the machine's standing record and what this browser
  // derives from it: OWED warns (the sign-out is not confirmed), ABANDONED
  // notices (the user went on; the session stays active until its expiry),
  // and `heldness` says whether this browser can still present the logout —
  // "held" (it still holds the named session), "gone" (a delivery replaced
  // the jar, or a presentation answered that the named lineage is not what
  // this browser presents), "unknown" (no identity read has named the jar's
  // lineage yet). Derived from the record on every change — a write here, a
  // storage event from another tab — so every tab converges (I6).
  signOutRecord: { record: SignOutRecord; heldness: Heldness } | null;
  // signOutUnconfirmed = an OWED record stands: the sign-out is not confirmed
  // and this browser may still hold the session the user asked to end.
  // Nothing adopts or renews it meanwhile, and the logout is presented again
  // before any identity is — on a retry, a reload, a sign-in.
  signOutUnconfirmed: boolean;
  // activeOrg is the OrgTreeView whose org_id matches activeOrgID.
  activeOrg: OrgTreeView | undefined;
  // teams is the active org's teams (derived) — the list the team
  // picker and team-scoped consumers read.
  teams: MembershipView[];
  // activeTeam is the MembershipView whose team_id matches
  // activeTeamID within the active org, or undefined.
  activeTeam: MembershipView | undefined;
  // isRestricted is the "submitter" tier: a signed-in user who can reach
  // no team in any org and isn't a super-admin (the public GitHub SSO
  // sign-up that matched no authorized team). They get a marketplace-only
  // shell. In local/desktop the synthetic identity is a super-admin, so
  // this is always false there.
  isRestricted: boolean;
  // signIn presents a pending sign-out first and HOLDS when the presentation
  // stays unconfirmed while this browser still holds the named session: the
  // form shows the held state and the explicit way past it. `anyway` is that
  // way — the user's "sign in anyway", which moves the record to ABANDONED
  // (the notice stays) and lets this sign-in land (I4).
  signIn: (email: string, password: string, options?: { anyway?: boolean }) => Promise<void>;
  signUp: (input: {
    email: string;
    password: string;
    name?: string;
    invitation?: string;
  }, options?: { anyway?: boolean }) => Promise<void>;
  signOut: () => Promise<void>;
  // Present the logout again for a sign-out still pending, and answer what it
  // came to ("none" when none is): the state says it too
  // (signOutUnconfirmed), but a caller about to hand the browser away — the
  // SSO redirect — has to read the answer BEFORE it goes. "other-session"
  // means the mark named a session this browser no longer holds: it is
  // dropped, and the session it does hold is loaded here.
  retrySignOut: () => Promise<SignOutResult | "none">;
  // Drop the account this tab shows and pin `notice` over the signed-out
  // page: a credential change the server committed without opening a session
  // cleared the cookies, so the browser IS signed out — no logout to present,
  // and nothing to retry.
  credentialSignedOut: (notice: string) => void;
  // Read the identity again for a connection whose keychain was locked,
  // telling the desktop this is a user action (one read now, whatever its
  // back-off). The only thing that leaves "keychain_locked".
  retryKeychainUnlock: () => Promise<void>;
  selectOrg: (orgID: string) => Promise<void>;
  selectTeam: (teamID: string) => Promise<void>;
  // Re-fetch /auth/me — used after a flow that mutated membership
  // server-side (accept invitation, org creation, org rename). The mutation
  // committed, so this never throws: a read that fails keeps the state as it
  // was, sets treeStale and schedules the read again until one lands.
  reloadIdentity: () => Promise<void>;
  // Load the identity of a session a credential route just delivered
  // (a password reset, a forced password change, the in-app password change):
  // /auth/me becomes the auth state — "identity_unavailable" when it answers
  // 5xx. Never throws: the credential step it follows succeeded. Every
  // delivered session goes through it, so the lineage a sign-out names is the
  // one the browser holds. The session answer carries the new expiry the
  // record names, and `kind` says what the delivery was to the machine: a
  // sign-in the user chose (a reset, a forced change) moves an owed record to
  // ABANDONED+GONE with its notice; a credential change on a session already
  // signed in (the in-app password change) leaves it OWED, resolved GONE —
  // the change does not confirm a sign-out.
  completeSignIn: (session?: { expires_at?: string }, kind?: "sign-in" | "delivery") => Promise<void>;
  // Re-run the full bootstrap (probe + identity) — the retry path of
  // the "server unreachable" screen.
  retryConnection: () => Promise<void>;
}

const Ctx = createContext<AuthCtx | null>(null);

const initial: AuthState = {
  status: "loading",
  user: null,
  orgs: [],
  activeOrgID: "",
  activeOrgRole: null,
  activeTeamID: "",
  activeRole: null,
  treeStale: false,
  signedOutNotice: null,
};

const BASE_URL = apiBase().replace(/\/$/, "");

// IDENTITY_RETRY_INTERVAL_MS is the cadence an identity the server could not
// answer is asked for again: the auto-retry of the screen an unsettled auth
// state shows, and the first background re-read of a tree a committed step
// could not refresh (TREE_REREAD_BACKOFF starts from it).
export const IDENTITY_RETRY_INTERVAL_MS = 3000;

// The background re-read of a stale tree backs off: the first on the
// identity-retry cadence, each next one TREE_REREAD_FACTOR times later, up to
// TREE_REREAD_CAP_MS. A short hiccup lands the tree within seconds; an outage
// of an hour costs a tab one re-read a minute instead of one every three
// seconds.
export const TREE_REREAD_FACTOR = 1.5;
export const TREE_REREAD_CAP_MS = 60_000;
const TREE_REREAD_BACKOFF: Backoff = { firstMs: IDENTITY_RETRY_INTERVAL_MS, factor: TREE_REREAD_FACTOR, capMs: TREE_REREAD_CAP_MS };

// probeAuth hits the unauthenticated /api/server/info to learn whether
// the deployment requires sign-in. Local / desktop returns
// auth_required=false and we render the studio as a synthetic
// super-admin so the no-login UX is preserved. A network failure or a
// 5xx means the server itself is down — surfaced as "unreachable" so
// the gate never mistakes an offline backend for an auth wall.
async function probeAuth(): Promise<"required" | "not_required" | "unreachable"> {
  try {
    const res = await fetch(`${BASE_URL}/server/info`, { credentials: "include" });
    if (res.ok) {
      const body = (await res.json()) as ServerInfo & { auth_required?: boolean };
      // The same body the server-info store holds, so fill it from here. That
      // store is loaded ONCE at module start and never retries: a backend that
      // is down at first paint leaves `info` null for the life of the
      // document, and "/" — which has to know the server mode to choose
      // between the product home and the studio — would wait on a probe that
      // is never run again. This probe IS re-run, by every reconnect tick and
      // by the manual retry, so the two are one.
      useServerInfoStore.setState({ info: body, loading: false, error: null });
      return body.auth_required !== false ? "required" : "not_required";
    }
    return res.status >= 500 ? "unreachable" : "required";
  } catch {
    return "unreachable";
  }
}

// localIdentity mirrors the synthetic principal the server's
// requireAuth middleware injects when DisableAuth=true.
const localIdentity: AuthState = {
  status: "authenticated",
  user: {
    id: "dev",
    email: "dev@local",
    status: "active",
    is_super_admin: true,
  },
  orgs: [],
  activeOrgID: "",
  activeOrgRole: null,
  activeTeamID: "",
  activeRole: null,
  treeStale: false,
  signedOutNotice: null,
};

// The machine's unnamed re-ask is gone: a sign-out presents the session its
// record names, and an answer that the presented credential belongs to
// another lineage resolves the record GONE — it never asks to end whatever
// else the jar holds (the machine's SIGN-OUT REQUESTED has no re-ask
// transition).

// applyIdentity makes a /auth/me answer the auth state: the account, its
// tree, and the active context. The tree it carries is the server's current
// one, so it clears any staleness mark.
function applyIdentity(prev: AuthState, me: AuthIdentity): AuthState {
  return withActiveContext(prev, me, me.orgs ?? [], false);
}

// applySession moves an authenticated state to the active context a switch
// minted, on the tree the state already holds: a switch changes the context,
// never the membership the tree describes, and its answer carries no tree —
// so it neither refreshes a stale tree nor makes a fresh one stale.
function applySession(prev: AuthState, s: AuthSession): AuthState {
  return withActiveContext(prev, s, prev.orgs, prev.treeStale);
}

function withActiveContext(prev: AuthState, res: AuthIdentity | AuthSession, orgs: OrgTreeView[], treeStale: boolean): AuthState {
  // Resolve the active org: the server's value, else the org that
  // contains the active team, else the first org.
  let activeOrgID = res.active_org_id ?? prev.activeOrgID ?? "";
  const activeTeamID = res.active_team_id ?? prev.activeTeamID ?? "";
  if (!activeOrgID || !orgs.some((o) => o.org_id === activeOrgID)) {
    const owner = orgs.find((o) => o.teams.some((t) => t.team_id === activeTeamID));
    activeOrgID = owner?.org_id ?? orgs.find((o) => !o.unavailable)?.org_id ?? orgs[0]?.org_id ?? "";
  }
  const activeOrg = orgs.find((o) => o.org_id === activeOrgID);
  return {
    status: "authenticated",
    user: res.user,
    orgs,
    activeOrgID,
    activeOrgRole: activeOrg?.unavailable ? null : (res.active_org_role || activeOrg?.org_role || null) as OrgRole | null,
    activeTeamID,
    // The credential's team role still selects the appropriate shell,
    // including config_editor, even when the org document cannot be read.
    activeRole: (res.active_role ?? null) as Role | null,
    treeStale,
    signedOutNotice: null,
  };
}

// IdentityOK is a /auth/me answer that succeeded, numbered by the order the
// request that produced it was SENT (sentSeq). The number is what keeps an
// answer that lands late from overwriting a fresher tree: a read sent after
// one already adopted describes the server at least as late, a read sent
// before it describes it earlier, however late the answer arrives — the
// stale-tree loop's own attempt in flight is exactly that case.
interface IdentityOK {
  kind: "ok";
  me: AuthIdentity;
  sentSeq: number;
}

// IdentityFetch is what loading /auth/me came to: the identity, or why not.
type IdentityFetch =
  | IdentityOK
  // 401: the session is not (or no longer) valid.
  | { kind: "unauthorized" }
  // 5xx: the server could not say who the session belongs to right now.
  | { kind: "unavailable" }
  // 503 keychain_locked: a desktop connection could not read the token
  // because the system keychain is locked. Terminal until the user acts —
  // each read is an OS unlock prompt — so it is never retried on its own.
  | { kind: "keychain_locked" }
  // A network error: the server did not answer.
  | { kind: "unreachable" }
  // Any other refusal (the account is gone, …).
  | { kind: "refused" };

// IdentityLoad is what loading /auth/me came to once a refused read (401) was
// answered by renewing the session: the identity, why not, or what the
// renewal said — refused, or not served (status null: no answer).
type IdentityLoad =
  | IdentityFetch
  | { kind: "renewal_refused" }
  | { kind: "renewal_unavailable"; status: number | null };

// fetchIdentity loads /auth/me, retrying a transient failure (5xx, network)
// with exponential backoff so a hiccup does not decide the state. `stamp`
// numbers each send, and the answer carries the number of the send that
// produced it — the retries of one read are separate sends, and the tree it
// answers is the server's as of the last of them.
async function fetchIdentity(stamp: () => number, options?: { keychainRetry?: boolean }): Promise<IdentityFetch> {
  let last: IdentityFetch = { kind: "unreachable" };
  for (let attempt = 0; attempt < 3; attempt++) {
    const sentSeq = stamp();
    try {
      return { kind: "ok", me: await getMe(options), sentSeq };
    } catch (err) {
      if (err instanceof ApiError && err.status === 401) return { kind: "unauthorized" };
      // A locked keychain is not a server hiccup: retrying it here would cost
      // an unlock prompt per attempt and still answer the same thing.
      if (err instanceof ApiError && errorCodeOf(err) === KEYCHAIN_LOCKED_CODE) return { kind: "keychain_locked" };
      if (err instanceof ApiError && err.status < 500) return { kind: "refused" };
      last = err instanceof ApiError ? { kind: "unavailable" } : { kind: "unreachable" };
    }
    if (attempt < 2) await new Promise((resolve) => setTimeout(resolve, 500 * 2 ** attempt));
  }
  return last;
}

// stateWithout is the auth state for an identity fetch that did not succeed.
function stateWithout(kind: Exclude<IdentityFetch["kind"], "ok">): AuthState {
  switch (kind) {
    case "unavailable":
      return { ...initial, status: "identity_unavailable" };
    case "keychain_locked":
      return { ...initial, status: "keychain_locked" };
    case "unreachable":
      return { ...initial, status: "unreachable" };
    default:
      return { ...initial, status: "anonymous" };
  }
}

export function AuthProvider({ children }: { children: ReactNode }) {
  const [state, setState] = useState<AuthState>(initial);

  // The tab publishes the team it displays (#2662): every adoption point —
  // bootstrap, switch, identity re-read — re-publishes, and a sign-out
  // clears. The access cookie is browser-wide, so what a tab displays must
  // travel with its own requests: the server scopes them to this team
  // (validated against the session's grants), whatever another tab's switch
  // did to the cookie since.
  useEffect(() => {
    publishExplicitTeam(state.status === "authenticated" ? state.activeTeamID || null : null);
  }, [state.status, state.activeTeamID]);

  // The machine's record, re-read on every change of it — this tab's writes
  // and another tab's storage events alike (I6: every tab re-renders from the
  // persisted state, never from a stale memory).
  const machineVersion = useSyncExternalStore(subscribe, snapshotVersion);
  const signOutRecord = useMemo<{ record: SignOutRecord; heldness: Heldness } | null>(() => {
    void machineVersion;
    const record = readRecord();
    return record ? { record, heldness: heldnessOf(record) } : null;
  }, [machineVersion]);

  // The identity the per-account stores are currently scoped to. A ref, and
  // compared in the callback body rather than inside the updater: a setState
  // updater must be pure — StrictMode double-invokes it and a discarded
  // concurrent render replays it — so an external-store write placed there
  // fires more than once per change, during another component's render.
  //
  // It survives a lapse to anonymous on purpose: cookies going stale does not
  // empty the stores, so the next account to sign in still has to displace
  // this one.
  const scopedToUserID = useRef<string | null>(null);

  // identityGen numbers the identity the state holds. EVERY /auth/me load is
  // started under the generation current then and adopts its answer only while
  // that generation still is; every step that MOVES the identity — a sign-out,
  // a switch, a session a credential route delivered — bumps it
  // (beginIdentityChange). A load cannot start while a sign-out is in progress
  // (readIdentityUnder), so the bump a sign-out makes when it starts is the
  // one it needs.
  //
  // One counter for every load, because guarding the switch's own re-read
  // alone leaves the others adopting whatever answers, whenever: a read sent
  // before a sign-out puts the signed-out user back in "authenticated" with
  // the cookies gone, and a reload sent before a switch puts the UI on the
  // team the token has left — the studio then labels and acts on one team
  // while the server scopes to the other.
  const identityGen = useRef(0);

  // identitySent numbers every /auth/me SEND, and adoptedSeq is the number of
  // the send whose answer the state holds. An answer from an earlier send is
  // dropped: it describes an older tree, and adopting it would put back the
  // org a fresher answer just added — and clear treeStale with it, so nothing
  // would say or fix the view. The generation (identityGen) cannot do this
  // job: it drops reads of an identity the browser has LEFT, while these are
  // reads of the same identity racing each other.
  const identitySent = useRef(0);
  const adoptedSeq = useRef(0);
  const stampIdentitySend = useCallback(() => ++identitySent.current, []);

  // mounted is false once the provider unmounted: no background loop starts
  // after that (the one running is cancelled with it).
  const mounted = useRef(false);

  // treeReread is the background re-read of a stale tree (markTreeStale),
  // held for the loop's whole life — waiting on its timer AND reading — so a
  // second stale step joins it instead of starting another.
  const treeReread = useRef<BackoffLoop | null>(null);

  const cancelTreeReread = useCallback(() => {
    treeReread.current?.cancel();
  }, []);

  // beginIdentityChange opens a new identity generation: every load started
  // before it is stale from here on, and the re-read scheduled for the
  // previous generation is dropped — whatever moved the identity reads it
  // again itself.
  const beginIdentityChange = useCallback(() => {
    cancelTreeReread();
    return ++identityGen.current;
  }, [cancelTreeReread]);

  // readIdentityUnder loads /auth/me and answers null when the load cannot
  // describe the browser's identity: it does not start while a sign-out is in
  // progress (signOutInProgress — this tab's logout on the wire, or a
  // sign-out pending in any tab), and its answer is dropped when one began
  // meanwhile or the generation it was started under is gone. The one gate
  // every identity load passes, so a load site added later cannot quietly
  // skip it.
  const readIdentityUnder = useCallback(async (gen: number, options?: { keychainRetry?: boolean }): Promise<IdentityFetch | null> => {
    if (signOutInProgress()) return null;
    const got = await fetchIdentity(stampIdentitySend, options);
    return gen === identityGen.current && !signOutInProgress() ? got : null;
  }, [stampIdentitySend]);

  // readIdentityRenewing is readIdentityUnder for a load that may find the
  // access token expired: a 401 renews the session through the client's
  // guarded refresh (renewSession — deduped with every other presentation, so
  // a sign-out settles it; refused while a sign-out is in progress) and reads
  // again. Twice at most: a renewal another tab made after the read was sent
  // needs no presentation, and a read refused after that one is the evidence
  // that the session itself needs one.
  const readIdentityRenewing = useCallback(async (gen: number, options?: { keychainRetry?: boolean }): Promise<IdentityLoad | null> => {
    for (let renewals = 0; ; renewals++) {
      const sentAt = Date.now();
      const got = await readIdentityUnder(gen, options);
      if (!got || got.kind !== "unauthorized" || renewals === 2) return got;
      const renewal = await renewSession(sentAt);
      if (gen !== identityGen.current) return null;
      if (renewal.kind === "rejected") return { kind: "renewal_refused" };
      if (renewal.kind === "unavailable") return { kind: "renewal_unavailable", status: renewal.status };
    }
  }, [readIdentityUnder]);

  // adoptIdentity is the ONE way an account becomes auth state: every /auth/me
  // answer goes through it. It drops the per-account context when the account
  // CHANGED — not only when someone signed out.
  //
  // Signing out is not the only way the browser's identity changes: a
  // password-reset link opened WITH a live session ends in fresh cookies for
  // the TOKEN'S owner (the confirm endpoint is public and delivers a session),
  // and the view that follows loads that identity, not signOut. Without the
  // reset here the previous account's active repo would survive into the new
  // one — the exact thing signOut's reset prevents.
  //
  // Keyed on the identity that changed, not on the route that changed it: a
  // guard at each caller would have to be repeated at the next one. A switch
  // (adoptSwitch) never changes the account, so it does not come here.
  //
  // It is also where an answer from an earlier send is dropped (sentSeq): the
  // one gate every /auth/me answer passes, so a late answer cannot overwrite a
  // fresher tree whichever load site produced it.
  const adoptIdentity = useCallback((read: IdentityOK) => {
    if (read.sentSeq < adoptedSeq.current) return;
    adoptedSeq.current = read.sentSeq;
    const res = read.me;
    // A tree landed: nothing left to re-read, and applyIdentity clears the
    // staleness mark.
    cancelTreeReread();
    // The session this credential belongs to — lineage, account and the
    // session's bound — for a sign-out started from here to name, and for
    // the machine to resolve its record against (HELD vs GONE). Every
    // adopted identity re-resolves: a read that names a lineage the record
    // is not about turns it GONE. The bound travels only when the read
    // names a session — a read naming none clears it, as it clears the
    // lineage; a read carrying no bound (an older server) leaves the one
    // the sign-in's own session answer gave.
    recordSessionIdentity({
      lineage: res.session_lineage ?? null,
      account: res.user?.id,
      expiresAt: res.session_lineage ? res.expires_at : null,
    });
    const nextID = res.user?.id ?? null;
    if (scopedToUserID.current !== null && nextID !== null && scopedToUserID.current !== nextID) {
      // Drop the repo-first context so a different account on this browser
      // never inherits the previous user's active repo.
      useActiveRepoStore.getState().reset();
    }
    // An empty response rescopes nothing; keep the identity the stores hold.
    if (nextID !== null) {
      scopedToUserID.current = nextID;
    }
    setState((prev) => applyIdentity(prev, res));
  }, [cancelTreeReread]);

  // sessionRefused is the state for a session the server refused on renewal
  // (or kept refusing once renewed): it is over.
  const sessionRefused = useCallback(() => {
    beginIdentityChange();
    setState({ ...initial, status: "anonymous" });
  }, [beginIdentityChange]);

  // markTreeStale records that the tree the state holds is behind the server's
  // — a /auth/me after a step the server COMMITTED, or after a switch, could
  // not be read — and re-reads it on TREE_REREAD_BACKOFF until one lands.
  // Keeping the state is right (the step succeeded, and reporting the read as
  // its failure would be false), but keeping it SILENTLY is not: the org just
  // created is absent from the switcher, a rename shows the old name, an
  // accepted team is missing from the list, with nothing scheduled to fix it
  // and nothing saying the view is behind.
  //
  // One loop however many steps fail their read: the loop holds treeReread
  // from its start until it ends. A read the server refuses (401) renews the
  // session and reads again (readIdentityRenewing), so the tree still lands
  // once the access token aged out during the incident; a session the server
  // refuses ends the loop and the state with it. Adopting an identity ends the
  // loop (adoptIdentity), so does anything that moves the identity — it reads
  // it again itself (beginIdentityChange) — and so does the provider
  // unmounting. Cancelling stops the loop from RE-ARMING; the attempt already
  // in flight still answers, and what keeps ITS answer from overwriting the
  // fresher tree that cancelled the loop is the send numbering in
  // adoptIdentity.
  const markTreeStale = useCallback(() => {
    setState((prev) => (prev.status === "authenticated" ? { ...prev, treeStale: true } : prev));
    if (treeReread.current !== null || !mounted.current) return;
    const gen = identityGen.current;
    const loop = startBackoffLoop(
      TREE_REREAD_BACKOFF,
      async () => {
        const got = await readIdentityRenewing(gen);
        if (!got) return "done";
        if (got.kind === "ok") {
          adoptIdentity(got);
          return "done";
        }
        if (got.kind === "unauthorized" || got.kind === "renewal_refused") {
          sessionRefused();
          return "done";
        }
        // A locked keychain is not a tree the next attempt can land: only a
        // user action reads it again (retryKeychainUnlock).
        if (got.kind === "keychain_locked") {
          setState(stateWithout("keychain_locked"));
          return "done";
        }
        return "retry";
      },
      () => {
        if (treeReread.current === loop) treeReread.current = null;
      },
    );
    treeReread.current = loop;
  }, [adoptIdentity, readIdentityRenewing, sessionRefused]);

  useEffect(() => {
    mounted.current = true;
    return () => {
      mounted.current = false;
      cancelTreeReread();
    };
  }, [cancelTreeReread]);

  // establishIdentity loads /auth/me into the auth state after a credential
  // route DELIVERED a session (a login, a registration, a password reset, a
  // forced change): the identity, or the state that says why not — never
  // "authenticated" on a tree nobody read. The new session's cookies replaced
  // the old ones, so the client is told (sessionDelivered: a pending
  // sign-out's mark is moot, requests of the previous session are left
  // behind), and a generation opens: a load started before it is a load of an
  // identity the browser no longer has.
  const establishIdentity = useCallback(async (session?: { expires_at?: string }, kind: "sign-in" | "delivery" = "sign-in") => {
    sessionDelivered();
    // The delivery replaced the cookies: the machine resolves its record
    // GONE before anything reads the new identity — an owed record whose
    // session is no longer held must not block the read, and a sign-in the
    // user chose moves it to ABANDONED, where its notice survives (I2).
    resolveDelivered(kind);
    recordSessionIdentity({ expiresAt: session?.expires_at ?? null });
    const gen = beginIdentityChange();
    const got = await readIdentityUnder(gen);
    if (!got) return;
    if (got.kind === "ok") {
      adoptIdentity(got);
      return;
    }
    setState(stateWithout(got.kind));
  }, [adoptIdentity, beginIdentityChange, readIdentityUnder]);

  // finishSignOut makes the state what a sign-out came to. The repo-first
  // context goes, so a different account on this browser never inherits the
  // previous user's active repo; nothing account-scoped is left, so the next
  // sign-in has nothing to displace.
  const finishSignOut = useCallback(() => {
    useActiveRepoStore.getState().reset();
    scopedToUserID.current = null;
    // Whether a warning stands is the machine record's answer, derived — an
    // unconfirmed sign-out left it standing, a confirmed one cleared it.
    setState({ ...initial, status: "anonymous" });
  }, []);

  // adoptHeldSession loads the identity of the session the browser holds and
  // makes it the state. The tail of the bootstrap, and the way back from a
  // logout that revoked nothing ("other-session") or a keychain the user just
  // unlocked — one switch over what a load can come to, so a recovery path
  // added later cannot answer one of them differently.
  const adoptHeldSession = useCallback(async (gen: number, options?: { keychainRetry?: boolean }) => {
    // Dropped (null): whatever moved the generation owns the state, and a
    // sign-out another tab began meanwhile signs this one out too (the storage
    // listener below).
    const got = await readIdentityRenewing(gen, options);
    if (!got) return;
    switch (got.kind) {
      case "ok":
        adoptIdentity(got);
        return;
      case "unauthorized":
      case "renewal_refused":
        setState({ ...initial, status: "anonymous" });
        return;
      case "renewal_unavailable":
        // A renewal the server could not serve (5xx, rate limit) or that never
        // got an answer leaves the session the browser's to present again.
        setState({ ...initial, status: got.status === null ? "unreachable" : "identity_unavailable" });
        return;
      default:
        // A 5xx is "the server could not say", not "signed out": the session
        // the browser holds is still valid.
        setState(stateWithout(got.kind));
    }
  }, [adoptIdentity, readIdentityRenewing]);

  // settleSignOut presents the logout again for a sign-out still pending — no
  // identity is adopted from cookies a sign-out was meant to end — makes the
  // state what it came to, and answers it. "other-session" leaves the state in
  // "signing_out": the mark named a session this browser no longer holds, so
  // nothing was ended and the caller loads the one it does hold.
  const settleSignOut = useCallback(async (): Promise<SignOutResult | "none"> => {
    setState({ ...initial, status: "signing_out" });
    const result = await settlePendingSignOut();
    if (result !== "other-session") finishSignOut();
    return result;
  }, [finishSignOut]);

  // The bootstrap LOADS the identity the browser holds, it does not move it, so
  // it reads under the current generation instead of opening one — and drops
  // everything it came to once that generation is gone (a sign-out while its
  // read, or its renewal, was in flight). A sign-out still pending is settled
  // before anything is read.
  const bootstrap = useCallback(async () => {
    const gen = identityGen.current;
    const probe = await probeAuth();
    if (gen !== identityGen.current) return;
    if (probe === "unreachable") {
      setState({ ...initial, status: "unreachable" });
      return;
    }
    if (probe === "not_required") {
      setState(localIdentity);
      return;
    }
    // CRASH/RESTART: the record survived the tab (that is what it is for).
    // A record whose session is past its own bound hides nothing live and is
    // dropped (I1's exception, documented).
    expireStale();
    const record = readRecord();
    if (record) {
      if (unnamedRecordWasReplaced(record)) {
        // An UNNAMED record ends whatever the jar holds, so it may only be
        // presented with the credential it was asked from. The cookies were
        // replaced since the ask (while the record stood nothing renews them,
        // so the stamp can only be a delivery): it cannot present any more.
        // It resolves GONE — the delivery proceeds, the warning/notice stands.
        resolveDelivered("sign-in");
        await adoptHeldSession(gen);
        return;
      }
      // The named presentation is self-targeting (the server ends that
      // lineage or nothing), so it is safe before any identity is adopted.
      // "other-session" resolves the record GONE and the held session is
      // this browser's own to read; unconfirmed, the browser stays on the
      // signed-out page — the record's warning or notice over it — and the
      // machine's retry loop takes over.
      const settled = await settleSignOut();
      if (settled === "other-session") {
        // The presentation resolved the record GONE: the session this browser
        // holds is its own to read, under the standing warning.
        if (gen !== identityGen.current) return;
        await adoptHeldSession(gen);
        return;
      }
      if (settled === "unconfirmed") {
        ensureLoop();
        return;
      }
      if (settled === "none" && readRecord()) {
        // The record stands but cannot be presented from here — it resolved
        // GONE (a presentation before this one, or a delivery): the held
        // session is this browser's own to use, under the warning/notice.
        if (gen !== identityGen.current) return;
        await adoptHeldSession(gen);
        return;
      }
      // "confirmed": the record cleared with the logout, and the cookies went
      // with it — the signed-out page is the honest state.
      return;
    }
    await adoptHeldSession(gen);
  }, [adoptHeldSession, settleSignOut]);

  useEffect(() => {
    void bootstrap();
  }, [bootstrap]);

  // settleBeforeCredential is the machine's SIGN-IN gate (I4): a pending
  // sign-out this browser still holds is presented FIRST — once the new
  // cookies replace the old ones nothing here can end that session — and
  // while the presentation stays unconfirmed the sign-in does not land except
  // through the explicit "sign in anyway" action. `anyway` is that action's
  // message from the form: the record moves to ABANDONED (its notice stays,
  // I2) and the sign-in proceeds. A record whose session is no longer held
  // (+GONE) only warns: the sign-in proceeds with the notice rendered.
  const settleBeforeCredential = useCallback(async (anyway: boolean) => {
    if (anyway) {
      abandon();
      return;
    }
    const settled = await beforeSignIn();
    if (settled === "confirmed" || settled === "other-session" || settled === "none") return;
    const record = readRecord();
    // A record this browser can no longer present (GONE, or unnamed with the
    // cookies replaced since the ask) only warns: the sign-in proceeds.
    if (!record || !canPresent(record)) return;
    throw new SignOutHeldError(record);
  }, []);

  // A login answers the session alone; the identity — the tree — is read
  // from /auth/me. A login that succeeded never throws over that read: the
  // state says what it came to.
  const signIn = useCallback(
    async (email: string, password: string, options?: { anyway?: boolean }) => {
      await settleBeforeCredential(options?.anyway === true);
      const session = await apiLogin(email, password);
      await establishIdentity(session, "sign-in");
    },
    [establishIdentity, settleBeforeCredential],
  );

  // Registration sets the session cookies server-side just like login; its
  // identity then flows into the auth state the same way.
  const signUp = useCallback(
    async (input: { email: string; password: string; name?: string; invitation?: string }, options?: { anyway?: boolean }) => {
      await settleBeforeCredential(options?.anyway === true);
      const session = await apiRegister(input);
      await establishIdentity(session, "sign-in");
    },
    [establishIdentity, settleBeforeCredential],
  );

  // switchAborts are the switches in flight. A switch mints a credential — the
  // access cookie of its context — so a sign-out cancels them: an aborted
  // request's answer never reaches the cookie jar.
  const switchAborts = useRef(new Set<AbortController>());

  const abortSwitches = useCallback(() => {
    for (const abort of switchAborts.current) abort.abort();
    switchAborts.current.clear();
  }, []);

  // The sign-out is a window — from the click until the logout settled — and
  // nothing may put the account back inside it. It opens here: the identity
  // generation moves (every read in flight is dropped), the switches in
  // flight are aborted, and the state leaves the account at once, so the
  // shell, its polls, the identity-unavailable screen and its retry unmount.
  // The client keeps the server quiet until the logout settled
  // (signOutOfServer — which a second sign-out joins), and finishSignOut
  // closes the window on what it came to.
  const signOut = useCallback(async () => {
    beginIdentityChange();
    abortSwitches();
    setState({ ...initial, status: "signing_out" });
    // SIGN-OUT REQUESTED: the machine wrote the record before the logout went
    // out, and the answer settles it — a 2xx clears it, an error or a lost
    // answer leaves it OWED (the loop re-presents), and an answer that the
    // presented credential belongs to ANOTHER lineage resolves the record
    // GONE with its warning; no ask to end whatever else the jar holds.
    await signOutOfServer();
    finishSignOut();
  }, [abortSwitches, beginIdentityChange, finishSignOut]);

  // leaveAccount drops the account this tab shows WITHOUT presenting a logout:
  // the session is already over server-side — another tab's sign-out answered
  // and the shared cookies are gone, or a credential change cleared them — so
  // a logout would end nothing (or, with no lineage to name, the next session
  // this browser opens). notice is what the user has to read before signing in
  // again, when there is one. The per-account stores go, as after a sign-out,
  // and the client presents nothing more for that session.
  const leaveAccount = useCallback((notice: string | null) => {
    beginIdentityChange();
    abortSwitches();
    cancelScheduledRefresh();
    useActiveRepoStore.getState().reset();
    scopedToUserID.current = null;
    setState({ ...initial, status: "anonymous", signedOutNotice: notice });
  }, [abortSwitches, beginIdentityChange]);

  const credentialSignedOut = useCallback((notice: string) => leaveAccount(notice), [leaveAccount]);

  const retrySignOut = useCallback(async (): Promise<SignOutResult | "none"> => {
    const result = await settlePendingSignOut();
    if (result === "other-session") {
      // The logout revoked nothing: the record named a session this browser
      // no longer holds, and the one it does hold is live. Load it — the
      // record stands resolved GONE, with its warning (I2).
      const gen = beginIdentityChange();
      await adoptHeldSession(gen);
      return result;
    }
    return result;
  }, [adoptHeldSession, beginIdentityChange]);

  // retryKeychainUnlock reads the identity again for a connection whose
  // keychain was locked, and says so on the wire: the desktop reads the
  // keychain now — one OS unlock prompt, the one the user asked for — instead
  // of waiting out its back-off.
  const retryKeychainUnlock = useCallback(async () => {
    const gen = identityGen.current;
    await adoptHeldSession(gen, { keychainRetry: true });
  }, [adoptHeldSession]);

  // ANOTHER TAB'S EVENT: every tab re-reads the record and re-renders from it
  // (I6) — an OWED record warns here, an ABANDONED one shows its notice here,
  // whatever this tab was showing. No tab clears or edits the record because
  // of an event alone. What a tab DOES about its own session:
  //   - the record APPEARED owed while this tab holds the account: the jar is
  //     shared, the other tab's sign-out is ending this tab's session too, so
  //     this tab signs out as well — its own logout reaches the server even
  //     when the other tab's cannot. The fact travels with the EVENT: a tab
  //     that processes it late (frozen background tab, busy main thread) reads
  //     the record at handling time, and one that finds it already gone has
  //     the case below.
  //   - the record is GONE and this tab holds the account: the machine
  //     removes the record only when the sign-out is confirmed (or the named
  //     session is past its bound) — the shared cookies went with it, so this
  //     tab's session is over too, and the account is left locally with
  //     nothing to present (an unnamed logout of its own would be about
  //     whatever session it opens next).
  // No other key moves this UI: a storage event for anything else — the
  // renewal stamp included — is re-rendered from the record only, and the
  // phantom listener that pretended otherwise is gone.
  const statusRef = useRef(state.status);
  useEffect(() => {
    statusRef.current = state.status;
  }, [state.status]);

  useEffect(() => {
    const onStorage = (e: StorageEvent) => {
      if (e.key !== SIGN_OUT_PENDING_KEY) return;
      externalChange();
      const record = readRecord();
      if (record === null) {
        if (statusRef.current === "authenticated") leaveAccount(null);
        return;
      }
      if (record.state === "owed" && canPresent(record) && statusRef.current === "authenticated") {
        void signOut();
      }
    };
    window.addEventListener("storage", onStorage);
    return () => window.removeEventListener("storage", onStorage);
  }, [leaveAccount, signOut]);

  // A tab frozen through a record write (suspended background tab, busy main
  // thread) processes no storage event and shows a stale view until one
  // arrives — which may be never. Becoming visible or focused again re-reads
  // the record and re-renders from the persisted state (I6): externalChange
  // notifies the subscribers, and no tab edits the record on an event alone.
  useEffect(() => {
    const reRead = () => externalChange();
    document.addEventListener("visibilitychange", reRead);
    window.addEventListener("focus", reRead);
    return () => {
      document.removeEventListener("visibilitychange", reRead);
      window.removeEventListener("focus", reRead);
    };
  }, []);

  // reloadIdentity re-reads /auth/me after a step the server committed and
  // makes it the state; a read that fails — after fetchIdentity's retries —
  // keeps the state as it was, marks the tree out of date and asks again until
  // one lands. It never throws: the step it follows succeeded, and reporting
  // the read as that step's failure would be false.
  const reloadIdentity = useCallback(async () => {
    const gen = identityGen.current;
    const got = await readIdentityUnder(gen);
    if (!got) return;
    if (got.kind === "ok") {
      adoptIdentity(got);
      return;
    }
    console.warn(`auth: the identity could not be re-read (${got.kind}); the state keeps the tree it holds, marked out of date, and re-reads it`);
    markTreeStale();
  }, [adoptIdentity, markTreeStale, readIdentityUnder]);

  // A switch answers the new active context; the state moves to it at once,
  // on the tree it holds (applySession), so the page never shows the context
  // the server already left. The tree is then re-read in the background: a
  // grant, a demotion or a promotion since the tree was read would otherwise
  // show permissions the server no longer applies until a reload. The switch
  // never fails over that read — it keeps the tree it holds when it fails,
  // marked out of date and re-read until one lands.
  const adoptSwitch = useCallback(
    (s: AuthSession) => {
      const gen = beginIdentityChange();
      setState((prev) => (prev.status === "authenticated" ? applySession(prev, s) : prev));
      void readIdentityUnder(gen).then((got) => {
        if (!got) return;
        if (got.kind === "ok") {
          adoptIdentity(got);
          return;
        }
        console.warn(`auth: the identity could not be re-read after a switch (${got.kind}); the state keeps the tree it holds, marked out of date, and re-reads it`);
        markTreeStale();
      });
    },
    [adoptIdentity, beginIdentityChange, markTreeStale, readIdentityUnder],
  );

  // runSwitch sends a switch inside the sign-out window's rules: none starts
  // while a sign-out is in progress, and an answer that arrives after one
  // began — or after another session was delivered — is dropped, whatever it
  // says: the session it switched is not the one the browser is on. The
  // window's defenses stand before the answer: the sign-out aborts the
  // switches in flight, so an aborted request's Set-Cookie never reaches the
  // jar, and the generation check here drops whatever answer slipped past —
  // a switch re-presents no logout (the machine's sign-in gates are what a
  // context change passes through). The session generation is the one
  // captured, not the identity generation: a second switch moves that one,
  // and its answer must not drop the first's — the cookie the browser keeps
  // is the last to answer, and so is the context the state ends on.
  const runSwitch = useCallback(
    async (send: (signal: AbortSignal) => Promise<AuthSession>) => {
      if (signOutInProgress()) return;
      const generation = currentSessionGeneration();
      const abort = new AbortController();
      switchAborts.current.add(abort);
      let answer: AuthSession;
      try {
        answer = await send(abort.signal);
      } catch (err) {
        // Cancelled by the sign-out, or failed for a session the browser has
        // left since: nothing to report — that session is over.
        if (abort.signal.aborted || generation !== currentSessionGeneration()) return;
        throw err;
      } finally {
        switchAborts.current.delete(abort);
      }
      if (generation !== currentSessionGeneration() || signOutInProgress()) {
        // The answer belongs to a session this browser has left: it is
        // dropped, and the user asks again. There is no re-presented logout
        // here — a switch is not a sign-out ask, and the machine's sign-in
        // gates are what a context change passes through.
        return;
      }
      adoptSwitch(answer);
    },
    [adoptSwitch],
  );

  const selectOrg = useCallback(
    (orgID: string) => runSwitch((signal) => apiSwitchOrg(orgID, signal)),
    [runSwitch],
  );

  const selectTeam = useCallback(
    (teamID: string) => runSwitch((signal) => apiSwitchTeam(teamID, signal)),
    [runSwitch],
  );

  const value = useMemo<AuthCtx>(() => {
    const activeOrg = state.orgs.find((o) => o.org_id === state.activeOrgID);
    const teams = activeOrg?.teams ?? [];
    return {
      ...state,
      signOutRecord,
      signOutUnconfirmed: signOutRecord?.record.state === "owed",
      activeOrg,
      teams,
      activeTeam: teams.find((t) => t.team_id === state.activeTeamID),
      isRestricted:
        state.status === "authenticated" &&
        !state.user?.is_super_admin &&
        state.orgs.every((o) => !o.unavailable && o.teams.length === 0),
      signIn,
      signUp,
      signOut,
      retrySignOut,
      credentialSignedOut,
      retryKeychainUnlock,
      selectOrg,
      selectTeam,
      reloadIdentity,
      completeSignIn: establishIdentity,
      retryConnection: bootstrap,
    };
  }, [state, signOutRecord, signIn, signUp, signOut, retrySignOut, credentialSignedOut, retryKeychainUnlock, selectOrg, selectTeam, reloadIdentity, establishIdentity, bootstrap]);

  return <Ctx.Provider value={value}>{children}</Ctx.Provider>;
}

export function useAuth(): AuthCtx {
  const v = useContext(Ctx);
  if (!v) throw new Error("useAuth used outside AuthProvider");
  return v;
}

// useMaybeAuth is the provider-optional variant for components whose
// auth-derived content is decoration (cross-links, context captions)
// rather than load-bearing — they can mount without an AuthProvider
// (jsdom a11y harness, storybook-style isolation) and degrade to null.
export function useMaybeAuth(): AuthCtx | null {
  return useContext(Ctx);
}

// isLocalIdentity reports whether user is the synthetic local-mode
// principal (auth disabled server-side, see localIdentity above).
// Consumers use it to hide cloud-only account chrome in local mode —
// keep the sentinel knowledge here rather than re-hardcoding "dev".
export function isLocalIdentity(user: UserView | null | undefined): boolean {
  return user?.id === localIdentity.user?.id;
}

// RequireRole: nested gate that checks an active-team role. Renders
// nothing when the requirement is not met (parent shows a friendly
// 403 message).
export function hasRole(role: Role | null, want: Role | "super-admin"): boolean {
  if (!role) return false;
  // config_editor is least-privilege (0): it satisfies no viewer/member/admin/
  // owner requirement — it can only reach the config editor, never the studio.
  const order: Record<Role, number> = {
    config_editor: 0,
    viewer: 1,
    member: 2,
    admin: 3,
    owner: 4,
  };
  if (want === "super-admin") return false; // checked separately via user.is_super_admin
  return order[role] >= order[want];
}

// canEditConfigShares mirrors the server gate (auth_authz.go
// canEditConfigShares): a super-admin, the least-privilege config_editor
// capability, or a team manager (admin/owner) may edit the team's
// config-shares. Used to gate the Config-editor nav entry / hub card / route so
// the editor scales with access — a config_editor lands on its minimal shell,
// an admin reaches the same editor as a route in the full studio.
export function canEditConfigShares(role: Role | null, isSuperAdmin: boolean): boolean {
  if (isSuperAdmin) return true;
  if (role === "config_editor") return true;
  return hasRole(role, "admin");
}

// hasOrgRole checks an active-org role against a requirement. Org roles
// are coarser than team roles (member < admin < owner).
export function hasOrgRole(role: OrgRole | null, want: OrgRole): boolean {
  if (!role) return false;
  const order: Record<OrgRole, number> = { member: 1, admin: 2, owner: 3 };
  return order[role] >= order[want];
}
