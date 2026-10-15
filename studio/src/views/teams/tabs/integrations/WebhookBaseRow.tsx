import { useState } from "react";

import { type ForgeConnection, setForgeConnectionWebhookBase } from "@/api/forgeConnections";
import { Button } from "@/components/ui/Button";
import { Input } from "@/components/ui/Input";
import { errorMessage } from "@/lib/errorHints";

const CAPTION =
  "For a forge that cannot reach this deployment's public URL — e.g. a GitLab whose " +
  "outbound allowlist lists another host. Repos enabled from now on use it; re-enable " +
  "an enabled repo to move its hook.";

const UNPINNED = "this deployment's public URL";

// WebhookBaseRow is the connection's hook delivery address: the base every
// inbound webhook URL iterion registers on the forge is built from. A forge
// that only dials allowlisted hosts refuses the deployment's public URL
// outright (HTTP 422 on the hook), and pinning a base it does accept is the
// way through — per connection, without touching the deployment.
//
// Renders nothing for a watch-only connection: it reads Dependabot alerts and
// registers no webhook at all, so it has no delivery address.
export function WebhookBaseRow({
  teamID,
  conn,
  canManage,
  onChanged,
  onError,
}: {
  teamID: string;
  conn: ForgeConnection;
  canManage: boolean;
  onChanged: () => void;
  /** Reports into the parent's error slot. The EMPTY STRING is a message of
   *  this channel too — "an attempt started, take the banner down" — as the
   *  card's other rows send it. */
  onError: (m: string) => void;
}) {
  const [editing, setEditing] = useState(false);
  const [draft, setDraft] = useState("");
  const [busy, setBusy] = useState(false);

  if (conn.purpose === "security_read") return null;

  const pinned = conn.webhook_base_url ?? "";
  const inputID = `webhook-base-${conn.id}`;

  const save = async (base: string) => {
    onError("");
    setBusy(true);
    try {
      await setForgeConnectionWebhookBase(teamID, conn.id, base);
      setEditing(false);
      onChanged();
    } catch (e) {
      onError(errorMessage(e));
    } finally {
      setBusy(false);
    }
  };

  const openEditor = () => {
    setDraft(pinned);
    setEditing(true);
  };

  return (
    <div className="space-y-1">
      <div className="flex flex-wrap items-center gap-2 text-caption text-fg-muted">
        <span>Webhook delivery:</span>
        {pinned ? (
          <span className="font-mono text-fg-default">{pinned}</span>
        ) : (
          <span>{UNPINNED}</span>
        )}
        {canManage && !editing && pinned && (
          <>
            <Button variant="ghost" size="sm" onClick={openEditor} disabled={busy}>
              Change
            </Button>
            <Button
              variant="ghost"
              size="sm"
              onClick={() => void save("")}
              loading={busy}
              disabled={busy}
              title="Deliver to this deployment's public URL again"
            >
              Unpin
            </Button>
          </>
        )}
        {canManage && !editing && !pinned && (
          <Button variant="ghost" size="sm" onClick={openEditor} disabled={busy}>
            Pin a base URL
          </Button>
        )}
      </div>

      {canManage && editing && (
        <div className="flex flex-wrap items-end gap-2">
          <div className="min-w-0 flex-1 max-w-sm">
            <label htmlFor={inputID} className="block text-caption text-fg-muted">
              Webhook base URL
            </label>
            <Input
              id={inputID}
              value={draft}
              placeholder="https://iterion.example.org"
              onChange={(e) => setDraft(e.target.value)}
              disabled={busy}
            />
          </div>
          <Button
            variant="secondary"
            size="sm"
            onClick={() => void save(draft.trim())}
            loading={busy}
            disabled={busy || draft.trim() === ""}
          >
            Save
          </Button>
          <Button variant="ghost" size="sm" onClick={() => setEditing(false)} disabled={busy}>
            Cancel
          </Button>
        </div>
      )}

      <div className="text-micro text-fg-subtle">{CAPTION}</div>
    </div>
  );
}
