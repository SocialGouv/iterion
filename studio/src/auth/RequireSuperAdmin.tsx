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
// Otherwise render the children.

import type { ReactNode } from "react";

import { useAuth } from "@/auth/AuthContext";
import { CloudOnlyNotice } from "@/components/shared/CloudOnlyNotice";
import { useServerInfoStore } from "@/store/serverInfo";

export function RequireSuperAdmin({ children }: { children: ReactNode }) {
  const { user } = useAuth();
  const isSuper = user?.is_super_admin ?? false;
  const serverInfo = useServerInfoStore((s) => s.info);
  const isCloud = serverInfo?.mode === "cloud";

  // While server_info is still loading, render the children: local mode
  // synthesises a super-admin and each page runs its own gate, so nothing
  // sensitive shows before the mode is known.
  if (serverInfo && !isCloud) {
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
