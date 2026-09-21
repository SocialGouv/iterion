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
import { getOrg, getTeam, type OrgView, type TeamSummary } from "@/api/orgs";
import { useAuth } from "@/auth/AuthContext";

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
  /** The owning org, when it is known (the tree knows it; the team row carries no org id). */
  orgID: string | null;
  isMember: boolean;
}

export interface SubjectResult<T> {
  subject: T | null;
  loading: boolean;
  error: unknown;
  /**
   * True once we know the caller cannot see this subject at all — the tree
   * does not have it and the server refused. Distinct from `subject ==
   * null` while loading, which is the state that used to render as "not a
   * member" before the request had even been made.
   */
  denied: boolean;
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
      error: null,
      denied: false,
    };
  }
  return {
    subject: query.data ? fromOrgView(query.data) : null,
    loading: query.isPending && orgID !== "",
    error: query.error,
    denied: query.error != null,
  };
}

export function useTeamSubject(teamID: string): SubjectResult<TeamSubject> {
  const { orgs, teams } = useAuth();
  const inTree = teams.find((t) => t.team_id === teamID);
  // The tree is also the only place the owning org is recorded: a team row
  // from the API carries no org id, and the org is what a team's member
  // picker and audit tab need.
  const owningOrg = orgs.find((o) => o.teams.some((t) => t.team_id === teamID));

  const query = useQuery({
    queryKey: ["team-subject", teamID],
    queryFn: () => getTeam(teamID),
    enabled: !inTree && teamID !== "",
  });

  if (inTree) {
    return {
      subject: {
        teamID: inTree.team_id,
        name: inTree.team_name,
        slug: inTree.team_slug,
        role: inTree.role,
        personal: inTree.personal ?? false,
        orgID: owningOrg?.org_id ?? null,
        isMember: true,
      },
      loading: false,
      error: null,
      denied: false,
    };
  }
  const t: TeamSummary | undefined = query.data;
  return {
    subject: t
      ? {
          teamID: t.id,
          name: t.name,
          slug: t.slug,
          role: null,
          personal: t.personal ?? false,
          status: t.status,
          orgID: null,
          isMember: false,
        }
      : null,
    loading: query.isPending && teamID !== "",
    error: query.error,
    denied: query.error != null,
  };
}
