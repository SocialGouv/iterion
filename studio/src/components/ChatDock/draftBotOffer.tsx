// A finished standalone draft already exists as a run artifact.
//
// Opening it is presentation, not a reply, so it remains a link in the
// assistant bubble. The earlier "Open the editor" venue button is deliberately
// absent: that transition now lives in the suggested-reply row.

import { useEffect } from "react";
import { ArrowRightIcon, Pencil2Icon } from "@radix-ui/react-icons";
import { Link, useLocation } from "wouter";

import { useDraftState } from "@/hooks/useDraftBot";

const openedDraftRuns = new Set<string>();

export function DraftBotOffer({
  runId,
  revision,
}: {
  runId: string | null;
  revision: number;
}) {
  const { source } = useDraftState(runId, revision);
  const [location, setLocation] = useLocation();
  const onEditor = location.startsWith("/editor");
  const hasDraft = !!source;

  // Open the draft tab once per run after the operator has navigated to the
  // editor. Later turns are handled by EditorTabHost's draft subscription.
  useEffect(() => {
    if (!runId || !hasDraft || !onEditor) return;
    if (openedDraftRuns.has(runId)) return;
    openedDraftRuns.add(runId);
    setLocation(`/editor?draft=${encodeURIComponent(runId)}`, { replace: true });
  }, [runId, hasDraft, onEditor, setLocation]);

  if (!runId || !hasDraft || onEditor) return null;

  return (
    <div className="mt-3 border-t border-border-subtle pt-2.5">
      <Link
        href={`/editor?draft=${encodeURIComponent(runId)}`}
        className="inline-flex w-full items-center justify-center gap-2 rounded-md bg-accent px-3 py-2 text-label font-medium text-accent-contrast hover:brightness-110 focus:outline-none focus:ring-2 focus:ring-accent-emphasis"
      >
        <Pencil2Icon className="h-4 w-4 shrink-0" aria-hidden="true" />
        Open this draft in the editor
        <ArrowRightIcon className="h-4 w-4 shrink-0" aria-hidden="true" />
      </Link>
    </div>
  );
}
