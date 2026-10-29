import { useState } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";

import { errorMessage } from "@/lib/errorHints";
import { formatDateTime } from "@/lib/format";
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
import { useConfirm } from "@/hooks/useConfirm";
import {
  createConnection,
  deleteConnection,
  listConnections,
  revokeConnection,
  type ConnectorConnection,
} from "@/api/connections";

const STATUS_VARIANT: Record<string, "success" | "warning" | "neutral" | "danger"> = {
  active: "success",
  degraded: "warning",
  revoked: "danger",
};

// The capability set the server admits (pkg/connection.CapabilityNames). `action`
// is the default server-side; `agent` lets a model choose calls through the MCP
// facade; `trigger` is stored but inert until the webhook ingress lands — the
// server's `warnings` say so on every row that grants it.
const CAPABILITIES = ["action", "agent", "trigger"] as const;

// ConnectionsTab is the tenant's connector connections (ADR-098, #2759): the
// records a `.bot`'s `connection: <alias>` and the agent facade resolve. The
// credential is write-only — the form sends it, nothing ever reads it back.
export default function ConnectionsTab({
  teamID,
  canManage,
}: {
  teamID: string;
  canManage: boolean;
}) {
  const queryClient = useQueryClient();
  const query = useQuery<ConnectorConnection[]>({
    queryKey: ["team-connections", teamID],
    queryFn: () => listConnections(teamID),
  });
  const addToast = useUIStore((s) => s.addToast);
  const { confirm, dialog } = useConfirm();
  const [showForm, setShowForm] = useState(false);
  const [err, setErr] = useState<string | null>(null);
  const [busy, setBusy] = useState<string | null>(null);

  const refresh = () => void queryClient.invalidateQueries({ queryKey: ["team-connections", teamID] });

  const revoke = async (c: ConnectorConnection) => {
    setBusy(c.id);
    setErr(null);
    try {
      await revokeConnection(teamID, c.id, "revoked from the studio");
      addToast(`Connection ${c.alias} revoked.`, "success");
      refresh();
    } catch (e) {
      setErr(errorMessage(e));
    } finally {
      setBusy(null);
    }
  };

  const remove = async (c: ConnectorConnection) => {
    if (
      !(await confirm({
        title: `Delete "${c.alias}"`,
        message: `Delete the ${c.connector} connection "${c.alias}"? Workflows naming it will fail to resolve it. This cannot be undone.`,
        confirmLabel: "Delete",
        confirmVariant: "danger",
      }))
    ) {
      return;
    }
    setBusy(c.id);
    setErr(null);
    try {
      await deleteConnection(teamID, c.id);
      addToast(`Connection ${c.alias} deleted.`, "success");
      refresh();
    } catch (e) {
      setErr(errorMessage(e));
    } finally {
      setBusy(null);
    }
  };

  return (
    <section className="grid gap-4">
      <div className="flex items-center justify-between">
        <p className="text-sm text-fg-muted">
          Connector connections bind a credential to a connector package — what a workflow's{" "}
          <code>connection:</code> alias and the agent facade (<code>capabilities: [connectors]</code>)
          resolve. The credential is sealed at the server and never returned.
        </p>
        {canManage && (
          <Button variant="primary" onClick={() => setShowForm((v) => !v)}>
            {showForm ? "Close" : "Add a connection"}
          </Button>
        )}
      </div>

      {err && (
        <InlineBanner tone="danger" layout="inline">
          {err}
        </InlineBanner>
      )}

      {showForm && canManage && (
        <AddConnectionForm
          teamID={teamID}
          onDone={() => {
            setShowForm(false);
            refresh();
          }}
        />
      )}

      {query.isPending ? (
        <PanelLoading />
      ) : (query.data ?? []).length === 0 ? (
        <EmptyState
          title="No connections yet"
          message="A connection authenticates one instance of one connector for this team — e.g. your Forgejo. Add one to let workflows and agents call it."
        />
      ) : (
        <Table caption="Connector connections">
          <THead>
            <Tr>
              <Th>Alias</Th>
              <Th>Connector</Th>
              <Th>Instance</Th>
              <Th>Capabilities</Th>
              <Th>Status</Th>
              <Th>Created</Th>
              {canManage && <Th aria-label="Actions" />}
            </Tr>
          </THead>
          <TBody>
            {(query.data ?? []).map((c) => (
              <Tr key={c.id}>
                <Td>
                  <span className="font-medium">{c.alias}</span>
                  {c.display_name && <span className="text-fg-muted"> — {c.display_name}</span>}
                  {(c.warnings ?? []).map((w) => (
                    <div key={w} className="text-xs text-warning-fg">
                      {w}
                    </div>
                  ))}
                </Td>
                <Td>{c.connector}</Td>
                <Td className="text-fg-muted">{c.base_url}</Td>
                <Td>{(c.capabilities ?? []).join(" + ")}</Td>
                <Td>
                  <Badge variant={STATUS_VARIANT[c.status] ?? "neutral"}>{c.status}</Badge>
                  {c.status_reason && <div className="text-xs text-fg-muted">{c.status_reason}</div>}
                </Td>
                <Td className="text-fg-muted">{formatDateTime(c.created_at)}</Td>
                {canManage && (
                  <Td>
                    <div className="flex gap-2 justify-end">
                      {c.status !== "revoked" && (
                        <Button
                          variant="ghost"
                          disabled={busy === c.id}
                          onClick={() => void revoke(c)}
                        >
                          Revoke
                        </Button>
                      )}
                      <Button
                        variant="ghost"
                        disabled={busy === c.id}
                        onClick={() => void remove(c)}
                      >
                        Delete
                      </Button>
                    </div>
                  </Td>
                )}
              </Tr>
            ))}
          </TBody>
        </Table>
      )}
      {dialog}
    </section>
  );
}

function AddConnectionForm({ teamID, onDone }: { teamID: string; onDone: () => void }) {
  const addToast = useUIStore((s) => s.addToast);
  const [connector, setConnector] = useState("forgejo");
  const [alias, setAlias] = useState("main");
  const [baseURL, setBaseURL] = useState("");
  const [schemeID, setSchemeID] = useState("");
  const [displayName, setDisplayName] = useState("");
  const [token, setToken] = useState("");
  const [caps, setCaps] = useState<string[]>(["action"]);
  const [err, setErr] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  const submit = async () => {
    setBusy(true);
    setErr(null);
    try {
      const created = await createConnection(teamID, {
        connector: connector.trim(),
        alias: alias.trim() || undefined,
        base_url: baseURL.trim() || undefined,
        scheme_id: schemeID.trim() || undefined,
        display_name: displayName.trim() || undefined,
        capabilities: caps,
        token,
      });
      addToast(`Connected ${created.connector} as "${created.alias}".`, "success");
      (created.warnings ?? []).forEach((w) => addToast(w, "warning"));
      onDone();
    } catch (e) {
      setErr(errorMessage(e));
    } finally {
      setBusy(false);
    }
  };

  return (
    <form
      className="grid gap-3 rounded-md border border-border-default p-4"
      onSubmit={(e) => {
        e.preventDefault();
        void submit();
      }}
    >
      <div className="grid grid-cols-1 sm:grid-cols-2 gap-3">
        <div>
          <FieldLabel htmlFor="conn-connector">Connector</FieldLabel>
          <Input
            id="conn-connector"
            value={connector}
            onChange={(e) => setConnector(e.target.value)}
            placeholder="forgejo"
            required
          />
        </div>
        <div>
          <FieldLabel htmlFor="conn-alias">Alias</FieldLabel>
          <Input
            id="conn-alias"
            value={alias}
            onChange={(e) => setAlias(e.target.value)}
            placeholder="main"
          />
        </div>
        <div>
          <FieldLabel htmlFor="conn-base-url">Instance URL</FieldLabel>
          <Input
            id="conn-base-url"
            value={baseURL}
            onChange={(e) => setBaseURL(e.target.value)}
            placeholder="https://git.example.com"
          />
        </div>
        <div>
          <FieldLabel htmlFor="conn-scheme">Auth scheme (when the package declares several)</FieldLabel>
          <Input
            id="conn-scheme"
            value={schemeID}
            onChange={(e) => setSchemeID(e.target.value)}
            placeholder="token"
          />
        </div>
        <div>
          <FieldLabel htmlFor="conn-name">Display name</FieldLabel>
          <Input
            id="conn-name"
            value={displayName}
            onChange={(e) => setDisplayName(e.target.value)}
            placeholder="Production Forgejo"
          />
        </div>
        <div>
          <FieldLabel htmlFor="conn-token">Credential</FieldLabel>
          <Input
            id="conn-token"
            type="password"
            value={token}
            onChange={(e) => setToken(e.target.value)}
            autoComplete="new-password"
            required
          />
        </div>
      </div>
      <fieldset>
        <legend className="text-sm text-fg-muted">May be used for</legend>
        <div className="flex gap-4">
          {CAPABILITIES.map((cap) => (
            <Checkbox
              key={cap}
              checked={caps.includes(cap)}
              onChange={(e) =>
                setCaps((cur) =>
                  e.target.checked ? [...cur, cap] : cur.filter((c) => c !== cap),
                )
              }
              label={cap}
            />
          ))}
        </div>
      </fieldset>
      {err && (
        <InlineBanner tone="danger" layout="inline">
          {err}
        </InlineBanner>
      )}
      <div>
        <Button variant="primary" type="submit" disabled={busy || caps.length === 0}>
          {busy ? "Connecting…" : "Connect"}
        </Button>
      </div>
    </form>
  );
}
