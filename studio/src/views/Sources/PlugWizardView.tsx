// The plug wizard, URL-driven: account → event & narrowing → bot →
// mapping (a live preview through the trigger preview route — the lane's
// own extraction and judgment, writing nothing) → review & enable. The
// URL carries the identity of every choice (?source=&step=&connection=
// &trigger=&bot=), so a reload or a shared link reopens the same step;
// the narrowing and the mapping are state — they describe a draft, not a
// place. Enable creates the connector webhook config bound to the
// connection (one delivery URL, one secret) and the subscription. A polled
// trigger (ADR-168 §7) has no delivery URL: iterion reads the source, so
// enable creates the subscription alone, with the query its poller is
// derived from server-side and an optional backfill.
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { useSearch, useLocation, Link } from "wouter";
import { useMemo, useState } from "react";

import { useAuth } from "@/auth/AuthContext";
import { Button } from "@/components/ui/Button";
import { EmptyState } from "@/components/ui/EmptyState";
import { InlineBanner } from "@/components/ui/InlineBanner";
import { Spinner } from "@/components/ui/Spinner";
import { Badge } from "@/components/ui/Badge";
import { Input } from "@/components/ui/Input";
import { Select } from "@/components/ui/Select";
import { TagInput } from "@/components/ui/TagInput";
import { SourceLogo } from "@/components/Sources/SourceLogo";
import {
  formatPollInterval,
  listSources,
  pollContractText,
  sourceDeliveryLabel,
  type Source,
  type SourceFilter,
  type SourceTrigger,
} from "@/api/sources";
import { listConnections, patchConnection } from "@/api/connections";
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

  // The draft's narrowing and mapping: state, not URL. A polled trigger's
  // query values and backfill are narrowing too — what the draft reads.
  const [filters, setFilters] = useState<Record<string, string[]>>({});
  const [varMap, setVarMap] = useState<Record<string, string>>({});
  const [pollQuery, setPollQuery] = useState<Record<string, string>>({});
  const [backfill, setBackfill] = useState("0");
  const [granting, setGranting] = useState(false);
  const [grantError, setGrantError] = useState<string | null>(null);
  const queryClient = useQueryClient();
  const [preview, setPreview] = useState<TriggerPreviewResult | null>(null);
  const [previewError, setPreviewError] = useState<string | null>(null);
  const [previewing, setPreviewing] = useState(false);
  const [enabling, setEnabling] = useState(false);
  const [enableError, setEnableError] = useState<string | null>(null);
  const [enabledHook, setEnabledHook] = useState<WebhookWithToken | null>(null);
  const [reusedHookId, setReusedHookId] = useState<string | null>(null);

  const trigger = source?.triggers?.find((t) => t.id === triggerID);
  const polled = trigger?.delivery === "poll";

  // The query values as the server keys the poller: typed values only — an
  // empty one is an unset one server-side too — sent as strings.
  const serverPollQuery = (): Record<string, string> | undefined => {
    const out = Object.fromEntries(
      Object.entries(pollQuery)
        .map(([id, v]) => [id, v.trim()] as const)
        .filter(([, v]) => v !== ""),
    );
    return Object.keys(out).length ? out : undefined;
  };
  const missingQuery = polled
    ? (trigger?.query ?? [])
        .filter((f) => f.required && !(pollQuery[f.id] ?? "").trim())
        .map((f) => f.id)
    : [];
  const backfillValue = /^\d+$/.test(backfill.trim()) ? Number(backfill.trim()) : null;

  // The grants the subscription leans on: the save requires `trigger`, and a
  // poller reads through the connection, which requires `action`. Judged on
  // the listed connection; one the list does not hold is the server's call.
  const pickedConnection = connections.data?.find((c) => c.id === connectionID);
  const missingGrants = pickedConnection
    ? (polled ? ["action", "trigger"] : ["trigger"]).filter(
        (cap) => !(pickedConnection.capabilities ?? []).includes(cap),
      )
    : [];
  const grant = async () => {
    if (!pickedConnection) return;
    setGranting(true);
    setGrantError(null);
    try {
      await patchConnection(teamID, connectionID, {
        capabilities: [...(pickedConnection.capabilities ?? []), ...missingGrants],
      });
      await queryClient.invalidateQueries({ queryKey: ["connections", teamID, sourceID] });
    } catch (e) {
      setGrantError(String(e));
    } finally {
      setGranting(false);
    }
  };

  // The server's var_map is {bot var: event expression} (binding judges
  // the key against the bot's declared vars and the value against the
  // event namespace); the operator types the bot var each output lands
  // in. An empty input is omitted — the absent mapping already lands an
  // output on the bot var of the same name.
  const serverVarMap = () =>
    Object.fromEntries(
      Object.entries(varMap)
        .filter(([, botVar]) => botVar.trim() !== "")
        .map(([output, botVar]) => [botVar.trim(), `event.${output}`]),
    );

  const runPreview = async () => {
    setPreviewing(true);
    setPreview(null);
    setPreviewError(null);
    try {
      setPreview(
        await previewTrigger({
          bot_id: botID,
          connection_id: connectionID,
          trigger_id: triggerID,
          filters,
          var_map: Object.keys(serverVarMap()).length ? serverVarMap() : undefined,
          ...(polled ? { poll_query: serverPollQuery() } : {}),
        }),
      );
    } catch (e) {
      setPreviewError(String(e));
    } finally {
      setPreviewing(false);
    }
  };

  const enable = async () => {
    setEnabling(true);
    setEnableError(null);
    try {
      // A polled trigger is read by its poller: nothing is delivered, so
      // no webhook config — no URL, no secret — is minted for it.
      if (!polled) {
        // One delivery URL per connection: reuse the webhook config one
        // exists, else create it (the secret is minted here, shown once).
        const hooks = await listWebhooks(teamID);
        const existing = hooks.find(
          (h) => h.connector_connection_id === connectionID,
        );
        if (!existing) {
          const hook: WebhookWithToken = await createWebhook(teamID, {
            name: `${source?.display_name ?? sourceID} — ${triggerID}`,
            provider: "connector",
            connector_connection_id: connectionID,
          });
          setEnabledHook(hook);
          setReusedHookId(null);
        } else {
          // The operator still declares the URL at the vendor: show it
          // even when no secret is minted.
          setReusedHookId(existing.id);
        }
      }
      await createTrigger({
        bot_id: botID,
        match: {},
        invocation: "connector",
        connection_id: connectionID,
        trigger_id: triggerID,
        filters,
        var_map: Object.keys(serverVarMap()).length ? serverVarMap() : undefined,
        ...(polled ? { poll_query: serverPollQuery(), backfill: backfillValue ?? 0 } : {}),
        enabled: true,
      });
      // The terminal state lives in the URL: a reload or a shared link
      // reopens DONE, not a live enable button — re-invoking enable would
      // mint a duplicate subscription (the server refuses it with 409,
      // but the wizard must not offer the gesture).
      setParams({ step: "review", enabled: "1" });
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
  if (sources.isError) {
    return (
      <InlineBanner tone="danger">
        Erreur de chargement de la source : {String(sources.error)}
      </InlineBanner>
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
            pollQuery={pollQuery}
            setPollQuery={setPollQuery}
            missingQuery={missingQuery}
            onPick={(id) => {
              // A narrowing belongs to its trigger: picking another one
              // leaves the previous draft's keys behind, where they would
              // dead-end the preview and the save. The new draft starts at
              // the package's declared defaults.
              const picked = source.triggers?.find((t) => t.id === id);
              const seeded: Record<string, string[]> = {};
              for (const f of picked?.filters ?? []) {
                if (f.default?.length) seeded[f.id] = f.default;
              }
              setFilters(seeded);
              setVarMap({});
              setPollQuery({});
              setBackfill("0");
              setPreview(null);
              // A trigger with something to read or type — its filters, a
              // polled one's contract and query — stays on this step until
              // the operator continues; one with nothing goes on.
              const configurable =
                (picked?.filters?.length ?? 0) > 0 || picked?.delivery === "poll";
              setParams(configurable ? { trigger: id } : { trigger: id, step: "bot" });
            }}
            onNext={() => setParams({ step: "bot" })}
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
            previewError={previewError}
            previewing={previewing}
            runPreview={runPreview}
            onNext={() => setParams({ step: "review" })}
          />
        )}
        {step === "review" && (
          <StepReview
            source={source}
            trigger={trigger}
            connectionID={connectionID}
            triggerID={triggerID}
            botID={botID}
            pollQuery={serverPollQuery()}
            missingQuery={missingQuery}
            backfill={backfill}
            setBackfill={setBackfill}
            backfillValid={backfillValue !== null}
            missingGrants={missingGrants}
            granting={granting}
            grantError={grantError}
            onGrant={grant}
            onBackToEvent={() => setParams({ step: "event" })}
            hook={enabledHook}
            reusedHookId={reusedHookId}
            done={params.get("enabled") === "1"}
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
  pollQuery,
  setPollQuery,
  missingQuery,
  onPick,
  onNext,
}: {
  source: Source;
  triggerID: string;
  filters: Record<string, string[]>;
  setFilters: (f: Record<string, string[]>) => void;
  pollQuery: Record<string, string>;
  setPollQuery: (q: Record<string, string>) => void;
  missingQuery: string[];
  onPick: (id: string) => void;
  onNext: () => void;
}) {
  const triggers = source.triggers ?? [];
  const selected = triggers.find((t) => t.id === triggerID);
  // A card mixing webhook and polled triggers says each one's delivery.
  const mixed = source.delivery === "mixed";
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
            <span className="flex items-center gap-1.5 font-medium text-fg-default">
              {t.display_name || t.id}
              {mixed && t.delivery && (
                <Badge variant={t.delivery === "poll" ? "info" : "neutral"}>
                  {sourceDeliveryLabel(t.delivery)}
                </Badge>
              )}
            </span>
            {t.description && (
              <span className="block text-xs text-fg-subtle mt-0.5">{t.description}</span>
            )}
          </button>
        ))}
      </div>
      {selected?.delivery === "poll" && (
        <PollFields
          trigger={selected}
          pollQuery={pollQuery}
          setPollQuery={setPollQuery}
        />
      )}
      {selected?.filters?.map((f) =>
        f.type === "list" ? (
          <ListFilterField
            key={`${triggerID}-${f.id}`}
            inputID={`filter-${triggerID}-${f.id}`}
            filter={f}
            values={filters[f.id] ?? []}
            onChange={(values) => {
              const next = { ...filters };
              if (values.length) next[f.id] = values;
              else delete next[f.id]; // untouched and emptied mean the same
              setFilters(next);
            }}
          />
        ) : (
          <label key={`${triggerID}-${f.id}`} className="flex flex-col gap-1 text-xs text-fg-default">
            Filtre {f.id}
            {f.type === "enum" ? (
              <Select
                value={filters[f.id]?.[0] ?? ""}
                onChange={(e) => {
                  const next = { ...filters };
                  if (e.target.value) next[f.id] = [e.target.value];
                  else delete next[f.id]; // untouched and emptied mean the same
                  setFilters(next);
                }}
                aria-label={`Filtre ${f.id}`}
              >
                {/* The server refills an absent narrowing with the package's
                    declared default, so "all" is only offered when the
                    package declares none — otherwise the label would lie. */}
                {(f.default?.length ?? 0) === 0 && <option value="">— tous —</option>}
                {(f.options ?? []).map((o) => (
                  <option key={o} value={o}>
                    {o}
                  </option>
                ))}
              </Select>
            ) : (
              <Input
                value={filters[f.id]?.[0] ?? ""}
                onChange={(e) => {
                  const next = { ...filters };
                  if (e.target.value) next[f.id] = [e.target.value];
                  else delete next[f.id];
                  setFilters(next);
                }}
                aria-label={`Filtre ${f.id}`}
              />
            )}
          </label>
        ),
      )}
      {selected && (
        <div className="flex items-center gap-2">
          <Button size="sm" onClick={onNext} disabled={missingQuery.length > 0}>
            Continuer
          </Button>
          {missingQuery.length > 0 && (
            <span className="text-xs text-fg-subtle">
              Requis : {missingQuery.join(", ")}
            </span>
          )}
        </div>
      )}
    </div>
  );
}

// ListFilterField narrows on a list the event carries (a feed entry's
// categories): the operator lists values, and the event matches when any
// element of its list is one of them. The values are compared as written,
// case and commas included. A label element would hand its clicks to the
// first chip's remove button, so the label names the draft input by id.
function ListFilterField({
  inputID,
  filter,
  values,
  onChange,
}: {
  inputID: string;
  filter: SourceFilter;
  values: string[];
  onChange: (values: string[]) => void;
}) {
  // The server refills an emptied narrowing with the package's declared
  // default, so "empty matches all" is only said when it declares none.
  const empty =
    (filter.default?.length ?? 0) === 0
      ? "vide, tout événement correspond"
      : "vide, la valeur par défaut du paquet s'applique";
  return (
    <div className="flex flex-col gap-1 text-xs text-fg-default">
      <label htmlFor={inputID}>Filtre {filter.id}</label>
      <TagInput
        id={inputID}
        value={values}
        onChange={onChange}
        placeholder="Une valeur, puis Entrée"
        literalValues
      />
      <span className="text-fg-subtle">
        Correspond si l&apos;événement porte au moins une de ces valeurs ; {empty}.
      </span>
    </div>
  );
}

// PollFields is a polled trigger's half of the event step: the contract
// polling keeps, said before the choice, and one input per query field the
// poller is keyed by — an int numeric, a bool true/false, every value kept
// as the string the server parses.
function PollFields({
  trigger,
  pollQuery,
  setPollQuery,
}: {
  trigger: SourceTrigger;
  pollQuery: Record<string, string>;
  setPollQuery: (q: Record<string, string>) => void;
}) {
  const set = (id: string, v: string) => setPollQuery({ ...pollQuery, [id]: v });
  return (
    <div className="flex flex-col gap-3">
      <p className="rounded-[var(--radius-md)] border border-border-default p-3 text-xs text-fg-subtle">
        {pollContractText(trigger.interval)}
      </p>
      {(trigger.query ?? []).map((f) => {
        const label = `Requête ${f.id}${f.required ? " (requis)" : ""}`;
        return (
          <label key={`${trigger.id}-${f.id}`} className="flex flex-col gap-1 text-xs text-fg-default">
            <span>
              Requête {f.id}
              {f.required && <span aria-hidden="true"> *</span>}
            </span>
            {f.type === "bool" ? (
              <Select
                value={pollQuery[f.id] ?? ""}
                onChange={(e) => set(f.id, e.target.value)}
                aria-label={label}
                required={f.required}
              >
                <option value="">— non défini —</option>
                <option value="true">oui (true)</option>
                <option value="false">non (false)</option>
              </Select>
            ) : (
              <Input
                type={f.type === "int" ? "number" : "text"}
                inputMode={f.type === "int" ? "numeric" : undefined}
                step={f.type === "int" ? 1 : undefined}
                value={pollQuery[f.id] ?? ""}
                onChange={(e) => set(f.id, e.target.value)}
                aria-label={label}
                required={f.required}
              />
            )}
            {f.description && <span className="text-fg-subtle">{f.description}</span>}
          </label>
        );
      })}
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
  if (bots.isError)
    return <InlineBanner tone="danger">Erreur : {String(bots.error)}</InlineBanner>;
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
  previewError,
  previewing,
  runPreview,
  onNext,
}: {
  trigger?: SourceTrigger;
  varMap: Record<string, string>;
  setVarMap: (m: Record<string, string>) => void;
  preview: TriggerPreviewResult | null;
  previewError: string | null;
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
              placeholder={`variable recevant ${o.name} (vide : même nom)`}
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
      {previewError && (
        <div className="rounded-[var(--radius-md)] border border-border-default p-3 text-xs" aria-live="polite">
          <span className="text-fg-subtle">Échec de la prévisualisation : {previewError}</span>
        </div>
      )}
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
                Correspond ✓{" "}
                {preview.sample
                  ? trigger?.delivery === "poll"
                    ? "(premier élément de l'échantillon)"
                    : "(échantillon du paquet)"
                  : ""}
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
  trigger,
  connectionID,
  triggerID,
  botID,
  pollQuery,
  missingQuery,
  backfill,
  setBackfill,
  backfillValid,
  missingGrants,
  granting,
  grantError,
  onGrant,
  onBackToEvent,
  hook,
  reusedHookId,
  done,
  error,
  enabling,
  onEnable,
}: {
  source: Source;
  trigger?: SourceTrigger;
  connectionID: string;
  triggerID: string;
  botID: string;
  pollQuery?: Record<string, string>;
  missingQuery: string[];
  backfill: string;
  setBackfill: (v: string) => void;
  backfillValid: boolean;
  missingGrants: string[];
  granting: boolean;
  grantError: string | null;
  onGrant: () => void;
  onBackToEvent: () => void;
  hook: WebhookWithToken | null;
  reusedHookId: string | null;
  done: boolean;
  error: string | null;
  enabling: boolean;
  onEnable: () => void;
}) {
  const polled = trigger?.delivery === "poll";
  const blocked = missingGrants.length > 0 || (polled && (missingQuery.length > 0 || !backfillValid));
  // A polled trigger shows the package's polling notes when it ships them:
  // its setup.md may walk through a webhook the polled flow never adds.
  const notes = polled ? (source.setup_poll ?? source.setup) : source.setup;
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
        {polled && (
          <li>
            Livraison :{" "}
            <span className="text-fg-default">
              interrogé
              {trigger?.interval ? ` toutes les ${formatPollInterval(trigger.interval)}` : ""}
            </span>
          </li>
        )}
        {polled &&
          Object.entries(pollQuery ?? {}).map(([id, v]) => (
            <li key={id}>
              Requête {id} : <code>{v}</code>
            </li>
          ))}
        <li>
          Bot : <code>{botID}</code>
        </li>
      </ul>
      {polled && (
        <p className="text-xs text-fg-subtle">{pollContractText(trigger?.interval)}</p>
      )}
      {!done && missingGrants.length > 0 && (
        <InlineBanner
          tone="warning"
          layout="inline"
          action={
            <Button size="sm" variant="secondary" onClick={onGrant} disabled={granting}>
              {granting ? "Mise à jour…" : `Accorder ${missingGrants.join(" et ")}`}
            </Button>
          }
        >
          Cette connexion n&apos;accorde pas {missingGrants.join(" ni ")}.{" "}
          {polled
            ? "Un déclencheur interrogé lit la source par la connexion (action) et en livre les éléments (trigger)."
            : "Un abonnement écoute les événements de la connexion (trigger)."}
          {grantError && <span className="block mt-1">Échec de la mise à jour : {grantError}</span>}
        </InlineBanner>
      )}
      {!done && polled && missingQuery.length > 0 && (
        <InlineBanner
          tone="warning"
          layout="inline"
          action={
            <Button size="sm" variant="secondary" onClick={onBackToEvent}>
              Revenir à l&apos;événement
            </Button>
          }
        >
          Requête incomplète : {missingQuery.join(", ")} requis.
        </InlineBanner>
      )}
      {!done && polled && (
        <label className="flex flex-col gap-1 text-xs text-fg-default">
          Backfill
          <Input
            type="number"
            inputMode="numeric"
            min={0}
            step={1}
            value={backfill}
            onChange={(e) => setBackfill(e.target.value)}
            aria-label="Backfill"
            aria-invalid={!backfillValid || undefined}
          />
          <span className="text-fg-subtle">
            {backfillValid
              ? "Livre une fois les N derniers éléments déjà présents quand l'abonnement démarre ; 0 n'en livre aucun."
              : "Un nombre entier, 0 ou plus."}
          </span>
        </label>
      )}
      {reusedHookId && !hook && (
        <div className="rounded-[var(--radius-md)] border border-border-default p-3 text-xs">
          <p className="font-medium text-fg-default">
            Webhook existant — URL à déclarer chez le fournisseur :
          </p>
          <code className="break-all text-fg-subtle">
            {`${window.location.origin}/api/webhooks/c/${encodeURIComponent(reusedHookId)}`}
          </code>
        </div>
      )}
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
      {notes && (
        <details className="rounded-[var(--radius-md)] border border-border-default p-3 text-xs">
          <summary className="cursor-pointer font-medium text-fg-default">
            {polled
              ? `Notes de configuration (${source.display_name})`
              : `Configuration chez le fournisseur (${source.display_name})`}
          </summary>
          <pre className="mt-2 whitespace-pre-wrap font-sans text-fg-subtle">{notes}</pre>
        </details>
      )}
      {done ? (
        <InlineBanner tone="success">
          {polled
            ? `Abonnement activé — iterion interroge ${source.display_name}${
                trigger?.interval ? ` toutes les ${formatPollInterval(trigger.interval)}` : ""
              } et lance ${botID || "le bot"} pour chaque élément nouveau ou révisé qui correspond.`
            : `Abonnement activé — cette source lance ${botID || "le bot"} à chaque événement correspondant.`}
        </InlineBanner>
      ) : (
        <>
          {error && (
            <InlineBanner tone="danger">Échec de l'activation : {error}</InlineBanner>
          )}
          <div>
            <Button onClick={onEnable} disabled={enabling || blocked}>
              {enabling ? "Activation…" : "Activer l'abonnement"}
            </Button>
          </div>
        </>
      )}
    </div>
  );
}
