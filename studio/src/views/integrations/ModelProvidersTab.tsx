import OAuthConnections from "@/views/account/OAuthConnections";

// Model providers groups the LLM subscriptions a team's bots run on
// (Claude Code / OpenAI Codex subscription OAuth). Split out of the forge tab
// so each integration surface stays single-purpose: git on one side, the model
// credentials that power the bots on the other. Reuses the same
// OAuthConnections panel as the personal/org settings, scoped to the team.
export default function ModelProvidersTab({ teamID }: { teamID: string }) {
  return (
    <div className="space-y-3">
      <div>
        <h3 className="font-medium">Model subscriptions</h3>
        <p className="text-xs text-fg-muted">
          Connect a Claude Code or OpenAI Codex subscription so this team's bots run on your
          subscription instead of metered API keys.
        </p>
      </div>
      <OAuthConnections scope={{ teamId: teamID }} org />
    </div>
  );
}
