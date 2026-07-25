// Extracted from BotBuilder/index.tsx to keep that file focused.
// The collapsed Advanced section — worktree/sandbox toggles, permission
// gate, budget caps, and the cron schedule with its humanized preview.

import { useState } from "react";

import { Card, Checkbox, FieldLabel, Input, Select } from "@/components/ui";
import { humanizeCron } from "@/lib/humanizeCron";

import type { BuilderDraft, PatchDraft } from "./model";

export default function AdvancedCard({
  draft,
  patch,
  costValid,
}: {
  draft: BuilderDraft;
  patch: PatchDraft;
  costValid: boolean;
}) {
  const [open, setOpen] = useState(false);
  const configured = [
    draft.worktree,
    draft.sandbox,
    draft.permission !== "off",
    draft.maxCostUsd.trim() !== "",
    draft.maxDuration.trim() !== "",
    draft.scheduleCron.trim() !== "",
  ].filter(Boolean).length;

  return (
    <Card>
      <button
        type="button"
        className="flex items-center gap-2 text-xs font-medium text-fg-muted hover:text-fg-default"
        onClick={() => setOpen((v) => !v)}
        aria-expanded={open}
      >
        <span>{open ? "▾" : "▸"}</span>
        <span>Advanced</span>
        {configured > 0 && (
          <span className="rounded-full bg-accent-soft px-1.5 py-0.5 text-caption text-accent-fg">
            {configured} set
          </span>
        )}
      </button>

      {open && (
        <div className="mt-3 flex flex-col gap-3">
          <div className="flex flex-col gap-2">
            <Checkbox
              label="Run in an isolated git worktree"
              help="Commits land on a storage branch and merge back only on a clean finish."
              checked={draft.worktree}
              onChange={(e) => patch({ worktree: e.target.checked })}
            />
            <Checkbox
              label="Run in a sandbox container"
              help="Per-run container isolation (sandbox: auto — devcontainer or the published slim image)."
              checked={draft.sandbox}
              onChange={(e) => patch({ sandbox: e.target.checked })}
            />
          </div>

          <div className="grid gap-3 sm:grid-cols-3">
            <div>
              <FieldLabel
                htmlFor="bot-permission"
                help="ask pauses for approval on non-allow-listed tool calls; deny hard-blocks them."
              >
                Permission gate
              </FieldLabel>
              <Select
                id="bot-permission"
                value={draft.permission}
                onChange={(e) =>
                  patch({ permission: e.currentTarget.value as BuilderDraft["permission"] })
                }
              >
                <option value="off">off (default)</option>
                <option value="ask">ask</option>
                <option value="deny">deny</option>
              </Select>
            </div>
            <div>
              <FieldLabel htmlFor="bot-max-cost">Max cost (USD)</FieldLabel>
              <Input
                id="bot-max-cost"
                type="number"
                min="0"
                step="0.5"
                value={draft.maxCostUsd}
                onChange={(e) => patch({ maxCostUsd: e.target.value })}
                placeholder="unlimited"
                error={!costValid}
              />
              {!costValid && (
                <p className="mt-1 text-caption text-danger-fg" role="alert">
                  Must be a number &gt; 0 (or empty).
                </p>
              )}
            </div>
            <div>
              <FieldLabel htmlFor="bot-max-duration">Max duration</FieldLabel>
              <Input
                id="bot-max-duration"
                type="text"
                value={draft.maxDuration}
                onChange={(e) => patch({ maxDuration: e.target.value })}
                placeholder="2h"
                className="font-mono"
              />
            </div>
          </div>

          <div>
            <FieldLabel
              htmlFor="bot-schedule"
              help="5-field cron — offered as a one-click trigger on the bot page."
            >
              Schedule (cron)
            </FieldLabel>
            <Input
              id="bot-schedule"
              type="text"
              value={draft.scheduleCron}
              onChange={(e) => patch({ scheduleCron: e.target.value })}
              placeholder="0 7 * * 1-5"
              className="max-w-56 font-mono"
            />
            {(() => {
              const human = draft.scheduleCron.trim()
                ? humanizeCron(draft.scheduleCron.trim())
                : null;
              return human ? (
                <p className="mt-1 text-caption text-fg-muted" aria-live="polite">
                  Runs {human}
                </p>
              ) : null;
            })()}
          </div>
        </div>
      )}
    </Card>
  );
}
