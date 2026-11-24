import { Link } from "wouter";

import OAuthConnections from "@/views/account/OAuthConnections";
import CredentialPreview from "@/views/account/CredentialPreview";

// Model providers groups the LLM subscriptions a team's bots run on
// (Claude Code / OpenAI Codex subscription OAuth). Split out of the forge tab
// so each integration surface stays single-purpose: git on one side, the model
// credentials that power the bots on the other. Reuses the same
// OAuthConnections panel as the personal/org settings, scoped to the team.
//
// The other model credential, a provider API key, is not managed here: it
// belongs to the team page's "API keys" tab, which this tab points to so that
// an operator looking for it under "Model providers" finds the way.
export default function ModelProvidersTab({ teamID }: { teamID: string }) {
  return (
    <div className="space-y-3">
      <div>
        <h3 className="font-medium">Model subscriptions</h3>
        <p className="text-xs text-fg-muted">
          Connect a Claude Code or OpenAI Codex subscription so this team's bots run on your
          subscription instead of metered API keys.
        </p>
        <p className="text-xs text-fg-muted mt-1">
          Provider API keys (Anthropic, an OpenAI-compatible gateway…) are set in the team
          settings, under{" "}
          <Link
            href={`/teams/${teamID}?tab=api-keys`}
            className="text-accent-text underline underline-offset-2 hover:decoration-2"
          >
            API keys
          </Link>
          .
        </p>
      </div>
      <OAuthConnections scope={{ teamId: teamID }} org />
      <CredentialPreview teamID={teamID} />
    </div>
  );
}
