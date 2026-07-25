import type { ForgeEnablePreview } from "@/api/forgeConnections";

// Pure model for the bind-bot wizard (/integrations/bind): URL-driven step
// resolution with prefill skipping, back navigation that honours arrival
// prefills, returnTo sanitizing, and the dry-run preview view-model. Kept
// free of React so the transitions stay unit-testable.

export type BindStep = "repo" | "bot" | "review" | "done";

export const BIND_STEP_ORDER: BindStep[] = ["repo", "bot", "review", "done"];

export const BIND_STEP_LABEL: Record<BindStep, string> = {
  repo: "Repository",
  bot: "Bot",
  review: "Review",
  done: "Done",
};

export interface BindQueryState {
  /** Explicit ?step= value (may be absent or garbage). */
  step?: string | null;
  /** Selected/prefilled repo key (forgeTeamRepoKey). */
  repo?: string | null;
  /** Selected/prefilled bot name. */
  bot?: string | null;
}

/** The first step whose input is still missing — the auto-skip rule:
 *  a prefilled repo/bot never shows its picker step. */
export function firstIncompleteBindStep(hasRepo: boolean, hasBot: boolean): BindStep {
  if (!hasRepo) return "repo";
  if (!hasBot) return "bot";
  return "review";
}

/**
 * Resolve the active step from the URL. An explicit valid ?step= wins,
 * but a forward step whose prerequisites are missing (hand-mangled URL,
 * stale link) degrades to the first incomplete step instead of rendering
 * a broken review/done screen.
 */
export function resolveBindStep(q: BindQueryState): BindStep {
  const hasRepo = !!(q.repo ?? "").trim();
  const hasBot = !!(q.bot ?? "").trim();
  const s = (q.step ?? "") as BindStep;
  if (!BIND_STEP_ORDER.includes(s)) return firstIncompleteBindStep(hasRepo, hasBot);
  if ((s === "review" || s === "done") && (!hasRepo || !hasBot)) {
    return firstIncompleteBindStep(hasRepo, hasBot);
  }
  if (s === "bot" && !hasRepo) return "repo";
  return s;
}

/**
 * The step the Back button targets, skipping steps whose value arrived as
 * an entry prefill (a bot page fixes the bot; a repo page fixes the repo).
 * Null = nothing to go back to (hide the button).
 */
export function prevBindStep(
  step: BindStep,
  prefill: { repo: boolean; bot: boolean },
): BindStep | null {
  const idx = BIND_STEP_ORDER.indexOf(step);
  for (let i = idx - 1; i >= 0; i--) {
    const s = BIND_STEP_ORDER[i];
    if (!s || s === "done") continue;
    if (s === "bot" && prefill.bot) continue;
    if (s === "repo" && prefill.repo) continue;
    return s;
  }
  return null;
}

/**
 * Only accept an in-app path as returnTo — "/x…" but not "//host" (a
 * protocol-relative URL would be an open redirect). Null = no return.
 */
export function sanitizeReturnTo(raw: string | null | undefined): string | null {
  const v = (raw ?? "").trim();
  if (!v.startsWith("/") || v.startsWith("//")) return null;
  return v;
}

/** Existing bound bots + the newly selected one, deduplicated, order kept. */
export function unionBotIds(existing: string[], next: string): string[] {
  const out = [...existing];
  if (next && !out.includes(next)) out.push(next);
  return out;
}

/** Build the /integrations/bind URL with optional prefills. */
export function bindBotPath(opts: {
  repoKey?: string;
  bot?: string;
  returnTo?: string;
}): string {
  const p = new URLSearchParams();
  if (opts.repoKey) p.set("repo", opts.repoKey);
  if (opts.bot) p.set("bot", opts.bot);
  if (opts.returnTo) p.set("returnTo", opts.returnTo);
  const qs = p.toString();
  return qs ? `/integrations/bind?${qs}` : "/integrations/bind";
}

/* ------------------------- preview view-model ------------------------ */

export interface BindPreviewModel {
  /** Forge-native event names the hook will subscribe to (falls back to
   *  the normalized names when the server didn't send native ones). */
  events: string[];
  /** Required token scopes: scope → why it's needed. */
  scopes: Array<{ scope: string; reason: string }>;
  /** Slash-commands the bots add to the webhook. */
  commands: Array<{ command: string; botId: string }>;
  /** Secret bindings the bots need; `missing` when the team has no secret
   *  by that name (never flagged when the secret list is unknown). */
  secrets: Array<{ botId: string; secret: string; missing: boolean }>;
  hasMissingSecrets: boolean;
  identity: { handle: string; provider: string; baseUrl: string } | null;
  conflicts: string[];
  hasConflicts: boolean;
}

/**
 * Normalize the dry-run response into what the review step renders.
 * Every field degrades gracefully when absent (older servers).
 * `knownSecretNames` null = the secret list couldn't be read — don't
 * flag anything as missing on unknown data.
 */
export function buildBindPreviewModel(
  preview: ForgeEnablePreview | null | undefined,
  knownSecretNames: string[] | null,
): BindPreviewModel | null {
  if (!preview) return null;
  const native = preview.forge_native_events ?? [];
  const events = native.length > 0 ? native : (preview.events_normalized ?? []);
  const scopes = Object.entries(preview.scopes ?? {}).map(([scope, reason]) => ({
    scope,
    reason: reason ?? "",
  }));
  scopes.sort((a, b) => a.scope.localeCompare(b.scope));
  const commands = (preview.commands ?? []).map((c) => ({
    command: c.command,
    botId: c.bot_id,
  }));
  const secrets = (preview.secrets ?? []).map((s) => ({
    botId: s.bot_id,
    secret: s.secret,
    missing: knownSecretNames !== null && !knownSecretNames.includes(s.secret),
  }));
  const identity = preview.identity?.handle
    ? {
        handle: preview.identity.handle,
        provider: preview.identity.provider ?? "",
        baseUrl: preview.identity.base_url ?? "",
      }
    : null;
  const conflicts = preview.conflicts ?? [];
  return {
    events,
    scopes,
    commands,
    secrets,
    hasMissingSecrets: secrets.some((s) => s.missing),
    identity,
    conflicts,
    hasConflicts: conflicts.length > 0,
  };
}
