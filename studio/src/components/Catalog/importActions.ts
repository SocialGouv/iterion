// Shared bot-import actions — the upload/install logic behind both the
// Catalog manager dialog and the /bots gallery "Import" affordance.
// Pure API orchestration (no React): callers own busy state + toasts.

import {
  installBot,
  uploadBotBundle,
  type InstallBotResult,
} from "@/api/bots";

export type { InstallBotResult };

/** importBotzFile imports a `.botz` archive into the workspace,
 *  overwriting an existing install of the same name (the "update"
 *  path — matches the historical Catalog dialog behaviour). */
export function importBotzFile(file: File): Promise<InstallBotResult> {
  return uploadBotBundle(file, { force: true });
}

export interface RepoImportInput {
  url: string;
  /** Git ref (branch/tag), optional. */
  ref?: string;
  /** Subpath or bot name within the repository, optional. */
  path?: string;
}

/** importBotFromRepo imports a bot bundle from a git URL (or a local
 *  path on a self-hosted server) into the workspace. */
export function importBotFromRepo(input: RepoImportInput): Promise<InstallBotResult> {
  return installBot({
    url: input.url.trim(),
    ref: input.ref?.trim() || undefined,
    path: input.path?.trim() || undefined,
  });
}

/** importSuccessMessage renders the shared success toast copy. */
export function importSuccessMessage(res: InstallBotResult): string {
  return `Imported ${res.name} (${res.presets} presets, ${res.skills} skills) → ${res.installed_path}`;
}
