// Extracted from BotBuilder/index.tsx to keep that file focused.
// The four primary fields — icon, name (with derived-slug feedback),
// description, and the instructions textarea.

import { Card, FieldLabel, Input, Textarea } from "@/components/ui";
import { EmojiPicker } from "@/components/ui/EmojiPicker";

import type { BuilderDraft, PatchDraft } from "./model";

export default function PrimaryFieldsCard({
  draft,
  patch,
  slug,
  slugValid,
}: {
  draft: BuilderDraft;
  patch: PatchDraft;
  slug: string;
  slugValid: boolean;
}) {
  return (
    <Card>
      <div className="flex items-start gap-3">
        <EmojiPicker
          onSelect={(emoji) => patch({ icon: emoji })}
          trigger={
            <button
              type="button"
              aria-label={draft.icon ? `Icon ${draft.icon} — change` : "Pick an emoji icon"}
              title="Pick an emoji icon for this bot"
              className="flex h-12 w-12 shrink-0 items-center justify-center rounded-md border border-border-strong bg-surface-1 text-2xl leading-none transition-colors hover:border-accent disabled:cursor-not-allowed disabled:opacity-60"
            >
              {draft.icon || "🤖"}
            </button>
          }
        />
        <div className="min-w-0 flex-1">
          <FieldLabel htmlFor="bot-name">Name</FieldLabel>
          <Input
            id="bot-name"
            type="text"
            value={draft.name}
            onChange={(e) => patch({ name: e.target.value })}
            placeholder="Review Bot"
            autoFocus
          />
          <p
            className={`mt-1 font-mono text-caption ${
              draft.name.trim() === ""
                ? "text-fg-subtle"
                : slugValid
                  ? "text-fg-subtle"
                  : "text-danger-fg"
            }`}
          >
            {draft.name.trim() === ""
              ? "bots/<slug>/ — derived from the name"
              : slugValid
                ? `bots/${slug}/`
                : "Name must derive a valid slug: starts with a letter, ≥ 2 chars of a-z 0-9 -"}
          </p>
        </div>
      </div>

      <div className="mt-3">
        <FieldLabel htmlFor="bot-description">Description</FieldLabel>
        <Input
          id="bot-description"
          type="text"
          value={draft.description}
          onChange={(e) => patch({ description: e.target.value })}
          placeholder="One line on what this bot does"
        />
      </div>

      <div className="mt-3">
        <FieldLabel htmlFor="bot-instructions">Instructions</FieldLabel>
        <Textarea
          id="bot-instructions"
          value={draft.instructions}
          onChange={(e) => patch({ instructions: e.target.value })}
          rows={8}
          placeholder={
            "You are a focused reviewer. Read the diff, flag correctness bugs, verify by running the tests…"
          }
        />
        <p className="mt-1 text-caption text-fg-subtle">
          This is the agent&apos;s system prompt — say what to do and how to verify it.
        </p>
      </div>
    </Card>
  );
}
