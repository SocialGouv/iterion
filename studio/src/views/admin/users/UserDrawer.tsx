// UserDrawer — one account's file, and the place an operator acts on it.
//
// It exists because /admin/users could enable, disable and reset an
// account but said nothing about where it belonged, so the question that
// actually brings an operator here — "this person signed in and sees
// nothing, why?" — had no answer on the page it lands on.
//
// The diagnosis is the top section read as a whole: an account that is
// ACTIVE, has an SSO link, has no password and has an empty roster is a
// GitHub login admitted outside the SSO org allow-list (provisionSubmitter).
// Each of those facts alone looks like a broken deployment; together they
// name the cause, and the placement section below is the repair.

import { useMemo, useState } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { Link } from "wouter";

import {
  getAdminUser,
  type AdminUserDetail,
  type AdminUserOrgView,
  type AdminUserTeamView,
} from "@/api/admin";
import type { OrgRole, Role } from "@/api/auth";
import { putTeamMember, removeMember, updateMemberRole } from "@/api/byok";
import { putOrgMember, removeOrgMember, updateOrgMemberRole } from "@/api/orgMembers";
import { listAdminOrgTeams, listOrgs } from "@/api/orgs";

import { Badge } from "@/components/ui/Badge";
import { Button } from "@/components/ui/Button";
import { Combobox, type ComboboxOption } from "@/components/ui/Combobox";
import { Dialog } from "@/components/ui/Dialog";
import { EmptyState } from "@/components/ui/EmptyState";
import { InlineBanner } from "@/components/ui/InlineBanner";
import { RoleSelect } from "@/components/shared/RoleSelect";
import { Table, THead, Th, TBody, Tr, Td } from "@/components/ui/Table";
import PanelLoading from "@/components/shared/PanelLoading";
import { useAsyncAction } from "@/hooks/useAsyncAction";
import { useConfirm, type Confirmer } from "@/hooks/useConfirm";
import { errorMessage } from "@/lib/errorHints";
import { formatDateTime } from "@/lib/format";
import { ORG_ROLES, TEAM_ROLES, confirmOwnerGrant } from "@/lib/roles";

export default function UserDrawer({
  userID,
  onClose,
}: {
  userID: string;
  onClose: () => void;
}) {
  const queryClient = useQueryClient();
  const { confirm, dialog } = useConfirm();
  // The shared busy/error slot every "fire an API call from a button" site
  // uses; the sibling org console wires it the same way.
  const action = useAsyncAction();
  const busy = action.busy;

  const detailQuery = useQuery({
    queryKey: ["admin-user", userID],
    queryFn: () => getAdminUser(userID),
  });
  const detail = detailQuery.data ?? null;

  const err =
    action.error ??
    (detailQuery.error && !detailQuery.isFetching
      ? errorMessage(detailQuery.error)
      : null);

  // Every mutation refreshes the account file. It does NOT invalidate the
  // users list: this drawer only writes memberships, and that list renders
  // email, name, status and super-admin — none of which a membership can
  // change. The mutations that DO change status live on the page itself
  // and invalidate there.
  const run = (fn: () => Promise<unknown>) =>
    action.run(async () => {
      await fn();
      await queryClient.invalidateQueries({ queryKey: ["admin-user", userID] });
    });

  return (
    <Dialog
      open
      onOpenChange={(v) => {
        if (!v) onClose();
      }}
      title={detail?.user.email ?? "Account"}
      description={<span className="font-mono text-xs">{userID}</span>}
      widthClass="max-w-3xl"
      footer={
        <Button variant="secondary" onClick={onClose}>
          Close
        </Button>
      }
    >
      {dialog}
      {err && (
        <InlineBanner tone="danger" layout="inline">
          {err}
        </InlineBanner>
      )}

      {detailQuery.isPending ? (
        <PanelLoading label="Loading account…" />
      ) : detail == null ? (
        <EmptyState message="This account could not be loaded." />
      ) : (
        <>
          <IdentitySection detail={detail} />
          <OrgsSection detail={detail} busy={busy} run={run} confirm={confirm} />
          <TeamsSection detail={detail} busy={busy} run={run} confirm={confirm} />
        </>
      )}
    </Dialog>
  );
}

// ---- Identity ----

function IdentitySection({ detail }: { detail: AdminUserDetail }) {
  const { user, has_password: hasPassword, sso_links: links } = detail;
  // The one inference this page makes, and it is the reason it exists: no
  // password AND no SSO link means no way in at all — a different problem
  // from "locked out", which a reset would fix.
  const noWayIn = !hasPassword && links.length === 0;
  const unplaced = detail.orgs.length === 0 && detail.teams.length === 0;

  return (
    <section className="space-y-3 mb-5">
      <h4 className="font-medium">Identity</h4>

      {unplaced && (
        <InlineBanner tone="warning" layout="inline" title="This account belongs to nothing">
          It can sign in{noWayIn ? " — except it has no credential either" : ""} and will
          see an empty workspace. A GitHub login admitted outside the SSO
          allow-list lands exactly here: place it in an organization below,
          then in a team.
        </InlineBanner>
      )}

      <dl className="grid grid-cols-1 sm:grid-cols-2 gap-x-6 gap-y-2 text-sm">
        <Fact label="Status">
          <Badge
            variant={
              user.status === "active"
                ? "success"
                : user.status === "disabled"
                  ? "danger"
                  : "warning"
            }
          >
            {user.status}
          </Badge>
        </Fact>
        <Fact label="Name">{user.name ?? "—"}</Fact>
        <Fact label="Platform role">
          {user.is_super_admin ? (
            <span className="text-warning-fg">super-admin</span>
          ) : (
            <span className="text-fg-muted">standard</span>
          )}
        </Fact>
        <Fact label="Password sign-in">
          {hasPassword ? (
            "enabled"
          ) : (
            <span className="text-fg-muted">
              none — this account cannot sign in with a password
            </span>
          )}
        </Fact>
        <Fact label="Created">{user.created_at ? formatDateTime(user.created_at) : "—"}</Fact>
        <Fact label="Last sign-in">
          {user.last_login_at ? (
            formatDateTime(user.last_login_at)
          ) : (
            <span className="text-fg-muted">never</span>
          )}
        </Fact>
      </dl>

      <div>
        <h5 className="text-sm font-medium mb-1">Connected identities</h5>
        {links.length === 0 ? (
          <p className="text-caption text-fg-subtle">
            No SSO identity linked.
            {hasPassword ? "" : " With no password either, this account has no way to sign in."}
          </p>
        ) : (
          <ul className="text-sm space-y-1">
            {links.map((l) => (
              <li key={`${l.provider}:${l.subject}`} className="flex flex-wrap gap-x-2">
                <span className="font-medium">{l.provider}</span>
                <span className="font-mono text-caption text-fg-subtle">{l.subject}</span>
                {l.email && <span className="text-fg-muted">{l.email}</span>}
                {l.created_at && (
                  <span className="text-caption text-fg-subtle">
                    linked {formatDateTime(l.created_at)}
                  </span>
                )}
              </li>
            ))}
          </ul>
        )}
      </div>
    </section>
  );
}

function Fact({ label, children }: { label: string; children: React.ReactNode }) {
  return (
    <div>
      <dt className="text-caption text-fg-subtle">{label}</dt>
      <dd>{children}</dd>
    </div>
  );
}

// ---- Organizations ----

type Runner = (fn: () => Promise<unknown>) => Promise<unknown>;

function OrgsSection({
  detail,
  busy,
  run,
  confirm,
}: {
  detail: AdminUserDetail;
  busy: boolean;
  run: Runner;
  confirm: Confirmer;
}) {
  const userID = detail.user.id;
  const memberOf = useMemo(
    () => new Set(detail.orgs.map((o) => o.org_id)),
    [detail.orgs],
  );

  // Every org on the platform, minus the ones already granted. The key is
  // the one the /admin/orgs console uses, so that page primes it; the
  // staleTime keeps an operator triaging twenty accounts from re-fetching
  // the whole platform org list twenty times for a picker they may never
  // open.
  const orgsQuery = useQuery({
    queryKey: ["admin-orgs"],
    queryFn: listOrgs,
    staleTime: 60_000,
  });
  const options = useMemo<ComboboxOption<string>[]>(
    () =>
      (orgsQuery.data ?? [])
        .filter((o) => !memberOf.has(o.id) && !o.personal)
        .map((o) => ({
          value: o.id,
          label: o.name,
          description: o.slug,
          searchHaystack: `${o.name} ${o.slug} ${o.id}`,
        })),
    [orgsQuery.data, memberOf],
  );

  const [orgID, setOrgID] = useState("");
  const [role, setRole] = useState<OrgRole>("member");

  const removeFrom = async (o: AdminUserOrgView) => {
    const ok = await confirm({
      title: "Remove from organization?",
      message: `Removing this account from "${o.org_name ?? o.org_id}" also revokes every team grant it holds inside that organization.`,
      confirmLabel: "Remove",
      confirmVariant: "danger",
    });
    if (!ok) return;
    await run(() => removeOrgMember(o.org_id, userID));
  };

  return (
    <section className="space-y-2 mb-5">
      <h4 className="font-medium">
        Organizations <span className="text-caption text-fg-subtle">({detail.orgs.length})</span>
      </h4>

      {detail.orgs.length === 0 ? (
        <EmptyState message="This account belongs to no organization." />
      ) : (
        <Table caption="Organizations this account belongs to">
          <THead>
            <Th>Organization</Th>
            <Th>Role</Th>
            <Th>Joined</Th>
            <Th align="right" srLabel="Actions" />
          </THead>
          <TBody>
            {detail.orgs.map((o) => (
              <Tr key={o.org_id}>
                <Td>
                  <Link href={`/orgs/${o.org_id}`} className="text-accent-text hover:underline">
                    {o.org_name ?? o.org_id}
                  </Link>
                  <div className="text-caption text-fg-subtle font-mono">
                    {o.org_slug ?? o.org_id}
                    {o.personal ? " · personal" : ""}
                  </div>
                </Td>
                <Td>
                  <RoleSelect
                    value={o.role}
                    roles={ORG_ROLES}
                    disabled={busy}
                    ariaLabel={`Org role for ${o.org_name ?? o.org_id}`}
                    confirmChangeFrom={o.role}
                    confirm={confirm}
                    onChange={(role) =>
                      run(() => updateOrgMemberRole(o.org_id, userID, role as OrgRole))
                    }
                  />
                </Td>
                <Td className="text-fg-muted">
                  {o.joined_at ? formatDateTime(o.joined_at) : "—"}
                </Td>
                <Td align="right">
                  <Button
                    size="sm"
                    variant="danger"
                    disabled={busy}
                    onClick={() => void removeFrom(o)}
                  >
                    Remove
                  </Button>
                </Td>
              </Tr>
            ))}
          </TBody>
        </Table>
      )}

      <div className="flex gap-2 items-end pt-1">
        <div className="flex-1 min-w-0">
          <label htmlFor="user-add-org" className="sr-only">
            Organization
          </label>
          <Combobox
            id="user-add-org"
            size="md"
            value={orgID}
            options={options}
            placeholder={
              orgsQuery.isPending
                ? "Loading organizations…"
                : options.length === 0
                  ? "No other organization"
                  : "Add to an organization…"
            }
            disabled={busy || orgsQuery.isPending || options.length === 0}
            onChange={(v) => setOrgID(v)}
          />
        </div>
        <div>
          <label htmlFor="user-add-org-role" className="sr-only">
            Organization role
          </label>
          <RoleSelect
            size="md"
            id="user-add-org-role"
            value={role}
            roles={ORG_ROLES}
            disabled={busy}
            onChange={(r) => setRole(r as OrgRole)}
          />
        </div>
        <Button
          variant="primary"
          loading={busy}
          disabled={!orgID || busy}
          onClick={() =>
            void (async () => {
              // The prompt guards the WRITE. On the select it guarded a
              // gesture that writes nothing, and the role survives a
              // successful add — so the next owner grant went out silent.
              if (await confirmOwnerGrant(confirm, role)) {
                await run(async () => {
                  await putOrgMember(orgID, userID, role);
                  setOrgID("");
                });
              }
            })()
          }
        >
          Add to org
        </Button>
      </div>
    </section>
  );
}

// ---- Teams ----

function TeamsSection({
  detail,
  busy,
  run,
  confirm,
}: {
  detail: AdminUserDetail;
  busy: boolean;
  run: Runner;
  confirm: Confirmer;
}) {
  const userID = detail.user.id;
  const [pickedOrgID, setPickedOrgID] = useState("");
  const [teamID, setTeamID] = useState("");
  const [role, setRole] = useState<Role>("member");

  // A team grant requires an org membership (the server answers 422
  // otherwise), so the only teams offered are those of an org this account
  // already belongs to.
  //
  // Reconciled against the refetched roster on every render rather than held
  // as state: revoking that org membership from the table above leaves the
  // picked id behind, and a stale pick still drives the team list and the
  // submit. The server's 422 catches it — but the panel would be showing a
  // scope it is no longer using and handing back an unexplained refusal.
  const orgID = detail.orgs.some((o) => o.org_id === pickedOrgID) ? pickedOrgID : "";

  const orgOptions = useMemo<ComboboxOption<string>[]>(
    () =>
      detail.orgs.map((o) => ({
        value: o.org_id,
        label: o.org_name ?? o.org_id,
        description: o.org_slug,
      })),
    [detail.orgs],
  );

  const teamsQuery = useQuery({
    queryKey: ["admin-org-teams", orgID],
    queryFn: () => listAdminOrgTeams(orgID),
    enabled: orgID !== "",
  });
  const granted = useMemo(
    () => new Set(detail.teams.map((t) => t.team_id)),
    [detail.teams],
  );
  const teamOptions = useMemo<ComboboxOption<string>[]>(
    () =>
      (teamsQuery.data ?? [])
        .filter((t) => !granted.has(t.id) && !t.personal)
        .map((t) => ({
          value: t.id,
          label: t.name,
          description: t.slug,
          searchHaystack: `${t.name} ${t.slug} ${t.id}`,
        })),
    [teamsQuery.data, granted],
  );

  const removeFrom = async (t: AdminUserTeamView) => {
    const ok = await confirm({
      title: "Revoke team access?",
      message: `Remove this account from "${t.team_name ?? t.team_id}"? Its organization membership is kept.`,
      confirmLabel: "Revoke",
      confirmVariant: "danger",
    });
    if (!ok) return;
    await run(() => removeMember(t.team_id, userID));
  };

  return (
    <section className="space-y-2">
      <h4 className="font-medium">
        Teams <span className="text-caption text-fg-subtle">({detail.teams.length})</span>
      </h4>

      {detail.teams.length === 0 ? (
        <EmptyState message="This account holds no team grant." />
      ) : (
        <Table caption="Teams this account can access">
          <THead>
            <Th>Team</Th>
            <Th>Organization</Th>
            <Th>Role</Th>
            <Th align="right" srLabel="Actions" />
          </THead>
          <TBody>
            {detail.teams.map((t) => (
              <Tr key={t.team_id}>
                <Td>
                  <Link href={`/teams/${t.team_id}`} className="text-accent-text hover:underline">
                    {t.team_name ?? t.team_id}
                  </Link>
                  <div className="text-caption text-fg-subtle font-mono">
                    {t.team_slug ?? t.team_id}
                    {t.personal ? " · personal" : ""}
                    {t.status && t.status !== "active" ? ` · ${t.status}` : ""}
                  </div>
                </Td>
                <Td className="text-fg-muted">
                  {t.org_name ?? t.org_id ?? "—"}
                  {/* Two drifts, two sentences: naming the wrong one is
                      worse than naming none. */}
                  {t.orphan_grant && (
                    <div className="text-caption text-danger">
                      no org membership — this grant should not exist
                    </div>
                  )}
                  {t.missing_team && (
                    <div className="text-caption text-danger">
                      the team no longer exists — a half-finished cascade
                    </div>
                  )}
                </Td>
                <Td>
                  <RoleSelect
                    value={t.role}
                    roles={TEAM_ROLES}
                    disabled={busy}
                    ariaLabel={`Team role for ${t.team_name ?? t.team_id}`}
                    confirmChangeFrom={t.role}
                    confirm={confirm}
                    onChange={(role) => run(() => updateMemberRole(t.team_id, userID, role))}
                  />
                </Td>
                <Td align="right">
                  <Button
                    size="sm"
                    variant="danger"
                    disabled={busy}
                    onClick={() => void removeFrom(t)}
                  >
                    Revoke
                  </Button>
                </Td>
              </Tr>
            ))}
          </TBody>
        </Table>
      )}

      {detail.orgs.length === 0 ? (
        <p className="text-caption text-fg-subtle pt-1">
          Add this account to an organization first — a team grant sits inside
          an org membership.
        </p>
      ) : (
        <div className="flex gap-2 items-end pt-1">
          <div className="flex-1 min-w-0">
            <label htmlFor="user-add-team-org" className="sr-only">
              Organization
            </label>
            <Combobox
              id="user-add-team-org"
              size="md"
              value={orgID}
              options={orgOptions}
              placeholder="Organization…"
              disabled={busy}
              onChange={(v) => {
                setPickedOrgID(v);
                // The team list belongs to the org: keeping a pick across a
                // change would submit a team the operator can no longer see.
                setTeamID("");
              }}
            />
          </div>
          <div className="flex-1 min-w-0">
            <label htmlFor="user-add-team" className="sr-only">
              Team
            </label>
            <Combobox
              id="user-add-team"
              size="md"
              value={teamID}
              options={teamOptions}
              placeholder={
                orgID === ""
                  ? "Pick an organization first"
                  : teamsQuery.isPending
                    ? "Loading teams…"
                    : teamOptions.length === 0
                      ? "No team left in this org"
                      : "Team…"
              }
              disabled={busy || orgID === "" || teamsQuery.isPending || teamOptions.length === 0}
              onChange={(v) => setTeamID(v)}
            />
          </div>
          <div>
            <label htmlFor="user-add-team-role" className="sr-only">
              Team role
            </label>
            <RoleSelect
              size="md"
              id="user-add-team-role"
              value={role}
              roles={TEAM_ROLES}
              disabled={busy}
              onChange={(r) => setRole(r as Role)}
            />
          </div>
          <Button
            variant="primary"
            loading={busy}
            // orgID is the reconciled value: when the org membership behind
            // the pick is revoked mid-flight it falls back to "", and the
            // still-held teamID must not be submittable on its own.
            disabled={!teamID || orgID === "" || busy}
            onClick={() =>
              void (async () => {
                if (await confirmOwnerGrant(confirm, role)) {
                  await run(async () => {
                    await putTeamMember(teamID, userID, role);
                    setTeamID("");
                  });
                }
              })()
            }
          >
            Add to team
          </Button>
        </div>
      )}
    </section>
  );
}
