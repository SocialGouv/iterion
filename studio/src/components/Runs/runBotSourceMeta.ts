import type { RunHeader } from "@/api/runs";

// Which bundle actually ran, for display. The server resolves a bot
// through team → platform → baked and stamps the tier that served
// (`bot_source_tier`); this is the single place the studio turns that
// stamp into words.
export interface BotSourceTierMeta {
  // label is the short chip text ("team bot", "platform override", …).
  label: string;
  // detail is the sentence a tooltip shows.
  detail: string;
  // notable marks a tier that is NOT the bundle shipped in the image, so
  // a surface can badge only the cases worth interrupting a reader for.
  notable: boolean;
}

// botSourceTierMeta returns null for any run whose tier this build cannot
// name — unrecorded (a local run, or one launched before the stamp
// existed) or a value a newer server introduced.
//
// Null means RENDER NOTHING. It must never fall back to "baked": #871 was
// a tier that had silently stopped applying while every surface kept
// reading as though it had, and a default would rebuild exactly that. A
// missing answer is shown as missing.
export function botSourceTierMeta(run: RunHeader): BotSourceTierMeta | null {
  switch (run.bot_source_tier?.trim()) {
    case "team":
      return {
        label: "team bot",
        detail: `Served by this team's own bot source${
          run.bot_source_tenant ? ` (${run.bot_source_tenant})` : ""
        } — a fork or a bot only this team has, not the bundle baked into the image.`,
        notable: true,
      };
    case "platform":
      return {
        label: "platform override",
        detail:
          "Served by a deployment-wide platform override, not the bundle baked into the image.",
        notable: true,
      };
    case "baked":
      return {
        label: "baked catalog",
        detail: "Served by the bot catalog baked into this build's image.",
        notable: false,
      };
    default:
      return null;
  }
}
