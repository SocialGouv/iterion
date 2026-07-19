// Extracted from LaunchView.tsx to keep that file focused.
// Deep-link with nothing to launch (no ?file= and a pristine editor
// buffer): never offer the implicit empty scaffold — route the user
// to a real workflow instead. In cloud mode /bots is the launch
// surface (the raw editor is a power-user detour); local/desktop keeps
// the editor as its primary entry point.

import { Button } from "@/components/ui/Button";
import { EmptyState } from "@/components/ui/EmptyState";

export interface NoSourceEmptyStateProps {
  cloud: boolean;
  onNavigate: (path: string) => void;
}

export default function NoSourceEmptyState({
  cloud,
  onNavigate,
}: NoSourceEmptyStateProps) {
  return (
    <div className="h-full flex flex-col bg-surface-1 text-fg-default">
      {cloud ? (
        <EmptyState
          title="No workflow to launch"
          message="This page launches a specific workflow, but none is selected. Pick a bot from the gallery, then launch from its home."
          action={
            <Button
              size="sm"
              variant="primary"
              onClick={() => onNavigate("/bots")}
            >
              Browse bots
            </Button>
          }
          secondaryAction={
            <Button
              size="sm"
              variant="secondary"
              onClick={() => onNavigate("/")}
            >
              Home
            </Button>
          }
        />
      ) : (
        <EmptyState
          title="No workflow to launch"
          message="This page launches a specific workflow, but none is selected. Open a .bot file in the Editor, or pick a bot or recent workflow from Home, then launch from there."
          action={
            <Button
              size="sm"
              variant="primary"
              onClick={() => onNavigate("/editor")}
            >
              Open the Editor
            </Button>
          }
          secondaryAction={
            <Button
              size="sm"
              variant="secondary"
              onClick={() => onNavigate("/")}
            >
              Browse workflows
            </Button>
          }
        />
      )}
    </div>
  );
}
