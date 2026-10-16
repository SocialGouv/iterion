// The copy of the Marketplace view's success toasts, as pure functions: a
// bot install, update, uninstall and bundle upload each confirm a gesture
// that also refreshed the orchestrator's bot catalog, so each carries the
// cause when that refresh failed (catalogStaleSuffix, empty on success).

import type { InstallBotResult } from "@/api/bots";
import { catalogStaleSuffix, type CatalogRefreshed } from "@/lib/catalogStale";

/** installedMessage confirms a bot entry's install (force=false) or update
 *  (force=true) and where its bundle landed. */
export function installedMessage(install: InstallBotResult, force: boolean): string {
  return `${force ? "Updated" : "Installed"} ${install.name} → ${install.installed_path}${catalogStaleSuffix(install)}`;
}

/** uninstalledMessage confirms the removal of a bot entry's bundle from the
 *  workspace. The registry entry itself is untouched. */
export function uninstalledMessage(name: string, removed: CatalogRefreshed): string {
  return `Uninstalled ${name}${catalogStaleSuffix(removed)}`;
}

/** uploadedMessage confirms a `.botz` upload (the submit panel's
 *  "Import from file" affordance) and where the bundle landed. */
export function uploadedMessage(uploaded: InstallBotResult): string {
  return `Imported ${uploaded.name} → ${uploaded.installed_path}${catalogStaleSuffix(uploaded)}`;
}
