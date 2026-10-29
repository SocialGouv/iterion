import { Button, InlineBanner } from "@/components/ui";
import type { LaunchVarsRefusal } from "@/api/forgeConnections";

export interface LaunchVarsRefusalBannerProps {
  refusal: LaunchVarsRefusal;
  // pins is the launch_vars map this page holds (the integration's). It is
  // what an echo re-sends, and the only source of a VALUE for a named key:
  // the refusal names keys, never their values.
  pins: Record<string, string>;
  busy: boolean;
  // onEcho re-sends the write with the named pins restored.
  onEcho: () => void;
  // onReplace re-sends the write with launch_vars_replace, making the map
  // this page shows the repo's whole set of pins.
  onReplace: () => void;
  // onRetry re-sends the SAME request: the retry-convergent divergence needs
  // no gesture, only another attempt.
  onRetry: () => void;
  onDismiss: () => void;
}

// echoableKeys are the named keys this page can supply a value for. A key the
// page does not hold is pinned on the webhook config alone (an out-of-band
// PATCH, or an integration write that crashed between the two stores), and
// echoing it with a blank value would REWRITE the pin to "" — a silent change
// of the repo's configuration dressed as a remedy.
function echoableKeys(keys: string[], pins: Record<string, string>): string[] {
  return keys.filter((k) => pins[k] !== undefined);
}

// LaunchVarsRefusalBanner turns a provisioning launch_vars refusal into the
// gestures the operator can actually make, instead of the dead end the ticket
// reports (#2173): the server names the pins at stake and the two remedies,
// and until now the studio showed a bare failure and the operator dropped to
// curl.
//
// The three kinds are three different remedies, read from the refusal's stable
// code — a retry-convergent divergence (the store blipped, provisioning
// converges) must NOT ask an operator to restate pins, and a deterministic one
// must not offer a retry that cannot work.
export default function LaunchVarsRefusalBanner({
  refusal,
  pins,
  busy,
  onEcho,
  onReplace,
  onRetry,
  onDismiss,
}: LaunchVarsRefusalBannerProps) {
  if (refusal.kind === "retry") {
    return (
      <InlineBanner tone="warning" title="Provisioning diverged — re-run the same request">
        <div className="space-y-2">
          <p>
            The write landed on one of the two stores the repo&apos;s settings live on. Provisioning
            is idempotent: re-sending the same request converges them. Nothing to restate.
          </p>
          <div className="flex gap-2">
            <Button size="sm" variant="primary" disabled={busy} onClick={onRetry}>
              Retry
            </Button>
            <Button size="sm" variant="ghost" disabled={busy} onClick={onDismiss}>
              Dismiss
            </Button>
          </div>
        </div>
      </InlineBanner>
    );
  }

  // A deterministic divergence (`diverged`) carries no key list: its answer is
  // a 500, whose payload is floored to its code, so the pins this page holds
  // are the ones to name. The drop refusal (a 400) keeps the server's list.
  const named = refusal.kind === "drop" ? refusal.keys : Object.keys(pins).sort();
  const echoable = echoableKeys(named, pins);
  const unknown = named.filter((k) => !echoable.includes(k));

  return (
    <InlineBanner
      tone="danger"
      title={
        refusal.kind === "drop"
          ? "That change would drop pins this repo already holds"
          : "This repo's pins disagree between its two stores"
      }
    >
      <div className="space-y-2">
        <p>
          {refusal.kind === "drop"
            ? "The request did not mention these pins, so applying it would silently unset them:"
            : "A re-provision cannot tell a deliberate drop from a stale store, so it refuses. State the set you want:"}
        </p>
        <ul className="font-mono text-caption">
          {named.map((k) => (
            <li key={k}>
              {k}
              {pins[k] !== undefined ? ` = ${pins[k]}` : " (value pinned out of band — unknown here)"}
            </li>
          ))}
        </ul>
        {named.length === 0 && (
          <p>
            {refusal.kind === "drop"
              ? "The refusal named no key, so there is nothing to echo — replace the map to state the set you want."
              : "This page holds no pin for the repo — replace the map to clear the stored set."}
          </p>
        )}
        {unknown.length > 0 && (
          <p>
            Echoing is unavailable: {unknown.join(", ")} {unknown.length === 1 ? "is" : "are"} pinned
            on the webhook config alone, so this page has no value to echo. Replacing the whole map
            states the set shown above and drops the rest.
          </p>
        )}
        <div className="flex gap-2">
          <Button
            size="sm"
            variant="primary"
            disabled={busy || unknown.length > 0 || echoable.length === 0}
            onClick={onEcho}
          >
            Echo the pins
          </Button>
          <Button size="sm" variant="secondary" disabled={busy} onClick={onReplace}>
            Replace the whole map
          </Button>
          <Button size="sm" variant="ghost" disabled={busy} onClick={onDismiss}>
            Dismiss
          </Button>
        </div>
        {refusal.message && <p className="text-caption text-fg-subtle">{refusal.message}</p>}
      </div>
    </InlineBanner>
  );
}
