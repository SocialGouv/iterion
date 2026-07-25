import {
  createContext,
  useCallback,
  useContext,
  useEffect,
  useMemo,
  useState,
  type ReactNode,
} from "react";
import { apiBase } from "@/lib/scope";
import { useActiveRepoStore } from "@/store/activeRepo";
import {
  ApiError,
  getMe,
  login as apiLogin,
  logout as apiLogout,
  register as apiRegister,
  refresh as apiRefresh,
  switchOrg as apiSwitchOrg,
  switchTeam as apiSwitchTeam,
  type AuthResponse,
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
  status: "loading" | "anonymous" | "authenticated" | "unreachable";
  user: UserView | null;
  // orgs is the user's full org→teams tree. teams/activeTeam are derived
  // from the ACTIVE org so existing consumers keep working unchanged.
  orgs: OrgTreeView[];
  activeOrgID: string;
  activeOrgRole: OrgRole | null;
  activeTeamID: string;
  activeRole: Role | null;
}

interface AuthCtx extends AuthState {
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
  signIn: (email: string, password: string) => Promise<void>;
  signUp: (input: {
    email: string;
    password: string;
    name?: string;
    invitation?: string;
  }) => Promise<void>;
  signOut: () => Promise<void>;
  selectOrg: (orgID: string) => Promise<void>;
  selectTeam: (teamID: string) => Promise<void>;
  // Re-fetch /auth/me — used after a flow that mutates membership
  // server-side (accept invitation, admin promotion).
  reloadIdentity: () => Promise<void>;
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
};

const BASE_URL = apiBase().replace(/\/$/, "");

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
      const body = (await res.json()) as { auth_required?: boolean };
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
};

function applyResponse(prev: AuthState, res: AuthResponse): AuthState {
  const orgs = res.orgs ?? [];
  // Resolve the active org: the server's value, else the org that
  // contains the active team, else the first org.
  let activeOrgID = res.active_org_id ?? prev.activeOrgID ?? "";
  const activeTeamID = res.active_team_id ?? prev.activeTeamID ?? "";
  if (!activeOrgID || !orgs.some((o) => o.org_id === activeOrgID)) {
    const owner = orgs.find((o) => o.teams.some((t) => t.team_id === activeTeamID));
    activeOrgID = owner?.org_id ?? orgs[0]?.org_id ?? "";
  }
  const activeOrg = orgs.find((o) => o.org_id === activeOrgID);
  return {
    status: "authenticated",
    user: res.user,
    orgs,
    activeOrgID,
    activeOrgRole: (res.active_org_role || activeOrg?.org_role || null) as OrgRole | null,
    activeTeamID,
    activeRole: (res.active_role ?? null) as Role | null,
  };
}

export function AuthProvider({ children }: { children: ReactNode }) {
  const [state, setState] = useState<AuthState>(initial);

  const bootstrap = useCallback(async () => {
    const probe = await probeAuth();
    if (probe === "unreachable") {
      setState({ ...initial, status: "unreachable" });
      return;
    }
    if (probe === "not_required") {
      setState(localIdentity);
      return;
    }
    // Try getMe up to 3 times with exponential backoff on transient
    // 5xx errors so a server hiccup at startup doesn't bounce a
    // user with a valid session out to the login screen.
    let lastErr: unknown = null;
    for (let attempt = 0; attempt < 3; attempt++) {
      try {
        const me = await getMe();
        setState((prev) => applyResponse(prev, me));
        return;
      } catch (err) {
        lastErr = err;
        if (err instanceof ApiError && err.status === 401) {
          break;
        }
        // Transient (network blip / 5xx) — wait and try again.
        await new Promise((resolve) => setTimeout(resolve, 500 * 2 ** attempt));
      }
    }
    // Every getMe attempt failed on a network error (server went down
    // between the probe and here) — that's unreachable, not anonymous.
    if (lastErr && !(lastErr instanceof ApiError)) {
      setState({ ...initial, status: "unreachable" });
      return;
    }
    if (lastErr instanceof ApiError && lastErr.status !== 401) {
      setState({ ...initial, status: "anonymous" });
      return;
    }
    try {
      const r = await apiRefresh();
      setState((prev) => applyResponse(prev, r));
      return;
    } catch (err) {
      setState({
        ...initial,
        status: err instanceof ApiError ? "anonymous" : "unreachable",
      });
    }
  }, []);

  useEffect(() => {
    void bootstrap();
  }, [bootstrap]);

  const signIn = useCallback(async (email: string, password: string) => {
    const res = await apiLogin(email, password);
    setState((prev) => applyResponse(prev, res));
  }, []);

  // Registration sets the session cookies server-side just like login; the
  // response must flow into the auth state or the shell stays anonymous.
  const signUp = useCallback(
    async (input: { email: string; password: string; name?: string; invitation?: string }) => {
      const res = await apiRegister(input);
      setState((prev) => applyResponse(prev, res));
    },
    [],
  );

  const signOut = useCallback(async () => {
    try {
      await apiLogout();
    } catch {
      // Cookies already cleared even if the server rejected — proceed.
    }
    // Drop the repo-first context so a different account on this browser
    // never inherits the previous user's active repo.
    useActiveRepoStore.getState().reset();
    setState({ ...initial, status: "anonymous" });
  }, []);

  const selectOrg = useCallback(async (orgID: string) => {
    const res = await apiSwitchOrg(orgID);
    setState((prev) => applyResponse(prev, res));
  }, []);

  const selectTeam = useCallback(async (teamID: string) => {
    const res = await apiSwitchTeam(teamID);
    setState((prev) => applyResponse(prev, res));
  }, []);

  const reloadIdentity = useCallback(async () => {
    const me = await getMe();
    setState((prev) => applyResponse(prev, me));
  }, []);

  const value = useMemo<AuthCtx>(() => {
    const activeOrg = state.orgs.find((o) => o.org_id === state.activeOrgID);
    const teams = activeOrg?.teams ?? [];
    return {
      ...state,
      activeOrg,
      teams,
      activeTeam: teams.find((t) => t.team_id === state.activeTeamID),
      isRestricted:
        state.status === "authenticated" &&
        !state.user?.is_super_admin &&
        state.orgs.every((o) => o.teams.length === 0),
      signIn,
      signUp,
      signOut,
      selectOrg,
      selectTeam,
      reloadIdentity,
      retryConnection: bootstrap,
    };
  }, [state, signIn, signUp, signOut, selectOrg, selectTeam, reloadIdentity, bootstrap]);

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
