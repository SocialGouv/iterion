// What a `/runs/new?bot=<slug>` URL names, decided in one place.
//
// Under a `?bot=` URL nothing but that bot may be launched. So the slug
// resolves to the bot — by name in cloud, by its workflow file locally — or
// the form WAITS (the facts that decide are still on their way), or it
// REFUSES by name. It never falls through to the editor's buffer or to an
// inline launch of some path: an operator who followed a bot's link would be
// shown, and would launch, a program that link did not name.

import type { BotEntryWithSchema } from "@/api/bots";
import type { ServerInfo } from "@/api/types";
import { botLaunch } from "@/views/Bots/botPaths";

export type BotSlugTarget =
  /** The URL names no bot (no `?bot=`, or `?file=` beside it, which wins). */
  | { state: "none" }
  /** The mode, or locally the catalog, has not answered yet. */
  | { state: "pending" }
  /** Cloud: the bot is launched by name. */
  | { state: "bot"; bot: string }
  /** Local: the bot is launched by its workflow file. */
  | { state: "file"; file: string }
  /** The slug names nothing this form may launch; `reason` says why. */
  | { state: "refused"; reason: string };

export function resolveBotSlug({
  slug,
  serverInfo,
  serverInfoPending,
  bots,
  botsError,
}: {
  /** The `?bot=` slug, "" when the URL names none. */
  slug: string;
  serverInfo: Pick<ServerInfo, "mode"> | null;
  /** True until the server info answered or failed. */
  serverInfoPending: boolean;
  bots: BotEntryWithSchema[] | null;
  botsError: string | null;
}): BotSlugTarget {
  if (!slug) return { state: "none" };
  if (serverInfoPending) return { state: "pending" };
  if (!serverInfo) {
    // Settled with no answer: the mode is UNKNOWN, which is not "local". A
    // cloud server read as local would open the bot's baked file — or the
    // editor's buffer — and launch it unnamed.
    return {
      state: "refused",
      reason: `Cannot open the launch form of bot “${slug}”: the server did not say whether it runs in cloud or local mode (its info did not load), and the two launch a bot differently. Reload the page.`,
    };
  }
  if (serverInfo.mode === "cloud") return { state: "bot", bot: slug };
  if (bots === null) {
    return botsError
      ? {
          state: "refused",
          reason: `Cannot open the launch form of bot “${slug}”: the bot catalog did not load (${botsError}). Reload the page.`,
        }
      : { state: "pending" };
  }
  const entry = bots.find((b) => b.name === slug);
  if (!entry) {
    return {
      state: "refused",
      reason: `No bot named “${slug}” in this workspace's catalog — nothing is launched in its place.`,
    };
  }
  const launch = botLaunch(entry, false);
  if (!launch || launch.by !== "file") {
    return {
      state: "refused",
      reason: `Bot “${slug}” has no workflow file under this workspace (the server could not relativise its path), so its launch form cannot be opened here — launch it from its own directory.`,
    };
  }
  return { state: "file", file: launch.file };
}
