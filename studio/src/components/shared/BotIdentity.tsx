// Reusable bot identity block — emoji avatar + display name + technical
// name + optional description. Shared by BotHome's identity header and
// the Launch form's persona header so a bot looks the same everywhere.
//
// Purely presentational: the avatar slot lets BotHome keep its
// EmojiPicker trigger, and nameExtras/meta let callers append badges or
// chips without this component knowing about them.

import type { ReactNode } from "react";

import { botVisual } from "@/lib/personas";

export interface BotIdentityInfo {
  /** Technical bot id (kebab/snake) — drives the persona colour + emoji. */
  name: string;
  /** Manifest persona name (e.g. "Nexie"); falls back to `name`. */
  display_name?: string;
  description?: string;
  /** Manifest emoji icon; wins over the built-in persona map. */
  icon?: string;
}

export type BotIdentitySize = "md" | "lg";

interface Props {
  bot: BotIdentityInfo;
  /** lg = BotHome header (h-12 avatar, text-base title); md = inline
   *  contexts like the Launch form (h-10 avatar, text-sm title). */
  size?: BotIdentitySize;
  /** Replaces the default emoji avatar (e.g. an EmojiPicker trigger). */
  avatar?: ReactNode;
  /** Rendered inline after the name row (badges). */
  nameExtras?: ReactNode;
  /** Rendered below the description (chips, secondary lines). */
  meta?: ReactNode;
  /** Clamp long manifest descriptions to two lines (list contexts). */
  clampDescription?: boolean;
  className?: string;
}

const SIZE = {
  md: { avatar: "h-10 w-10 text-xl", title: "text-sm" },
  lg: { avatar: "h-12 w-12 text-2xl", title: "text-base" },
} as const;

export default function BotIdentity({
  bot,
  size = "md",
  avatar,
  nameExtras,
  meta,
  clampDescription = false,
  className = "",
}: Props) {
  const identity = botVisual(bot);
  const label = bot.display_name?.trim() || bot.name;
  const s = SIZE[size];
  return (
    <div className={`flex items-start gap-3 ${className}`.trim()}>
      {avatar ?? (
        <span
          className={`flex ${s.avatar} shrink-0 items-center justify-center leading-none`}
          aria-hidden="true"
        >
          {identity.emoji}
        </span>
      )}
      <div className="min-w-0 flex-1">
        <div className="flex flex-wrap items-baseline gap-x-2 gap-y-0.5">
          <h1 className={`${s.title} font-semibold ${identity.color}`}>{label}</h1>
          {bot.display_name?.trim() && (
            <span className="font-mono text-caption text-fg-subtle">{bot.name}</span>
          )}
          {nameExtras}
        </div>
        {bot.description && (
          <p
            className={`mt-0.5 text-xs text-fg-muted ${clampDescription ? "line-clamp-2" : ""}`.trim()}
            title={clampDescription ? bot.description : undefined}
          >
            {bot.description}
          </p>
        )}
        {meta}
      </div>
    </div>
  );
}
