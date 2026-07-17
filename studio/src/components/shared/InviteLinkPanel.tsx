import { useState } from "react";

import { Button } from "@/components/ui/Button";
import { CopyButton } from "@/components/ui/CopyButton";
import { inviteAcceptURL } from "@/lib/inviteURL";

// InviteLinkPanel shows a freshly-issued invitation as the thing the
// admin actually shares: the acceptance URL. The raw token stays
// available behind a disclosure for out-of-band flows (CLI, chat
// paste into the accept form). Server-side the token appears once, so
// the panel stays until explicitly dismissed.
export default function InviteLinkPanel({
  email,
  token,
  onDismiss,
}: {
  email: string;
  token: string;
  onDismiss: () => void;
}) {
  const [showToken, setShowToken] = useState(false);
  const url = inviteAcceptURL(token);
  return (
    <div className="text-xs bg-surface-0 border border-border-subtle rounded p-3 space-y-2">
      <div className="text-fg-muted">
        Invite link for <span className="font-medium text-fg-default">{email}</span>{" "}
        — share it now, it appears only once:
      </div>
      <div className="flex items-center gap-2">
        <code className="font-mono break-all flex-1 min-w-0">{url}</code>
        <CopyButton value={url} label="Copy invite link" copiedLabel="Copied" />
      </div>
      <div className="flex items-center gap-2">
        <Button variant="ghost" size="sm" onClick={() => setShowToken((v) => !v)}>
          {showToken ? "Hide raw token" : "Raw token"}
        </Button>
        <span className="flex-1" />
        <Button variant="ghost" size="sm" onClick={onDismiss}>
          Done — hide
        </Button>
      </div>
      {showToken && (
        <div className="flex items-center gap-2">
          <code className="font-mono break-all flex-1 min-w-0">{token}</code>
          <CopyButton value={token} label="Copy token" copiedLabel="Copied" />
        </div>
      )}
    </div>
  );
}
