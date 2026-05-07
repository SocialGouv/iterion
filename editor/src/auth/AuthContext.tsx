import {
  createContext,
  useCallback,
  useContext,
  useEffect,
  useMemo,
  useState,
  type ReactNode,
} from "react";
import {
  ApiError,
  getMe,
  login as apiLogin,
  logout as apiLogout,
  refresh as apiRefresh,
  switchTeam as apiSwitchTeam,
  type AuthResponse,
  type MembershipView,
  type Role,
  type UserView,
} from "@/api/auth";

interface AuthState {
  status: "loading" | "anonymous" | "authenticated";
  user: UserView | null;
  teams: MembershipView[];
  activeTeamID: string;
  activeRole: Role | null;
}

interface AuthCtx extends AuthState {
  // activeTeam is the MembershipView whose team_id matches
  // activeTeamID, or undefined when no team is active. Derived in
  // the provider so consumers don't re-search the teams array.
  activeTeam: MembershipView | undefined;
  signIn: (email: string, password: string) => Promise<void>;
  signOut: () => Promise<void>;
  selectTeam: (teamID: string) => Promise<void>;
  // Re-fetch /auth/me — used after a flow that mutates membership
  // server-side (accept invitation, admin promotion).
  reloadIdentity: () => Promise<void>;
}

const Ctx = createContext<AuthCtx | null>(null);

const initial: AuthState = {
  status: "loading",
  user: null,
  teams: [],
  activeTeamID: "",
  activeRole: null,
};

function applyResponse(prev: AuthState, res: AuthResponse): AuthState {
  return {
    status: "authenticated",
    user: res.user,
    teams: res.teams ?? [],
    activeTeamID: res.active_team_id ?? prev.activeTeamID ?? "",
    activeRole: (res.active_role ?? null) as Role | null,
  };
}

export function AuthProvider({ children }: { children: ReactNode }) {
  const [state, setState] = useState<AuthState>(initial);

  const bootstrap = useCallback(async () => {
    try {
      const me = await getMe();
      setState((prev) => applyResponse(prev, me));
      return;
    } catch (err) {
      if (!(err instanceof ApiError) || err.status !== 401) {
        // Non-401 = transient; retry through refresh just in case the
        // access cookie expired between page reloads.
      }
    }
    try {
      const r = await apiRefresh();
      setState((prev) => applyResponse(prev, r));
      return;
    } catch {
      setState({ ...initial, status: "anonymous" });
    }
  }, []);

  useEffect(() => {
    void bootstrap();
  }, [bootstrap]);

  const signIn = useCallback(async (email: string, password: string) => {
    const res = await apiLogin(email, password);
    setState((prev) => applyResponse(prev, res));
  }, []);

  const signOut = useCallback(async () => {
    try {
      await apiLogout();
    } catch {
      // Cookies already cleared even if the server rejected — proceed.
    }
    setState({ ...initial, status: "anonymous" });
  }, []);

  const selectTeam = useCallback(async (teamID: string) => {
    const res = await apiSwitchTeam(teamID);
    setState((prev) => applyResponse(prev, res));
  }, []);

  const reloadIdentity = useCallback(async () => {
    const me = await getMe();
    setState((prev) => applyResponse(prev, me));
  }, []);

  const value = useMemo<AuthCtx>(() => ({
    ...state,
    activeTeam: state.teams.find((t) => t.team_id === state.activeTeamID),
    signIn,
    signOut,
    selectTeam,
    reloadIdentity,
  }), [state, signIn, signOut, selectTeam, reloadIdentity]);

  return <Ctx.Provider value={value}>{children}</Ctx.Provider>;
}

export function useAuth(): AuthCtx {
  const v = useContext(Ctx);
  if (!v) throw new Error("useAuth used outside AuthProvider");
  return v;
}

// RequireAuth wraps children and renders <fallback/> when the user
// is unauthenticated. Used at the routing level to gate the editor.
export function RequireAuth({ children, fallback }: { children: ReactNode; fallback: ReactNode }) {
  const { status } = useAuth();
  if (status === "loading") {
    return <div className="h-screen flex items-center justify-center text-fg-default">Loading…</div>;
  }
  if (status === "anonymous") {
    return <>{fallback}</>;
  }
  return <>{children}</>;
}

// RequireRole: nested gate that checks an active-team role. Renders
// nothing when the requirement is not met (parent shows a friendly
// 403 message).
export function hasRole(role: Role | null, want: Role | "super-admin"): boolean {
  if (!role) return false;
  const order: Record<Role, number> = { viewer: 1, member: 2, admin: 3, owner: 4 };
  if (want === "super-admin") return false; // checked separately via user.is_super_admin
  return order[role] >= order[want];
}
