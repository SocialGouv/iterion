// What the editor knows of the bot it edits: which registry bot the open file
// is (resolveOpenBot — the inverse of every way the studio opens one), and
// the sources its enabled subscriptions listen to (sourcesOf, read by the
// start node's chip, #3275).
import type { BotEntryWithSchema } from "@/api/bots";
import { parseBotSourceEditorPath } from "@/api/client";
import type { TriggerSubscription } from "@/api/triggers";
import { botLaunchFile } from "@/views/Bots/botPaths";

// resolveOpenBot finds the registry bot the editor's open file is:
//   - a workspace bundle opened at `<rel_path>/main.bot`, a loose bot at its
//     own `rel_path` (botLaunchFile, the opener the bot pages use);
//   - a team's or the platform's bot opened from its source as
//     `botsource://<team>/<slug>/main.bot`, in the active team only.
// A file that is none of these — a scratch file, another file of a bundle, a
// path the server could not relativise — is no bot.
export function resolveOpenBot(
  bots: BotEntryWithSchema[] | null,
  filePath: string | null,
  activeTeamID: string,
): BotEntryWithSchema | undefined {
  if (!bots || !filePath) return undefined;
  const source = parseBotSourceEditorPath(filePath);
  if (source) {
    if (source.rel !== "main.bot" || source.teamID === "" || source.teamID !== activeTeamID) return undefined;
    return bots.find((b) => (b.origin === "tenant" || b.origin === "platform") && b.name === source.slug);
  }
  return bots.find((b) => {
    const file = botLaunchFile(b);
    return file !== null && file !== "" && file === filePath;
  });
}

// What a family reads as on the chip, in the studio's language.
const FAMILY_LABELS: Record<string, string> = {
  forge: "forge",
  command: "commande",
  schedule: "planifié",
  keepalive: "permanent",
  board: "tableau",
  run: "fin de run",
  custom: "personnalisé",
  manual: "manuel",
};

// familyOf names a non-connector subscription's family: its invocation, or —
// a trigger created on the triggers page carries none — the source it
// matches.
function familyOf(s: TriggerSubscription): string {
  const key = s.invocation || s.match?.sources?.[0] || "";
  return FAMILY_LABELS[key] ?? key;
}

// A source as the chip shows it: a connector (its logo) or a family (its
// label), failing when one of its subscriptions says it did not fire.
export interface ChipSource {
  kind: "connector" | "family";
  id: string;
  failing: boolean;
}

// sourcesOf reads the enabled subscriptions as the chip shows them: each
// connector once, in id order, then each other family once; a source is
// failing when any of its subscriptions carries a last error (a launch
// refused, a poller that stopped reading). Nothing is ever shown unnamed.
export function sourcesOf(subs: TriggerSubscription[]): ChipSource[] {
  const connectors = new Map<string, boolean>();
  const families = new Map<string, boolean>();
  for (const s of subs) {
    if (!s.enabled) continue;
    const failing = (s.last_error ?? "") !== "";
    if (s.invocation === "connector") {
      const id = (s.trigger_id ?? "").split(".")[0];
      if (id) connectors.set(id, (connectors.get(id) ?? false) || failing);
      continue;
    }
    const family = familyOf(s);
    if (family) families.set(family, (families.get(family) ?? false) || failing);
  }
  const sorted = (m: Map<string, boolean>, kind: ChipSource["kind"]): ChipSource[] =>
    [...m.entries()].sort(([a], [b]) => a.localeCompare(b)).map(([id, failing]) => ({ kind, id, failing }));
  return [...sorted(connectors, "connector"), ...sorted(families, "family")];
}
