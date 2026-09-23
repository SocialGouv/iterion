// Resolving the org or team a page is ABOUT.
//
// Every tenant-scoped page used to read its subject out of `useAuth()` —
// the caller's own org/team tree. That works for a member and fails
// silently for anyone else: a super-admin who is not a member of an org
// got "You are not a member of this organization" on /orgs/:id, although
// every API behind that page would have answered (canViewOrg and
// canManageOrg short-circuit on IsSuperAdmin). The org→teams drill-down in
// the super-admin console was therefore a dead end — it could list an
// org's teams and open none of them.
//
// These hooks keep the tree as the fast path (no request for the common
// case, and it carries the caller's ROLE, which the API rows do not) and
// fall back to the server when the subject is not in it. The server stays
// the authority on who may look: the fallback simply asks.

import { useQuery } from "@tanstack/react-query";

import type { OrgTreeView, MembershipView } from "@/api/auth";
import { ApiError } from "@/api/client";
import { getOrg, getTeam, type OrgView, type TeamSummary } from "@/api/orgs";
import { useAuth } from "@/auth/AuthContext";
import { errorMessage } from "@/lib/errorHints";

/**
 * The single "what did this caller get on team X" lookup.
 *
 * It walks `orgs`, NOT `useAuth().teams` — the latter is `activeOrg?.teams`
 * (AuthContext), so a team the caller genuinely belongs to in a non-active
 * org is absent from it. Two readers asking that question from two
 * projections is how one of them starts answering differently from the
 * other; this is the one both use.
 */
export function findTeamGrant(
  orgs: OrgTreeView[],
  teamID: string,
): { org: OrgTreeView; team: MembershipView } | null {
  if (!teamID) return null;
  for (const org of orgs) {
    const team = org.teams.find((t) => t.team_id === teamID);
    if (team) return { org, team };
  }
  return null;
}

export interface OrgSubject {
  orgID: string;
  name: string;
  slug: string;
  /** The caller's own role in this org, or null when they hold none. */
  role: OrgTreeView["org_role"] | null;
  personal: boolean;
  /** True when the subject came from the identity tree rather than the API. */
  isMember: boolean;
}

export interface TeamSubject {
  teamID: string;
  name: string;
  slug: string;
  /** The caller's own role on this team, or null when they hold none. */
  role: MembershipView["role"] | null;
  personal: boolean;
  status?: string;
  /** The owning org's id. Always known: the tree has it, and so does the API row. */
  orgID: string | null;
  /** The owning org's NAME, which only the tree carries — null otherwise. */
  orgName: string | null;
  isMember: boolean;
}

export interface SubjectResult<T> {
  subject: T | null;
  loading: boolean;
  /**
   * True once we know the caller may not see this subject — the tree does
   * not have it and the server answered 401/403. Distinct from `subject ==
   * null` while loading, which is the state that used to render as "not a
   * member" before the request had even been made.
   *
   * NARROW on purpose. A super-admin passes canViewOrg/canViewTeam, so a
   * tenant that simply does not exist comes back 404 — reporting that as
   * denied tells the operator they lack an access they in fact hold, and a
   * transient 5xx would say the same. Those two get their own answers below.
   */
  denied: boolean;
  /** The subject does not exist (404), as distinct from being refused. */
  notFound: boolean;
  /**
   * Anything else that went wrong, already formatted. Surfacing it beats
   * the repo's least favourite shape: an error dressed as a permission.
   */
  error: string | null;
}

// classify splits a failed subject fetch into the three answers a page owes
// its reader. Anything that is not an ApiError (a network drop, a parse
// failure) is an error, never a refusal.
function classify(err: unknown): Pick<SubjectResult<never>, "denied" | "notFound" | "error"> {
  if (err == null) return { denied: false, notFound: false, error: null };
  if (err instanceof ApiError) {
    if (err.status === 401 || err.status === 403) {
      return { denied: true, notFound: false, error: null };
    }
    if (err.status === 404) {
      return { denied: false, notFound: true, error: null };
    }
  }
  return { denied: false, notFound: false, error: errorMessage(err) };
}

function fromOrgView(o: OrgView): OrgSubject {
  return {
    orgID: o.id,
    name: o.name,
    slug: o.slug,
    role: null,
    personal: o.personal ?? false,
    isMember: false,
  };
}

export function useOrgSubject(orgID: string): SubjectResult<OrgSubject> {
  const { orgs } = useAuth();
  const inTree = orgs.find((o) => o.org_id === orgID);

  const query = useQuery({
    queryKey: ["org-subject", orgID],
    queryFn: () => getOrg(orgID),
    // Only asked when the tree cannot answer — a member pays nothing.
    enabled: !inTree && orgID !== "",
  });

  if (inTree) {
    return {
      subject: {
        orgID: inTree.org_id,
        name: inTree.org_name,
        slug: inTree.org_slug,
        role: inTree.org_role || null,
        personal: inTree.personal ?? false,
        isMember: true,
      },
      loading: false,
      denied: false,
      notFound: false,
      error: null,
    };
  }
  return {
    subject: query.data ? fromOrgView(query.data) : null,
    loading: query.isPending && orgID !== "",
    ...classify(query.error),
  };
}

export function useTeamSubject(teamID: string): SubjectResult<TeamSubject> {
  const { orgs } = useAuth();
  const grant = findTeamGrant(orgs, teamID);

  const query = useQuery({
    queryKey: ["team-subject", teamID],
    queryFn: () => getTeam(teamID),
    enabled: grant == null && teamID !== "",
  });

  if (grant) {
    return {
      subject: {
        teamID: grant.team.team_id,
        name: grant.team.team_name,
        slug: grant.team.team_slug,
        role: grant.team.role,
        personal: grant.team.personal ?? false,
        orgID: grant.org.org_id,
        orgName: grant.org.org_name,
        isMember: true,
      },
      loading: false,
      denied: false,
      notFound: false,
      error: null,
    };
  }
  const t: TeamSummary | undefined = query.data;
  return {
    // The team row carries its own org_id, so the non-member path resolves
    // the parent too — which is what lets a super-admin act on a team they
    // do not belong to rather than merely look at it.
    subject: t
      ? {
          teamID: t.id,
          name: t.name,
          slug: t.slug,
          role: null,
          personal: t.personal ?? false,
          status: t.status,
          orgID: t.org_id ?? null,
          orgName: null,
          isMember: false,
        }
      : null,
    loading: query.isPending && teamID !== "",
    ...classify(query.error),
  };
}
