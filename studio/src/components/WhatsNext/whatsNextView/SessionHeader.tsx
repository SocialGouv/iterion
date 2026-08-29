import { Link } from "wouter";
import { ExternalLinkIcon } from "@radix-ui/react-icons";

import { CopyButton } from "@/components/ui/CopyButton";
import { InlineBanner } from "@/components/ui/InlineBanner";
import { useActiveRepo } from "@/hooks/useActiveRepo";
import type { useWhatsNextSession } from "@/lib/whats-next/useWhatsNextSession";
import { useNewSessionAction } from "@/lib/whats-next/useNewSessionAction";
import { useRunStore } from "@/store/run";

import { humanStatus } from "./humanStatus";
import SessionModelControl from "./SessionModelControl";

export default function SessionHeader({
  bot,
  session,
}: {
  bot: { label: string };
  session: ReturnType<typeof useWhatsNextSession>;
}) {
  const newSession = useNewSessionAction({ bot, session });
  // Session spend, straight off the run checkpoint (authoritative
  // across resume segments) — a chat can span many bursts, so the
  // number belongs next to the status word, not one click away in the
  // console.
  const costUSD = useRunStore(
    (s) => s.snapshot?.run.checkpoint?.cost_usd_total ?? 0,
  );
  const tokensUsed = useRunStore(
    (s) => s.snapshot?.run.checkpoint?.budget_tokens_used ?? 0,
  );
  // Scope-mismatch guard: the attached session may be bound to another
  // repo than the sidebar's active one (auto-attach across a repo
  // switch). Compare loosely — project_path may be host-prefixed.
  const { activeRepo, overview, enabled: repoScopeEnabled } = useActiveRepo();
  const sessionRepo = session.sessionRepo;
  const repoMismatch =
    repoScopeEnabled &&
    !overview &&
    !!activeRepo &&
    !!sessionRepo &&
    !sessionRepo.endsWith(activeRepo.repo_full_name);
  // Kept local: the LABEL changes with liveness ("Abandon & restart" vs
  // "New session"), which the shared action deliberately does not decide.
  const isLive =
    session.runId !== null &&
    session.status !== "ended" &&
    session.status !== "idle";


  return (
    <div className="border-b border-border-subtle">
    <div className="px-4 py-3 flex items-baseline justify-between gap-3">
      {newSession.dialog}
      <h2 className="text-label font-semibold text-fg-default inline-flex items-baseline gap-2">
        {bot.label}
        {sessionRepo && (
          <span
            className="font-normal font-mono text-caption text-fg-subtle truncate max-w-[16rem]"
            title={`This session operates on ${sessionRepo}`}
          >
            {sessionRepo.split("/").slice(-2).join("/")}
          </span>
        )}
        {session.runId && (
          <CopyButton
            value={session.runId}
            label="copy run id"
            className="font-normal"
          />
        )}
      </h2>
      <div className="flex items-baseline gap-3">
        {/* Which model is answering, and the one click that changes it. It
            sits next to the spend on purpose: the two numbers an operator
            weighs against each other are cost and capability. */}
        <SessionModelControl pref={session.modelPref} liveRun={isLive} />
        {(costUSD > 0 || tokensUsed > 0) && (
          <span
            className="text-caption text-fg-subtle"
            title="Session spend so far (all bursts)"
          >
            ${costUSD.toFixed(2)}
            {tokensUsed > 0 && ` · ${Math.round(tokensUsed / 1000)}k tok`}
          </span>
        )}
        {session.runId && (
          <Link
            href={`/runs/${encodeURIComponent(session.runId)}`}
            className="inline-flex items-center gap-1 text-micro text-accent-text hover:underline"
          >
            <ExternalLinkIcon className="w-3 h-3" />
            console
          </Link>
        )}
        <div className="text-caption uppercase tracking-wide text-fg-subtle">
          {humanStatus(session.status, session.runStatus)}
        </div>
        {newSession.available && (
          <button
            type="button"
            onClick={() => void newSession.start()}
            disabled={newSession.busy}
            className="text-micro text-accent-text hover:underline cursor-pointer disabled:opacity-50 disabled:cursor-wait"
            title={
              isLive
                ? `Cancel the current run and start a fresh ${bot.label} session.`
                : "Start fresh — the current run stays in the run list."
            }
          >
            {newSession.busy ? "Cancelling…" : isLive ? "Abandon & restart" : "New session"}
          </button>
        )}
      </div>
    </div>
    {repoMismatch && activeRepo && (
      <div className="px-4 pb-2">
        <InlineBanner tone="warning" layout="inline">
          This session is attached to{" "}
          <code className="font-mono">{sessionRepo}</code>, not the sidebar&apos;s
          active repo ({activeRepo.repo_full_name}). Replies here act on the
          session&apos;s repo — switch the sidebar back, or start a new session.
        </InlineBanner>
      </div>
    )}
    </div>
  );
}
