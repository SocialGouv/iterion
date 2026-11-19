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
import { Select } from "@/components/ui/Select";
import { TBody, THead, Table, Td, Th, Tr } from "@/components/ui/Table";
import PanelLoading from "@/components/shared/PanelLoading";
import { useUIStore } from "@/store/ui";
import { useConfirm } from "@/hooks/useConfirm";
import {
  createConnection,
  credentialFields,
  credentialShapeOf,
  deleteConnection,
  listConnections,
  revokeConnection,
  type ConnectorConnection,
} from "@/api/connections";
import { listTeamSources } from "@/api/sources";

const STATUS_VARIANT: Record<string, "success" | "warning" | "neutral" | "danger"> = {
  active: "success",
  degraded: "warning",
  revoked: "danger",
};

// The capability set the server admits (pkg/connection.CapabilityNames). `action`
// is the default server-side; `agent` lets a model choose calls through the MCP
// facade; `trigger` lets the connection feed a source — a connector ingress
// believes a delivery on it, and a subscription to its package's triggers
// (pushed or polled) requires it.
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
  const [username, setUsername] = useState("");
  const [password, setPassword] = useState("");
  const [caps, setCaps] = useState<string[]>(["action"]);
  const [err, setErr] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  // The source catalog names a package's auth schemes and their kinds, so
  // the credential fields follow the scheme: none for a public resource,
  // username + password for HTTP basic, a token otherwise. It lists the
  // packages that declare triggers; another connector keeps the free scheme
  // input and the token field, and the server judges the shape at the paste.
  const sources = useQuery({
    queryKey: ["team-sources", teamID],
    queryFn: () => listTeamSources(teamID),
  });
  const card = sources.data?.find((s) => s.kind === "connector" && s.id === connector.trim());
  const schemes = card?.auth ?? [];
  const effectiveScheme = schemeID || (schemes.length === 1 ? (schemes[0]?.id ?? "") : "");
  const scheme = schemes.find((a) => a.id === effectiveScheme);
  // A package declaring several schemes asks for no credential before one
  // is chosen: the fields depend on it.
  const shape = schemes.length > 0 && !scheme ? null : credentialShapeOf(scheme?.kind);
  // A polled trigger reads through the connection (action) and delivers what
  // it reads (trigger): the package that declares one needs both granted.
  const polled = (card?.triggers ?? []).filter((t) => t.delivery === "poll");
  const pollGrantsMissing =
    polled.length > 0 && !(caps.includes("action") && caps.includes("trigger"));

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
        ...(shape ? credentialFields(shape, { token, username, password }) : {}),
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
            onChange={(e) => {
              // A scheme belongs to its package: another connector starts
              // from its own.
              setConnector(e.target.value);
              setSchemeID("");
            }}
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
          {schemes.length > 0 ? (
            <Select
              id="conn-scheme"
              value={effectiveScheme}
              onChange={(e) => setSchemeID(e.target.value)}
              required
            >
              {schemes.length > 1 && <option value="">— choose a scheme —</option>}
              {schemes.map((a) => (
                <option key={a.id} value={a.id}>
                  {a.display_name ? `${a.display_name} (${a.kind})` : `${a.id} (${a.kind})`}
                </option>
              ))}
            </Select>
          ) : (
            <Input
              id="conn-scheme"
              value={schemeID}
              onChange={(e) => setSchemeID(e.target.value)}
              placeholder="token"
            />
          )}
          {sources.isError && (
            <p className="mt-1 text-xs text-fg-muted">
              The connector catalog did not load ({errorMessage(sources.error)}): the
              credential field cannot follow the scheme&apos;s kind, so it asks for a token.
            </p>
          )}
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
        {shape === "none" && (
          <p className="text-xs text-fg-muted self-end">
            This scheme presents no credential — a public resource. The connection still pins
            its instance URL and states what it may be used for.
          </p>
        )}
        {shape === "basic" && (
          <>
            <div>
              <FieldLabel htmlFor="conn-username">Username</FieldLabel>
              <Input
                id="conn-username"
                value={username}
                onChange={(e) => setUsername(e.target.value)}
                autoComplete="off"
                required
              />
            </div>
            <div>
              <FieldLabel htmlFor="conn-password">Password</FieldLabel>
              <Input
                id="conn-password"
                type="password"
                value={password}
                onChange={(e) => setPassword(e.target.value)}
                autoComplete="new-password"
                required
              />
            </div>
          </>
        )}
        {shape === "token" && (
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
        )}
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
        {pollGrantsMissing && (
          <p className="mt-1 text-xs text-warning-fg">
            {card?.display_name ?? connector} declares polled triggers (
            {polled.map((t) => t.display_name || t.id).join(", ")}): polling reads through the
            connection and delivers what it reads, so grant both action and trigger.
          </p>
        )}
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
