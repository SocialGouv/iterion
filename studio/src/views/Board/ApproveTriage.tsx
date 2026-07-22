import { ShieldAlert } from "lucide-react";

import { Button } from "@/components/ui/Button";
import { useAsyncAction } from "@/hooks/useAsyncAction";
import { useUIStore } from "@/store/ui";
import { patchIssue, type NativeIssue } from "@/api/native";

// Board-local trust labels stamped by the forge→board ingest gate. Keep in
// sync with pkg/dispatcher/native/board.go (LabelTriageAuto /
// LabelNeedsApproval).
export const LABEL_TRIAGE_AUTO = "triage:auto";
export const LABEL_NEEDS_APPROVAL = "needs:approval";

export function needsApproval(iss: NativeIssue): boolean {
  return (iss.labels ?? []).includes(LABEL_NEEDS_APPROVAL);
}

// approveTriageLabels is the label swap the approve gesture performs:
// needs:approval out, triage:auto in — the trigger spine picks the card up
// from there (consumes triage:auto and launches the triage bot on it).
function approveTriageLabels(iss: NativeIssue): string[] {
  const rest = (iss.labels ?? []).filter(
    (l) => l !== LABEL_NEEDS_APPROVAL && l !== LABEL_TRIAGE_AUTO,
  );
  return [...rest, LABEL_TRIAGE_AUTO];
}

// ApproveTriageBanner renders the parked-card affordance: an amber "external
// author — approval required" notice plus the explicit "Approve & triage"
// action. Shown wherever a card carrying needs:approval is rendered (card
// footer + issue modal). Dragging the card is NOT an approval — only this
// label swap (or a manual triage:auto in the label editor) arms the triage.
export function ApproveTriageBanner({
  iss,
  compact,
  onApproved,
}: {
  iss: NativeIssue;
  compact?: boolean;
  // Notified with the new label set after a successful approve, so an
  // embedding editor (issue modal) can sync its local label state instead
  // of overwriting the swap on its next save.
  onApproved?: (labels: string[]) => void;
}) {
  const addToast = useUIStore((s) => s.addToast);
  const approveAction = useAsyncAction();

  if (!needsApproval(iss)) return null;

  const author = iss.external?.author;

  const approve = async (e: React.MouseEvent) => {
    e.stopPropagation();
    const next = approveTriageLabels(iss);
    const res = await approveAction.run(() => patchIssue(iss.id, { labels: next }));
    if (res) {
      addToast(
        "Approved — the triage bot will stamp the handler bot on this card shortly.",
        "success",
      );
      onApproved?.(res.labels ?? next);
    } else if (approveAction.error) {
      addToast(approveAction.error, "error");
    }
  };

  return (
    <div
      className={`flex items-center gap-2 rounded bg-warning-soft px-1.5 py-1 text-caption text-warning-fg ${
        compact ? "mt-1" : ""
      }`}
      title={
        "This issue was opened by an external author without write access on the repo. " +
        "No bot runs on it until you approve — approving swaps needs:approval for triage:auto, " +
        "which fires a cheap triage run that stamps the handler bot (the card stays here; " +
        "drag to Ready to launch the handler)."
      }
    >
      <ShieldAlert className="h-3.5 w-3.5 shrink-0" aria-hidden="true" />
      <span className="flex-1 truncate">
        external{author ? `: @${author}` : " author"} — approval required
      </span>
      <Button
        variant="secondary"
        size="sm"
        loading={approveAction.busy}
        onClick={(e) => void approve(e)}
      >
        Approve &amp; triage
      </Button>
    </div>
  );
}
