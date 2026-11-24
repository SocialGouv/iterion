import type { BotEntryWithSchema } from "@/api/bots";

/**
 * botLaunchFile returns the workspace-relative workflow path the launch
 * (`/runs/new?file=`) and editor (`/editor?file=`) surfaces expect:
 * `<rel_path>/main.bot` for bundles (same rule as the Catalog dialog's
 * bundleMainRel), the .bot file itself for loose entries. Null when the
 * server couldn't relativise the path (no workspace root) — callers
 * disable the affordance.
 */
export function botLaunchFile(b: BotEntryWithSchema): string | null {
  if (!b.rel_path) return null;
  return b.is_bundle ? `${b.rel_path}/main.bot` : b.rel_path;
}

/** How a registry bot is launched: by its slug — the server resolves the
 *  tier and the run carries the bot's identity — or by a workflow file. */
export type BotLaunch = { by: "bot"; bot: string } | { by: "file"; file: string };

/**
 * botLaunch is the ONE decision of how the studio launches a registry bot,
 * for every affordance that offers it (a Launch button, a Test pane, a link
 * to the launch form):
 *   - in cloud, by slug. Every bot the registry lists is launchable: a
 *     stored bot (a team's own, a platform override) has no workspace path,
 *     and a path would name another tier's bytes anyway.
 *   - locally, by the workspace file botLaunchFile names — null when the
 *     server could not relativise it, and the affordance is then disabled.
 */
export function botLaunch(b: BotEntryWithSchema, cloud: boolean): BotLaunch | null {
  if (cloud) return { by: "bot", bot: b.name };
  const file = botLaunchFile(b);
  return file ? { by: "file", file } : null;
}

/** The launch form's URL for a bot launched that way. */
export function botLaunchHref(launch: BotLaunch): string {
  return launch.by === "bot"
    ? `/runs/new?bot=${encodeURIComponent(launch.bot)}`
    : `/runs/new?file=${encodeURIComponent(launch.file)}`;
}

/** What the studio knows of the caller, for the one question a launch FORM
 *  asks before it opens: may they read this bot's program. */
export interface ProgramReader {
  /** May edit the bots of the active team (the server's canEditBots): a bot
   *  editor, an admin or an owner of the team or of its org, a platform
   *  admin. The one fact the rule asks of the caller — a platform override
   *  and the baked catalog are every signed-in member's. */
  editsTeamBots: boolean;
}

/**
 * Why the launch FORM of a registry bot cannot open for this caller in cloud,
 * or null when it can. The form shows the bot's program, which the server
 * serves by one rule (GET /api/v1/bots/{name}/launch-source,
 * botLaunchSourceReadable): a team's own bot to those who may edit the team's
 * bots; a platform override and the baked catalog to every signed-in member —
 * both are what the platform serves every tenant. An affordance that leads to
 * a form the server refuses
 * is a dead end, so each one that offers the form asks here first — from the
 * entry's origin and the session, never a request per bot.
 *
 * It is the studio's reading of the server's rule, and the server stays the
 * authority: an origin the studio does not know reads as openable, and the
 * form then says the server's own refusal.
 *
 * Launching the bot BY ID shows no program and stays everyone's who may
 * launch — the Test pane, the CLI, the API, MCP — which is what the sentence
 * offers in the form's place.
 */
export function botLaunchFormRefusal(
  b: BotEntryWithSchema,
  cloud: boolean,
  reader: ProgramReader,
): string | null {
  if (!cloud) return null;
  const byID = `You can still launch it by id, which shows no program: \`iterion remote runs launch --bot ${b.name}\` from the CLI, or bot_id through the API or MCP.`;
  if (b.origin === "tenant" && !reader.editsTeamBots) {
    return `The launch form shows this bot's program, and a team's own bot is readable by a bot editor, a team admin or an owner. ${byID}`;
  }
  return null;
}
