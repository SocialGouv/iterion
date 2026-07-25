// Extracted from BotBuilder/index.tsx to keep that file focused.
// The live summary card on the right column (pre-create) — identity
// preview plus badges for everything the draft configures.

import { Badge, Card, Chip } from "@/components/ui";
import { humanizeCron } from "@/lib/humanizeCron";

import type { BuilderDraft } from "./model";

export default function SummaryCard({
  draft,
  slug,
  slugValid,
  varCount,
}: {
  draft: BuilderDraft;
  slug: string;
  slugValid: boolean;
  varCount: number;
}) {
  return (
    <Card>
      <div className="flex items-start gap-3">
        <span
          className="flex h-12 w-12 shrink-0 items-center justify-center rounded-md border border-border-default bg-surface-2 text-2xl leading-none"
          aria-hidden="true"
        >
          {draft.icon || "🤖"}
        </span>
        <div className="min-w-0 flex-1">
          <div className="text-sm font-semibold text-fg-default">
            {draft.name.trim() || <span className="italic text-fg-subtle">Unnamed bot</span>}
          </div>
          {slugValid && <div className="font-mono text-caption text-fg-subtle">bots/{slug}/</div>}
          <p className="mt-0.5 text-xs text-fg-muted">
            {draft.description.trim() || (
              <span className="italic text-fg-subtle">No description yet.</span>
            )}
          </p>
        </div>
      </div>

      <div className="mt-3 flex flex-wrap gap-1">
        <Badge variant={draft.backend || draft.model ? "info" : "neutral"}>
          {draft.backend || draft.model
            ? [draft.backend, draft.model].filter(Boolean).join(" · ")
            : "auto backend"}
        </Badge>
        {draft.skills.length > 0 && (
          <Badge variant="info">
            {draft.skills.length} skill{draft.skills.length === 1 ? "" : "s"}
          </Badge>
        )}
        {varCount > 0 && (
          <Badge>
            {varCount} var{varCount === 1 ? "" : "s"}
          </Badge>
        )}
        {draft.worktree && <Badge variant="accent">worktree</Badge>}
        {draft.sandbox && <Badge variant="accent">sandbox</Badge>}
        {draft.permission !== "off" && <Badge variant="warning">permission: {draft.permission}</Badge>}
        {draft.maxCostUsd.trim() !== "" && <Badge>≤ ${draft.maxCostUsd}</Badge>}
        {draft.maxDuration.trim() !== "" && <Badge>≤ {draft.maxDuration}</Badge>}
        {draft.scheduleCron.trim() !== "" && (
          <Chip>
            <span className="font-mono">{draft.scheduleCron.trim()}</span>
            {humanizeCron(draft.scheduleCron.trim()) && (
              <span className="ml-1 text-fg-muted">
                — {humanizeCron(draft.scheduleCron.trim())}
              </span>
            )}
          </Chip>
        )}
      </div>

      <p className="mt-3 text-caption text-fg-subtle">
        Once created, this panel becomes a live test pane — run the bot and chat with it without
        leaving the page.
      </p>
    </Card>
  );
}
