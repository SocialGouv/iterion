// Extracted from RunHeader.tsx to keep that file focused.
// "What bot ran this?" chip in Row 2 of the RunHeader. Leads with the
// manifest persona when set, otherwise the technical workflow name.

import { FileTextIcon, OpenInNewWindowIcon } from "@radix-ui/react-icons";

import type { RunHeader as RunHeaderType } from "@/api/runs";
import { Tooltip } from "@/components/ui";
import { botIdentity } from "@/lib/personas";

// BotChip renders the "what bot ran this?" cell in Row 2 of the run
// header. The previous chip showed only the file basename ("main.bot"),
// which was ambiguous: every iterion bot's entrypoint is called
// main.bot. We now lead with the workflow's declared name (the
// `workflow <name>:` token in the DSL — e.g. "feature_dev") and add
// the bundle's manifest name when it exists and differs, so the
// operator can tell a feature_dev run from a docs-refresh run at a
// glance. The basename + file path stay reachable via tooltip + the
// click-to-open-in-editor affordance.
export default function BotChip({
  run,
  fileBase,
  onOpenFile,
}: {
  run: RunHeaderType;
  fileBase: string | null;
  onOpenFile: (path: string) => void;
}) {
  const workflowName = run.workflow_name || "";
  // Bundle name diverges from workflow_name only when the .botz
  // manifest's `name:` field was customised (e.g. bundle "docs-refresh"
  // ships `workflow docs_refresh:`). Render it as a secondary chip in
  // that case; suppress when redundant.
  const bundleName = run.bundle_name?.trim() ?? "";
  const personaName = run.bundle_display_name?.trim() ?? "";
  // Per-bot emoji + accent colour (presentation), keyed by the bot's
  // technical id; deterministic fallback for unknown bots.
  const identity = botIdentity(bundleName || workflowName);
  const normalisedWorkflow = workflowName.replace(/[-_]/g, "");
  const normalisedBundle = bundleName.replace(/[-_]/g, "");
  const showBundleAside =
    bundleName.length > 0 &&
    normalisedBundle.toLowerCase() !== normalisedWorkflow.toLowerCase();
  const techPrimary = workflowName || bundleName || fileBase || "(unnamed)";
  // When the manifest declares a persona (display_name), it becomes
  // the lead chip — that's the "Nexie" / "Billy" the operator thinks
  // in. The technical workflow_name moves to a muted aside so it
  // stays one click away. Without a persona, the technical name is
  // the lead and the chip falls back to the prior layout.
  const tooltip = run.file_path ?? techPrimary;
  return (
    <Tooltip content={tooltip}>
      <button
        type="button"
        className="inline-flex items-center gap-1 hover:text-fg-default focus:outline-none min-w-0"
        onClick={() => run.file_path && onOpenFile(run.file_path)}
        disabled={!run.file_path}
        title={
          run.file_path
            ? `Open ${run.file_path} in the editor`
            : "Workflow source path not recorded for this run"
        }
      >
        {personaName ? (
          <span
            // A glyph with a text alternative — aria-label on a roleless
            // span is prohibited (axe aria-prohibited-attr).
            role="img"
            className="shrink-0 text-[0.95em] leading-none"
            aria-label="persona bot"
          >
            {identity.emoji}
          </span>
        ) : (
          <FileTextIcon className="w-3 h-3 shrink-0" />
        )}
        {personaName ? (
          <>
            <span className={`truncate max-w-[12rem] font-medium ${identity.color}`}>
              {personaName}
            </span>
            <span className="font-mono truncate max-w-[14rem] text-fg-subtle">
              · {techPrimary}
            </span>
          </>
        ) : (
          <>
            <span className="font-mono truncate max-w-[18rem] text-fg-default">
              {techPrimary}
            </span>
            {showBundleAside && (
              <span className="font-mono truncate max-w-[12rem] text-fg-subtle">
                · {bundleName}
              </span>
            )}
          </>
        )}
        {run.file_path && (
          <OpenInNewWindowIcon className="w-2.5 h-2.5 opacity-70 shrink-0" />
        )}
      </button>
    </Tooltip>
  );
}
