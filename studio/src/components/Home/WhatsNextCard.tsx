import { Link } from "wouter";
import { RocketIcon, ChevronRightIcon } from "@radix-ui/react-icons";

import type { RunStatus } from "@/api/runs";
import { LiveDot } from "@/components/ui/LiveDot";
import { getFirstClassBot, DEFAULT_WHATS_NEXT_BOT_ID } from "@/lib/whats-next/firstClassBots";

// WhatsNextCard sits at the top of the Home view as a full-width hero
// pointing operators at the first-class whats-next experience. It's
// the curated entry point: don't pick a workflow file, don't fill a
// vars form — just step into the conversation. When a session is
// already in flight the card says so, so an operator returning to Home
// knows Nexie is live (or waiting on them) before clicking.

export default function WhatsNextCard({
  liveStatus = null,
}: {
  // Status of an in-flight whats-next run, when the caller knows of
  // one (Home derives it from the runs it already fetches).
  liveStatus?: RunStatus | null;
}) {
  const bot = getFirstClassBot(DEFAULT_WHATS_NEXT_BOT_ID);
  if (!bot) return null;

  const waiting = liveStatus === "paused_waiting_human";
  const live = liveStatus !== null;

  return (
    <Link
      href="/whats-next"
      className="block group rounded-[var(--radius-lg)] border border-accent/40 bg-accent-soft shadow-[var(--shadow-sm)] hover:border-accent/60 hover:shadow-[var(--shadow-md)] hover:-translate-y-0.5 transition-[box-shadow,transform,border-color] duration-[var(--motion-fast)] ease-[var(--motion-ease)] p-4"
    >
      <div className="flex items-center gap-4">
        <div className="shrink-0 w-10 h-10 rounded-full bg-accent text-accent-fg flex items-center justify-center">
          <RocketIcon className="w-5 h-5" />
        </div>
        <div className="flex-1 min-w-0">
          <h2 className="text-base font-semibold text-fg-default">
            {bot.label}
          </h2>
          <p className="mt-1 text-label text-fg-muted line-clamp-2">
            {bot.description}
          </p>
        </div>
        {live && (
          <span className="shrink-0 inline-flex items-center gap-1.5 text-caption text-info-fg">
            <LiveDot tone={waiting ? "warning" : "info"} size="sm" />
            {waiting ? "Waiting for you" : "Session in progress"}
          </span>
        )}
        <ChevronRightIcon className="w-5 h-5 text-fg-muted shrink-0 group-hover:text-fg-default transition-colors" />
      </div>
    </Link>
  );
}
