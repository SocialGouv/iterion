// Extracted from ConnectRepoWizard.tsx to keep that file focused.
// Manifest-flow card: GitHub creates the App from the manifest iterion
// sends, then 302s back to our callback.

import { useState } from "react";
import { useQuery } from "@tanstack/react-query";

import {
  getMyGitHubOrgs,
  startGitHubManifest,
  startGitHubOrgsPicker,
} from "@/api/forgeConnections";
import { Button } from "@/components/ui/Button";
import { Checkbox } from "@/components/ui/Checkbox";
import { Input } from "@/components/ui/Input";
import { Select } from "@/components/ui/Select";
import { errorMessage } from "@/lib/errorHints";

import { RETURN_PATH } from "./model";

export interface CreateGitHubAppCardProps {
  teamID: string;
  baseURL: string;
  onError: (m: string) => void;
}

export default function CreateGitHubAppCard({
  teamID,
  baseURL,
  onError,
}: CreateGitHubAppCardProps) {
  const [busy, setBusy] = useState(false);
  const [orgIsCustom, setOrgIsCustom] = useState(false);
  const [githubOrg, setGithubOrg] = useState("");
  const [allowRepoCreation, setAllowRepoCreation] = useState(true);
  const [allowAppDelivery, setAllowAppDelivery] = useState(true);
  // Off by default: alert data names every vulnerable dependency of every
  // repo, so the team opts in deliberately.
  const [allowSecurityRead, setAllowSecurityRead] = useState(false);

  // Fetch failures stay silent (no error surfaced) — the "Pick from
  // GitHub" link below covers the empty case.
  const orgsQuery = useQuery<string[]>({
    queryKey: ["github-my-orgs"],
    queryFn: () => getMyGitHubOrgs(),
  });
  const orgs = orgsQuery.data ?? [];

  const pickOrgs = async () => {
    try {
      const { authorize_url } = await startGitHubOrgsPicker(RETURN_PATH);
      window.location.href = authorize_url;
    } catch (e) {
      onError(errorMessage(e));
    }
  };

  const create = async () => {
    setBusy(true);
    try {
      const { post_url, manifest } = await startGitHubManifest(teamID, {
        forge_base_url: baseURL.trim() || undefined,
        github_org: githubOrg.trim() || undefined,
        allow_repo_creation: allowRepoCreation,
        allow_app_delivery: allowAppDelivery,
        allow_security_read: allowSecurityRead,
        next: RETURN_PATH,
      });
      // Auto-submit the hidden form: GitHub swallows the manifest, creates
      // the App, and 302s back to our callback (which redirects us here
      // with ?installed=<oauth_app_id>).
      const form = document.createElement("form");
      form.method = "POST";
      form.action = post_url;
      const field = document.createElement("input");
      field.type = "hidden";
      field.name = "manifest";
      field.value = JSON.stringify(manifest);
      form.appendChild(field);
      document.body.appendChild(form);
      form.submit();
    } catch (e) {
      onError(errorMessage(e));
      setBusy(false);
    }
  };

  return (
    <div className="rounded-[var(--radius-lg)] border border-border-default bg-surface-1 p-4 space-y-3">
      <div>
        <div className="font-medium text-sm">Create your GitHub App</div>
        <p className="text-caption text-fg-muted mt-0.5">
          GitHub creates the App with the manifest iterion sends, then brings
          you back here. One click, no admin token.
        </p>
      </div>

      <div className="space-y-1">
        <label
          htmlFor="wizard-gh-org"
          className="block text-caption text-fg-muted"
        >
          Create under
        </label>
        <div className="flex items-center gap-2">
          <Select
            size="md"
            id="wizard-gh-org"
            value={orgIsCustom ? "__other__" : githubOrg}
            onChange={(e) => {
              const v = e.target.value;
              if (v === "__other__") {
                setOrgIsCustom(true);
                setGithubOrg("");
              } else {
                setOrgIsCustom(false);
                setGithubOrg(v);
              }
            }}
          >
            <option value="">Your personal account</option>
            {orgs.map((o) => (
              <option key={o} value={o}>
                {o}
              </option>
            ))}
            <option value="__other__">Other organization…</option>
          </Select>
          <button
            type="button"
            className="text-caption text-accent-text hover:underline shrink-0"
            onClick={() => void pickOrgs()}
          >
            {orgs.length > 0 ? "Refresh from GitHub" : "Pick from GitHub"}
          </button>
        </div>
        {orgIsCustom && (
          <Input
            size="md"
            placeholder="Org login (e.g. SocialGouv)"
            value={githubOrg}
            onChange={(e) => setGithubOrg(e.target.value)}
            autoComplete="off"
          />
        )}
      </div>

      <Checkbox
        id="wizard-allow-repo-creation"
        checked={allowRepoCreation}
        onChange={(e) => setAllowRepoCreation(e.target.checked)}
        label={
          <span className="text-fg-default text-xs">
            <span className="font-medium">
              Allow iterion to create repositories in this org
            </span>
            <span className="block text-caption text-fg-muted mt-0.5">
              Adds the Administration: write permission so bots can bootstrap new
              repositories (recommended). You can revoke this on GitHub later.
            </span>
          </span>
        }
      />

      <Checkbox
        id="wizard-allow-app-delivery"
        checked={allowAppDelivery}
        onChange={(e) => setAllowAppDelivery(e.target.checked)}
        label={
          <span className="text-fg-default text-xs">
            <span className="font-medium">
              Allow iterion to publish CI workflows and container images
            </span>
            <span className="block text-caption text-fg-muted mt-0.5">
              Adds Workflows: write + Packages: write, so a bot can ship an app
              it built. Without them GitHub refuses any push that touches
              .github/workflows/, which blocks the build-and-deploy chain.
            </span>
          </span>
        }
      />

      <Checkbox
        id="wizard-allow-security-read"
        checked={allowSecurityRead}
        onChange={(e) => setAllowSecurityRead(e.target.checked)}
        label={
          <span className="text-fg-default text-xs">
            <span className="font-medium">
              Allow iterion to read this org&apos;s Dependabot alerts
            </span>
            <span className="block text-caption text-fg-muted mt-0.5">
              Adds Dependabot alerts: read, so a vulnerability-watch bot can see
              which repositories are affected. Alert data names every vulnerable
              dependency of every repository, so it is off by default — and it
              is only ever minted into a dedicated token, never the one bots
              push with.
            </span>
          </span>
        }
      />

      <Button
        variant="primary"
        onClick={() => void create()}
        disabled={busy}
        loading={busy}
      >
        {busy ? "Opening GitHub…" : "Create a GitHub App"}
      </Button>
    </div>
  );
}
