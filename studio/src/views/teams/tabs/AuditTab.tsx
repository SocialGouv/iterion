import { errorMessage } from "@/lib/errorHints";
import { formatDateTime } from "@/lib/format";
import { useEffect, useMemo, useState } from "react";
import { useLocation } from "wouter";
import { InlineBanner } from "@/components/ui/InlineBanner";

import {
  type AuditEvent,
  type AuditQuery,
  FeatureUnavailableError,
  listAdminAudit,
  listOrgAudit,
  listTeamAudit,
} from "@/api/audit";

import { useMaybeAuth } from "@/auth/AuthContext";
import { Button } from "@/components/ui/Button";
import { EmptyState } from "@/components/ui/EmptyState";
import { Input } from "@/components/ui/Input";
import { Table, THead, Th, TBody, Tr, Td, TableSkeleton } from "@/components/ui/Table";

// Three scopes, one table: teamID (team audit), orgID (org control-plane
// audit), or platform (the super-admin /api/admin/audit feed spanning
// every org). Exactly one should be set.
interface Props {
  teamID?: string;
  orgID?: string;
  platform?: boolean;
  canManage: boolean;
}

const PAGE = 50;

export default function AuditTab({ teamID, orgID, platform, canManage }: Props) {
  const id = platform ? "platform" : (orgID ?? teamID ?? "");
  const load = (q: AuditQuery) =>
    platform
      ? listAdminAudit(q)
      : orgID
        ? listOrgAudit(orgID, q)
        : listTeamAudit(teamID ?? "", q);
  const [, navigate] = useLocation();
  // Cross-link the two audit surfaces. When rendering the team audit we
  // resolve the containing org (the team is expected to live under the
  // active org's tree — the studio only routes to /teams/:id while
  // switched into its org); when rendering the org audit we point at the
  // Teams tab as the entry to each team's own audit page.
  // Provider-optional: the cross-link is decoration — the tab must
  // still mount without an AuthProvider (jsdom a11y harness).
  const auth = useMaybeAuth();
  const orgs = auth?.orgs ?? [];
  const activeOrg = auth?.activeOrg ?? null;
  const containingOrgID = useMemo(() => {
    if (!teamID) return "";
    if (activeOrg?.teams.some((t) => t.team_id === teamID)) return activeOrg.org_id;
    const owner = orgs.find((o) => o.teams.some((t) => t.team_id === teamID));
    return owner?.org_id ?? "";
  }, [orgs, activeOrg, teamID]);
  const [events, setEvents] = useState<AuditEvent[]>([]);
  const [filter, setFilter] = useState<AuditQuery>({});
  const [nextOffset, setNextOffset] = useState<number | null>(null);
  const [loading, setLoading] = useState(false);
  const [err, setErr] = useState<string | null>(null);
  const [unavailable, setUnavailable] = useState(false);

  const fetchPage = async (q: AuditQuery, append: boolean) => {
    setLoading(true);
    setErr(null);
    try {
      const r = await load({ ...q, limit: PAGE });
      // The Go handler marshals an empty audit log as `events: null` (a nil
      // slice), so coalesce to [] — otherwise `events.length` in the render
      // (and below) throws "can't access property length, … is null".
      const page = r.events ?? [];
      setEvents(append ? [...events, ...page] : page);
      // When the server returns a full page assume there may be more. We treat
      // anything < requested page size as exhausted.
      setNextOffset(page.length < PAGE ? null : r.next_offset);
    } catch (e) {
      if (e instanceof FeatureUnavailableError) setUnavailable(true);
      else setErr(errorMessage(e));
    } finally {
      setLoading(false);
    }
  };

  useEffect(() => {
    if (!canManage) return;
    void fetchPage({ ...filter, offset: 0 }, false);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [id, canManage]);

  if (!canManage) {
    return (
      <EmptyState
        title="Audit log is admin-only"
        message="Audit rows expose actor emails and IPs across the whole org. Only team admins can read them."
      />
    );
  }
  if (unavailable) {
    return (
      <EmptyState
        title="Audit log not enabled on this server"
        message="The audit log requires the cloud-mode audit store."
      />
    );
  }

  const apply = () => void fetchPage({ ...filter, offset: 0 }, false);
  const loadMore = () => {
    if (nextOffset == null) return;
    void fetchPage({ ...filter, offset: nextOffset }, true);
  };

  return (
    <div className="space-y-3">
      {err && (
        <InlineBanner tone="danger" layout="inline">
          {err}
        </InlineBanner>
      )}

      {platform ? (
        <p className="text-caption text-fg-subtle">
          Platform scope: every control-plane action across all orgs and teams,
          including super-admin operations. Tenant-scoped views live on each
          org and team page.
        </p>
      ) : orgID ? (
        <p className="text-caption text-fg-subtle">
          Team-scope changes (webhooks, secrets, bindings) are audited on each team's page.
        </p>
      ) : containingOrgID ? (
        <p className="text-caption text-fg-subtle">
          Org-wide actions (SSO, teams, domains) appear in the{" "}
          <button
            type="button"
            className="text-accent-text hover:underline"
            onClick={() => navigate(`/orgs/${containingOrgID}?tab=audit`)}
          >
            organization audit log
          </button>
          .
        </p>
      ) : (
        <p className="text-caption text-fg-subtle">
          Org-wide actions (SSO, teams, domains) appear in the organization audit log.
        </p>
      )}

      <form
        onSubmit={(e) => {
          e.preventDefault();
          apply();
        }}
      >
        <div className="grid grid-cols-1 sm:grid-cols-4 gap-2 text-sm">
          <Input
            aria-label="Filter by action"
            placeholder="Action (e.g. webhook.rotated)"
            value={filter.action ?? ""}
            onChange={(e) => setFilter({ ...filter, action: e.target.value })}
          />
          <Input
            aria-label="Filter by actor id"
            placeholder="Actor id"
            value={filter.actor ?? ""}
            onChange={(e) => setFilter({ ...filter, actor: e.target.value })}
          />
          <Input
            aria-label="From date"
            type="datetime-local"
            value={filter.from ?? ""}
            onChange={(e) => setFilter({ ...filter, from: e.target.value })}
          />
          <Input
            aria-label="To date"
            type="datetime-local"
            value={filter.to ?? ""}
            onChange={(e) => setFilter({ ...filter, to: e.target.value })}
          />
        </div>
        <div className="flex gap-2 mt-3">
          <Button size="sm" variant="primary" type="submit" loading={loading}>
            Apply filters
          </Button>
          <Button
            size="sm"
            variant="ghost"
            type="button"
            onClick={() => {
              setFilter({});
              void fetchPage({}, false);
            }}
          >
            Clear
          </Button>
        </div>
      </form>

      {events.length === 0 && loading ? (
        <TableSkeleton rows={4} cols={5} />
      ) : events.length === 0 ? (
        <EmptyState message="No events for this filter." />
      ) : (
        <Table caption="Audit log events">
          <THead>
            <Th>When</Th>
            <Th>Actor</Th>
            <Th>Action</Th>
            <Th>Target</Th>
            {platform && <Th>Tenant</Th>}
            <Th>IP</Th>
          </THead>
          <TBody>
            {events.map((e) => (
              <Tr key={e.id} className="align-top">
                <Td className="text-fg-muted whitespace-nowrap">
                  {formatDateTime(e.created_at)}
                </Td>
                <Td className="text-xs">
                  <div>{e.actor_id ?? "—"}</div>
                  <div className="text-fg-subtle">{e.actor_kind ?? ""}</div>
                </Td>
                <Td className="font-mono text-xs">{e.action}</Td>
                <Td className="text-xs">
                  <div>{e.target ?? ""}</div>
                  <div className="text-fg-subtle font-mono break-all">{e.target_id ?? ""}</div>
                  {e.meta && (
                    <details className="text-fg-muted">
                      <summary className="cursor-pointer text-caption">meta</summary>
                      <pre className="whitespace-pre-wrap break-all text-caption">
                        {JSON.stringify(e.meta, null, 2)}
                      </pre>
                    </details>
                  )}
                </Td>
                {platform && (
                  <Td className="text-xs font-mono text-fg-muted break-all">
                    {e.tenant_id ?? "—"}
                  </Td>
                )}
                <Td className="text-xs font-mono text-fg-muted">{e.ip ?? "—"}</Td>
              </Tr>
            ))}
          </TBody>
        </Table>
      )}

      {nextOffset != null && (
        <div className="flex justify-center">
          <Button size="sm" variant="ghost" loading={loading} onClick={loadMore}>
            Load more
          </Button>
        </div>
      )}
    </div>
  );
}
