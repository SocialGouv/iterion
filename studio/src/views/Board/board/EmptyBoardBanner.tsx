import { useState } from "react";

import { Button } from "@/components/ui/Button";
import { Kbd } from "@/components/ui/Kbd";
import { readBooleanFlag, writeBooleanFlag } from "@/lib/localStorageFlag";
import { useServerInfoStore } from "@/store/serverInfo";

const EMPTY_BANNER_DISMISSED_KEY = "iterion.board.empty-banner-dismissed";

// EmptyBoardBanner renders a compact, dismissable onboarding strip
// above the column grid when the board has zero issues. Dismissal
// persists across reloads via localStorage so the chrome only nags on
// first encounter; the columns themselves stay visible regardless.
export function EmptyBoardBanner({ onCreate }: { onCreate: () => void }) {
  const [dismissed, setDismissed] = useState(() =>
    readBooleanFlag(EMPTY_BANNER_DISMISSED_KEY),
  );
  const dispatcherEnabled = useServerInfoStore(
    (s) => s.info?.dispatcher_enabled === true,
  );
  if (dismissed) return null;
  const dismiss = () => {
    setDismissed(true);
    writeBooleanFlag(EMPTY_BANNER_DISMISSED_KEY, true);
  };
  return (
    <div className="shrink-0 mx-3 mt-3 rounded border border-border-default bg-surface-1 p-3 text-sm text-fg-default flex items-start gap-3">
      <div className="flex-1 min-w-0">
        <div className="font-medium mb-0.5">Your kanban is empty</div>
        <div className="text-fg-muted text-xs leading-relaxed">
          Create your first issue (or press <Kbd>c</Kbd>) · Issues land in the
          first <em>eligible</em> column (green dot) ·{" "}
          {dispatcherEnabled ? (
            <>
              Wire a dispatcher at{" "}
              <code className="text-xs bg-surface-2 px-1 rounded">/dispatcher</code>{" "}
              to auto-run workflows
            </>
          ) : (
            <>Assign a bot to an issue and it launches when the card is staged</>
          )}{" "}
          · Press <Kbd>?</Kbd> for shortcuts
        </div>
      </div>
      <Button variant="primary" size="sm" onClick={onCreate}>
        + Create issue
      </Button>
      <button
        type="button"
        onClick={dismiss}
        className="text-fg-subtle hover:text-fg-default transition-colors leading-none text-lg px-1"
        title="Dismiss"
        aria-label="Dismiss onboarding banner"
      >
        ×
      </button>
    </div>
  );
}
