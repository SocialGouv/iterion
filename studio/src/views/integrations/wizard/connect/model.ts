// Extracted from ConnectRepoWizard.tsx to keep that file focused.
// Shared step/URL model for the connect-repo wizard: the step order, the
// provider picker metadata, and the URL helpers driving the fully
// query-string-backed state machine.

import type { ForgeProvider } from "@/api/forgeConnections";

export type Step = "provider" | "authorize" | "repos" | "done";
export const STEP_ORDER: Step[] = ["provider", "authorize", "repos", "done"];
export const STEP_LABEL: Record<Step, string> = {
  provider: "Provider",
  authorize: "Authorize",
  repos: "Repositories",
  done: "Done",
};

export const RETURN_PATH = "/integrations/connect";
export const CONNECT_RETURN_KEY = "iterion.connect-wizard.returnTo";

// Provider-facing metadata for the picker cards. GitHub is flagged as
// "Recommended" (least-privilege App path when the platform is wired).
export const PROVIDER_META: Array<{
  id: ForgeProvider;
  title: string;
  blurb: string;
  recommended?: boolean;
}> = [
  {
    id: "github",
    title: "GitHub",
    blurb: "GitHub App (least privilege) or OAuth / PAT.",
    recommended: true,
  },
  {
    id: "gitlab",
    title: "GitLab",
    blurb: "OAuth if an app is registered — token otherwise.",
  },
  {
    id: "forgejo",
    title: "Forgejo",
    blurb: "Forgejo / Codeberg / Gitea via OAuth or PAT.",
  },
];

// URL parameter helpers — the wizard's state machine is fully driven by
// ?step=/?installed=/?connected=/?provider=/?base= so a page reload or
// back-navigation lands the operator exactly where they left off.
export function readQuery(search: string): URLSearchParams {
  return new URLSearchParams(search);
}

export function withQuery(params: URLSearchParams): string {
  const s = params.toString();
  return s ? `${RETURN_PATH}?${s}` : RETURN_PATH;
}
