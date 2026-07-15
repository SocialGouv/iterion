// Reconciliation between marketplace entries and the artifacts already
// installed locally — bots in the workspace (.botz/), plugins in
// ~/.iterion/plugins/. A registry entry's `name` is the manifest name,
// which is also the installed artifact's `name` — so we match on that,
// within the entry's kind (a bot and a plugin sharing a name never
// cross-match).

import type { BotEntry } from "@/api/bots";
import type { MarketplaceEntry } from "@/api/marketplace";
import type { PluginView } from "@/api/plugins";

/** InstalledState is the tri-state a marketplace card renders:
 *  - "absent"    → not in the workspace (offer Install)
 *  - "installed" → present, same version (offer Uninstall)
 *  - "update"    → present, registry version is newer (offer Update) */
export type InstalledState = "absent" | "installed" | "update";

/** installedVersions maps installed bot name → its manifest version, the
 *  lookup the marketplace view builds once per refresh. */
export type InstalledVersions = Map<string, string | undefined>;

/** buildInstalledVersions indexes the workspace bot list by name. */
export function buildInstalledVersions(bots: BotEntry[]): InstalledVersions {
  const m: InstalledVersions = new Map();
  for (const b of bots) m.set(b.name, b.version);
  return m;
}

/** buildInstalledPluginVersions indexes the plugin registry views by
 *  name — the plugin-kind counterpart of buildInstalledVersions. */
export function buildInstalledPluginVersions(plugins: PluginView[]): InstalledVersions {
  const m: InstalledVersions = new Map();
  for (const p of plugins) m.set(p.name, p.version);
  return m;
}

/** resolveInstalledState computes a card's tri-state from the registry
 *  entry and the installed-versions indexes — bots for bot entries,
 *  plugins for plugin entries. An entry is "update" only when both
 *  versions parse and the registry's is strictly newer; otherwise a
 *  present artifact is "installed" (never silently flagged stale on a
 *  version we can't compare). */
export function resolveInstalledState(
  entry: MarketplaceEntry,
  installedBots: InstalledVersions,
  installedPlugins: InstalledVersions = new Map(),
): InstalledState {
  const installed = (entry.kind ?? "bot") === "plugin" ? installedPlugins : installedBots;
  if (!installed.has(entry.name)) return "absent";
  const have = installed.get(entry.name);
  if (entry.version && have && compareVersions(entry.version, have) > 0) {
    return "update";
  }
  return "installed";
}

/** compareVersions does a lightweight dotted-numeric comparison
 *  (1.2.10 > 1.2.9). Non-numeric segments fall back to a string compare
 *  of that segment. Returns -1, 0, or 1. A leading "v" is ignored. */
export function compareVersions(a: string, b: string): number {
  const pa = a.replace(/^v/, "").split(".");
  const pb = b.replace(/^v/, "").split(".");
  const n = Math.max(pa.length, pb.length);
  for (let i = 0; i < n; i++) {
    const sa = pa[i] ?? "0";
    const sb = pb[i] ?? "0";
    const na = Number(sa);
    const nb = Number(sb);
    if (Number.isInteger(na) && Number.isInteger(nb)) {
      if (na !== nb) return na < nb ? -1 : 1;
    } else if (sa !== sb) {
      return sa < sb ? -1 : 1;
    }
  }
  return 0;
}
