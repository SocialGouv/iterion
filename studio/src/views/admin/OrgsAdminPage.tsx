// OrgsAdminPage — super-admin org console orchestrator. Gating (super-admin
// + cloud mode) and layout live here; the working parts are in ./orgs/:
// useOrgsAdmin (list query + shared mutation slot), CreateOrgForm,
// OrgsTable, and OrgDrawer (details / usage / quotas / status / deletion).

import { useAuth } from "@/auth/AuthContext";
import { CloudOnlyNotice } from "@/components/shared/CloudOnlyNotice";
import { useHeaderSlot } from "@/components/shared/useHeaderSlot";
import { InlineBanner } from "@/components/ui/InlineBanner";
import { useServerInfoStore } from "@/store/serverInfo";

import AdminNav from "./AdminNav";
import { CreateOrgForm } from "./orgs/CreateOrgForm";
import { OrgDrawer } from "./orgs/OrgDrawer";
import { OrgsTable } from "./orgs/OrgsTable";
import { useOrgsAdmin } from "./orgs/useOrgsAdmin";

export default function OrgsAdminPage() {
  const { user, activeOrgID, reloadIdentity } = useAuth();
  const isSuper = user?.is_super_admin ?? false;
  // Org administration is a cloud-mode console (/api/admin/orgs isn't
  // registered locally) — gate on server_info BEFORE fetching so local
  // mode never fires a doomed 404 and never shows an enabled create form.
  const serverInfo = useServerInfoStore((s) => s.info);
  const isCloud = serverInfo?.mode === "cloud";

  const { orgs, loaded, busy, bannerErr, active, setActive, refresh, run } =
    useOrgsAdmin(isSuper && isCloud);

  useHeaderSlot({
    left: <span className="text-sm font-semibold">Organizations</span>,
    right: <span className="text-xs text-fg-muted">{orgs.length} org(s)</span>,
  });

  if (!isSuper) {
    return (
      <div className="p-6">
        <p className="text-sm text-fg-muted">Super-admin only.</p>
      </div>
    );
  }

  // Deliberate local-mode gate: no fetch fired, no enabled create form.
  // While server_info is still loading we fall through to the skeleton
  // below instead of flashing this notice on cloud.
  if (serverInfo && !isCloud) {
    return (
      <div className="h-full overflow-auto">
        <div className="max-w-5xl mx-auto p-3 sm:p-6">
          <CloudOnlyNotice feature="Organization administration" />
        </div>
      </div>
    );
  }

  return (
    <div className="h-full overflow-auto">
      <div className="max-w-5xl mx-auto p-3 sm:p-6 space-y-4">
        <AdminNav />

        {bannerErr && (
          <InlineBanner tone="danger" layout="inline">
            {bannerErr}
          </InlineBanner>
        )}

        <CreateOrgForm busy={busy} run={run} reloadIdentity={reloadIdentity} />
        <OrgsTable orgs={orgs} loaded={loaded} onOpen={setActive} />
      </div>

      {active && (
        <OrgDrawer
          org={active}
          busy={busy}
          isActiveOrg={active.id === activeOrgID}
          onClose={() => setActive(null)}
          onChanged={setActive}
          onAfterUpdate={refresh}
          reloadIdentity={reloadIdentity}
          run={run}
        />
      )}
    </div>
  );
}
