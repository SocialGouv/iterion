import { useEffect, useState } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";

import { errorMessage } from "@/lib/errorHints";
import { formatDateTime } from "@/lib/format";
import { Button } from "@/components/ui/Button";
import { InlineBanner } from "@/components/ui/InlineBanner";
import { Spinner } from "@/components/ui/Spinner";
import {
  type RoutingPolicy,
  type RoutingPolicyPut,
  type RoutingPolicyView,
} from "@/api/orgGovernance";

// RoutingPolicySection is the org-level and team-level editor of the
// adaptive-routing policy (ADR-121 delivery 2): the stored block, its
// origin, and a strict-JSON editor whose writes carry the CAS stamp.
// The schema is validated server-side (unknown field or bad value = a
// 400 naming it); a structured editor grows with the delivery-2 fields.
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
  const [draft, setDraft] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  const query = useQuery({
    queryKey: [`${scope}-routing-policy`, id],
    queryFn: () => getPolicy(id),
  });
  const view = query.data ?? null;
  const stored = JSON.stringify(view?.policy ?? null, null, 2);
  useEffect(() => {
    setDraft(null);
  }, [stored]);

  if (query.isLoading) return <Spinner />;
  if (query.isError) {
    return <InlineBanner tone="danger">{errorMessage(query.error)}</InlineBanner>;
  }
  // An older backend (404 → null): say so rather than render an editor
  // whose every save would fail.
  if (view === null) return null;

  const value = draft ?? stored;
  const dirty = value !== stored;

  const save = async (routing: RoutingPolicy | null) => {
    setBusy(true);
    setErr(null);
    try {
      await putPolicy(id, {
        routing,
        expected_updated_at: view.updated_at,
      });
      await queryClient.invalidateQueries({ queryKey: [`${scope}-routing-policy`, id] });
      setDraft(null);
    } catch (e) {
      setErr(errorMessage(e));
    } finally {
      setBusy(false);
    }
  };

  const parseDraft = (): RoutingPolicy | null | "invalid" => {
    if (!dirty) return view.policy;
    try {
      return JSON.parse(value) as RoutingPolicy | null;
    } catch {
      return "invalid";
    }
  };

  return (
    <section className="bg-surface-1 border border-border-subtle rounded-[var(--radius-lg)] shadow-[var(--shadow-sm)] p-4 space-y-2">
      <div className="flex items-baseline justify-between gap-2">
        <h3 className="text-sm font-medium">
          Routing policy
          <span className="ml-2 text-xs font-normal text-content-2">
            {scope} level · ADR-121 · origin {view.origin}
          </span>
        </h3>
        {view.updated_at && (
          <span className="text-xs text-content-2">
            {formatDateTime(view.updated_at)}
            {view.updated_by ? ` · ${view.updated_by}` : ""}
          </span>
        )}
      </div>
      <p className="text-xs text-content-2">
        The (harness, credential) pairs this {scope} may occupy, in order —
        refined by the levels below it, bounded by the platform ceiling. JSON:
        an object to replace, null to unset.
      </p>
      {err && <InlineBanner tone="danger">{err}</InlineBanner>}
      <textarea
        className="w-full h-48 font-mono text-xs bg-surface-0 border border-border-subtle rounded-[var(--radius-md)] p-2"
        value={value}
        readOnly={!canManage}
        onChange={(e) => setDraft(e.target.value)}
        spellCheck={false}
      />
      {canManage && (
        <div className="flex gap-2">
          <Button
            variant="primary"
            disabled={busy || !dirty || parseDraft() === "invalid"}
            onClick={() => {
              const parsed = parseDraft();
              if (parsed !== "invalid") void save(parsed);
            }}
          >
            Save
          </Button>
          <Button variant="ghost" disabled={busy || !dirty} onClick={() => setDraft(null)}>
            Discard
          </Button>
        </div>
      )}
    </section>
  );
}
