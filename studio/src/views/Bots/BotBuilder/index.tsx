// BotBuilderView — /bots/new. Guided (Mistral-style) bot creation in
// two phases on one page: a template gallery (GET /api/v1/bots/templates),
// then a focused form — four primary fields, a compact model & skills
// group, a vars editor, and a collapsed Advanced section — with a live
// summary card on the right that swaps to an embedded TestRunPane once
// the bot is created, so the create → test → iterate loop never leaves
// the page. Drafts auto-save to localStorage. Local mode only: the
// server refuses POST /api/v1/bots with 403 in cloud mode, so the whole
// flow is gated on serverInfo.mode.
//
// This file is the orchestrator: the draft model + create-spec mapping
// live in model.ts, the cross-phase state in useBuilderDraft.ts, and
// each form section in its own sibling component.

import { useMemo, useState } from "react";
import { Link, useLocation } from "wouter";

import { createBot, type BotEntryWithSchema } from "@/api/bots";
import { botSourceEditorPath } from "@/api/client";
import TestRunPane from "@/components/Runs/TestRunPane";
import { useHeaderSlot } from "@/components/shared/useHeaderSlot";
import { useAuth } from "@/auth/AuthContext";
import { Button, InlineBanner } from "@/components/ui";
import { errorMessage } from "@/lib/errorHints";
import { useServerInfoStore } from "@/store/serverInfo";
import { useTabsStore } from "@/store/tabs";

import { botLaunchFile } from "../botPaths";
import AdvancedCard from "./AdvancedCard";
import {
  activeVarRows,
  buildCreateSpec,
  costInputValid,
  draftFromTemplate,
  varNamesValid,
  type BuilderDraft,
} from "./model";
import ModelSkillsCard from "./ModelSkillsCard";
import PrimaryFieldsCard from "./PrimaryFieldsCard";
import { deriveSlug, isValidSlug } from "./slug";
import SummaryCard from "./SummaryCard";
import TemplateGallery from "./TemplateGallery";
import { useBuilderDraft } from "./useBuilderDraft";
import VarsEditorCard from "./VarsEditorCard";

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

  // Cloud without a bot store wired can't persist a new bot — fall back to the
  // browse-catalog guidance. With the store wired, the builder works in cloud
  // (it scaffolds into the team-authored bot store instead of the filesystem).
  if (serverInfo?.mode === "cloud" && !serverInfo?.bot_editing_enabled) {
    return (
      <div className="mx-auto w-full max-w-2xl p-4">
        <InlineBanner tone="info" title="Bot creation is unavailable on this server">
          This cloud server has no team-authored bot store wired, so a new bot has nowhere to
          persist. Ask an administrator to enable bot editing, or run <code>iterion studio</code>
          against your repo locally to build a bot and push it. You can also browse the catalog
          and marketplace and enable a bot on a connected repository.
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
  const { draft, setDraft, draftSaved, created, onCreated } = useBuilderDraft();

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
  const [, setLocation] = useLocation();
  const [creating, setCreating] = useState(false);
  const [createError, setCreateError] = useState<string | null>(null);
  const isCloud = useServerInfoStore((s) => s.info?.mode === "cloud");
  const { activeTeamID } = useAuth();

  const slug = useMemo(() => deriveSlug(draft.name), [draft.name]);
  const slugValid = isValidSlug(slug);

  const patch = (p: Partial<BuilderDraft>) => setDraft((d) => ({ ...d, ...p }));

  const activeVars = activeVarRows(draft.vars);
  const varsValid = varNamesValid(activeVars);
  const costValid = costInputValid(draft.maxCostUsd);

  const canCreate =
    draft.name.trim() !== "" &&
    slugValid &&
    draft.instructions.trim() !== "" &&
    varsValid &&
    costValid &&
    !creating &&
    created === null;

  const onCreate = async () => {
    setCreating(true);
    setCreateError(null);
    try {
      const entry = await createBot(buildCreateSpec(draft, slug, activeVars));
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
              {isCloud ? (
                <>
                  Saved to your team&apos;s bot store and visible to the orchestrator catalog. Open
                  it in the editor to refine it, or head to its page to wire triggers.
                </>
              ) : (
                <>
                  Scaffolded into <code className="font-mono">bots/{created.name}/</code> and visible
                  to the orchestrator catalog. Give it a first spin in the test pane, or head to its
                  page to wire triggers.
                </>
              )}
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
              {isCloud && (
                <Button
                  variant="secondary"
                  size="md"
                  onClick={() => {
                    const file = botSourceEditorPath(activeTeamID, created.name);
                    useTabsStore.getState().openTab("editor", { file });
                    setLocation(`/editor?file=${encodeURIComponent(file)}`);
                  }}
                >
                  Open in editor
                </Button>
              )}
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
