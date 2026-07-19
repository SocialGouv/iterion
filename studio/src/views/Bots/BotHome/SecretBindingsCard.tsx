// Extracted from BotHome/index.tsx to keep that file focused.
// Secret bindings card (cloud mode only) — the team's secrets bound to
// this bot, linking to Integrations → Bindings for management.

import { useQuery } from "@tanstack/react-query";
import { Link } from "wouter";

import { listBindings, type BotSecretBinding } from "@/api/botBindings";
import { FeatureUnavailableError } from "@/api/client";
import { useAuth } from "@/auth/AuthContext";
import { Card, InlineBanner, Spinner } from "@/components/ui";
import { errorMessage } from "@/lib/errorHints";

import SectionTitle from "./SectionTitle";

export default function SecretBindingsCard({ botName }: { botName: string }) {
  const { activeTeam } = useAuth();
  const teamID = activeTeam?.team_id;
  const bindingsQuery = useQuery<BotSecretBinding[]>({
    queryKey: ["bot-secret-bindings", teamID, botName],
    queryFn: () => listBindings(teamID ?? "", botName),
    enabled: !!teamID,
  });
  const bindings = bindingsQuery.data ?? null;
  // No binding store wired (FeatureUnavailableError) stays silent.
  const err =
    bindingsQuery.error && !(bindingsQuery.error instanceof FeatureUnavailableError)
      ? errorMessage(bindingsQuery.error)
      : null;

  if (!teamID) return null;

  return (
    <Card>
      <div className="flex items-center justify-between">
        <SectionTitle flush>Secret bindings</SectionTitle>
        <Link
          href="/integrations?tab=bindings"
          className="text-caption text-accent-text hover:underline"
        >
          Manage →
        </Link>
      </div>
      {err && (
        <InlineBanner tone="danger" title="Couldn't load bindings">
          {err}
        </InlineBanner>
      )}
      {bindings === null && !err ? (
        <div className="flex items-center gap-2 py-2 text-sm text-fg-muted">
          <Spinner /> Loading bindings…
        </div>
      ) : (bindings ?? []).length === 0 && !err ? (
        <p className="py-1 text-xs text-fg-subtle">
          No secrets bound to this bot — bind one from Integrations → Bindings.
        </p>
      ) : (
        <ul className="mt-1 space-y-1">
          {(bindings ?? []).map((b) => (
            <li
              key={b.id}
              className="flex items-center gap-2 rounded-md border border-border-default bg-surface-2 px-2 py-1.5 text-xs"
            >
              <span className="font-mono text-fg-default">{b.secret_name_for_workflow}</span>
              {b.allowed_hosts && b.allowed_hosts.length > 0 && (
                <span className="text-caption text-fg-subtle">
                  hosts: {b.allowed_hosts.join(", ")}
                </span>
              )}
            </li>
          ))}
        </ul>
      )}
    </Card>
  );
}
