// Shared chrome for the platform-settings consoles (bot-roles, sandbox,
// bot-vars, platform-credentials). Every family follows the same shape: a
// super-admin + cloud-mode gate, the AdminNav, a description, a shared error
// banner, a skeleton on first load, and a Save footer that is disabled until
// the draft differs from what is stored. The per-family screen supplies only
// its title, description, the fetched view, and the form body.

import type { ReactNode } from "react";

import { useAuth } from "@/auth/AuthContext";
import { FeatureUnavailableError } from "@/api/adminSettings";
import { errorMessage } from "@/lib/errorHints";

import { Button } from "@/components/ui/Button";
import { EmptyState } from "@/components/ui/EmptyState";
import { InlineBanner } from "@/components/ui/InlineBanner";
import { TableSkeleton } from "@/components/ui/Table";
import { CloudOnlyNotice } from "@/components/shared/CloudOnlyNotice";
import { useHeaderSlot } from "@/components/shared/useHeaderSlot";
import { useServerInfoStore } from "@/store/serverInfo";

import AdminNav from "../AdminNav";

export interface SettingsScaffoldProps {
  // Short label shown in the header slot.
  title: string;
  // The origin/source badge shown at the right of the header ("db", "default",
  // "db+env", …). Omitted while the view is loading.
  headerRight?: ReactNode;
  // One-line explanation under the nav.
  description: ReactNode;
  // "not enabled" empty-state copy for the feature-unavailable (404) case.
  featureName: string;
  unavailableMessage: string;
  // Query state, threaded from the screen's useQuery.
  loaded: boolean;
  unavailable: boolean;
  error: unknown;
  fetching: boolean;
  // Mutation error surfaced in the same banner (wins over the fetch error).
  mutError: string | null;
  // Save footer.
  saveDisabled: boolean;
  saving: boolean;
  onSave: () => void;
  footerLeft?: ReactNode;
  children: ReactNode;
}

export function SettingsScaffold({
  title,
  headerRight,
  description,
  featureName,
  unavailableMessage,
  loaded,
  unavailable,
  error,
  fetching,
  mutError,
  saveDisabled,
  saving,
  onSave,
  footerLeft,
  children,
}: SettingsScaffoldProps) {
  const { user } = useAuth();
  const isSuper = user?.is_super_admin ?? false;
  const serverInfo = useServerInfoStore((s) => s.info);
  const isCloud = serverInfo?.mode === "cloud";

  useHeaderSlot({
    left: <span className="text-sm font-semibold">{title}</span>,
    right: headerRight ?? null,
  });

  if (!isSuper) {
    return (
      <div className="p-6">
        <p className="text-sm text-fg-muted">Super-admin only.</p>
      </div>
    );
  }

  if (serverInfo && !isCloud) {
    return (
      <div className="h-full overflow-auto">
        <div className="max-w-3xl mx-auto p-3 sm:p-6">
          <CloudOnlyNotice feature={featureName} />
        </div>
      </div>
    );
  }

  if (unavailable) {
    return (
      <div className="h-full overflow-auto">
        <div className="max-w-3xl mx-auto p-3 sm:p-6 space-y-4">
          <AdminNav />
          <EmptyState title={`${title} not enabled`} message={unavailableMessage} />
        </div>
      </div>
    );
  }

  const fetchErr =
    error && !(error instanceof FeatureUnavailableError) && !fetching
      ? errorMessage(error)
      : null;
  const banner = mutError ?? fetchErr;

  return (
    <div className="h-full overflow-auto">
      <div className="max-w-3xl mx-auto p-3 sm:p-6 space-y-4">
        <AdminNav />

        <p className="text-caption text-fg-subtle max-w-2xl">{description}</p>

        {banner && (
          <InlineBanner tone="danger" layout="inline">
            {banner}
          </InlineBanner>
        )}

        {!loaded ? (
          <div className="p-3">
            <TableSkeleton rows={3} cols={2} />
          </div>
        ) : (
          <section className="bg-surface-1 border border-border-subtle rounded-[var(--radius-lg)] shadow-[var(--shadow-sm)] p-4 space-y-5">
            {children}
            <div className="flex items-center justify-between gap-3 pt-1">
              <span className="text-caption text-fg-subtle">{footerLeft}</span>
              <Button size="sm" loading={saving} disabled={saveDisabled} onClick={onSave}>
                Save
              </Button>
            </div>
          </section>
        )}
      </div>
    </div>
  );
}

// OriginBadge renders the source/origin readout the families put in the header.
export function OriginBadge({ origin }: { origin: string | undefined }) {
  if (!origin) return null;
  return <span className="text-xs text-fg-muted">origin: {origin}</span>;
}
