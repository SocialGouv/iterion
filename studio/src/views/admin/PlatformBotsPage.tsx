import { useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";

import {
  deletePlatformBot,
  listPlatformBots,
  type PlatformBotRow,
} from "@/api/adminBots";
import { useAuth } from "@/auth/AuthContext";
import { CloudOnlyNotice } from "@/components/shared/CloudOnlyNotice";
import ConfirmDialog from "@/components/shared/ConfirmDialog";
import { useHeaderSlot } from "@/components/shared/useHeaderSlot";
import { Button } from "@/components/ui/Button";
import { EmptyState } from "@/components/ui/EmptyState";
import { Table, TableSkeleton, TBody, Td, Th, THead, Tr } from "@/components/ui/Table";
import { errorMessage } from "@/lib/errorHints";
import { formatDateTime } from "@/lib/format";
import { useServerInfoStore } from "@/store/serverInfo";
import { useUIStore } from "@/store/ui";

import AdminNav from "./AdminNav";

// Platform bot overrides console: the DB-backed form of the baked bot
// catalog. Read + revert surface — the iteration loop itself is the CLI
// (`iterion remote admin bots push <bundle-dir>`), which this page points
// at rather than duplicating.
export default function PlatformBotsPage() {
  const { user: me } = useAuth();
  const isSuper = me?.is_super_admin ?? false;
  const serverInfo = useServerInfoStore((s) => s.info);
  const isCloud = serverInfo?.mode === "cloud";
  const addToast = useUIStore((s) => s.addToast);
  const qc = useQueryClient();
  const [pendingRevert, setPendingRevert] = useState<PlatformBotRow | null>(null);

  useHeaderSlot({
    left: <span className="text-sm font-semibold">Platform bot overrides</span>,
  });

  const query = useQuery<PlatformBotRow[]>({
    queryKey: ["admin-bots"],
    queryFn: listPlatformBots,
    enabled: isSuper && isCloud,
  });

  const del = useMutation({
    mutationFn: (slug: string) => deletePlatformBot(slug),
    onSuccess: (_d, slug) => {
      addToast(`Override "${slug}" removed — the baked catalog serves it again.`, "success");
      void qc.invalidateQueries({ queryKey: ["admin-bots"] });
    },
    onError: (err) => addToast(errorMessage(err), "error"),
  });

  if (!isSuper) {
    return (
      <div className="p-6">
        <p className="text-sm text-fg-muted">Super-admin only.</p>
      </div>
    );
  }
  if (serverInfo && !isCloud) {
    return (
      <div className="h-full overflow-auto">
        <div className="max-w-5xl mx-auto p-3 sm:p-6">
          <CloudOnlyNotice feature="Platform bot overrides" />
        </div>
      </div>
    );
  }

  const rows = query.data ?? [];
  return (
    <div className="h-full overflow-auto">
      <div className="max-w-5xl mx-auto p-3 sm:p-6 space-y-6">
        <AdminNav />
        <p className="text-sm text-fg-muted">
          A pushed bundle overrides the same-slug baked bot for every tenant and every
          launch surface from the next launch; deleting it reverts to the baked catalog.
          Push from a local checkout with{" "}
          <code className="font-mono text-xs">iterion remote admin bots push bots/&lt;slug&gt;</code>.
        </p>
        {query.error != null && (
          <p className="text-sm text-danger">{errorMessage(query.error)}</p>
        )}
        {(query.isLoading || serverInfo == null) && <TableSkeleton rows={3} cols={6} />}
        {serverInfo != null && !query.isLoading && rows.length === 0 && query.error == null && (
          <EmptyState
            title="No overrides"
            message="Every bot serves from the baked catalog."
          />
        )}
        {!query.isLoading && rows.length > 0 && (
          <Table caption="Platform bot overrides">
            <THead>
              <Th>Slug</Th>
              <Th>Version</Th>
              <Th>Digest</Th>
              <Th>Updated</Th>
              <Th>By</Th>
              <Th srLabel="Actions" />
            </THead>
            <TBody>
              {rows.map((b) => (
                <Tr key={b.id}>
                  <Td className="font-mono">{b.slug}</Td>
                  <Td>v{b.version}</Td>
                  <Td className="font-mono text-xs text-fg-subtle">
                    {(b.digest ?? "").slice(0, 12)}
                  </Td>
                  <Td>{formatDateTime(b.updated_at)}</Td>
                  <Td>{b.updated_by || b.created_by || "—"}</Td>
                  <Td align="right">
                    <Button
                      variant="danger"
                      size="sm"
                      disabled={del.isPending}
                      onClick={() => setPendingRevert(b)}
                    >
                      Revert to baked
                    </Button>
                  </Td>
                </Tr>
              ))}
            </TBody>
          </Table>
        )}
      </div>

      <ConfirmDialog
        open={pendingRevert !== null}
        title="Revert to the baked catalog?"
        message={`Remove the platform override "${pendingRevert?.slug ?? ""}"? The baked catalog version serves again from the next launch.`}
        confirmLabel="Revert"
        confirmVariant="danger"
        onConfirm={() => {
          if (pendingRevert) del.mutate(pendingRevert.slug);
          setPendingRevert(null);
        }}
        onCancel={() => setPendingRevert(null)}
      />
    </div>
  );
}
