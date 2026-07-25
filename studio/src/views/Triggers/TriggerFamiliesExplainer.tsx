import { Link } from "wouter";
import { ChevronRight } from "lucide-react";

// TriggerFamiliesExplainer is the collapsed "what can fire a bot?" panel on
// the Automations tab. The intro copy enumerates the event families; this
// panel explains each one and — crucially — WHICH surface owns it, because
// only two of the five (board, run-completion) are created on this page:
// forge webhooks are provisioned by the repo binding (Integrations →
// Repositories), schedules live on the sibling tab, and inbound iwh_ tokens
// live in Integrations → Webhooks.
export default function TriggerFamiliesExplainer({
  onOpenSchedules,
  onNewTrigger,
}: {
  /** Switches to the Schedules tab of this page (URL-synced). */
  onOpenSchedules: () => void;
  /**
   * Opens the New-trigger dialog (board / run-completion kinds). Omit when
   * the trigger store is unavailable — the two "created here" families then
   * state that instead of offering a dead-end CTA.
   */
  onNewTrigger?: () => void;
}) {
  const createdHere = (label: string) =>
    onNewTrigger ? (
      <button type="button" onClick={onNewTrigger} className="text-accent-text hover:underline">
        {label}
      </button>
    ) : (
      <span className="text-fg-subtle">{label} — not enabled on this server</span>
    );
  return (
    <details className="group rounded border border-border-subtle bg-surface-1">
      <summary className="flex cursor-pointer select-none items-center gap-1.5 px-3 py-2 text-xs font-medium text-fg-default list-none [&::-webkit-details-marker]:hidden">
        <ChevronRight
          size={12}
          className="shrink-0 text-fg-subtle transition-transform duration-[var(--motion-fast)] group-open:rotate-90"
          aria-hidden
        />
        What can fire a bot?
        <span className="font-normal text-fg-subtle">
          — the five automation families and where each is managed
        </span>
      </summary>
      <dl className="space-y-3 border-t border-border-subtle px-3 py-3">
        <Family
          name="Board trigger"
          where={createdHere("created here — New trigger → “Board card”")}
        >
          Fires when a native-board card enters a state (e.g. “ready”) with the
          required labels. The card is promoted to the bot and the dispatcher
          claims and launches it.
        </Family>
        <Family
          name="Run-completion"
          where={createdHere("created here — New trigger → “Run finished”")}
        >
          Chains a bot after another run ends (<code>run.finished</code> /{" "}
          <code>run.failed</code>), optionally filtered by the upstream bot —
          e.g. a review bot after every feature-dev run.
        </Family>
        <Family
          name="Schedule (cron)"
          where={
            <button
              type="button"
              onClick={onOpenSchedules}
              className="text-accent-text hover:underline"
            >
              Schedules tab of this page
            </button>
          }
        >
          Launches a bot on a recurring cron cadence — nightly audits, weekly
          dependency sweeps.
        </Family>
        <Family
          name="Forge webhook"
          where={
            <Link href="/integrations?tab=forges" className="text-accent-text hover:underline">
              Integrations → Repositories
            </Link>
          }
        >
          A PR/MR opens or a <code>/revi</code> note lands on your forge.
          Provisioned automatically when you connect a repository and enable
          bots on it — managed per repo, not created on this page.
        </Family>
        <Family
          name="Custom integration"
          where={
            <Link href="/integrations?tab=webhooks" className="text-accent-text hover:underline">
              Integrations → Webhooks
            </Link>
          }
        >
          External systems launch a bot with a long-lived <code>iwh_</code>{" "}
          token (managed under Integrations → Webhooks). Local custom events
          can also be posted to <code>/api/v1/triggers/emit</code> and matched
          by a “Custom integration” trigger here.
        </Family>
      </dl>
    </details>
  );
}

function Family({
  name,
  where,
  children,
}: {
  name: string;
  where: React.ReactNode;
  children: React.ReactNode;
}) {
  return (
    <div className="text-xs">
      <dt className="font-medium text-fg-default">
        {name}
        <span className="ml-2 font-normal">{where}</span>
      </dt>
      <dd className="mt-0.5 text-fg-muted">{children}</dd>
    </div>
  );
}
