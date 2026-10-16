// CreatedBotBanner — what /bots/new shows once the scaffold landed. The
// local-mode copy claims the new bundle is visible to the orchestrator
// catalog, which only holds while the server's catalog refresh succeeded:
// when it failed, the bot is on disk and unroutable, and the operator reads
// the cause and the retry here.

import type { BotEntryWithSchema } from "@/api/bots";
import { InlineBanner } from "@/components/ui";
import type { CatalogRefreshed } from "@/lib/catalogStale";

export interface CreatedBotBannerProps {
  created: BotEntryWithSchema & CatalogRefreshed;
  isCloud: boolean;
}

export default function CreatedBotBanner({ created, isCloud }: CreatedBotBannerProps) {
  const catalogError = created.catalog_error?.trim();
  return (
    <>
      <InlineBanner
        tone="success"
        title={`${created.display_name?.trim() || created.name} is live`}
      >
        {isCloud ? (
          <>
            Saved to your team&apos;s bot store and visible to the orchestrator catalog. Open it in
            the editor to refine it, or head to its page to wire triggers.
          </>
        ) : catalogError ? (
          <>
            Scaffolded into <code className="font-mono">bots/{created.name}/</code>. Give it a first
            spin in the test pane, or head to its page to wire triggers.
          </>
        ) : (
          <>
            Scaffolded into <code className="font-mono">bots/{created.name}/</code> and visible to
            the orchestrator catalog. Give it a first spin in the test pane, or head to its page to
            wire triggers.
          </>
        )}
      </InlineBanner>
      {catalogError && (
        <InlineBanner tone="warning" title="Catalog not refreshed" layout="inline">
          The orchestrator catalog still advertises the previous set, so nothing routes to this bot
          yet: {catalogError}. Run <code className="font-mono">iterion bots regen-catalog</code>{" "}
          once the cause is fixed.
        </InlineBanner>
      )}
    </>
  );
}
