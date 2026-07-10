import type { ReactNode } from "react";

import { EmptyState } from "@/components/ui/EmptyState";

/** CloudOnlyNotice is the standard local-mode degradation for cloud-only
 * surfaces (/account API keys, /admin, /integrations). Gate on
 * `useServerInfoStore` (`info.mode !== "cloud"`) BEFORE any fetch so the
 * view never fires a doomed request, then render this instead of the
 * feature's forms/tables. The wording mirrors the /admin
 * FeatureUnavailableError banner so every route degrades the same way. */
export function CloudOnlyNotice({
  feature,
  title,
  hint,
}: {
  /** Sentence subject, e.g. "Organization administration". */
  feature: string;
  /** Headline; defaults to the feature name. */
  title?: ReactNode;
  /** Optional extra line, e.g. a pointer to the local-mode equivalent. */
  hint?: ReactNode;
}) {
  return (
    <EmptyState
      title={title ?? feature}
      message={
        <>
          {feature} is a cloud-mode feature — it isn't available on this
          server (local/desktop mode).
          {hint ? <> {hint}</> : null}
        </>
      }
    />
  );
}
