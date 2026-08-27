import { useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { ArrowUpFromLine } from "lucide-react";

import { Button } from "@/components/ui/Button";
import { Dialog } from "@/components/ui/Dialog";
import { IconButton } from "@/components/ui/IconButton";
import { InlineBanner } from "@/components/ui/InlineBanner";
import { Input } from "@/components/ui/Input";
import { Select } from "@/components/ui/Select";
import { useAuth } from "@/auth/AuthContext";
import { useAsyncAction } from "@/hooks/useAsyncAction";
import { errorMessage } from "@/lib/errorHints";
import { useServerInfoStore } from "@/store/serverInfo";
import { useUIStore } from "@/store/ui";
import {
  listForgeConnections,
  type ForgeConnection,
} from "@/api/forgeConnections";
import { pushIssueToForge, type NativeIssue } from "@/api/native";

// PushToForgeButton renders a discreet "push this card to a forge" affordance
// in the card footer. Cloud-mode only (self-hosted boards have no forge
// connection concept, so the button would only clutter). A card already
// linked to a forge (external.url set) pushes directly (the server updates the
// linked issue); an unlinked card opens a small dialog to pick a forge
// connection + type an "owner/repo" target before creating-and-linking.
export function PushToForgeButton({ iss }: { iss: NativeIssue }) {
  const mode = useServerInfoStore((s) => s.info?.mode);
  const { activeTeamID } = useAuth();
  const addToast = useUIStore((s) => s.addToast);
  const [dialogOpen, setDialogOpen] = useState(false);
  const pushAction = useAsyncAction();

  if (mode !== "cloud") return null;

  const linkedURL = iss.external?.url ?? "";

  const onSuccess = (url: string) => {
    addToast("Pushed to forge", "success", {
      action: { label: "Open", onClick: () => window.open(url, "_blank", "noopener") },
    });
  };

  const onDirectPush = async (e: React.MouseEvent) => {
    e.stopPropagation();
    if (linkedURL) {
      const res = await pushAction.run(() => pushIssueToForge(iss.id));
      if (res) onSuccess(res.url);
      else if (pushAction.error) addToast(pushAction.error, "error");
    } else {
      setDialogOpen(true);
    }
  };

  return (
    <>
      <IconButton
        size="sm"
        variant="ghost"
        label="Push to forge"
        tooltip="Push to forge"
        className="ml-auto h-5 w-5"
        disabled={pushAction.busy}
        onClick={(e) => void onDirectPush(e)}
      >
        <ArrowUpFromLine className="h-3.5 w-3.5" aria-hidden="true" />
      </IconButton>
      {dialogOpen && (
        <PushToForgeDialog
          teamID={activeTeamID}
          onClose={() => setDialogOpen(false)}
          onPush={async (connectionId, repo) => {
            const res = await pushAction.run(() =>
              pushIssueToForge(iss.id, { connection_id: connectionId, repo }),
            );
            if (res) {
              setDialogOpen(false);
              onSuccess(res.url);
            } else if (pushAction.error) {
              throw new Error(pushAction.error);
            }
          }}
        />
      )}
    </>
  );
}

// PushToForgeDialog collects the forge connection + "owner/repo" target for an
// UNLINKED card, then delegates the push to the parent.
function PushToForgeDialog({
  teamID,
  onClose,
  onPush,
}: {
  teamID: string;
  onClose: () => void;
  onPush: (connectionId: string, repo: string) => Promise<void>;
}) {
  const [connectionId, setConnectionId] = useState("");
  const [repo, setRepo] = useState("");
  const submitAction = useAsyncAction();

  // The dialog mounts only while open, so the query fetches on open.
  const connectionsQuery = useQuery<ForgeConnection[]>({
    queryKey: ["forge-connections", teamID],
    queryFn: () => listForgeConnections(teamID),
  });
  // Watch-only connections (Dependabot alerts, read) cannot push: their App
  // holds no contents/issues grant. They must not appear here at all — the
  // picker below defaults to the FIRST connection, so a team that connected
  // its watch-only App first would push through it by default and take a 403.
  const connections =
    connectionsQuery.data?.filter((c) => c.purpose !== "security_read") ?? null;
  const loadError = connectionsQuery.error
    ? errorMessage(connectionsQuery.error)
    : null;

  // Default the picker to the first connection; an explicit operator
  // choice always wins.
  const selectedConnectionId = connectionId || (connections?.[0]?.id ?? "");

  const submit = async () => {
    await submitAction.run(() => onPush(selectedConnectionId, repo.trim()));
  };

  const canSubmit =
    !!selectedConnectionId && repo.trim().length > 0 && !submitAction.busy;

  return (
    <Dialog
      open
      onOpenChange={(o) => {
        if (!o) onClose();
      }}
      title="Push to forge"
      description="This card isn't linked to a forge yet. Pick a connection and a repository to create and link the issue."
      footer={
        <>
          <Button variant="secondary" size="sm" onClick={onClose} disabled={submitAction.busy}>
            Cancel
          </Button>
          <Button
            variant="primary"
            size="sm"
            onClick={() => void submit()}
            loading={submitAction.busy}
            disabled={!canSubmit}
          >
            Push
          </Button>
        </>
      }
    >
      <div className="space-y-3" onClick={(e) => e.stopPropagation()}>
        {(loadError || submitAction.error) && (
          <InlineBanner tone="danger" layout="inline">
            {submitAction.error ?? loadError}
          </InlineBanner>
        )}
        <label className="block">
          <span className="text-xs text-fg-muted mb-1 block">Forge connection</span>
          {connectionsQuery.isLoading ? (
            <p className="text-xs text-fg-subtle italic">Loading connections…</p>
          ) : connections && connections.length === 0 ? (
            <p className="text-xs text-fg-subtle">
              No forge connections. Connect one from the team Integrations tab first.
            </p>
          ) : (
            <Select
              value={selectedConnectionId}
              onChange={(e) => setConnectionId(e.target.value)}
              size="md"
            >
              {(connections ?? []).map((c) => (
                <option key={c.id} value={c.id}>
                  {c.provider} · @{c.account_login ?? c.id}
                </option>
              ))}
            </Select>
          )}
        </label>
        <label className="block">
          <span className="text-xs text-fg-muted mb-1 block">Repository (owner/repo)</span>
          <Input
            value={repo}
            onChange={(e) => setRepo(e.target.value)}
            placeholder="owner/repo"
            size="md"
          />
        </label>
      </div>
    </Dialog>
  );
}
