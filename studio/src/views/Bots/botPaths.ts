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
