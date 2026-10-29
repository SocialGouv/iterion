import { useMemo, useState } from "react";
import { useQuery } from "@tanstack/react-query";

import { type ApiKeyView, listApiKeys } from "@/api/byok";
import { previewCredentials } from "@/api/credentialPreview";
import { type WebhookConfig, updateWebhook } from "@/api/webhooks";
import { Button } from "@/components/ui/Button";
import { Dialog } from "@/components/ui/Dialog";
import { InlineBanner } from "@/components/ui/InlineBanner";
import { Select } from "@/components/ui/Select";
import { Spinner } from "@/components/ui/Spinner";
import { errorMessage } from "@/lib/errorHints";

// NO_PIN is the picker's "leave the resolver alone" option: an absent entry in
// key_overrides is not the same as a pinned key, so it is a value the operator
// can come back to, not a blank the form has to interpret.
const NO_PIN = "";

// A gateway key is storable but no run spends it yet (#2005 PR-C'), and
// pkg/server's validateKeyOverrides REFUSES a pin on one. Offering it would
// hand the operator a save that cannot succeed.
const UNPINNABLE_PROVIDER = "openai_compatible";

// pinnableKeys are the team keys an override may name: one entry per provider
// the team actually holds a key for, grouped so the picker of a provider can
// never show another provider's key — which is the refusal
// validateKeyOverrides answers ("api key %q is for provider %q"), made
// impossible instead of reported.
export function pinnableKeys(keys: ApiKeyView[]): Map<string, ApiKeyView[]> {
  const byProvider = new Map<string, ApiKeyView[]>();
  for (const k of keys) {
    if (k.provider === UNPINNABLE_PROVIDER) continue;
    const bucket = byProvider.get(k.provider);
    if (bucket) bucket.push(k);
    else byProvider.set(k.provider, [k]);
  }
  return new Map([...byProvider.entries()].sort(([a], [b]) => a.localeCompare(b)));
}

// previewRepo is the repository the server stamps on a webhook preview: ONE
// literal entry of the project allowlist. A pattern ("group/*") is not a
// repository, and several entries name no single one — in both cases the
// launch would pick a repo this surface cannot predict, so the audience
// warning stays silent rather than guessing (pkg/server/credential_preview.go
// reads the allowlist the same way).
export function previewRepo(cfg: WebhookConfig): string {
  const list = cfg.project_allowlist ?? [];
  if (list.length !== 1) return "";
  const entry = (list[0] ?? "").trim();
  return entry.includes("*") ? "" : entry;
}

// outOfAudience reports whether a key's repository audience excludes the repo
// this webhook's deliveries would run for. Such a pin stores fine and then
// withholds the key at every launch, so it is named BEFORE the save.
//
// The comparison is the server's: secrets.ApiKey.ServesRepoForLaunch matches
// case-insensitively, and a case-only difference flagged here would be a
// warning about a key every delivery spends. An unknown repo (a pattern
// allowlist, or several entries) is NOT warned about from here — the server
// refuses a restricted key for a launch that names no repository, and the
// preview row below is what reads that refusal, rather than this surface
// guessing which repository the next delivery will name.
export function outOfAudience(key: ApiKeyView, repo: string): boolean {
  const repos = key.repos ?? [];
  if (repos.length === 0 || !repo) return false;
  const want = repo.toLowerCase();
  return !repos.some((r) => r.toLowerCase() === want);
}

function keyLabel(k: ApiKeyView): string {
  // Never a key VALUE: the model is key ids, and last4 is all the server
  // keeps of the secret itself.
  const parts = [k.name];
  if (k.last4) parts.push(`…${k.last4}`);
  if (k.is_default) parts.push("(provider default)");
  if ((k.repos ?? []).length > 0) parts.push(`repos: ${(k.repos ?? []).join(" ")}`);
  return parts.join(" ");
}

export interface KeyOverridesDialogProps {
  teamID: string;
  webhook: WebhookConfig;
  onClose: () => void;
  // onSaved hands back the config the server answered, so the list that opened
  // this dialog shows the stored pins without a second read.
  onSaved: (cfg: WebhookConfig) => void;
}

// KeyOverridesDialog edits a webhook's key_overrides — the per-repo credential
// pin of #2005/#2648. The map is provider → BYOK key id, so every row is a
// picker over the team's keys of that one provider, and the preview row names
// the key a delivery would actually seal (read from the server's own
// resolution, never recomputed here).
export default function KeyOverridesDialog({
  teamID,
  webhook,
  onClose,
  onSaved,
}: KeyOverridesDialogProps) {
  const [pins, setPins] = useState<Record<string, string>>(() => ({
    ...(webhook.key_overrides ?? {}),
  }));
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<string | null>(null);
  // previewNonce makes the preview query key change on every save, so the row
  // naming "the key it would seal" is re-read against the STORED pins instead
  // of showing the resolution of the map the operator just replaced.
  const [previewNonce, setPreviewNonce] = useState(0);

  const keysQuery = useQuery({
    queryKey: ["team-api-keys", teamID],
    queryFn: () => listApiKeys({ team_id: teamID }),
  });
  const byProvider = useMemo(() => pinnableKeys(keysQuery.data ?? []), [keysQuery.data]);
  const repo = previewRepo(webhook);

  // The bot the preview resolves for. An ambiguous webhook (several bots, no
  // default) has no single answer, and the server refuses rather than pick:
  // the dialog says so instead of sending a guess.
  const previewBot = webhook.default_bot_id || (webhook.bot_ids ?? []).length === 1
    ? webhook.default_bot_id || (webhook.bot_ids ?? [])[0]
    : undefined;

  const previewQuery = useQuery({
    queryKey: ["webhook-credential-preview", teamID, webhook.id, previewNonce],
    enabled: !!previewBot,
    queryFn: () =>
      previewCredentials(teamID, {
        source: { kind: "webhook", id: webhook.id },
        bot_id: previewBot,
      }),
  });

  const sealed = (previewQuery.data?.candidates ?? []).filter((c) => c.selected);

  const save = async () => {
    setBusy(true);
    setErr(null);
    try {
      // The whole map, every save: key_overrides is the exact desired set
      // server-side, so a row cleared to "no pin" must leave as an ABSENT
      // entry, not an empty string the resolver would look up as a key id.
      const body: Record<string, string> = {};
      for (const [provider, keyID] of Object.entries(pins)) {
        if (keyID !== NO_PIN) body[provider] = keyID;
      }
      const cfg = await updateWebhook(teamID, webhook.id, { key_overrides: body });
      setPins({ ...(cfg.key_overrides ?? {}) });
      setPreviewNonce((n) => n + 1);
      onSaved(cfg);
    } catch (e) {
      setErr(errorMessage(e));
    } finally {
      setBusy(false);
    }
  };

  return (
    <Dialog
      open
      onOpenChange={(v) => {
        if (!v) onClose();
      }}
      title={`Credential pins — ${webhook.name}`}
      description="Pin a team key per model provider for the runs this webhook launches. Leave a provider unpinned to let the org default resolve it."
      widthClass="max-w-2xl"
      footer={
        <>
          <Button variant="secondary" onClick={onClose} disabled={busy}>
            Cancel
          </Button>
          <Button variant="primary" onClick={() => void save()} loading={busy}>
            Save pins
          </Button>
        </>
      }
    >
      <div className="space-y-3">
        {err && (
          <InlineBanner tone="danger" layout="inline">
            {err}
          </InlineBanner>
        )}
        {keysQuery.isPending ? (
          <Spinner />
        ) : keysQuery.error ? (
          <InlineBanner tone="danger" layout="inline" title="Team keys unavailable">
            {errorMessage(keysQuery.error)}
          </InlineBanner>
        ) : byProvider.size === 0 ? (
          <p className="text-xs text-fg-muted">
            This team holds no pinnable key. Add a provider key to the team to pin one here.
          </p>
        ) : (
          <div className="space-y-2">
            {[...byProvider.entries()].map(([provider, keys]) => {
              const pinned = pins[provider] ?? NO_PIN;
              const pinnedKey = keys.find((k) => k.id === pinned);
              return (
                <div key={provider} className="space-y-1">
                  <label htmlFor={`key-pin-${provider}`} className="text-xs text-fg-muted">
                    {provider}
                  </label>
                  <Select
                    id={`key-pin-${provider}`}
                    aria-label={`Key for ${provider}`}
                    value={pinned}
                    onChange={(e) => {
                      // Read before the updater: a lazy setState callback runs
                      // after the handler returns, by which time React has
                      // released currentTarget.
                      const chosen = e.currentTarget.value;
                      setPins((p) => ({ ...p, [provider]: chosen }));
                    }}
                  >
                    <option value={NO_PIN}>No pin — the org default resolves it</option>
                    {keys.map((k) => (
                      <option key={k.id} value={k.id}>
                        {keyLabel(k)}
                      </option>
                    ))}
                  </Select>
                  {pinnedKey && outOfAudience(pinnedKey, repo) && (
                    <p className="text-caption text-warning-fg">
                      This key is restricted to {(pinnedKey.repos ?? []).join(", ")}, and this
                      webhook serves {repo} — every delivery would find it withheld.
                    </p>
                  )}
                </div>
              );
            })}
          </div>
        )}

        <div
          className="border-t border-border-subtle pt-2 space-y-1"
          data-testid="webhook-sealed-key"
        >
          <h4 className="text-xs font-medium">The key a delivery would seal</h4>
          {!previewBot ? (
            <p className="text-caption text-fg-muted">
              This webhook can launch several bots and pins no default, so there is no single
              resolution to preview. Set a default bot to see it.
            </p>
          ) : previewQuery.isPending ? (
            <Spinner />
          ) : previewQuery.error ? (
            <p className="text-caption text-warning-fg">
              Preview unavailable: {errorMessage(previewQuery.error)}
            </p>
          ) : sealed.length === 0 ? (
            <p className="text-caption text-fg-muted">
              No credential resolves for {previewBot} with the stored pins.
            </p>
          ) : (
            <ul className="text-caption space-y-0.5">
              {sealed.map((c) => (
                <li key={c.id}>
                  <span className="text-fg-muted">{c.wire}</span> → {c.label}
                  {c.pinned ? " (pinned here)" : ""}
                </li>
              ))}
            </ul>
          )}
        </div>
      </div>
    </Dialog>
  );
}
