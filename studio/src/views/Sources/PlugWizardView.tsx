// The plug wizard, URL-driven: account → event & narrowing → bot →
// mapping (a live preview through the trigger preview route — the lane's
// own extraction and judgment, writing nothing) → review & enable. The
// URL carries the identity of every choice (?source=&step=&connection=
// &trigger=&bot=), so a reload or a shared link reopens the same step;
// the narrowing and the mapping are state — they describe a draft, not a
// place. Enable creates the connector webhook config bound to the
// connection (one delivery URL, one secret) and the subscription.
import { useQuery } from "@tanstack/react-query";
import { useSearch, useLocation, Link } from "wouter";
import { useMemo, useState } from "react";

import { useAuth } from "@/auth/AuthContext";
import { Button } from "@/components/ui/Button";
import { EmptyState } from "@/components/ui/EmptyState";
import { InlineBanner } from "@/components/ui/InlineBanner";
import { Spinner } from "@/components/ui/Spinner";
import { Input } from "@/components/ui/Input";
import { Select } from "@/components/ui/Select";
import { SourceLogo } from "@/components/Sources/SourceLogo";
import { listSources, type Source } from "@/api/sources";
import { listConnections } from "@/api/connections";
import { listBots } from "@/api/bots";
import {
  createTrigger,
  previewTrigger,
  type TriggerPreviewResult,
} from "@/api/triggers";
import { createWebhook, listWebhooks, type WebhookWithToken } from "@/api/webhooks";
import type { ConnectorConnection } from "@/api/connections";
import { useServerInfoStore } from "@/store/serverInfo";
import type { BotEntryWithSchema } from "@/api/bots";
import type { UseQueryResult } from "@tanstack/react-query";

const STEPS = ["account", "event", "bot", "mapping", "review"] as const;
type Step = (typeof STEPS)[number];

const STEP_LABELS: Record<Step, string> = {
  account: "Compte",
  event: "Événement",
  bot: "Bot",
  mapping: "Mapping",
  review: "Activation",
};

export default function PlugWizardView() {
  const search = useSearch();
  const [, navigate] = useLocation();
  const { activeTeam } = useAuth();
  // The connections REST family is the cloud gate: a local process has no
  // auth service and no team for the path to name (`iterion connections`
  // is the local surface). The account step answers that honestly instead
  // of spinning on a query that can never fire.
  const cloud = useServerInfoStore((s) => s.info?.mode === "cloud");
  const teamID = activeTeam?.team_id ?? "";

  const params = useMemo(() => new URLSearchParams(search), [search]);
  const sourceID = params.get("source") ?? "";
  const step = (STEPS as readonly string[]).includes(params.get("step") ?? "")
    ? (params.get("step") as Step)
    : "account";
  const connectionID = params.get("connection") ?? "";
  const triggerID = params.get("trigger") ?? "";
  const botID = params.get("bot") ?? "";

  const setParams = (patch: Record<string, string>) => {
    const next = new URLSearchParams(params);
    for (const [k, v] of Object.entries(patch)) {
      if (v) next.set(k, v);
      else next.delete(k);
    }
    navigate(`/sources/plug?${next}`, { replace: true });
  };

  const sources = useQuery({ queryKey: ["sources"], queryFn: listSources });
  const source: Source | undefined = sources.data?.find(
    (s) => s.kind === "connector" && s.id === sourceID,
  );

  const connections = useQuery({
    queryKey: ["connections", teamID, sourceID],
    queryFn: () => listConnections(teamID, sourceID),
    enabled: cloud && !!teamID && !!sourceID,
  });
  const bots = useQuery({
    queryKey: ["bots"],
    queryFn: listBots,
    enabled: !!sourceID,
  });

  // The draft's narrowing and mapping: state, not URL.
  const [filters, setFilters] = useState<Record<string, string[]>>({});
  const [varMap, setVarMap] = useState<Record<string, string>>({});
  const [preview, setPreview] = useState<TriggerPreviewResult | null>(null);
  const [previewing, setPreviewing] = useState(false);
  const [enabling, setEnabling] = useState(false);
  const [enableError, setEnableError] = useState<string | null>(null);
  const [enabledHook, setEnabledHook] = useState<WebhookWithToken | null>(null);

  const trigger = source?.triggers?.find((t) => t.id === triggerID);

  const runPreview = async () => {
    setPreviewing(true);
    setPreview(null);
    try {
      setPreview(
        await previewTrigger({
          bot_id: botID,
          connection_id: connectionID,
          trigger_id: triggerID,
          filters,
          var_map: varMap,
        }),
      );
    } finally {
      setPreviewing(false);
    }
  };

  const enable = async () => {
    setEnabling(true);
    setEnableError(null);
    try {
      // One delivery URL per connection: reuse the webhook config one
      // exists, else create it (the secret is minted here, shown once).
      const hooks = await listWebhooks(teamID);
      const existing = hooks.find(
        (h) =>
          (h as unknown as { connector_connection_id?: string })
            .connector_connection_id === connectionID,
      );
      let hook: WebhookWithToken | null = null;
      if (!existing) {
        hook = await createWebhook(teamID, {
          name: `${source?.display_name ?? sourceID} — ${triggerID}`,
          provider: "connector",
          connector_connection_id: connectionID,
        });
        setEnabledHook(hook);
      }
      await createTrigger({
        bot_id: botID,
        match: {},
        invocation: "connector",
        connection_id: connectionID,
        trigger_id: triggerID,
        filters,
        var_map: Object.keys(varMap).length ? varMap : undefined,
        enabled: true,
      });
      setParams({ step: "review", done: "1" });
    } catch (e) {
      setEnableError(String(e));
    } finally {
      setEnabling(false);
    }
  };

  if (sourceID && sources.isSuccess && !source) {
    return (
      <EmptyState
        title="Source inconnue"
        message="Ce lien n'identifie aucune source du catalogue."
        action={
          <Link href="/sources">
            <Button variant="secondary">Retour au catalogue</Button>
          </Link>
        }
      />
    );
  }
  // No source named: the entry that came from a bot page (or a bare link)
  // picks one first — every later step keeps the bot it carried.
  if (!source && sources.isSuccess) {
    const pickable = (sources.data ?? []).filter((s) => s.kind === "connector");
    return (
      <div className="mx-auto w-full max-w-3xl p-4">
        <h1 className="text-lg font-semibold text-fg-default">Brancher une source</h1>
        {botID && (
          <p className="text-xs text-fg-subtle mt-0.5">
            Pour le bot <code>{botID}</code>
          </p>
        )}
        <div className="mt-3 flex flex-col gap-2" role="radiogroup" aria-label="Source">
          {pickable.map((s) => (
            <button
              key={s.id}
              type="button"
              role="radio"
              aria-checked={false}
              onClick={() => setParams({ source: s.id, step: "account" })}
              className="rounded-[var(--radius-md)] border border-border-default p-3 text-left hover:border-fg-subtle"
            >
              <span className="flex items-center gap-2 text-sm font-medium text-fg-default">
                <SourceLogo connectorId={s.id} />
                {s.display_name}
              </span>
            </button>
          ))}
        </div>
      </div>
    );
  }
  if (sources.isPending || !source) {
    return (
      <div className="flex justify-center py-16">
        <Spinner label="Chargement de la source" />
      </div>
    );
  }

  const stepIndex = STEPS.indexOf(step);

  return (
    <div className="mx-auto w-full max-w-3xl p-4">
      <div className="flex items-center gap-2">
        <SourceLogo connectorId={source.id} />
        <h1 className="text-lg font-semibold text-fg-default">
          Brancher {source.display_name}
        </h1>
      </div>
      <ol className="mt-3 flex gap-2 text-xs" aria-label="Étapes">
        {STEPS.map((s, i) => (
          <li
            key={s}
            aria-current={s === step ? "step" : undefined}
            className={
              i === stepIndex
                ? "font-semibold text-fg-default"
                : i < stepIndex
                  ? "text-fg-subtle"
                  : "text-fg-muted"
            }
          >
            {i + 1}. {STEP_LABELS[s]}
          </li>
        ))}
      </ol>

      <div className="mt-4">
        {step === "account" &&
          (cloud ? (
            <StepAccount
              connections={connections}
              connectionID={connectionID}
              onPick={(id) => setParams({ connection: id, step: "event" })}
            />
          ) : (
            <InlineBanner tone="info">
              En local, les connexions se créent avec{" "}
              <code>iterion connections</code> ; le branchement guidé —
              compte, événement, bot, mapping, activation — est le mode
              cloud.
            </InlineBanner>
          ))}
        {step === "event" && (
          <StepEvent
            source={source}
            triggerID={triggerID}
            filters={filters}
            setFilters={setFilters}
            onPick={(id) => setParams({ trigger: id, step: "bot" })}
          />
        )}
        {step === "bot" && (
          <StepBot
            bots={bots}
            botID={botID}
            onPick={(id) => setParams({ bot: id, step: "mapping" })}
          />
        )}
        {step === "mapping" && (
          <StepMapping
            trigger={trigger}
            varMap={varMap}
            setVarMap={setVarMap}
            preview={preview}
            previewing={previewing}
            runPreview={runPreview}
            onNext={() => setParams({ step: "review" })}
          />
        )}
        {step === "review" && (
          <StepReview
            source={source}
            connectionID={connectionID}
            triggerID={triggerID}
            botID={botID}
            hook={enabledHook}
            error={enableError}
            enabling={enabling}
            onEnable={enable}
          />
        )}
      </div>
    </div>
  );
}

function StepAccount({
  connections,
  connectionID,
  onPick,
}: {
  connections: UseQueryResult<ConnectorConnection[], Error>;
  connectionID: string;
  onPick: (id: string) => void;
}) {
  if (connections.isPending) return <Spinner label="Chargement des connexions" />;
  if (connections.isError)
    return <InlineBanner tone="danger">Erreur : {String(connections.error)}</InlineBanner>;
  const list = connections.data ?? [];
  if (list.length === 0)
    return (
      <EmptyState
        title="Aucune connexion"
        message="Créez d'abord une connexion pour ce connecteur, depuis la page Intégrations."
        action={
          <Link href="/integrations">
            <Button variant="secondary">Ouvrir Intégrations</Button>
          </Link>
        }
      />
    );
  return (
    <div className="flex flex-col gap-2" role="radiogroup" aria-label="Connexion">
      {list.map((c) => (
        <button
          key={c.id}
          type="button"
          role="radio"
          aria-checked={c.id === connectionID}
          onClick={() => onPick(c.id)}
          className={`rounded-[var(--radius-md)] border p-3 text-left text-sm ${
            c.id === connectionID
              ? "border-fg-default"
              : "border-border-default hover:border-fg-subtle"
          }`}
        >
          <span className="font-medium text-fg-default">{c.alias}</span>
          <span className="text-fg-subtle"> — {c.base_url || c.connector}</span>
        </button>
      ))}
    </div>
  );
}

function StepEvent({
  source,
  triggerID,
  filters,
  setFilters,
  onPick,
}: {
  source: Source;
  triggerID: string;
  filters: Record<string, string[]>;
  setFilters: (f: Record<string, string[]>) => void;
  onPick: (id: string) => void;
}) {
  const triggers = source.triggers ?? [];
  const selected = triggers.find((t) => t.id === triggerID);
  return (
    <div className="flex flex-col gap-3">
      <div className="flex flex-col gap-2" role="radiogroup" aria-label="Événement">
        {triggers.map((t) => (
          <button
            key={t.id}
            type="button"
            role="radio"
            aria-checked={t.id === triggerID}
            onClick={() => onPick(t.id)}
            className={`rounded-[var(--radius-md)] border p-3 text-left text-sm ${
              t.id === triggerID
                ? "border-fg-default"
                : "border-border-default hover:border-fg-subtle"
            }`}
          >
            <span className="font-medium text-fg-default">
              {t.display_name || t.id}
            </span>
            {t.description && (
              <span className="block text-xs text-fg-subtle mt-0.5">{t.description}</span>
            )}
          </button>
        ))}
      </div>
      {selected?.filters?.map((f) => (
        <label key={f.id} className="flex flex-col gap-1 text-xs text-fg-default">
          Filtre {f.id}
          {f.type === "enum" ? (
            <Select
              value={filters[f.id]?.[0] ?? ""}
              onChange={(e) =>
                setFilters({ ...filters, [f.id]: e.target.value ? [e.target.value] : [] })
              }
              aria-label={`Filtre ${f.id}`}
            >
              <option value="">— tous —</option>
              {(f.options ?? []).map((o) => (
                <option key={o} value={o}>
                  {o}
                </option>
              ))}
            </Select>
          ) : (
            <Input
              value={filters[f.id]?.[0] ?? ""}
              onChange={(e) =>
                setFilters({ ...filters, [f.id]: e.target.value ? [e.target.value] : [] })
              }
              aria-label={`Filtre ${f.id}`}
            />
          )}
        </label>
      ))}
    </div>
  );
}

function StepBot({
  bots,
  botID,
  onPick,
}: {
  bots: UseQueryResult<BotEntryWithSchema[], Error>;
  botID: string;
  onPick: (id: string) => void;
}) {
  if (bots.isPending) return <Spinner label="Chargement des bots" />;
  const list = bots.data ?? [];
  if (list.length === 0)
    return (
      <EmptyState
        title="Aucun bot"
        message="Créez d'abord le bot que cette source lancera."
        action={
          <Link href="/bots">
            <Button variant="secondary">Ouvrir les bots</Button>
          </Link>
        }
      />
    );
  return (
    <div className="flex flex-col gap-2" role="radiogroup" aria-label="Bot">
      {list.map((b) => (
        <button
          key={b.name}
          type="button"
          role="radio"
          aria-checked={b.name === botID}
          onClick={() => onPick(b.name)}
          className={`rounded-[var(--radius-md)] border p-3 text-left text-sm ${
            b.name === botID
              ? "border-fg-default"
              : "border-border-default hover:border-fg-subtle"
          }`}
        >
          <span className="font-medium text-fg-default">{b.name}</span>
        </button>
      ))}
    </div>
  );
}

function StepMapping({
  trigger,
  varMap,
  setVarMap,
  preview,
  previewing,
  runPreview,
  onNext,
}: {
  trigger?: Source["triggers"] extends (infer T)[] | undefined ? T : never;
  varMap: Record<string, string>;
  setVarMap: (m: Record<string, string>) => void;
  preview: TriggerPreviewResult | null;
  previewing: boolean;
  runPreview: () => void;
  onNext: () => void;
}) {
  const outputs = trigger?.outputs ?? [];
  return (
    <div className="flex flex-col gap-3">
      {outputs.length === 0 ? (
        <p className="text-xs text-fg-subtle">
          Cet événement ne déclare pas de valeurs nommées : passez au mapping
          dans l'éditeur si le bot a besoin d'arguments.
        </p>
      ) : (
        outputs.map((o) => (
          <label key={o.name} className="flex items-center gap-2 text-xs text-fg-default">
            <code className="w-40 shrink-0">{o.name}</code>
            <span className="text-fg-subtle">→ variable du bot</span>
            <Input
              value={varMap[o.name] ?? ""}
              placeholder={o.name}
              aria-label={`Variable pour ${o.name}`}
              onChange={(e) => setVarMap({ ...varMap, [o.name]: e.target.value })}
            />
          </label>
        ))
      )}
      <div className="flex items-center gap-2">
        <Button variant="secondary" size="sm" onClick={runPreview} disabled={previewing}>
          {previewing ? "Prévisualisation…" : "Prévisualiser sur l'échantillon"}
        </Button>
        <Button size="sm" onClick={onNext}>
          Continuer
        </Button>
      </div>
      {preview && (
        <div
          className="rounded-[var(--radius-md)] border border-border-default p-3 text-xs"
          aria-live="polite"
        >
          {preview.filtered ? (
            <span className="text-fg-subtle">Filtré : {preview.filtered}</span>
          ) : preview.matched ? (
            <>
              <span className="font-medium text-fg-default">
                Correspond ✓ {preview.sample ? "(échantillon du paquet)" : ""}
              </span>
              {preview.vars && (
                <pre className="mt-1 whitespace-pre-wrap text-fg-subtle">
                  {JSON.stringify(preview.vars, null, 2)}
                </pre>
              )}
            </>
          ) : (
            <span className="text-fg-subtle">
              Refusé{preview.reason ? ` : ${preview.reason}` : ""}
              {preview.refusal ? ` — ${preview.refusal}` : ""}
            </span>
          )}
        </div>
      )}
    </div>
  );
}

function StepReview({
  source,
  connectionID,
  triggerID,
  botID,
  hook,
  error,
  enabling,
  onEnable,
}: {
  source: Source;
  connectionID: string;
  triggerID: string;
  botID: string;
  hook: WebhookWithToken | null;
  error: string | null;
  enabling: boolean;
  onEnable: () => void;
}) {
  return (
    <div className="flex flex-col gap-3 text-sm text-fg-default">
      <ul className="flex flex-col gap-1 text-xs text-fg-subtle">
        <li>
          Source : <span className="text-fg-default">{source.display_name}</span>
        </li>
        <li>
          Connexion : <code>{connectionID}</code>
        </li>
        <li>
          Événement : <code>{triggerID}</code>
        </li>
        <li>
          Bot : <code>{botID}</code>
        </li>
      </ul>
      {hook && (
        <div className="rounded-[var(--radius-md)] border border-border-default p-3 text-xs">
          <p className="font-medium text-fg-default">Webhook créé — URL à déclarer chez le fournisseur :</p>
          <code className="break-all text-fg-subtle">
            {`${window.location.origin}/api/webhooks/c/${encodeURIComponent(hook.config.id)}`}
          </code>
          <p className="mt-2">
            Secret (affiché une seule fois) :{" "}
            <code className="break-all">{hook.token}</code>
          </p>
        </div>
      )}
      {error && <InlineBanner tone="danger">Échec de l'activation : {error}</InlineBanner>}
      <div>
        <Button onClick={onEnable} disabled={enabling}>
          {enabling ? "Activation…" : "Activer l'abonnement"}
        </Button>
      </div>
    </div>
  );
}
