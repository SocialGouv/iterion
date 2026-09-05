import { useState } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";

import { Button } from "@/components/ui/Button";
import { Input } from "@/components/ui/Input";
import { Select } from "@/components/ui/Select";
import { InlineBanner } from "@/components/ui/InlineBanner";
import { Spinner } from "@/components/ui/Spinner";
import { errorMessage } from "@/lib/errorHints";
import { useConfirm } from "@/hooks/useConfirm";
import { listForgeConnections } from "@/api/forgeConnections";
import {
  bindBoard,
  formatStatusMap,
  getBoardBinding,
  parseStatusMap,
  unbindBoard,
  type BoardBinding,
} from "@/api/boardBinding";

// The team's project board (GitHub Projects v2, ADR-097). Binding one makes
// the board's Status column two-way with the native board's columns and
// imports Area/Mode/Priority onto cards as area:/mode:/prio: labels.
//
// The card renders the EFFECTIVE status map, not the default: what a
// deployment actually runs has to be readable here, not inferred from the
// absence of an override.

type FormField = "project" | "ownerKind" | "connection" | "statusMap" | "syncEvery";

/** SYNC_CHOICES are the intervals the card offers; the API accepts any ≥60s. */
const SYNC_CHOICES: Array<{ value: string; label: string }> = [
  { value: "60", label: "every minute" },
  { value: "120", label: "every 2 minutes (default)" },
  { value: "300", label: "every 5 minutes" },
  { value: "900", label: "every 15 minutes" },
  { value: "0", label: "off — no reconciliation" },
];

export default function ProjectBoardTab({
  teamID,
  canManage,
}: {
  teamID: string;
  canManage: boolean;
}) {
  const qc = useQueryClient();
  const { confirm, dialog } = useConfirm();
  const [err, setErr] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  const { data: binding, isLoading } = useQuery({
    queryKey: ["board-binding", teamID],
    queryFn: () => getBoardBinding(teamID),
  });
  const { data: connections = [] } = useQuery({
    queryKey: ["forge-connections", teamID],
    queryFn: () => listForgeConnections(teamID),
  });

  // The form shows what is BOUND until the operator edits a field: each value
  // is the edit if there is one, otherwise derived from the binding. Seeding
  // state from the query in an effect instead would wipe a half-typed form on
  // any background refetch, and flash blanks before the first load resolves.
  const [edits, setEdits] = useState<Partial<Record<FormField, string>>>({});
  const bound: Record<FormField, string> = {
    project: binding ? `${binding.owner}/${binding.number}` : "",
    ownerKind: binding?.owner_kind || "org",
    connection: binding?.connection_id ?? "",
    statusMap: formatStatusMap(binding ?? null),
    syncEvery: String(binding?.sync_every_seconds ?? 120),
  };
  const field = (k: FormField) => edits[k] ?? bound[k];
  const setField = (k: FormField) => (v: string) => setEdits((e) => ({ ...e, [k]: v }));

  const project = field("project");
  const ownerKind = field("ownerKind");
  const connection = field("connection");
  const statusMap = field("statusMap");
  const syncEvery = field("syncEvery");

  async function submit(e: React.FormEvent) {
    e.preventDefault();
    setErr(null);
    const [owner, num] = project.split("/");
    const number = Number(num);
    if (!owner?.trim() || !Number.isInteger(number) || number <= 0) {
      setErr("The board is <owner>/<number> — e.g. SocialGouv/203.");
      return;
    }
    const { map, error } = parseStatusMap(statusMap);
    if (error) {
      setErr(error);
      return;
    }
    setBusy(true);
    try {
      await bindBoard(teamID, {
        owner: owner.trim(),
        number,
        owner_kind: ownerKind,
        connection_id: connection,
        ...(map ? { status_map: map } : {}),
        sync_every_seconds: Number(syncEvery),
      });
      await qc.invalidateQueries({ queryKey: ["board-binding", teamID] });
      setEdits({});
    } catch (e) {
      setErr(errorMessage(e));
    } finally {
      setBusy(false);
    }
  }

  async function unbind() {
    if (
      !(await confirm({
        title: "Unbind the project board?",
        message:
          "Cards keep their labels and columns; they simply stop syncing with the board.",
        confirmLabel: "Unbind",
        confirmVariant: "danger",
      }))
    ) {
      return;
    }
    setErr(null);
    setBusy(true);
    try {
      await unbindBoard(teamID);
      await qc.invalidateQueries({ queryKey: ["board-binding", teamID] });
      setEdits({});
    } catch (e) {
      setErr(errorMessage(e));
    } finally {
      setBusy(false);
    }
  }

  return (
    <div className="space-y-4">
      {dialog}
      <div>
        <h3 className="font-medium">Project board</h3>
        <p className="text-xs text-fg-muted">
          Bind a GitHub Projects v2 board and its <code>Status</code> column becomes two-way
          with this team's kanban columns; <code>Area</code>, <code>Mode</code> and{" "}
          <code>Priority</code> land on cards as <code>area:</code> / <code>mode:</code> /{" "}
          <code>prio:</code> labels. Cards themselves come from the repositories' issue sync —
          the board hydrates them, it never creates them.
        </p>
      </div>

      {err && (
        <InlineBanner tone="danger" layout="inline">
          {err}
        </InlineBanner>
      )}

      {isLoading ? (
        <Spinner label="Loading the project-board binding" />
      ) : (
        <>
          {binding && <BindingSummary binding={binding} />}

          {canManage ? (
            <form
              onSubmit={submit}
              className="bg-surface-1 border border-border-subtle rounded-[var(--radius-lg)] shadow-[var(--shadow-sm)] p-4 space-y-3"
            >
              <h4 className="font-medium text-sm">{binding ? "Change binding" : "Bind a board"}</h4>

              <div className="grid gap-3 sm:grid-cols-2">
                <label className="block text-sm">
                  <span className="text-fg-muted text-xs">Board</span>
                  <Input
                    size="md"
                    value={project}
                    onChange={(e) => setField("project")(e.target.value)}
                    placeholder="SocialGouv/203"
                    required
                  />
                </label>

                <label className="block text-sm">
                  <span className="text-fg-muted text-xs">Owner kind</span>
                  <Select
                    size="md"
                    value={ownerKind}
                    onChange={(e) => setField("ownerKind")(e.target.value)}
                  >
                    <option value="org">Organization</option>
                    <option value="user">User</option>
                  </Select>
                </label>

                <label className="block text-sm">
                  <span className="text-fg-muted text-xs">Forge connection</span>
                  <Select
                    size="md"
                    value={connection}
                    onChange={(e) => setField("connection")(e.target.value)}
                    required
                  >
                    <option value="">Select a connection…</option>
                    {connections.map((c) => (
                      <option key={c.id} value={c.id}>
                        {c.provider} — {c.account_login || c.id}
                      </option>
                    ))}
                  </Select>
                </label>

                <label className="block text-sm">
                  <span className="text-fg-muted text-xs">Reconcile</span>
                  <Select size="md" value={syncEvery} onChange={(e) => setField("syncEvery")(e.target.value)}>
                    {SYNC_CHOICES.map((c) => (
                      <option key={c.value} value={c.value}>
                        {c.label}
                      </option>
                    ))}
                  </Select>
                </label>
              </div>

              <label className="block text-sm">
                <span className="text-fg-muted text-xs">
                  Status map (optional — leave empty for Inbox/Planned/In progress/Blocked/Done)
                </span>
                <Input
                  size="md"
                  value={statusMap}
                  onChange={(e) => setField("statusMap")(e.target.value)}
                  placeholder="Todo=ready,Doing=in_progress,Shipped=done"
                />
              </label>

              <div className="flex gap-2">
                <Button variant="primary" type="submit" loading={busy}>
                  {binding ? "Re-bind" : "Bind board"}
                </Button>
                {binding && (
                  <Button variant="ghost" type="button" onClick={unbind} disabled={busy}>
                    Unbind
                  </Button>
                )}
              </div>

              <p className="text-caption text-fg-subtle">
                Column and option ids are discovered by name when you bind, so nothing is
                hardcoded. The credential needs Projects read &amp; write — a GitHub App's
                organization grant has to be approved by an org owner.
              </p>
            </form>
          ) : (
            !binding && (
              <InlineBanner tone="info" layout="inline">
                No project board is bound. A team admin can bind one.
              </InlineBanner>
            )
          )}
        </>
      )}
    </div>
  );
}

/**
 * BindingSummary renders what is actually bound: the board, the cadence, and
 * the EFFECTIVE column map with the columns the board does not carry flagged —
 * a mapped-but-absent column is silently inert, so it has to be visible here.
 */
function BindingSummary({ binding }: { binding: BoardBinding }) {
  const missing = new Set(binding.missing_statuses ?? []);
  const sync = binding.sync_every_seconds ?? 0;
  return (
    <section className="bg-surface-1 border border-border-subtle rounded-[var(--radius-lg)] shadow-[var(--shadow-sm)] p-4 space-y-3">
      <div className="flex items-baseline justify-between gap-2 flex-wrap">
        <h4 className="font-medium text-sm">
          {binding.project_url ? (
            <a
              href={binding.project_url}
              target="_blank"
              rel="noreferrer noopener"
              className="underline"
            >
              {binding.project_title || `${binding.owner}/${binding.number}`}
            </a>
          ) : (
            binding.project_title || `${binding.owner}/${binding.number}`
          )}
        </h4>
        <span className="text-xs text-fg-muted">
          {sync > 0 ? `reconciles every ${sync}s` : "reconciliation off"}
        </span>
      </div>

      {!binding.status_field_id && (
        <InlineBanner tone="warning" layout="inline">
          This board has no <code>Status</code> field — labels are imported, but there is no
          status projection in either direction.
        </InlineBanner>
      )}

      {binding.status_mapping?.length ? (
        <div>
          <p className="text-xs text-fg-muted mb-1">Status map</p>
          <ul className="text-sm space-y-0.5">
            {binding.status_mapping.map((m) => (
              <li key={m.status} className="flex gap-2 items-baseline">
                <span className={missing.has(m.status ?? "") ? "text-fg-subtle line-through" : ""}>
                  {m.status}
                </span>
                <span className="text-fg-subtle">→</span>
                <code className="text-xs">{m.state}</code>
                {missing.has(m.status ?? "") && (
                  <span className="text-caption text-warning">not on this board</span>
                )}
              </li>
            ))}
          </ul>
        </div>
      ) : null}

      {binding.label_fields?.length ? (
        <div>
          <p className="text-xs text-fg-muted mb-1">Imported as labels</p>
          <ul className="text-sm space-y-0.5">
            {binding.label_fields.map((f) => (
              <li key={f.field_id}>
                {f.name} → <code className="text-xs">{f.prefix}&lt;value&gt;</code>
              </li>
            ))}
          </ul>
        </div>
      ) : null}
    </section>
  );
}
