import { useEffect, useState } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { useLocation } from "wouter";

import {
  FeatureUnavailableError,
  type WebhookConfig,
  deleteWebhook,
  listWebhooks,
  rotateWebhook,
  updateWebhook,
} from "@/api/webhooks";
import { errorMessage } from "@/lib/errorHints";
import { Badge } from "@/components/ui/Badge";
import { Button } from "@/components/ui/Button";
import { Checkbox } from "@/components/ui/Checkbox";
import { EmptyState } from "@/components/ui/EmptyState";
import { InlineBanner } from "@/components/ui/InlineBanner";
import { Table, THead, Th, TBody, Tr, Td, TableSkeleton } from "@/components/ui/Table";
import { useAsyncAction } from "@/hooks/useAsyncAction";
import { useConfirm } from "@/hooks/useConfirm";
import { useBotsStore } from "@/store/bots";

import { CreateWebhookDialog } from "./webhooks/CreateWebhookDialog";
import { DeliveriesDrawer } from "./webhooks/DeliveriesDrawer";
import { TokenOncePanel } from "./webhooks/TokenOncePanel";

interface Props {
  teamID: string;
  canManage: boolean;
}

// WebhooksTab orchestrates the inbound-webhook surface for a team: lists
// existing entries, opens the create / rotate / delete / deliveries
// affordances, and hands off the one-time token panel after a successful
// create or rotate. The form and drawer live in ./webhooks/.
export default function WebhooksTab({ teamID, canManage }: Props) {
  const [, navigate] = useLocation();
  const queryClient = useQueryClient();
  const [creating, setCreating] = useState(false);
  // Bots come from the shared cache so a metadata edit or catalog toggle
  // elsewhere in the studio re-renders this tab. The Webhooks tab only
  // needs the catalog inside the Create dialog (for the picker), so a
  // lazy fetch on mount is enough.
  const bots = useBotsStore((s) => s.bots) ?? [];
  const botsError = useBotsStore((s) => s.error);
  const fetchBots = useBotsStore((s) => s.fetch);
  const [issued, setIssued] = useState<{ config: WebhookConfig; token: string } | null>(
    null,
  );
  const [deliveriesFor, setDeliveriesFor] = useState<WebhookConfig | null>(null);

  // Mutations (toggle/rotate/delete) flow through `run()`; list-load
  // failures surface from the query. One banner shows whichever is
  // current.
  const { error: actionErr, run } = useAsyncAction();
  const { confirm, dialog: confirmDialog } = useConfirm();

  const webhooksQuery = useQuery({
    queryKey: ["team-webhooks", teamID],
    queryFn: () => listWebhooks(teamID),
  });
  const webhooks = webhooksQuery.data ?? [];
  // isFetching (not isLoading): every reload — initial and post-mutation
  // — shows the skeleton, an explicit visible refresh.
  const loading = webhooksQuery.isFetching;
  const unavailable = webhooksQuery.error instanceof FeatureUnavailableError;
  const err =
    actionErr ??
    (webhooksQuery.error && !unavailable && !loading
      ? errorMessage(webhooksQuery.error)
      : null);

  const reload = () =>
    queryClient.invalidateQueries({ queryKey: ["team-webhooks", teamID] });

  useEffect(() => {
    void fetchBots();
  }, [fetchBots]);

  const toggleEnabled = (cfg: WebhookConfig) =>
    run(async () => {
      await updateWebhook(teamID, cfg.id, { enabled: !cfg.enabled });
      await reload();
    });

  const askRotate = async (cfg: WebhookConfig) => {
    const ok = await confirm({
      title: `Rotate ${cfg.name}?`,
      message:
        "The current token will stop working immediately. You will see the new token once — make sure to copy it before closing the panel.",
      confirmLabel: "Rotate",
      confirmVariant: "danger",
    });
    if (!ok) return;
    await run(async () => {
      const r = await rotateWebhook(teamID, cfg.id);
      setIssued(r);
      await reload();
    });
  };

  const askDelete = async (cfg: WebhookConfig) => {
    const ok = await confirm({
      title: `Delete ${cfg.name}?`,
      message:
        "The webhook URL will return 404 immediately and incoming events will be discarded. This cannot be undone.",
      confirmLabel: "Delete",
      confirmVariant: "danger",
    });
    if (!ok) return;
    await run(async () => {
      await deleteWebhook(teamID, cfg.id);
      await reload();
    });
  };

  // The unavailable banner is the whole tab — render it before the rest.
  if (unavailable) {
    return (
      <EmptyState
        title="Webhooks not enabled on this server"
        message="Inbound webhooks require a multi-tenant deployment. Run iterion server in cloud mode to use them."
      />
    );
  }

  return (
    <div className="space-y-4">
      {confirmDialog}
      {err && (
        <InlineBanner tone="danger" layout="inline">
          {err}
        </InlineBanner>
      )}
      {botsError && (
        <InlineBanner tone="danger" layout="inline" title="Bots unavailable">
          {botsError}
        </InlineBanner>
      )}

      <div className="flex items-center justify-between">
        <div>
          <h3 className="font-medium">Inbound webhooks</h3>
          <p className="text-xs text-fg-subtle mt-0.5">
            Long-lived tokens an external forge can present to launch a bot.
          </p>
          <p className="text-caption text-fg-subtle mt-0.5">
            Repository provisioning (webhook + token on the forge) is handled automatically for
            each connected repo — see{" "}
            <button
              type="button"
              className="text-accent-text hover:underline"
              onClick={() => navigate("/integrations?tab=forges")}
            >
              Repositories
            </button>
            . Manage stand-alone tokens here.
          </p>
        </div>
        {canManage && (
          <Button size="sm" variant="primary" onClick={() => setCreating(true)}>
            New webhook
          </Button>
        )}
      </div>

      {loading ? (
        <TableSkeleton rows={4} cols={6} />
      ) : webhooks.length === 0 ? (
        <EmptyState
          message={
            canManage
              ? "No webhooks yet. Create one to give a forge access to your bots."
              : "No webhooks yet. Ask an admin to create one."
          }
        />
      ) : (
        <Table caption="Inbound webhooks">
          <THead>
            <Th>Name</Th>
            <Th>Provider</Th>
            <Th>Bots</Th>
            <Th>Last4</Th>
            <Th>Status</Th>
            <Th align="right">Actions</Th>
          </THead>
          <TBody>
            {webhooks.map((w) => (
              <Tr key={w.id} className="align-top">
                <Td>
                  <div className="font-medium">{w.name}</div>
                  <div className="text-caption text-fg-subtle font-mono break-all">
                    {w.id}
                  </div>
                </Td>
                <Td>
                  <Badge variant="neutral">{w.provider}</Badge>
                </Td>
                <Td className="text-xs">
                  {w.wildcard_bots ? (
                    <Badge variant="warning">wildcard</Badge>
                  ) : (
                    (w.bot_ids ?? []).join(", ") || "—"
                  )}
                </Td>
                <Td className="text-xs font-mono text-fg-muted">
                  …{w.token_last4 || "????"}
                </Td>
                <Td>
                  {canManage ? (
                    <label className="inline-flex items-center gap-1 text-xs cursor-pointer">
                      <Checkbox
                        checked={w.enabled}
                        onChange={() => void toggleEnabled(w)}
                      />
                      {w.enabled ? "enabled" : "disabled"}
                    </label>
                  ) : (
                    <span className="text-xs">{w.enabled ? "enabled" : "disabled"}</span>
                  )}
                </Td>
                <Td align="right" className="space-x-1 whitespace-nowrap">
                  <Button size="sm" variant="ghost" onClick={() => setDeliveriesFor(w)}>
                    Deliveries
                  </Button>
                  {canManage && (
                    <>
                      <Button size="sm" variant="ghost" onClick={() => void askRotate(w)}>
                        Rotate
                      </Button>
                      <Button
                        size="sm"
                        variant="ghost"
                        onClick={() => void askDelete(w)}
                        className="text-danger"
                      >
                        Delete
                      </Button>
                    </>
                  )}
                </Td>
              </Tr>
            ))}
          </TBody>
        </Table>
      )}

      {creating && (
        <CreateWebhookDialog
          teamID={teamID}
          bots={bots}
          onClose={() => setCreating(false)}
          onCreated={(r) => {
            setCreating(false);
            setIssued(r);
            void reload();
          }}
        />
      )}

      {issued && (
        <TokenOncePanel
          config={issued.config}
          token={issued.token}
          onClose={() => setIssued(null)}
        />
      )}

      {deliveriesFor && (
        <DeliveriesDrawer
          teamID={teamID}
          webhook={deliveriesFor}
          onClose={() => setDeliveriesFor(null)}
        />
      )}
    </div>
  );
}
