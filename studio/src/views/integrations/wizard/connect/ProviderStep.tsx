// Extracted from ConnectRepoWizard.tsx to keep that file focused.
// Step 1 — pick a forge provider (+ optional self-hosted base URL); the
// PAT/OAuth "other methods" pocket stays reachable behind a disclosure.

import { useState } from "react";

import type {
  ForgeConnection,
  ForgeOAuthApp,
  ForgeProvider,
} from "@/api/forgeConnections";
import { Badge } from "@/components/ui/Badge";
import { Button } from "@/components/ui/Button";
import { Input } from "@/components/ui/Input";
import {
  CONNECTABLE,
  DEFAULT_BASE,
} from "@/views/teams/tabs/integrations/forgeShared";

import { PROVIDER_META } from "./model";
import PatFallback from "./PatFallback";

export interface ProviderStepProps {
  teamID: string;
  oauthApps: ForgeOAuthApp[];
  initialProvider: ForgeProvider;
  initialBase: string;
  onError: (m: string) => void;
  onNextAuthorize: (provider: ForgeProvider, base: string) => void;
  onPatConnected: (conn: ForgeConnection) => void;
}

export default function ProviderStep({
  teamID,
  oauthApps,
  initialProvider,
  initialBase,
  onError,
  onNextAuthorize,
  onPatConnected,
}: ProviderStepProps) {
  const [provider, setProvider] = useState<ForgeProvider>(
    CONNECTABLE.includes(initialProvider) ? initialProvider : "github",
  );
  const [baseURL, setBaseURL] = useState<string>(initialBase);
  const [showSelfHosted, setShowSelfHosted] = useState<boolean>(!!initialBase);

  // The self-hosted disclosure is provider-scoped; switching provider without
  // changing the base URL still hides the panel when it's empty.
  const selfHostedApplicable = provider !== "github" || showSelfHosted;

  return (
    <div className="space-y-4">
      <header>
        <h2 className="text-headline font-semibold">Connect a repository</h2>
        <p className="text-xs text-fg-muted mt-1">
          Choose a forge to authorize iterion against. You'll pick which
          repository to wire up in the next step.
        </p>
      </header>

      <div className="grid grid-cols-1 sm:grid-cols-3 gap-3">
        {PROVIDER_META.map((p) => {
          const disabled = !CONNECTABLE.includes(p.id);
          const active = provider === p.id;
          return (
            <button
              key={p.id}
              type="button"
              onClick={() => !disabled && setProvider(p.id)}
              disabled={disabled}
              aria-pressed={active}
              className={[
                "text-left rounded-[var(--radius-lg)] border p-3 transition-colors",
                active
                  ? "border-accent bg-accent-soft"
                  : "border-border-default bg-surface-1 hover:border-border-strong",
                disabled ? "opacity-60 cursor-not-allowed" : "cursor-pointer",
              ].join(" ")}
            >
              <div className="flex items-center gap-2">
                <span className="font-medium text-sm">{p.title}</span>
                {p.recommended && (
                  <Badge variant="accent" size="sm">
                    Recommended
                  </Badge>
                )}
              </div>
              <p className="text-caption text-fg-muted mt-1">{p.blurb}</p>
            </button>
          );
        })}
      </div>

      <div className="space-y-2">
        {provider !== "github" || showSelfHosted ? (
          <div>
            <label
              htmlFor="wizard-base-url"
              className="block text-caption text-fg-muted mb-1"
            >
              {provider === "github"
                ? "GitHub Enterprise base URL"
                : "Self-hosted base URL"}
            </label>
            <Input
              size="md"
              id="wizard-base-url"
              placeholder={
                provider === "github"
                  ? "https://github.example.com"
                  : provider === "gitlab"
                    ? "https://gitlab.example.com"
                    : "https://codeberg.org"
              }
              value={baseURL}
              onChange={(e) => setBaseURL(e.target.value)}
              autoComplete="off"
            />
            <p className="text-caption text-fg-subtle mt-1">
              Leave blank for {DEFAULT_BASE[provider]}.
            </p>
          </div>
        ) : (
          <button
            type="button"
            className="text-caption text-accent-text hover:underline"
            onClick={() => setShowSelfHosted(true)}
          >
            Self-hosted instance?
          </button>
        )}
        {!selfHostedApplicable && null}
      </div>

      {/* PAT fallback — always available, subdued behind a disclosure so the
          primary OAuth/App path stays the visual centre. */}
      <PatFallback
        teamID={teamID}
        provider={provider}
        baseURL={baseURL}
        oauthApps={oauthApps}
        onError={onError}
        onConnected={onPatConnected}
      />

      <div className="flex items-center justify-end pt-2">
        <Button
          variant="primary"
          onClick={() => onNextAuthorize(provider, baseURL.trim())}
        >
          Continue
        </Button>
      </div>
    </div>
  );
}
