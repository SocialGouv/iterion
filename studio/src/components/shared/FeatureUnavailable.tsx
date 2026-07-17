import { LockClosedIcon } from "@radix-ui/react-icons";
import { useLocation } from "wouter";

import { Button } from "@/components/ui/Button";
import { EmptyState } from "@/components/ui/EmptyState";
import { PageHeader } from "@/components/ui/PageHeader";

export interface FeatureUnavailableProps {
  /** Feature name — the page-level title (e.g. "Automations"). */
  title: string;
  /** One-sentence description of what the feature does. */
  description: string;
  /** Explanation shown in the body — defaults to the standard copy. */
  reason?: string;
  /** Alternative surface CTA label. */
  ctaLabel?: string;
  /** Alternative surface CTA href (client-side navigate). */
  ctaHref?: string;
  /** Supplementary hint under the CTA — points at the sensible alternative. */
  ctaHint?: string;
}

// FeatureUnavailable lands on a gated route whose server-info flag is off.
// The router previously fell through to the Home catch-all, which read as
// a broken deep-link; this page names the feature, why it isn't reachable,
// and the adjacent surface that performs the same job on this server.
export default function FeatureUnavailable({
  title,
  description,
  reason = "This feature isn't enabled on this server.",
  ctaLabel,
  ctaHref,
  ctaHint,
}: FeatureUnavailableProps) {
  const [, navigate] = useLocation();
  const cta =
    ctaLabel && ctaHref ? (
      <Button variant="primary" size="sm" onClick={() => navigate(ctaHref)}>
        {ctaLabel}
      </Button>
    ) : undefined;
  return (
    <div className="flex h-full min-h-0 flex-col bg-surface-0 text-fg-default">
      <PageHeader
        icon={<LockClosedIcon className="h-5 w-5" />}
        title={title}
        description={description}
      />
      <div className="flex flex-1 items-center justify-center p-6">
        <EmptyState title={reason} message={ctaHint ?? ""} action={cta} />
      </div>
    </div>
  );
}
