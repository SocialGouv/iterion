import { useEffect, useMemo, useRef, useState } from "react";

import type { BotEntryWithSchema, BotPatch } from "@/api/bots";
import { CheckboxField, TagListField, TextField } from "@/components/Panels/forms/FormField";
import { Button } from "@/components/ui/Button";
import { EmojiPicker } from "@/components/ui/EmojiPicker";
import { botIdentity } from "@/lib/personas";
import { useBotsStore } from "@/store/bots";
import { useUIStore } from "@/store/ui";

const AUTO_SAVE_DELAY_MS = 800;

interface Draft {
  display_name: string;
  description: string;
  when_to_use: string;
  triggers: string[];
  author: string;
  version: string;
  icon: string;
  enabled: boolean; // edits the MANIFEST default (manifest_enabled)
}

function toDraft(b: BotEntryWithSchema): Draft {
  return {
    display_name: b.display_name ?? "",
    description: b.description ?? "",
    when_to_use: b.when_to_use ?? "",
    triggers: b.triggers ?? [],
    author: b.author ?? "",
    version: b.version ?? "",
    icon: b.icon ?? "",
    enabled: b.manifest_enabled !== false,
  };
}

function toPatch(d: Draft): BotPatch {
  return {
    display_name: d.display_name.trim(),
    description: d.description,
    when_to_use: d.when_to_use,
    triggers: d.triggers,
    author: d.author.trim(),
    version: d.version.trim(),
    icon: d.icon.trim(),
    enabled: d.enabled,
  };
}

/**
 * BotMetadataForm is the Inspector "Bot" tab — edits a bundle's
 * manifest.yaml (persona, description, the Nexie-facing "when to use",
 * triggers, author/version, and the catalog default). Mounted with
 * `key={bot.name}` so switching files re-seeds the draft.
 *
 * Edits AUTO-SAVE, debounced: a change schedules a PUT after a short
 * pause; a failed save keeps the draft and shows the error inline (a
 * later edit retries). Until the user touches a field the draft follows
 * outside updates to the entry (e.g. the bot-home header icon picker),
 * so autosave can never revert a change made elsewhere. A pending edit
 * is flushed when the form unmounts.
 *
 * The catalog checkbox writes the manifest DEFAULT; a workspace overlay
 * (set via the Catalog manager) can override it locally — surfaced as a
 * note when the resolved `enabled` diverges from the manifest default.
 */
export default function BotMetadataForm({ bot }: { bot: BotEntryWithSchema }) {
  const saveBot = useBotsStore((s) => s.saveBot);
  const addToast = useUIStore((s) => s.addToast);
  const [draft, setDraft] = useState<Draft>(() => toDraft(bot));
  const [saving, setSaving] = useState(false);
  const [saveError, setSaveError] = useState<string | null>(null);
  const [savedFlash, setSavedFlash] = useState(false);

  const baseline = useMemo(() => toDraft(bot), [bot]);
  const dirty = useMemo(
    () => JSON.stringify(draft) !== JSON.stringify(baseline),
    [draft, baseline],
  );
  const overlayDiffers = bot.enabled !== bot.manifest_enabled;

  // touched = the user edited since the last acknowledged save; while
  // false the draft tracks outside changes to the entry instead of
  // fighting them.
  const touchedRef = useRef(false);
  const draftRef = useRef(draft);
  useEffect(() => {
    draftRef.current = draft;
  }, [draft]);
  // Last patch acknowledged by the server (or errored — no auto-retry
  // until the user edits again, the error stays visible instead).
  const settledPatchRef = useRef<string | null>(null);
  const pendingRef = useRef<BotPatch | null>(null);

  useEffect(() => {
    if (!touchedRef.current) setDraft(baseline);
  }, [baseline]);

  const update = <K extends keyof Draft>(k: K, v: Draft[K]) => {
    touchedRef.current = true;
    setDraft((d) => ({ ...d, [k]: v }));
  };

  const doSave = async (patch: BotPatch) => {
    setSaving(true);
    setSaveError(null);
    setSavedFlash(false);
    try {
      await saveBot(bot.name, patch);
      settledPatchRef.current = JSON.stringify(patch);
      if (JSON.stringify(toPatch(draftRef.current)) === settledPatchRef.current) {
        touchedRef.current = false;
      }
      setSavedFlash(true);
    } catch (e) {
      settledPatchRef.current = JSON.stringify(patch);
      setSaveError(e instanceof Error ? e.message : "Failed to save bot metadata");
    } finally {
      setSaving(false);
    }
  };

  useEffect(() => {
    const patch = toPatch(draft);
    const json = JSON.stringify(patch);
    if (json === JSON.stringify(toPatch(baseline)) || json === settledPatchRef.current) {
      pendingRef.current = null;
      return;
    }
    pendingRef.current = patch;
    const t = window.setTimeout(() => {
      pendingRef.current = null;
      void doSave(patch);
    }, AUTO_SAVE_DELAY_MS);
    return () => window.clearTimeout(t);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [draft, baseline]);

  // Flush a still-debouncing edit on unmount (tab switch, navigation).
  useEffect(
    () => () => {
      const pending = pendingRef.current;
      if (!pending) return;
      saveBot(bot.name, pending).catch((e) => {
        addToast(
          e instanceof Error ? e.message : "Failed to save bot metadata",
          "error",
        );
      });
    },
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [],
  );

  return (
    <div className="h-full overflow-y-auto p-3">
      <div className="mb-3">
        <div className="mb-1 text-xs text-fg-subtle">Bot (technical name — immutable)</div>
        <div className="font-mono text-sm text-fg-default">{bot.name}</div>
      </div>

      <div className="mb-2">
        <div className="mb-1 text-xs text-fg-subtle">Icon</div>
        <div className="flex items-center gap-2">
          <EmojiPicker
            onSelect={(emoji) => update("icon", emoji)}
            trigger={
              <button
                type="button"
                aria-label={draft.icon ? `Icon ${draft.icon} — change` : "Pick an icon"}
                title="Pick an emoji icon for this bot"
                className="flex h-9 w-9 items-center justify-center rounded-md border border-border-strong bg-surface-1 text-lg leading-none transition-colors hover:border-accent"
              >
                {draft.icon || (
                  <span className="text-fg-subtle" aria-hidden="true">
                    {botIdentity(bot.name).emoji}
                  </span>
                )}
              </button>
            }
          />
          {draft.icon ? (
            <Button variant="ghost" size="sm" onClick={() => update("icon", "")}>
              Clear
            </Button>
          ) : (
            <span className="text-caption text-fg-subtle">
              No manifest icon — using the built-in default.
            </span>
          )}
        </div>
      </div>

      <TextField
        label="Persona name (display_name)"
        value={draft.display_name}
        onChange={(v) => update("display_name", v)}
        placeholder="e.g. Nexie"
      />
      <TextField
        label="Description"
        value={draft.description}
        onChange={(v) => update("description", v)}
        multiline
        rows={4}
      />
      <TextField
        label="When to use (shown to Nexie)"
        help="Nexie reads this to decide whether to route a task to this bot — like a skill's “when to use it”."
        value={draft.when_to_use}
        onChange={(v) => update("when_to_use", v)}
        multiline
        rows={3}
        placeholder="Use when…"
      />
      <TagListField
        label="Triggers"
        values={draft.triggers}
        onChange={(v) => update("triggers", v)}
        placeholder="Add trigger…"
      />
      <div className="grid grid-cols-2 gap-2">
        <TextField label="Author" value={draft.author} onChange={(v) => update("author", v)} />
        <TextField label="Version" value={draft.version} onChange={(v) => update("version", v)} />
      </div>

      <div className="mt-2 border-t border-border-default pt-2">
        <CheckboxField
          label="Active in catalog (exposed to Nexie)"
          checked={draft.enabled}
          onChange={(v) => update("enabled", v)}
          help="When on, Nexie can route tasks to this bot and it shows in the board bot picker. This sets the bot's manifest default; the Catalog manager can override it per-workspace."
        />
        {overlayDiffers && (
          <p className="mt-0.5 text-caption text-warning">
            Locally overridden: this workspace currently treats it as{" "}
            {bot.enabled ? "enabled" : "disabled"} (via the Catalog manager),
            regardless of the manifest default above.
          </p>
        )}
      </div>

      <div className="mt-3 flex items-center gap-2" aria-live="polite">
        {saveError ? (
          <span className="text-caption text-danger">
            Save failed: {saveError} — your edits are kept; change a field to retry.
          </span>
        ) : saving ? (
          <span className="text-caption text-fg-muted">Saving…</span>
        ) : dirty ? (
          <span className="text-caption text-fg-muted">Autosaving…</span>
        ) : savedFlash ? (
          <span className="text-caption text-success">Saved</span>
        ) : null}
      </div>

      {bot.forge && <ForgeAccessSection forge={bot.forge} />}
    </div>
  );
}

/**
 * ForgeAccessSection renders the manifest `forge:` block read-only — what
 * the studio's Integrations flow will auto-provision when this bot is
 * enabled on a connected repo (webhook events + token scopes + the bound
 * secret name). Declared in manifest.yaml; edited there, not here.
 */
function ForgeAccessSection({ forge }: { forge: NonNullable<BotEntryWithSchema["forge"]> }) {
  const events = forge.events ?? [];
  const scopes = Object.entries(forge.token_scopes ?? {});
  return (
    <div className="mt-3 border-t border-border-default pt-2">
      <div className="mb-1 flex items-center gap-2">
        <span className="text-xs font-medium text-fg-default">Forge access</span>
        <span className="rounded bg-surface-2 px-1 text-caption text-fg-subtle">auto-provisioned · read-only</span>
      </div>
      <p className="mb-2 text-caption text-fg-subtle">
        What enabling this bot on a connected repo (Integrations) will set up. Declared in
        manifest.yaml.
      </p>

      {events.length > 0 && (
        <div className="mb-2">
          <div className="mb-0.5 text-caption text-fg-subtle">Webhook events</div>
          <div className="flex flex-wrap gap-1">
            {events.map((e) => (
              <span key={e} className="rounded bg-surface-2 px-1.5 py-0.5 font-mono text-caption text-fg-default">
                {e}
              </span>
            ))}
          </div>
        </div>
      )}

      {scopes.length > 0 && (
        <div className="mb-2">
          <div className="mb-0.5 text-caption text-fg-subtle">Token scopes</div>
          <ul className="space-y-0.5">
            {scopes.map(([k, v]) => (
              <li key={k} className="font-mono text-caption text-fg-default">
                {k}: <span className="text-accent-text">{v}</span>
              </li>
            ))}
          </ul>
        </div>
      )}

      <div className="mb-2 grid grid-cols-2 gap-2">
        <div>
          <div className="mb-0.5 text-caption text-fg-subtle">Bound secret</div>
          <div className="font-mono text-caption text-fg-default">{forge.secret || "forge_token"}</div>
        </div>
        {forge.webhook?.min_replier_role && (
          <div>
            <div className="mb-0.5 text-caption text-fg-subtle">Min replier role</div>
            <div className="font-mono text-caption text-fg-default">{forge.webhook.min_replier_role}</div>
          </div>
        )}
      </div>

      {forge.rationale && (
        <p className="whitespace-pre-wrap text-caption italic text-fg-subtle">{forge.rationale}</p>
      )}
    </div>
  );
}
