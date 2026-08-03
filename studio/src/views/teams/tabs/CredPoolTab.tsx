import { errorMessage } from "@/lib/errorHints";
import { formatCost, formatDateTime } from "@/lib/format";
import { useEffect, useState } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { Badge } from "@/components/ui/Badge";
import { Button } from "@/components/ui/Button";
import { Checkbox } from "@/components/ui/Checkbox";
import { EmptyState } from "@/components/ui/EmptyState";
import { FieldLabel } from "@/components/ui/FieldLabel";
import { InlineBanner } from "@/components/ui/InlineBanner";
import { Input } from "@/components/ui/Input";
import { TBody, THead, Table, Td, Th, Tr } from "@/components/ui/Table";
import PanelLoading from "@/components/shared/PanelLoading";
import { useUIStore } from "@/store/ui";
import {
  getTeamPool,
  saveTeamPool,
  type PledgeStatus,
  type PoolAudience,
  type PoolView,
} from "@/api/credpool";

const STATUS_TONE: Record<PledgeStatus, "success" | "warning" | "neutral" | "danger"> = {
  active: "success",
  paused: "neutral",
  cooling: "warning",
  out_of_hours: "neutral",
  exhausted: "warning",
  unhealthy: "danger",
  bot_filtered: "neutral",
};

const csv = (v?: string[]) => (v ?? []).join(", ");
const parseCsv = (s: string) =>
  s
    .split(",")
    .map((x) => x.trim())
    .filter(Boolean);

export default function CredPoolTab({
  teamID,
  canManage,
}: {
  teamID: string;
  canManage: boolean;
}) {
  const queryClient = useQueryClient();
  const query = useQuery<PoolView>({
    queryKey: ["team-pool", teamID],
    queryFn: () => getTeamPool(teamID),
  });
  const [err, setErr] = useState<string | null>(null);
  const [enabled, setEnabled] = useState(false);
  const [audience, setAudience] = useState<PoolAudience>({});
  const [teamsCsv, setTeamsCsv] = useState("");
  const [orgsCsv, setOrgsCsv] = useState("");
  const [busy, setBusy] = useState(false);
  const addToast = useUIStore((s) => s.addToast);

  const pool = query.data;
  useEffect(() => {
    setEnabled(pool?.enabled ?? false);
    setAudience(pool?.audience ?? {});
    setTeamsCsv(csv(pool?.audience?.teams));
    setOrgsCsv(csv(pool?.audience?.orgs));
  }, [pool]);

  const save = async () => {
    setBusy(true);
    setErr(null);
    try {
      await saveTeamPool(teamID, {
        enabled,
        audience: { ...audience, teams: parseCsv(teamsCsv), orgs: parseCsv(orgsCsv) },
      });
      addToast("Credential pool updated.", "success");
      void queryClient.invalidateQueries({ queryKey: ["team-pool", teamID] });
    } catch (e) {
      setErr(errorMessage(e));
    } finally {
      setBusy(false);
    }
  };

  if (query.isLoading) return <PanelLoading />;
  if (query.error && !query.isFetching) {
    return (
      <InlineBanner tone="danger" layout="inline">
        {errorMessage(query.error)}
      </InlineBanner>
    );
  }

  const donors = pool?.donors ?? [];

  return (
    <div className="space-y-4">
      <div>
        <h2 className="text-lg font-semibold">Credential pool</h2>
        <p className="text-sm text-fg-muted mt-1">
          Contributors can lend their Claude or ChatGPT subscription to this org. A run with no
          credential of its own then draws on the least-used available contribution, capped by the
          ceilings that contributor set. Each donor keeps their own kill switch.
        </p>
      </div>

      <InlineBanner tone="warning" layout="inline">
        Widening the audience below lets more runs spend contributors&apos; personal
        subscriptions. Those are individual licences — keep the pool to work the contributors
        signed up for, and prefer API keys for production automation.
      </InlineBanner>

      {err && (
        <InlineBanner tone="danger" layout="inline">
          {err}
        </InlineBanner>
      )}

      <section className="bg-surface-1 border border-border-subtle rounded p-4 space-y-3">
        <Checkbox
          label="Pool enabled"
          help="Master switch, independent of each contributor's own. Off = no run ever draws on the pool."
          checked={enabled}
          disabled={!canManage}
          onChange={(e) => setEnabled(e.target.checked)}
        />

        <div className="space-y-2">
          <h3 className="text-sm font-medium">Who may draw on it</h3>
          <p className="text-xs text-fg-muted">
            These are independent options, combined as a union. With none of them set, only teams
            of this org may draw — the strictest setting, and the default.
          </p>
          <Checkbox
            label="Contributors may draw (reciprocity)"
            help="Anyone actively lending to this pool may use it, whichever team they launch from. Pausing your own sharing stops your borrowing too."
            checked={audience.contributors ?? false}
            disabled={!canManage}
            onChange={(e) => setAudience({ ...audience, contributors: e.target.checked })}
          />
          <Checkbox
            label="Every team on this instance may draw"
            help="The widest setting. Only meaningful for an instance that IS a shared community deployment."
            checked={audience.all_teams ?? false}
            disabled={!canManage}
            onChange={(e) => setAudience({ ...audience, all_teams: e.target.checked })}
          />
          <div className="grid grid-cols-1 sm:grid-cols-2 gap-3">
            <div>
              <FieldLabel help="Comma-separated team ids allowed to draw, on top of this org's own teams.">
                Extra team ids
              </FieldLabel>
              <Input
                value={teamsCsv}
                disabled={!canManage}
                placeholder="team-abc, team-def"
                onChange={(e) => setTeamsCsv(e.target.value)}
              />
            </div>
            <div>
              <FieldLabel help="Comma-separated org ids whose every team may draw.">
                Extra org ids
              </FieldLabel>
              <Input
                value={orgsCsv}
                disabled={!canManage}
                placeholder="org-xyz"
                onChange={(e) => setOrgsCsv(e.target.value)}
              />
            </div>
          </div>
        </div>

        {canManage && (
          <Button variant="primary" loading={busy} onClick={() => void save()}>
            Save pool settings
          </Button>
        )}
      </section>

      <section className="space-y-2">
        <h3 className="text-sm font-medium">Contributors</h3>
        {donors.length === 0 ? (
          <EmptyState
            title="No contributions yet"
            message="A contributor lends their subscription from Account settings → Share my quota."
          />
        ) : (
          <div className="border border-border-subtle rounded">
            <Table caption="Contributors lending to this pool" density="sm">
              <THead className="bg-surface-1">
                <Th>Contributor</Th>
                <Th>Subscription</Th>
                <Th>State</Th>
                <Th align="right">Runs today</Th>
                <Th align="right">Given today (est.)</Th>
                <Th>Last served</Th>
              </THead>
              <TBody>
                {donors.map((d) => (
                  <Tr key={`${d.user_id}-${d.kind}`}>
                    <Td>{d.user_id}</Td>
                    <Td>{d.kind}</Td>
                    <Td>
                      <Badge variant={STATUS_TONE[d.status] ?? "neutral"}>{d.status}</Badge>
                      {d.cooldown_until && (
                        <span className="text-fg-muted ml-2">
                          until {formatDateTime(d.cooldown_until)}
                        </span>
                      )}
                    </Td>
                    <Td align="right">{d.today_runs}</Td>
                    <Td align="right">{formatCost(d.today_cost_usd)}</Td>
                    <Td>{d.last_served_at ? formatDateTime(d.last_served_at) : "—"}</Td>
                  </Tr>
                ))}
              </TBody>
            </Table>
          </div>
        )}
      </section>
    </div>
  );
}
