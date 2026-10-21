import { useState } from "react";

import { Button } from "@/components/ui/Button";
import { InlineBanner } from "@/components/ui/InlineBanner";

// RoutingPolicyEditor is the strict-JSON editor of one routing-policy
// block (pkg/llmroute, ADR-121/129), shared by every level that edits
// one: the org/team records (RoutingPolicySection) and the per-repo
// integration (RepoDetail's routing section). The schema itself is
// validated SERVER-side — unknown field or bad value = a 400 naming it —
// so the editor only guards the wire shape: an object to replace, null
// to clear, a parse error to refuse. A structured editor grows with the
// delivery-2 fields.
export function RoutingPolicyEditor({
  heading,
  scopeLabel,
  description,
  metaLine,
  storedPolicy,
  canManage,
  busy,
  saveError,
  onSave,
}: {
  heading: string;
  scopeLabel: string;
  description: string;
  /** The stored block as the API returned it (null = the level is unset). */
  storedPolicy: unknown;
  metaLine?: React.ReactNode;
  canManage: boolean;
  busy: boolean;
  saveError: string | null;
  /** Called with the parsed draft: an object replaces, null clears. */
  onSave: (routing: unknown) => void | Promise<void>;
}) {
  const [draft, setDraft] = useState<string | null>(null);
  const stored = JSON.stringify(storedPolicy ?? null, null, 2);
  // A stored block that changes under the editor (a refetch after save, a
  // concurrent editor) discards the draft: the rendered JSON is the
  // server's truth. Adjusting state DURING render is the documented
  // reset-on-prop-change pattern — no effect, no cascading render.
  const [seenStored, setSeenStored] = useState(stored);
  if (seenStored !== stored) {
    setSeenStored(stored);
    setDraft(null);
  }

  const value = draft ?? stored;
  const dirty = value !== stored;
  let parsed: unknown = null;
  let invalid = false;
  if (dirty) {
    try {
      parsed = JSON.parse(value);
    } catch {
      invalid = true;
    }
  }

  return (
    <section className="bg-surface-1 border border-border-subtle rounded-[var(--radius-lg)] shadow-[var(--shadow-sm)] p-4 space-y-2">
      <div className="flex items-baseline justify-between gap-2">
        <h3 className="text-sm font-medium">
          {heading}
          <span className="ml-2 text-xs font-normal text-content-2">{scopeLabel}</span>
        </h3>
        {metaLine}
      </div>
      <p className="text-xs text-content-2">{description}</p>
      {saveError && <InlineBanner tone="danger">{saveError}</InlineBanner>}
      <textarea
        aria-label={heading}
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
            disabled={busy || !dirty || invalid}
            onClick={() => {
              if (!invalid) void onSave(parsed);
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
