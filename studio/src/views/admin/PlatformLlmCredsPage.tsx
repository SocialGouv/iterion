import { useAuth } from "@/auth/AuthContext";
import { CloudOnlyNotice } from "@/components/shared/CloudOnlyNotice";
import { useHeaderSlot } from "@/components/shared/useHeaderSlot";
import { useServerInfoStore } from "@/store/serverInfo";
import ApiKeysPanel from "@/views/account/ApiKeys";
import OAuthConnections from "@/views/account/OAuthConnections";

import AdminNav from "./AdminNav";

// Platform LLM credentials console: the deployment's own DB-backed
// provider credentials — the fallback tier serving every run that
// resolved no tenant credential (after BYOK, org/user forfaits and the
// mutualised pool). Managing them here replaces editing the runner
// deployment's env (k8s secret + rollout) with a sealed row rotation:
// new launches and resumes pick the fresh value immediately.
export default function PlatformLlmCredsPage() {
  const { user: me } = useAuth();
  const isSuper = me?.is_super_admin ?? false;
  const serverInfo = useServerInfoStore((s) => s.info);
  const isCloud = serverInfo?.mode === "cloud";

  useHeaderSlot({
    left: <span className="text-sm font-semibold">Platform LLM credentials</span>,
  });

  if (!isSuper) {
    return (
      <div className="p-6">
        <p className="text-sm text-fg-muted">Super-admin only.</p>
      </div>
    );
  }

  // Deliberate local-mode gate: the /api/admin/llm routes only exist on a
  // cloud deployment. While server_info is still loading we fall through
  // to the panels (their own queries are gated the same way).
  if (serverInfo && !isCloud) {
    return (
      <div className="h-full overflow-auto">
        <div className="max-w-5xl mx-auto p-3 sm:p-6">
          <CloudOnlyNotice feature="Platform LLM credentials" />
        </div>
      </div>
    );
  }

  return (
    <div className="h-full overflow-auto">
      <div className="max-w-5xl mx-auto p-3 sm:p-6 space-y-6">
        <AdminNav />
        <p className="text-sm text-fg-muted">
          These credentials fund every run that resolved no credential of its own — after
          tenant BYOK keys, user/org subscriptions and the mutualised pool, and before the
          runner's env fallback. Rotating a value here takes effect on the next launch or
          resume, with no redeploy.
        </p>
        <ApiKeysPanel platform />
        <OAuthConnections scope={{ platform: true }} />
      </div>
    </div>
  );
}
