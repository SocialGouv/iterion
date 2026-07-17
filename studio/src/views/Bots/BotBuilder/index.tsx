// BotBuilderView — /bots/new. Guided (Mistral-style) bot creation in
// two phases on one page: a template gallery (GET /api/v1/bots/templates),
// then a focused form — four primary fields, a compact model & skills
// group, a vars editor, and a collapsed Advanced section — with a live
// summary card on the right that swaps to an embedded TestRunPane once
// the bot is created, so the create → test → iterate loop never leaves
// the page. Drafts auto-save to localStorage. Local mode only: the
// server refuses POST /api/v1/bots with 403 in cloud mode, so the whole
// flow is gated on serverInfo.mode.

import { useEffect, useMemo, useRef, useState } from "react";
import { Cross1Icon, PlusIcon } from "@radix-ui/react-icons";
import { Link, useLocation } from "wouter";

import {
  createBot,
  listBotTemplates,
  type BotCreateSpec,
  type BotEntryWithSchema,
  type BotTemplate,
} from "@/api/bots";
import { FeatureUnavailableError } from "@/api/client";
import { listLocalSkills, type LibrarySkill } from "@/api/skills";
import TestRunPane from "@/components/Runs/TestRunPane";
import { useHeaderSlot } from "@/components/shared/useHeaderSlot";
import {
  Badge,
  Button,
  Card,
  Checkbox,
  Chip,
  FieldLabel,
  IconButton,
  InlineBanner,
  Input,
  Select,
  Spinner,
  Textarea,
} from "@/components/ui";
import { EmojiPicker } from "@/components/ui/EmojiPicker";
import { errorMessage } from "@/lib/errorHints";
import { humanizeCron } from "@/lib/humanizeCron";
import { useBackendDetectStore } from "@/store/backendDetect";
import { useBotsStore } from "@/store/bots";
import { useServerInfoStore } from "@/store/serverInfo";

import { botLaunchFile } from "../botPaths";
import { deriveSlug, isValidSlug, isValidVarName } from "./slug";

// ---------------------------------------------------------------------------
// Draft model (persisted to localStorage)
// ---------------------------------------------------------------------------

const DRAFT_KEY = "bot-builder-draft";

const VAR_TYPES = ["string", "int", "bool", "float"] as const;
type VarType = (typeof VAR_TYPES)[number];

interface VarRow {
  name: string;
  type: VarType;
  default: string;
  description: string;
}

interface BuilderDraft {
  phase: 1 | 2;
  templateId: string | null;
  name: string;
  icon: string;
  description: string;
  instructions: string;
  // Carried through from a template spec (not directly editable in the
  // four-field form) so template-provided routing metadata isn't lost.
  whenToUse: string;
  capabilities: string[];
  model: string;
  backend: string;
  skills: string[];
  vars: VarRow[];
  worktree: boolean;
  sandbox: boolean;
  permission: "off" | "ask" | "deny";
  maxCostUsd: string;
  maxDuration: string;
  scheduleCron: string;
}

function emptyDraft(): BuilderDraft {
  return {
    phase: 1,
    templateId: null,
    name: "",
    icon: "",
    description: "",
    instructions: "",
    whenToUse: "",
    capabilities: [],
    model: "",
    backend: "",
    skills: [],
    vars: [],
    worktree: false,
    sandbox: false,
    permission: "off",
    maxCostUsd: "",
    maxDuration: "",
    scheduleCron: "",
  };
}

function loadDraft(): BuilderDraft {
  try {
    const raw = localStorage.getItem(DRAFT_KEY);
    if (!raw) return emptyDraft();
    const parsed = JSON.parse(raw) as Partial<BuilderDraft>;
    // Merge over the empty draft so a stale/older draft shape can't
    // leave fields undefined.
    return { ...emptyDraft(), ...parsed };
  } catch {
    return emptyDraft();
  }
}

function draftFromTemplate(t: BotTemplate): BuilderDraft {
  const s = t.spec;
  return {
    ...emptyDraft(),
    phase: 2,
    templateId: t.id,
    name: s.display_name || (t.id === "blank" ? "" : t.name),
    icon: s.icon || t.icon || "",
    description: s.description ?? "",
    instructions: s.instructions ?? "",
    whenToUse: s.when_to_use ?? "",
    capabilities: s.capabilities ?? [],
    model: s.model ?? "",
    backend: s.backend ?? "",
    skills: s.skills ?? [],
    vars: (s.vars ?? []).map((v) => ({
      name: v.name,
      type: (VAR_TYPES as readonly string[]).includes(v.type) ? (v.type as VarType) : "string",
      default: v.default ?? "",
      description: v.description ?? "",
    })),
    worktree: s.worktree ?? false,
    sandbox: s.sandbox ?? false,
    permission: s.permission === "ask" || s.permission === "deny" ? s.permission : "off",
    maxCostUsd: s.max_cost_usd != null ? String(s.max_cost_usd) : "",
    maxDuration: s.max_duration ?? "",
    scheduleCron: s.schedule_cron ?? "",
  };
}

// ---------------------------------------------------------------------------
// View shell — breadcrumb + cloud-mode gate
// ---------------------------------------------------------------------------

export default function BotBuilderView() {
  const serverInfo = useServerInfoStore((s) => s.info);

  useHeaderSlot({
    left: (
      <span className="flex items-center gap-1.5 text-xs font-medium text-fg-default">
        <Link href="/bots" className="text-fg-muted hover:text-fg-default hover:underline">
          Bots
        </Link>
        <span className="text-fg-subtle">/</span>
        <span>New bot</span>
      </span>
    ),
  });

  if (serverInfo?.mode === "cloud") {
    return (
      <div className="mx-auto w-full max-w-2xl p-4">
        <InlineBanner tone="info" title="Bot creation is a local-studio feature">
          Creating a bot scaffolds files into the workspace&apos;s <code>bots/</code> directory,
          which a cloud server has no filesystem for. Run <code>iterion studio</code> against your
          repo locally to build a bot, then push it. On this server, the way to put bots to work
          is to browse the catalog and marketplace, and enable them on a connected repository.
        </InlineBanner>
        <div className="mt-3 flex items-center gap-2">
          <Link href="/bots">
            <Button variant="secondary" size="sm">
              Back to Bots
            </Button>
          </Link>
          {serverInfo?.marketplace_enabled && (
            <Link href="/marketplace">
              <Button variant="secondary" size="sm">
                Browse marketplace
              </Button>
            </Link>
          )}
          <Link href="/integrations/connect">
            <Button variant="primary" size="sm">
              Connect a repository
            </Button>
          </Link>
        </div>
      </div>
    );
  }

  return <Builder />;
}

// ---------------------------------------------------------------------------
// Builder — phase state machine + draft persistence
// ---------------------------------------------------------------------------

function Builder() {
  const [, setLocation] = useLocation();
  const [draft, setDraft] = useState<BuilderDraft>(loadDraft);
  const [created, setCreated] = useState<BotEntryWithSchema | null>(null);
  const [draftSaved, setDraftSaved] = useState(false);

  // Auto-save the draft on every change (skip the initial mount and
  // anything after a successful create — the draft is cleared then).
  const firstRender = useRef(true);
  useEffect(() => {
    if (created) return;
    if (firstRender.current) {
      firstRender.current = false;
      return;
    }
    try {
      localStorage.setItem(DRAFT_KEY, JSON.stringify(draft));
    } catch {
      return; // quota/private-mode — the form still works, just unsaved
    }
    // Hint shown/hidden from timers (not synchronously in the effect
    // body) so the write→feedback flow doesn't cascade a re-render.
    const show = window.setTimeout(() => setDraftSaved(true), 0);
    const hide = window.setTimeout(() => setDraftSaved(false), 2000);
    return () => {
      window.clearTimeout(show);
      window.clearTimeout(hide);
    };
  }, [draft, created]);

  const onCreated = (entry: BotEntryWithSchema) => {
    try {
      localStorage.removeItem(DRAFT_KEY);
    } catch {
      /* non-fatal */
    }
    setCreated(entry);
    // Refresh the shared bots store so /bots and /bots/<slug> see the
    // new entry immediately.
    void useBotsStore.getState().refetch();
  };

  if (draft.phase === 1) {
    return (
      <TemplateGallery
        onPick={(t) => setDraft(draftFromTemplate(t))}
        onSkip={() => setDraft((d) => ({ ...d, phase: 2 }))}
        hasDraft={draft.name !== "" || draft.instructions !== ""}
        onResumeDraft={() => setDraft((d) => ({ ...d, phase: 2 }))}
      />
    );
  }

  return (
    <BuilderForm
      draft={draft}
      setDraft={setDraft}
      draftSaved={draftSaved}
      created={created}
      onCreated={onCreated}
      onBackToTemplates={() => setDraft((d) => ({ ...d, phase: 1 }))}
      onGoToBotPage={(slug) => setLocation(`/bots/${encodeURIComponent(slug)}`)}
    />
  );
}

// ---------------------------------------------------------------------------
// Phase 1 — template gallery
// ---------------------------------------------------------------------------

function TemplateGallery({
  onPick,
  onSkip,
  hasDraft,
  onResumeDraft,
}: {
  onPick: (t: BotTemplate) => void;
  onSkip: () => void;
  hasDraft: boolean;
  onResumeDraft: () => void;
}) {
  const [templates, setTemplates] = useState<BotTemplate[] | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [reloadTick, setReloadTick] = useState(0);

  useEffect(() => {
    let cancelled = false;
    listBotTemplates()
      .then((ts) => {
        if (cancelled) return;
        setTemplates(ts);
        setError(null);
      })
      .catch((e: unknown) => {
        if (!cancelled) setError(errorMessage(e));
      });
    return () => {
      cancelled = true;
    };
  }, [reloadTick]);

  return (
    <div className="mx-auto w-full max-w-4xl p-4">
      <h1 className="text-base font-semibold text-fg-default">Create a bot</h1>
      <p className="mt-1 text-xs text-fg-muted">
        Pick a starting point — every template pre-fills the form and stays fully editable.
      </p>

      {hasDraft && (
        <div className="mt-3">
          <InlineBanner tone="info" title="You have an unfinished draft">
            <Button variant="secondary" size="sm" onClick={onResumeDraft}>
              Resume draft
            </Button>
          </InlineBanner>
        </div>
      )}

      {error && (
        <div className="mt-3">
          <InlineBanner tone="danger" title="Couldn't load templates">
            {error}
            <div className="mt-2">
              <Button
                variant="secondary"
                size="sm"
                onClick={() => {
                  setError(null);
                  setReloadTick((n) => n + 1);
                }}
              >
                Retry
              </Button>
            </div>
          </InlineBanner>
        </div>
      )}

      {templates === null && !error ? (
        <div className="mt-4 flex items-center gap-2 text-sm text-fg-muted">
          <Spinner /> Loading templates…
        </div>
      ) : (
        <div className="mt-4 grid gap-3 sm:grid-cols-2 lg:grid-cols-3">
          {(templates ?? []).map((t) => (
            <button
              key={t.id}
              type="button"
              onClick={() => onPick(t)}
              className="flex flex-col items-start gap-1.5 rounded-md border border-border-default bg-surface-1 p-3 text-left transition-colors hover:border-accent hover:bg-surface-2 focus-visible:border-accent"
            >
              <span className="text-2xl leading-none" aria-hidden="true">
                {t.icon || "🤖"}
              </span>
              <span className="text-xs font-semibold text-fg-default">{t.name}</span>
              <span className="text-caption text-fg-muted">{t.description}</span>
            </button>
          ))}
        </div>
      )}

      <button
        type="button"
        onClick={onSkip}
        className="mt-4 text-caption text-fg-subtle hover:text-fg-default hover:underline"
      >
        Skip — start blank
      </button>
    </div>
  );
}

// ---------------------------------------------------------------------------
// Phase 2 — the form + live summary / test pane
// ---------------------------------------------------------------------------

interface BuilderFormProps {
  draft: BuilderDraft;
  setDraft: React.Dispatch<React.SetStateAction<BuilderDraft>>;
  draftSaved: boolean;
  created: BotEntryWithSchema | null;
  onCreated: (entry: BotEntryWithSchema) => void;
  onBackToTemplates: () => void;
  onGoToBotPage: (slug: string) => void;
}

function BuilderForm({
  draft,
  setDraft,
  draftSaved,
  created,
  onCreated,
  onBackToTemplates,
  onGoToBotPage,
}: BuilderFormProps) {
  const [creating, setCreating] = useState(false);
  const [createError, setCreateError] = useState<string | null>(null);

  const slug = useMemo(() => deriveSlug(draft.name), [draft.name]);
  const slugValid = isValidSlug(slug);

  const patch = (p: Partial<BuilderDraft>) => setDraft((d) => ({ ...d, ...p }));

  // Rows that are entirely empty are ignored (dropped on submit); any
  // partially-filled row must carry a valid, unique name.
  const activeVars = draft.vars.filter(
    (v) => v.name.trim() !== "" || v.default !== "" || v.description !== "",
  );
  const varNamesValid =
    activeVars.every((v) => isValidVarName(v.name.trim())) &&
    new Set(activeVars.map((v) => v.name.trim())).size === activeVars.length;

  const costValid =
    draft.maxCostUsd.trim() === "" ||
    (Number.isFinite(Number(draft.maxCostUsd)) && Number(draft.maxCostUsd) > 0);

  const canCreate =
    draft.name.trim() !== "" &&
    slugValid &&
    draft.instructions.trim() !== "" &&
    varNamesValid &&
    costValid &&
    !creating &&
    created === null;

  const onCreate = async () => {
    setCreating(true);
    setCreateError(null);
    try {
      const spec: BotCreateSpec = {
        slug,
        display_name: draft.name.trim(),
        instructions: draft.instructions.trim(),
        ...(draft.icon ? { icon: draft.icon } : {}),
        ...(draft.description.trim() ? { description: draft.description.trim() } : {}),
        ...(draft.whenToUse.trim() ? { when_to_use: draft.whenToUse.trim() } : {}),
        ...(draft.model.trim() ? { model: draft.model.trim() } : {}),
        ...(draft.backend ? { backend: draft.backend } : {}),
        ...(draft.skills.length > 0 ? { skills: draft.skills } : {}),
        ...(draft.capabilities.length > 0 ? { capabilities: draft.capabilities } : {}),
        ...(activeVars.length > 0
          ? {
              vars: activeVars.map((v) => ({
                name: v.name.trim(),
                type: v.type,
                ...(v.default !== "" ? { default: v.default } : {}),
                ...(v.description.trim() ? { description: v.description.trim() } : {}),
              })),
            }
          : {}),
        ...(draft.worktree ? { worktree: true } : {}),
        ...(draft.sandbox ? { sandbox: true } : {}),
        ...(draft.permission !== "off" ? { permission: draft.permission } : {}),
        ...(draft.maxCostUsd.trim() !== "" ? { max_cost_usd: Number(draft.maxCostUsd) } : {}),
        ...(draft.maxDuration.trim() !== "" ? { max_duration: draft.maxDuration.trim() } : {}),
        ...(draft.scheduleCron.trim() !== "" ? { schedule_cron: draft.scheduleCron.trim() } : {}),
      };
      const entry = await createBot(spec);
      onCreated(entry);
    } catch (e) {
      // 409 (duplicate slug) and 400 (validation) carry explicit server
      // messages — surface them verbatim.
      setCreateError(errorMessage(e));
    } finally {
      setCreating(false);
    }
  };

  const createdFile = created ? botLaunchFile(created) : null;

  return (
    <div className="mx-auto w-full max-w-7xl p-4">
      <div className="grid items-start gap-4 xl:grid-cols-[minmax(0,1fr)_minmax(360px,480px)]">
        {/* ------------------------------------------------ left: form */}
        <div className="flex flex-col gap-3">
          <div className="flex flex-wrap items-center gap-2">
            <h1 className="text-base font-semibold text-fg-default">
              {created ? "Bot created" : "New bot"}
            </h1>
            {!created && (
              <button
                type="button"
                onClick={onBackToTemplates}
                className="text-caption text-fg-subtle hover:text-fg-default hover:underline"
              >
                ← Templates
              </button>
            )}
            <span
              aria-live="polite"
              className={`ml-auto text-caption text-fg-subtle transition-opacity ${draftSaved ? "opacity-100" : "opacity-0"}`}
            >
              Draft saved
            </span>
          </div>

          {created && (
            <InlineBanner tone="success" title={`${created.display_name?.trim() || created.name} is live`}>
              Scaffolded into <code className="font-mono">bots/{created.name}/</code> and visible
              to the orchestrator catalog. Give it a first spin in the test pane, or head to its
              page to wire triggers.
            </InlineBanner>
          )}

          <fieldset disabled={created !== null} className="flex min-w-0 flex-col gap-3">
            <PrimaryFieldsCard draft={draft} patch={patch} slug={slug} slugValid={slugValid} />
            <ModelSkillsCard draft={draft} patch={patch} />
            <VarsEditorCard draft={draft} patch={patch} />
            <AdvancedCard draft={draft} patch={patch} costValid={costValid} />

            {createError && (
              <InlineBanner tone="danger" title="Couldn't create the bot">
                {createError}
              </InlineBanner>
            )}

            {created === null && (
              <div className="flex flex-col gap-1.5">
                <div>
                  <Button
                    variant="primary"
                    size="md"
                    onClick={() => void onCreate()}
                    disabled={!canCreate}
                    loading={creating}
                  >
                    {creating ? "Creating…" : "Create bot"}
                  </Button>
                </div>
                <p className="text-caption text-fg-subtle">
                  The bot lands in <code className="font-mono">bots/{slugValid ? slug : "<slug>"}/</code>{" "}
                  and becomes visible to the orchestrator catalog.
                </p>
              </div>
            )}
          </fieldset>
        </div>

        {/* --------------------------------- right: summary / test pane */}
        <div className="flex min-w-0 flex-col gap-3 xl:sticky xl:top-4">
          {created ? (
            <>
              <Button variant="primary" size="md" onClick={() => onGoToBotPage(created.name)}>
                Go to bot page →
              </Button>
              {createdFile ? (
                <TestRunPane file={createdFile} vars={created.vars?.fields ?? []} />
              ) : (
                <InlineBanner tone="warning" title="Test pane unavailable">
                  The server couldn&apos;t relativise the new bot&apos;s path to the workspace —
                  open its page and launch it from there.
                </InlineBanner>
              )}
            </>
          ) : (
            <SummaryCard draft={draft} slug={slug} slugValid={slugValid} varCount={activeVars.length} />
          )}
        </div>
      </div>
    </div>
  );
}

// ---------------------------------------------------------------------------
// Form sections
// ---------------------------------------------------------------------------

function PrimaryFieldsCard({
  draft,
  patch,
  slug,
  slugValid,
}: {
  draft: BuilderDraft;
  patch: (p: Partial<BuilderDraft>) => void;
  slug: string;
  slugValid: boolean;
}) {
  return (
    <Card>
      <div className="flex items-start gap-3">
        <EmojiPicker
          onSelect={(emoji) => patch({ icon: emoji })}
          trigger={
            <button
              type="button"
              aria-label={draft.icon ? `Icon ${draft.icon} — change` : "Pick an emoji icon"}
              title="Pick an emoji icon for this bot"
              className="flex h-12 w-12 shrink-0 items-center justify-center rounded-md border border-border-strong bg-surface-1 text-2xl leading-none transition-colors hover:border-accent disabled:cursor-not-allowed disabled:opacity-60"
            >
              {draft.icon || "🤖"}
            </button>
          }
        />
        <div className="min-w-0 flex-1">
          <FieldLabel htmlFor="bot-name">Name</FieldLabel>
          <Input
            id="bot-name"
            type="text"
            value={draft.name}
            onChange={(e) => patch({ name: e.target.value })}
            placeholder="Review Bot"
            autoFocus
          />
          <p
            className={`mt-1 font-mono text-caption ${
              draft.name.trim() === ""
                ? "text-fg-subtle"
                : slugValid
                  ? "text-fg-subtle"
                  : "text-danger-fg"
            }`}
          >
            {draft.name.trim() === ""
              ? "bots/<slug>/ — derived from the name"
              : slugValid
                ? `bots/${slug}/`
                : "Name must derive a valid slug: starts with a letter, ≥ 2 chars of a-z 0-9 -"}
          </p>
        </div>
      </div>

      <div className="mt-3">
        <FieldLabel htmlFor="bot-description">Description</FieldLabel>
        <Input
          id="bot-description"
          type="text"
          value={draft.description}
          onChange={(e) => patch({ description: e.target.value })}
          placeholder="One line on what this bot does"
        />
      </div>

      <div className="mt-3">
        <FieldLabel htmlFor="bot-instructions">Instructions</FieldLabel>
        <Textarea
          id="bot-instructions"
          value={draft.instructions}
          onChange={(e) => patch({ instructions: e.target.value })}
          rows={8}
          placeholder={
            "You are a focused reviewer. Read the diff, flag correctness bugs, verify by running the tests…"
          }
        />
        <p className="mt-1 text-caption text-fg-subtle">
          This is the agent&apos;s system prompt — say what to do and how to verify it.
        </p>
      </div>
    </Card>
  );
}

function ModelSkillsCard({
  draft,
  patch,
}: {
  draft: BuilderDraft;
  patch: (p: Partial<BuilderDraft>) => void;
}) {
  const report = useBackendDetectStore((s) => s.report);
  const detectLoading = useBackendDetectStore((s) => s.loading);
  const refreshDetect = useBackendDetectStore((s) => s.refresh);
  useEffect(() => {
    if (!report && !detectLoading) void refreshDetect();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  const [skillCatalog, setSkillCatalog] = useState<LibrarySkill[] | null>(null);
  const [skillsNote, setSkillsNote] = useState<string | null>(null);
  useEffect(() => {
    let cancelled = false;
    listLocalSkills()
      .then((skills) => {
        if (!cancelled) setSkillCatalog(skills);
      })
      .catch((e: unknown) => {
        if (cancelled) return;
        setSkillCatalog([]);
        setSkillsNote(
          e instanceof FeatureUnavailableError
            ? "The skills library isn't available on this server — skills can't be browsed here."
            : `Couldn't load the skill library: ${errorMessage(e)}`,
        );
      });
    return () => {
      cancelled = true;
    };
  }, []);

  const toggleSkill = (name: string) =>
    patch({
      skills: draft.skills.includes(name)
        ? draft.skills.filter((s) => s !== name)
        : [...draft.skills, name],
    });

  const modelSuggestions = useMemo(() => {
    const set = new Set<string>();
    for (const p of report?.providers ?? []) {
      if (p.available && p.suggested_model) set.add(p.suggested_model);
    }
    return [...set].sort();
  }, [report]);
  const suggestionsId = "bot-builder-model-suggestions";

  // Skills referenced by the draft (e.g. from a template) that the
  // catalog doesn't know — keep them visible + removable.
  const knownNames = new Set((skillCatalog ?? []).map((s) => s.name));
  const orphanSkills = draft.skills.filter((s) => !knownNames.has(s));

  return (
    <Card>
      <SectionTitle>Model &amp; skills</SectionTitle>

      <div className="mt-2 grid gap-3 sm:grid-cols-2">
        <div>
          <FieldLabel htmlFor="bot-backend">Backend</FieldLabel>
          <Select
            id="bot-backend"
            value={draft.backend}
            onChange={(e) => patch({ backend: e.currentTarget.value })}
          >
            <option value="">
              Auto (detected{report?.resolved_default ? ` — ${report.resolved_default}` : ""})
            </option>
            {(report?.backends ?? []).map((b) => (
              <option key={b.name} value={b.name} disabled={!b.available}>
                {b.name}
                {b.available ? "" : " — no credential"}
              </option>
            ))}
          </Select>
        </div>
        <div>
          <FieldLabel htmlFor="bot-model">Model</FieldLabel>
          <Input
            id="bot-model"
            type="text"
            list={suggestionsId}
            value={draft.model}
            onChange={(e) => patch({ model: e.target.value })}
            placeholder="Auto (detected)"
            className="font-mono"
          />
          <datalist id={suggestionsId}>
            {modelSuggestions.map((m) => (
              <option key={m} value={m} />
            ))}
          </datalist>
        </div>
      </div>
      <p className="mt-1.5 text-caption text-fg-subtle">
        Leave both empty for auto-detection from the host&apos;s credentials — the recommended
        default.
      </p>

      <div className="mt-3">
        <FieldLabel>Skills</FieldLabel>
        {skillsNote && (
          <p className="mb-1.5 text-caption text-warning" role="note">
            {skillsNote}
          </p>
        )}
        {skillCatalog === null && !skillsNote ? (
          <div className="flex items-center gap-2 py-1 text-xs text-fg-muted">
            <Spinner size="sm" /> Loading skills…
          </div>
        ) : (
          <>
            {(skillCatalog ?? []).length === 0 && !skillsNote && (
              <p className="text-caption text-fg-subtle">
                No skills in the library yet — add some under the Skills view and they become
                attachable here.
              </p>
            )}
            <div className="flex flex-wrap gap-1.5">
              {(skillCatalog ?? []).map((s) => {
                const selected = draft.skills.includes(s.name);
                return (
                  <button
                    key={`${s.scope}:${s.name}`}
                    type="button"
                    onClick={() => toggleSkill(s.name)}
                    aria-pressed={selected}
                    title={s.description || s.name}
                    className={`rounded-full border px-2 py-0.5 font-mono text-caption transition-colors ${
                      selected
                        ? "border-accent bg-accent-soft text-fg-default"
                        : "border-border-default bg-surface-2 text-fg-muted hover:border-border-strong hover:text-fg-default"
                    }`}
                  >
                    {selected ? "☑ " : ""}
                    {s.name}
                  </button>
                );
              })}
              {orphanSkills.map((name) => (
                <span
                  key={name}
                  className="inline-flex items-center gap-1 rounded-full border border-border-default bg-surface-2 px-2 py-0.5 font-mono text-caption text-fg-muted"
                >
                  {name}
                  <button
                    type="button"
                    onClick={() => toggleSkill(name)}
                    aria-label={`Remove skill ${name}`}
                    className="text-fg-subtle hover:text-fg-default"
                  >
                    <Cross1Icon className="h-2.5 w-2.5" />
                  </button>
                </span>
              ))}
            </div>
          </>
        )}
      </div>
    </Card>
  );
}

function VarsEditorCard({
  draft,
  patch,
}: {
  draft: BuilderDraft;
  patch: (p: Partial<BuilderDraft>) => void;
}) {
  const setRow = (i: number, p: Partial<VarRow>) =>
    patch({ vars: draft.vars.map((v, j) => (j === i ? { ...v, ...p } : v)) });
  const removeRow = (i: number) => patch({ vars: draft.vars.filter((_, j) => j !== i) });
  const addRow = () =>
    patch({ vars: [...draft.vars, { name: "", type: "string", default: "", description: "" }] });

  const names = draft.vars.map((v) => v.name.trim());

  return (
    <Card>
      <div className="flex items-center justify-between">
        <SectionTitle>Variables</SectionTitle>
        <Button variant="secondary" size="sm" onClick={addRow}>
          <PlusIcon className="mr-1 h-3 w-3" />
          Add variable
        </Button>
      </div>
      {draft.vars.length === 0 ? (
        <p className="mt-1 text-caption text-fg-subtle">
          Optional launch-time inputs (<code className="font-mono">{"{{vars.name}}"}</code> in the
          instructions).
        </p>
      ) : (
        <div className="mt-2 flex flex-col gap-2">
          {draft.vars.map((v, i) => {
            const trimmed = v.name.trim();
            const rowActive = trimmed !== "" || v.default !== "" || v.description !== "";
            const nameInvalid = rowActive && !isValidVarName(trimmed);
            const duplicate =
              rowActive && trimmed !== "" && names.filter((n) => n === trimmed).length > 1;
            return (
              <div key={i} className="rounded-md border border-border-default bg-surface-2 p-2">
                <div className="grid grid-cols-[minmax(0,1fr)_96px_minmax(0,1fr)_auto] items-center gap-2">
                  <Input
                    type="text"
                    value={v.name}
                    onChange={(e) => setRow(i, { name: e.target.value })}
                    placeholder="name"
                    aria-label={`Variable ${i + 1} name`}
                    size="sm"
                    className="font-mono"
                    error={nameInvalid || duplicate}
                  />
                  <Select
                    value={v.type}
                    onChange={(e) => setRow(i, { type: e.currentTarget.value as VarType })}
                    aria-label={`Variable ${i + 1} type`}
                  >
                    {VAR_TYPES.map((t) => (
                      <option key={t} value={t}>
                        {t}
                      </option>
                    ))}
                  </Select>
                  <Input
                    type="text"
                    value={v.default}
                    onChange={(e) => setRow(i, { default: e.target.value })}
                    placeholder="default (empty = required)"
                    aria-label={`Variable ${i + 1} default`}
                    size="sm"
                    className="font-mono"
                  />
                  <IconButton
                    label={`Remove variable ${trimmed || i + 1}`}
                    size="sm"
                    variant="ghost"
                    onClick={() => removeRow(i)}
                  >
                    <Cross1Icon className="h-3 w-3" />
                  </IconButton>
                </div>
                <Input
                  type="text"
                  value={v.description}
                  onChange={(e) => setRow(i, { description: e.target.value })}
                  placeholder="description (optional)"
                  aria-label={`Variable ${i + 1} description`}
                  size="sm"
                  className="mt-1.5"
                />
                {(nameInvalid || duplicate) && (
                  <p className="mt-1 text-caption text-danger-fg" role="alert">
                    {nameInvalid
                      ? "Var names must match ^[a-z_][a-z0-9_]*$ (snake_case)."
                      : "Duplicate var name."}
                  </p>
                )}
              </div>
            );
          })}
        </div>
      )}
    </Card>
  );
}

function AdvancedCard({
  draft,
  patch,
  costValid,
}: {
  draft: BuilderDraft;
  patch: (p: Partial<BuilderDraft>) => void;
  costValid: boolean;
}) {
  const [open, setOpen] = useState(false);
  const configured = [
    draft.worktree,
    draft.sandbox,
    draft.permission !== "off",
    draft.maxCostUsd.trim() !== "",
    draft.maxDuration.trim() !== "",
    draft.scheduleCron.trim() !== "",
  ].filter(Boolean).length;

  return (
    <Card>
      <button
        type="button"
        className="flex items-center gap-2 text-xs font-medium text-fg-muted hover:text-fg-default"
        onClick={() => setOpen((v) => !v)}
        aria-expanded={open}
      >
        <span>{open ? "▾" : "▸"}</span>
        <span>Advanced</span>
        {configured > 0 && (
          <span className="rounded-full bg-accent-soft px-1.5 py-0.5 text-caption text-accent-fg">
            {configured} set
          </span>
        )}
      </button>

      {open && (
        <div className="mt-3 flex flex-col gap-3">
          <div className="flex flex-col gap-2">
            <Checkbox
              label="Run in an isolated git worktree"
              help="Commits land on a storage branch and merge back only on a clean finish."
              checked={draft.worktree}
              onChange={(e) => patch({ worktree: e.target.checked })}
            />
            <Checkbox
              label="Run in a sandbox container"
              help="Per-run container isolation (sandbox: auto — devcontainer or the published slim image)."
              checked={draft.sandbox}
              onChange={(e) => patch({ sandbox: e.target.checked })}
            />
          </div>

          <div className="grid gap-3 sm:grid-cols-3">
            <div>
              <FieldLabel
                htmlFor="bot-permission"
                help="ask pauses for approval on non-allow-listed tool calls; deny hard-blocks them."
              >
                Permission gate
              </FieldLabel>
              <Select
                id="bot-permission"
                value={draft.permission}
                onChange={(e) =>
                  patch({ permission: e.currentTarget.value as BuilderDraft["permission"] })
                }
              >
                <option value="off">off (default)</option>
                <option value="ask">ask</option>
                <option value="deny">deny</option>
              </Select>
            </div>
            <div>
              <FieldLabel htmlFor="bot-max-cost">Max cost (USD)</FieldLabel>
              <Input
                id="bot-max-cost"
                type="number"
                min="0"
                step="0.5"
                value={draft.maxCostUsd}
                onChange={(e) => patch({ maxCostUsd: e.target.value })}
                placeholder="unlimited"
                error={!costValid}
              />
              {!costValid && (
                <p className="mt-1 text-caption text-danger-fg" role="alert">
                  Must be a number &gt; 0 (or empty).
                </p>
              )}
            </div>
            <div>
              <FieldLabel htmlFor="bot-max-duration">Max duration</FieldLabel>
              <Input
                id="bot-max-duration"
                type="text"
                value={draft.maxDuration}
                onChange={(e) => patch({ maxDuration: e.target.value })}
                placeholder="2h"
                className="font-mono"
              />
            </div>
          </div>

          <div>
            <FieldLabel
              htmlFor="bot-schedule"
              help="5-field cron — offered as a one-click trigger on the bot page."
            >
              Schedule (cron)
            </FieldLabel>
            <Input
              id="bot-schedule"
              type="text"
              value={draft.scheduleCron}
              onChange={(e) => patch({ scheduleCron: e.target.value })}
              placeholder="0 7 * * 1-5"
              className="max-w-56 font-mono"
            />
            {(() => {
              const human = draft.scheduleCron.trim()
                ? humanizeCron(draft.scheduleCron.trim())
                : null;
              return human ? (
                <p className="mt-1 text-caption text-fg-muted" aria-live="polite">
                  Runs {human}
                </p>
              ) : null;
            })()}
          </div>
        </div>
      )}
    </Card>
  );
}

// ---------------------------------------------------------------------------
// Right column — live summary (pre-create)
// ---------------------------------------------------------------------------

function SummaryCard({
  draft,
  slug,
  slugValid,
  varCount,
}: {
  draft: BuilderDraft;
  slug: string;
  slugValid: boolean;
  varCount: number;
}) {
  return (
    <Card>
      <div className="flex items-start gap-3">
        <span
          className="flex h-12 w-12 shrink-0 items-center justify-center rounded-md border border-border-default bg-surface-2 text-2xl leading-none"
          aria-hidden="true"
        >
          {draft.icon || "🤖"}
        </span>
        <div className="min-w-0 flex-1">
          <div className="text-sm font-semibold text-fg-default">
            {draft.name.trim() || <span className="italic text-fg-subtle">Unnamed bot</span>}
          </div>
          {slugValid && <div className="font-mono text-caption text-fg-subtle">bots/{slug}/</div>}
          <p className="mt-0.5 text-xs text-fg-muted">
            {draft.description.trim() || (
              <span className="italic text-fg-subtle">No description yet.</span>
            )}
          </p>
        </div>
      </div>

      <div className="mt-3 flex flex-wrap gap-1">
        <Badge variant={draft.backend || draft.model ? "info" : "neutral"}>
          {draft.backend || draft.model
            ? [draft.backend, draft.model].filter(Boolean).join(" · ")
            : "auto backend"}
        </Badge>
        {draft.skills.length > 0 && (
          <Badge variant="info">
            {draft.skills.length} skill{draft.skills.length === 1 ? "" : "s"}
          </Badge>
        )}
        {varCount > 0 && (
          <Badge>
            {varCount} var{varCount === 1 ? "" : "s"}
          </Badge>
        )}
        {draft.worktree && <Badge variant="accent">worktree</Badge>}
        {draft.sandbox && <Badge variant="accent">sandbox</Badge>}
        {draft.permission !== "off" && <Badge variant="warning">permission: {draft.permission}</Badge>}
        {draft.maxCostUsd.trim() !== "" && <Badge>≤ ${draft.maxCostUsd}</Badge>}
        {draft.maxDuration.trim() !== "" && <Badge>≤ {draft.maxDuration}</Badge>}
        {draft.scheduleCron.trim() !== "" && (
          <Chip>
            <span className="font-mono">{draft.scheduleCron.trim()}</span>
            {humanizeCron(draft.scheduleCron.trim()) && (
              <span className="ml-1 text-fg-muted">
                — {humanizeCron(draft.scheduleCron.trim())}
              </span>
            )}
          </Chip>
        )}
      </div>

      <p className="mt-3 text-caption text-fg-subtle">
        Once created, this panel becomes a live test pane — run the bot and chat with it without
        leaving the page.
      </p>
    </Card>
  );
}

// ---------------------------------------------------------------------------

function SectionTitle({ children }: { children: React.ReactNode }) {
  return <h2 className="text-xs font-semibold text-fg-default">{children}</h2>;
}
