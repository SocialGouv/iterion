import { errorMessage } from "@/lib/errorHints";
import { useState } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";

import { FeatureUnavailableError } from "@/api/client";
import { listOrgSSOProviders } from "@/api/orgSso";
import { InlineBanner } from "@/components/ui/InlineBanner";

import { DomainsSection } from "./sso/DomainsSection";
import { GitHubSection } from "./sso/GitHubSection";
import { KeycloakSection } from "./sso/KeycloakSection";

// SSOTab orchestrates the three per-org SSO sections: Keycloak/OIDC
// providers, verified email domains (the auto-link gate), and the
// GitHub team-gating allow-list. The actual section logic lives in
// ./sso/ — this file only fetches the provider list and routes it.
export default function SSOTab({ teamID, canManage }: { teamID: string; canManage: boolean }) {
  const queryClient = useQueryClient();
  // Section mutations report through setActionErr; load failures surface
  // from the query. One banner shows whichever is current.
  const [actionErr, setActionErr] = useState<string | null>(null);

  const query = useQuery({
    queryKey: ["org-sso-providers", teamID],
    queryFn: () => listOrgSSOProviders(teamID),
  });
  const providers = query.data ?? [];
  const unavailable = query.error instanceof FeatureUnavailableError;
  const err =
    actionErr ??
    (query.error && !unavailable && !query.isFetching
      ? errorMessage(query.error)
      : null);

  const reload = () => {
    setActionErr(null);
    void queryClient.invalidateQueries({ queryKey: ["org-sso-providers", teamID] });
  };

  const oidc = providers.filter((p) => p.kind === "oidc");
  const github = providers.find((p) => p.kind === "github");

  if (unavailable) {
    return (
      <InlineBanner tone="info" layout="inline">
        Per-org SSO is not enabled on this server.
      </InlineBanner>
    );
  }

  return (
    <div className="space-y-6">
      {err && (
        <InlineBanner tone="danger" layout="inline">
          {err}
        </InlineBanner>
      )}
      <KeycloakSection
        teamID={teamID}
        canManage={canManage}
        rows={oidc}
        onChange={reload}
        onError={setActionErr}
      />
      <DomainsSection teamID={teamID} canManage={canManage} onError={setActionErr} />
      <GitHubSection
        key={github?.id ?? "new"}
        teamID={teamID}
        canManage={canManage}
        row={github}
        onChange={reload}
        onError={setActionErr}
      />
    </div>
  );
}
