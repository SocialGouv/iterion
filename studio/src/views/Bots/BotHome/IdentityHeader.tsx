// Extracted from BotHome/index.tsx to keep that file focused.
// Identity header — emoji (editable), names, chips, activation toggle
// with the manifest-default reset affordance.

import { useState } from "react";

import type { BotEntryWithSchema } from "@/api/bots";
import BotIdentity from "@/components/shared/BotIdentity";
import { Badge, Button, Card, Chip } from "@/components/ui";
import { EmojiPicker } from "@/components/ui/EmojiPicker";
import { botVisual } from "@/lib/personas";
import { useBotsStore } from "@/store/bots";
import { useUIStore } from "@/store/ui";

export default function IdentityHeader({ entry }: { entry: BotEntryWithSchema }) {
  const saveBot = useBotsStore((s) => s.saveBot);
  const setOverlay = useBotsStore((s) => s.setOverlay);
  const addToast = useUIStore((s) => s.addToast);
  const [busy, setBusy] = useState(false);

  const identity = botVisual(entry);
  const label = entry.display_name?.trim() || entry.name;
  const enabled = entry.enabled !== false;
  const manifestEnabled = entry.manifest_enabled !== false;
  const overridden = enabled !== manifestEnabled;

  const onPickIcon = async (emoji: string) => {
    try {
      await saveBot(entry.name, { icon: emoji });
      addToast(`Icon updated for ${label}`, "success");
    } catch (e) {
      addToast(e instanceof Error ? e.message : "Failed to save icon", "error");
    }
  };

  const onToggle = async () => {
    setBusy(true);
    try {
      await setOverlay(entry.name, !enabled);
      addToast(
        !enabled ? `${label} enabled — exposed to Nexie` : `${label} disabled — hidden from Nexie`,
        !enabled ? "success" : "info",
      );
    } catch (e) {
      addToast(e instanceof Error ? e.message : `Failed to update ${label}`, "error");
    } finally {
      setBusy(false);
    }
  };

  const onReset = async () => {
    setBusy(true);
    try {
      await setOverlay(entry.name, null);
      addToast(`${label} follows its manifest default again`, "info");
    } catch (e) {
      addToast(e instanceof Error ? e.message : `Failed to reset ${label}`, "error");
    } finally {
      setBusy(false);
    }
  };

  return (
    <Card>
      <div className="flex items-start gap-3">
        <BotIdentity
          bot={entry}
          size="lg"
          className="min-w-0 flex-1"
          avatar={
            entry.is_bundle ? (
              <EmojiPicker
                onSelect={(emoji) => void onPickIcon(emoji)}
                trigger={
                  <button
                    type="button"
                    aria-label={`Icon ${identity.emoji} — change`}
                    title="Pick an emoji icon for this bot"
                    className="flex h-12 w-12 shrink-0 items-center justify-center rounded-md border border-border-strong bg-surface-1 text-2xl leading-none transition-colors hover:border-accent"
                  >
                    {identity.emoji}
                  </button>
                }
              />
            ) : undefined
          }
          nameExtras={!entry.is_bundle && <Badge>single file</Badge>}
          meta={
            <div className="mt-1.5 flex flex-wrap gap-1">
              {entry.version && <Chip>v{entry.version}</Chip>}
              {entry.author && <Chip>by {entry.author}</Chip>}
            </div>
          }
        />

        <div className="flex shrink-0 flex-col items-end gap-1">
          <Button
            variant={enabled ? "success" : "secondary"}
            size="sm"
            role="switch"
            aria-checked={enabled}
            disabled={busy}
            onClick={() => void onToggle()}
            title={enabled ? "Disable (hide from Nexie)" : "Enable (expose to Nexie)"}
          >
            {enabled ? "Enabled" : "Disabled"}
          </Button>
          <span className="text-caption text-fg-subtle">
            Default from manifest: {manifestEnabled ? "On" : "Off"}
          </span>
          {overridden && (
            <button
              type="button"
              onClick={() => void onReset()}
              disabled={busy}
              className="text-caption text-accent-text hover:underline"
            >
              Reset to default
            </button>
          )}
        </div>
      </div>
    </Card>
  );
}
