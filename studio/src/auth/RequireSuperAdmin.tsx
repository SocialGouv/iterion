// RequireSuperAdmin — the single route-level gate for the /admin/* console.
// Applied once around the admin route group (App.tsx) so the authorization
// decision lives in one place; the individual pages keep their own
// super-admin/cloud checks as defense-in-depth.
//
// Two tiers, matching what the /api/admin/* routes enforce server-side:
//   - not cloud mode (local/desktop): the consoles aren't registered, so show
//     the CloudOnlyNotice rather than a doomed page. Note local/desktop
//     synthesises a super-admin identity, so the block here is purely the
//     "cloud-only feature" one, never a permission wall for the single operator.
//   - cloud + not super-admin: a friendly 403 notice.
//   - mode not yet known (server_info loading): a neutral spinner, so a cloud
//     non-super-admin never flashes the admin shell (and its failing requests)
//     before the 403 resolves.
// Otherwise render the children.

import type { ReactNode } from "react";

import { useAuth } from "@/auth/AuthContext";
import { CloudOnlyNotice } from "@/components/shared/CloudOnlyNotice";
import PanelLoading from "@/components/shared/PanelLoading";
import { useServerInfoStore } from "@/store/serverInfo";

export function RequireSuperAdmin({ children }: { children: ReactNode }) {
  const { user } = useAuth();
  const isSuper = user?.is_super_admin ?? false;
  const serverInfo = useServerInfoStore((s) => s.info);
  const isCloud = serverInfo?.mode === "cloud";

  // Hold a neutral panel until the mode is known: rendering the children here
  // would flash the admin shell + its failing /api/admin/* requests to a cloud
  // non-super-admin before the 403 below resolves (raised in review of #1447).
  if (!serverInfo) {
    return <PanelLoading label="Loading admin console" />;
  }

  if (!isCloud) {
    return (
      <div className="h-full overflow-auto">
        <div className="max-w-3xl mx-auto p-3 sm:p-6">
          <CloudOnlyNotice feature="The admin console" />
        </div>
      </div>
    );
  }

  if (!isSuper) {
    return (
      <div className="h-full overflow-auto">
        <div className="max-w-3xl mx-auto p-6">
          <p className="text-sm text-fg-muted">Super-admin only.</p>
        </div>
      </div>
    );
  }

  return <>{children}</>;
}
