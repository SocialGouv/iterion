import { useState } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";

import { RoutingPolicyEditor } from "@/components/shared/RoutingPolicyEditor";
import { errorMessage } from "@/lib/errorHints";
import { formatDateTime } from "@/lib/format";
import { InlineBanner } from "@/components/ui/InlineBanner";
import { Spinner } from "@/components/ui/Spinner";
import {
  type RoutingPolicy,
  type RoutingPolicyPut,
  type RoutingPolicyView,
} from "@/api/orgGovernance";

// RoutingPolicySection is the org-level and team-level editor of the
// adaptive-routing policy (ADR-121 delivery 2): the stored block, its
// origin, and the shared strict-JSON editor whose writes carry the CAS
// stamp. The schema is validated server-side (unknown field or bad value
// = a 400 naming it).
export default function RoutingPolicySection({
  scope,
  id,
  canManage,
  getPolicy,
  putPolicy,
}: {
  scope: "org" | "team";
  id: string;
  canManage: boolean;
  getPolicy: (id: string) => Promise<RoutingPolicyView | null>;
  putPolicy: (id: string, body: RoutingPolicyPut) => Promise<RoutingPolicyView>;
}) {
  const queryClient = useQueryClient();
  const [err, setErr] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  const query = useQuery({
    queryKey: [`${scope}-routing-policy`, id],
    queryFn: () => getPolicy(id),
  });
  const view = query.data ?? null;

  if (query.isLoading) return <Spinner />;
  if (query.isError) {
    return <InlineBanner tone="danger">{errorMessage(query.error)}</InlineBanner>;
  }
  // An older backend (404 → null): say so rather than render an editor
  // whose every save would fail.
  if (view === null) return null;

  const save = async (routing: unknown) => {
    setBusy(true);
    setErr(null);
    try {
      await putPolicy(id, {
        routing: routing as RoutingPolicy | null,
        expected_updated_at: view.updated_at,
      });
      await queryClient.invalidateQueries({ queryKey: [`${scope}-routing-policy`, id] });
    } catch (e) {
      setErr(errorMessage(e));
    } finally {
      setBusy(false);
    }
  };

  return (
    <RoutingPolicyEditor
      heading="Routing policy"
      scopeLabel={`${scope} level · ADR-121 · origin ${view.origin}`}
      description={`The (harness, credential) pairs this ${scope} may occupy, in order — refined by the levels below it, bounded by the platform ceiling. JSON: an object to replace, null to unset.`}
      metaLine={
        view.updated_at ? (
          <span className="text-xs text-fg-muted">
            {formatDateTime(view.updated_at)}
            {view.updated_by ? ` · ${view.updated_by}` : ""}
          </span>
        ) : undefined
      }
      storedPolicy={view.policy}
      canManage={canManage}
      busy={busy}
      saveError={err}
      onSave={save}
    />
  );
}
