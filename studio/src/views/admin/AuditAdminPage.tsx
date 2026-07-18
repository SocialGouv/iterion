import { useAuth } from "@/auth/AuthContext";
import { CloudOnlyNotice } from "@/components/shared/CloudOnlyNotice";
import { useHeaderSlot } from "@/components/shared/useHeaderSlot";
import { TableSkeleton } from "@/components/ui/Table";
import { useServerInfoStore } from "@/store/serverInfo";
import AuditTab from "@/views/teams/tabs/AuditTab";

import AdminNav from "./AdminNav";

// Platform audit console: the super-admin /api/admin/audit feed spanning
// every org and team. Reuses AuditTab's table/filters in platform mode;
// AuditTab itself handles the "audit store not configured" 404 via its
// FeatureUnavailableError empty state.
export default function AuditAdminPage() {
  const { user: me } = useAuth();
  const isSuper = me?.is_super_admin ?? false;
  // Cloud-mode console (/api/admin/audit isn't registered locally) — gate
  // on server_info BEFORE fetching so local mode never fires a doomed
  // 404 request.
  const serverInfo = useServerInfoStore((s) => s.info);
  const isCloud = serverInfo?.mode === "cloud";

  useHeaderSlot({
    left: <span className="text-sm font-semibold">Platform audit</span>,
  });

  if (!isSuper) {
    return (
      <div className="p-6">
        <p className="text-sm text-fg-muted">Super-admin only.</p>
      </div>
    );
  }

  // Deliberate local-mode gate. While server_info is still loading we fall
  // through to the (fetch-gated) tab instead of flashing this notice on cloud.
  if (serverInfo && !isCloud) {
    return (
      <div className="h-full overflow-auto">
        <div className="max-w-5xl mx-auto p-3 sm:p-6">
          <CloudOnlyNotice feature="Platform audit" />
        </div>
      </div>
    );
  }

  return (
    <div className="h-full overflow-auto">
      <div className="max-w-6xl mx-auto p-3 sm:p-6 space-y-4">
        <AdminNav />
        {/* Wait for server_info before mounting the tab: its first fetch
            fires on mount, and local mode would 404. The cloud/local split
            above only settles once server_info arrives. */}
        {serverInfo ? (
          <AuditTab platform canManage={isSuper} />
        ) : (
          <TableSkeleton rows={4} cols={5} />
        )}
      </div>
    </div>
  );
}
