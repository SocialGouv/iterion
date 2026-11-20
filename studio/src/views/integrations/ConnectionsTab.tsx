import { useState } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { Link, useLocation, useSearch } from "wouter";

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
import { sanitizeReturnTo } from "./wizard/bindModel";

const STATUS_VARIANT: Record<string, "success" | "warning" | "neutral" | "danger"> = {
  active: "success",
  degraded: "warning",
  revoked: "danger",
};

// The status a connection is in, as the operator reads it; a status this map
// does not know is shown as the server names it.
const STATUS_LABEL: Record<string, string> = {
  active: "active",
  degraded: "dégradée",
  revoked: "révoquée",
};

// The select's "another connector" choice. A connector id never starts with a
// space (the server trims it), so no catalogue entry can take this value.
const OTHER_CONNECTOR = " other";

// The capability set the server admits (pkg/connection.CapabilityNames). `action`
// is the default server-side; `agent` lets a model choose calls through the MCP
// facade; `trigger` lets the connection feed a source — a connector ingress
// believes a delivery on it, and a subscription to its package's triggers
// (pushed or polled) requires it.
const CAPABILITIES = ["action", "agent", "trigger"] as const;

// ConnectionsTab is the tenant's connector connections (ADR-098, #2759): the
// records a `.bot`'s `connection: <alias>` and the agent facade resolve. The
// credential is write-only — the form sends it, nothing ever reads it back.
//
// A surface that needs a connection deep-links here (connectorsTabPath):
// `connector` opens the add form on that connector, and `returnTo` — an in-app
// path, admitted only through sanitizeReturnTo — is where the operator goes
// back to, offered at once and taken as soon as the connection exists.
export default function ConnectionsTab({
  teamID,
  canManage,
}: {
  teamID: string;
  canManage: boolean;
}) {
  const queryClient = useQueryClient();
  const params = new URLSearchParams(useSearch());
  const requestedConnector = (params.get("connector") ?? "").trim();
  const returnTo = sanitizeReturnTo(params.get("returnTo"));
  const [, navigate] = useLocation();
  const query = useQuery<ConnectorConnection[]>({
    queryKey: ["team-connections", teamID],
    queryFn: () => listConnections(teamID),
  });
  const addToast = useUIStore((s) => s.addToast);
  const { confirm, dialog } = useConfirm();
  const [showForm, setShowForm] = useState(requestedConnector !== "");
  const [err, setErr] = useState<string | null>(null);
  const [busy, setBusy] = useState<string | null>(null);

  const refresh = () => void queryClient.invalidateQueries({ queryKey: ["team-connections", teamID] });

  const revoke = async (c: ConnectorConnection) => {
    setBusy(c.id);
    setErr(null);
    try {
      await revokeConnection(teamID, c.id, "revoked from the studio");
      addToast(`Connexion ${c.alias} révoquée.`, "success");
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
        title: `Supprimer « ${c.alias} »`,
        message: `Supprimer la connexion ${c.connector} « ${c.alias} » ? Les workflows qui la nomment ne pourront plus la résoudre. Cette action est définitive.`,
        confirmLabel: "Supprimer",
        confirmVariant: "danger",
      }))
    ) {
      return;
    }
    setBusy(c.id);
    setErr(null);
    try {
      await deleteConnection(teamID, c.id);
      addToast(`Connexion ${c.alias} supprimée.`, "success");
      refresh();
    } catch (e) {
      setErr(errorMessage(e));
    } finally {
      setBusy(null);
    }
  };

  return (
    <section className="grid gap-4">
      {returnTo && (
        <div>
          <Link href={returnTo} className="text-sm text-accent-text underline underline-offset-2">
            ← {returnTo.startsWith("/sources/plug") ? "Revenir au branchement de la source" : "Revenir"}
          </Link>
        </div>
      )}
      <div className="flex items-center justify-between">
        <p className="text-sm text-fg-muted">
          Une connexion lie un secret d&apos;accès à un paquet de connecteur : c&apos;est ce que
          résolvent l&apos;alias <code>connection:</code> d&apos;un workflow et la façade des agents
          (<code>capabilities: [connectors]</code>). Le secret est scellé côté serveur et n&apos;est
          jamais renvoyé.
        </p>
        {canManage && (
          <Button variant="primary" onClick={() => setShowForm((v) => !v)}>
            {showForm ? "Fermer" : "Ajouter une connexion"}
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
          initialConnector={requestedConnector}
          onDone={() => {
            setShowForm(false);
            refresh();
            // The surface that sent the operator here lists connections under
            // its own key: it reads the new one on the way back.
            void queryClient.invalidateQueries({ queryKey: ["connections", teamID] });
            if (returnTo) navigate(returnTo);
          }}
        />
      )}

      {query.isPending ? (
        <PanelLoading />
      ) : (query.data ?? []).length === 0 ? (
        <EmptyState
          title="Aucune connexion pour l'instant"
          message="Une connexion authentifie une instance d'un connecteur pour cette équipe — par exemple votre Forgejo. Ajoutez-en une pour que les workflows et les agents puissent l'appeler."
        />
      ) : (
        <Table caption="Connexions aux connecteurs">
          <THead>
            <Tr>
              <Th>Alias</Th>
              <Th>Connecteur</Th>
              <Th>Instance</Th>
              <Th>Usages autorisés</Th>
              <Th>État</Th>
              <Th>Créée le</Th>
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
                  <Badge variant={STATUS_VARIANT[c.status] ?? "neutral"}>
                    {STATUS_LABEL[c.status] ?? c.status}
                  </Badge>
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
                          Révoquer
                        </Button>
                      )}
                      <Button
                        variant="ghost"
                        disabled={busy === c.id}
                        onClick={() => void remove(c)}
                      >
                        Supprimer
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

function AddConnectionForm({
  teamID,
  initialConnector,
  onDone,
}: {
  teamID: string;
  initialConnector: string;
  onDone: () => void;
}) {
  const addToast = useUIStore((s) => s.addToast);
  const [connector, setConnector] = useState(initialConnector);
  const [otherConnector, setOtherConnector] = useState(false);
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
  const catalogue = (sources.data ?? []).filter((s) => s.kind === "connector");
  const card = catalogue.find((s) => s.id === connector.trim());
  // The connector is picked from the catalogue. One it does not list — it
  // lists the packages that declare triggers, and an outbound-only package is
  // connected all the same — is typed by id, as every connector is while the
  // catalogue cannot load.
  const typedConnector =
    !sources.isError && (otherConnector || (sources.isSuccess && connector.trim() !== "" && !card));
  const pickConnector = (id: string) => {
    // A scheme belongs to its package: another connector starts from its own.
    setConnector(id);
    setSchemeID("");
  };
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
      addToast(`${created.connector} connecté sous l'alias « ${created.alias} ».`, "success");
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
          <FieldLabel htmlFor="conn-connector">Connecteur</FieldLabel>
          {sources.isError ? (
            <Input
              id="conn-connector"
              value={connector}
              onChange={(e) => pickConnector(e.target.value)}
              placeholder="identifiant du paquet, ex. jira"
              required
            />
          ) : (
            <Select
              id="conn-connector"
              value={typedConnector ? OTHER_CONNECTOR : connector.trim()}
              disabled={sources.isPending}
              onChange={(e) => {
                const other = e.target.value === OTHER_CONNECTOR;
                setOtherConnector(other);
                pickConnector(other ? "" : e.target.value);
              }}
              required
            >
              {sources.isPending ? (
                <option value={connector.trim()}>Chargement du catalogue…</option>
              ) : (
                <>
                  {!typedConnector && connector.trim() === "" && (
                    <option value="">— choisir un connecteur —</option>
                  )}
                  {catalogue.map((s) => (
                    <option key={s.id} value={s.id}>
                      {s.display_name} ({s.id})
                    </option>
                  ))}
                  <option value={OTHER_CONNECTOR}>Autre connecteur, par identifiant…</option>
                </>
              )}
            </Select>
          )}
          {typedConnector && (
            <Input
              className="mt-1"
              aria-label="Identifiant du connecteur"
              value={connector}
              onChange={(e) => pickConnector(e.target.value)}
              placeholder="identifiant du paquet"
              required
            />
          )}
          {sources.isError && (
            <p className="mt-1 text-xs text-warning-fg">
              Le catalogue des connecteurs n&apos;a pas pu être chargé ({errorMessage(sources.error)}) :
              saisissez l&apos;identifiant du connecteur. Sans le catalogue, le champ du secret ne peut
              pas suivre la méthode d&apos;authentification : il demande un jeton.
            </p>
          )}
        </div>
        <div>
          <FieldLabel htmlFor="conn-alias">Alias</FieldLabel>
          <Input
            id="conn-alias"
            value={alias}
            onChange={(e) => setAlias(e.target.value)}
            placeholder="main (par défaut)"
          />
        </div>
        <div>
          <FieldLabel htmlFor="conn-base-url">URL de l&apos;instance</FieldLabel>
          <Input
            id="conn-base-url"
            value={baseURL}
            onChange={(e) => setBaseURL(e.target.value)}
            placeholder="https://git.example.com"
          />
        </div>
        <div>
          <FieldLabel htmlFor="conn-scheme">
            Méthode d&apos;authentification (si le paquet en déclare plusieurs)
          </FieldLabel>
          {schemes.length > 0 ? (
            <Select
              id="conn-scheme"
              value={effectiveScheme}
              onChange={(e) => setSchemeID(e.target.value)}
              required
            >
              {schemes.length > 1 && <option value="">— choisir une méthode —</option>}
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
              placeholder="identifiant de la méthode, ex. token"
            />
          )}
        </div>
        <div>
          <FieldLabel htmlFor="conn-name">Nom affiché</FieldLabel>
          <Input
            id="conn-name"
            value={displayName}
            onChange={(e) => setDisplayName(e.target.value)}
            placeholder="Forgejo de production"
          />
        </div>
        {shape === "none" && (
          <p className="text-xs text-fg-muted self-end">
            Cette méthode ne présente aucun secret : une ressource publique. La connexion fixe tout
            de même l&apos;URL de son instance et ce à quoi elle peut servir.
          </p>
        )}
        {shape === "basic" && (
          <>
            <div>
              <FieldLabel htmlFor="conn-username">Nom d&apos;utilisateur</FieldLabel>
              <Input
                id="conn-username"
                value={username}
                onChange={(e) => setUsername(e.target.value)}
                autoComplete="off"
                required
              />
            </div>
            <div>
              <FieldLabel htmlFor="conn-password">Mot de passe</FieldLabel>
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
            <FieldLabel htmlFor="conn-token">Jeton d&apos;accès</FieldLabel>
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
        <legend className="text-sm text-fg-muted">Peut servir à</legend>
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
            {card?.display_name ?? connector} déclare des déclencheurs interrogés (
            {polled.map((t) => t.display_name || t.id).join(", ")}) : l&apos;interrogation lit par la
            connexion et livre ce qu&apos;elle lit, accordez donc action et trigger.
          </p>
        )}
      </fieldset>
      {err && (
        <InlineBanner tone="danger" layout="inline">
          {err}
        </InlineBanner>
      )}
      <div>
        <Button
          variant="primary"
          type="submit"
          disabled={busy || caps.length === 0 || connector.trim() === ""}
        >
          {busy ? "Connexion…" : "Connecter"}
        </Button>
      </div>
    </form>
  );
}
