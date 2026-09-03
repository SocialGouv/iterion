// Extracted from ConnectRepoWizard.tsx to keep that file focused.
// Step 4 — success summary + next actions (return to caller, repos,
// board, launch, connect another).

import { CheckIcon } from "@radix-ui/react-icons";

import { Button } from "@/components/ui/Button";

export interface DoneStepProps {
  connectionID: string;
  repo: string;
  /** The enable was PARKED for an org admin (202): nothing exists on the
   *  forge yet. Rendering the success summary here would tell the operator
   *  automation is live and leave them waiting for reviews that never fire. */
  pendingApproval?: boolean;
  onGoToRepos: () => void;
  onOpenBoard: () => void;
  onLaunchBot: () => void;
  onConnectAnother: () => void;
  /** In-app path the caller asked to come back to (sanitized). */
  returnTo?: string | null;
  onReturn?: () => void;
}

export default function DoneStep({
  connectionID,
  repo,
  pendingApproval,
  onGoToRepos,
  onOpenBoard,
  onLaunchBot,
  onConnectAnother,
  returnTo,
  onReturn,
}: DoneStepProps) {
  if (pendingApproval) {
    return (
      <div className="space-y-4">
        <header className="space-y-1">
          <h2 className="text-headline font-semibold">Awaiting org approval</h2>
          <p className="text-xs text-fg-muted">
            The connection is ready, but your organization requires an org
            admin to approve repo provisioning. The request to enable your
            selected bots on{" "}
            <span className="font-mono">{repo || "this repository"}</span> is
            queued — <strong>nothing is created on the forge</strong> until it
            is approved, so no bot runs yet. It appears under the team&apos;s
            Integrations tab and the org admins&apos; approval queue.
          </p>
        </header>

        {connectionID && (
          <div className="text-caption text-fg-subtle">
            Connection <span className="font-mono">{connectionID}</span>
          </div>
        )}

        <div className="flex flex-wrap items-center gap-2">
          {onReturn && (
            <Button variant="primary" onClick={onReturn}>
              Continue
            </Button>
          )}
          <Button variant={onReturn ? "secondary" : "primary"} onClick={onGoToRepos}>
            Go to Repositories
          </Button>
          <Button variant="ghost" onClick={onConnectAnother}>
            Connect another
          </Button>
        </div>
      </div>
    );
  }
  return (
    <div className="space-y-4">
      <header className="space-y-1">
        <h2 className="text-headline font-semibold">
          <span className="inline-flex items-center gap-2">
            <span
              aria-hidden
              className="inline-flex h-6 w-6 items-center justify-center rounded-full bg-success-soft text-success-fg"
            >
              <CheckIcon className="h-4 w-4" />
            </span>
            Repository connected
          </span>
        </h2>
        <p className="text-xs text-fg-muted">
          {repo ? (
            <>
              <span className="font-mono">{repo}</span> is now wired to this
              team. Selected bots have been provisioned with the required
              webhooks and tokens.
            </>
          ) : (
            "Your bots have been provisioned. You can enable more repositories on this connection any time from the Integrations page."
          )}
        </p>
      </header>

      {connectionID && (
        <div className="text-caption text-fg-subtle">
          Connection <span className="font-mono">{connectionID}</span>
        </div>
      )}

      <div className="flex flex-wrap items-center gap-2">
        {onReturn && (
          <Button variant="primary" onClick={onReturn}>
            {returnTo?.startsWith("/integrations/bind") ? "Continue binding" : "Continue"}
          </Button>
        )}
        <Button variant={onReturn ? "secondary" : "primary"} onClick={onGoToRepos}>
          Go to Repositories
        </Button>
        <Button variant="secondary" onClick={onOpenBoard}>
          Open the board
        </Button>
        <Button variant="secondary" onClick={onLaunchBot}>
          Launch a bot
        </Button>
        <Button variant="ghost" onClick={onConnectAnother}>
          Connect another
        </Button>
      </div>
    </div>
  );
}
