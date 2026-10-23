import type { BotDiscoveryError } from "@/api/bots";
import { InlineBanner } from "@/components/ui";

/** The gallery's word on what discovery could not read.
 *
 *  The workspace overlay its reader refused is no skipped bot: every bot is
 *  then listed DISABLED — the operator's visibility decisions are unread —
 *  and the doors that read bot visibility (card launch, slash-command
 *  discovery, the catalogue's regeneration) route none until it is read;
 *  the server's text says every other door does not read it (#2676). It is
 *  said apart, as the danger it is, with the refusal and its remedy: the
 *  toggles beside it show no decision of the operator's. Every other kind
 *  is a source the walk skipped or listed short, counted as such. */
export function DiscoveryErrorBanners({ errors }: { errors: readonly BotDiscoveryError[] }) {
  const overlay = errors.filter((d) => d.kind === "overlay");
  const skipped = errors.filter((d) => d.kind !== "overlay");
  return (
    <>
      {overlay.map((d, i) => (
        <InlineBanner key={`overlay-${i}`} tone="danger" title="The workspace's bot visibility overlay is not read">
          <code>{d.path}</code>: {d.error}
        </InlineBanner>
      ))}
      {skipped.length > 0 && (
        <InlineBanner tone="warning" title={`${skipped.length} bot${skipped.length === 1 ? "" : "s"} skipped`}>
          <ul className="list-disc pl-4">
            {skipped.map((d, i) => (
              <li key={`${i}-${d.path}`}>
                <code>{d.path}</code>: {d.error}
              </li>
            ))}
          </ul>
        </InlineBanner>
      )}
    </>
  );
}
