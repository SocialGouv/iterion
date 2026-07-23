// Extracted from RunHeader.tsx to keep that file focused.
// Result-links banner: a run's headline deliverables (opened PR, live
// deploy, published app) surfaced as prominent clickable links at the top
// of the run summary — like a CI run's "View deployment" button. Fed by
// the store's event-derived `resultLinks` (see store/run.ts ResultLink),
// so it rebuilds on a reloaded terminal run from the replayed event log.
//
// This is a link-only surface: a PR page blocks iframing, so it is never
// embedded in the Browser pane. Embeddable kinds (deploy/app live sites)
// still drive the Browser pane in parallel — this row just gives their URL
// a prominent, always-visible home in the summary.

import {
  ArrowRightIcon,
  ExternalLinkIcon,
  GlobeIcon,
  Link2Icon,
  RocketIcon,
} from "@radix-ui/react-icons";
import type { ComponentType } from "react";

import type { ResultLink } from "@/store/run";

interface KindMeta {
  label: string;
  icon: ComponentType<{ className?: string }>;
}

// Per-kind presentation. Unknown result kinds fall back to a neutral
// "Result" link rather than being dropped — the store already gates which
// kinds reach this list, so anything here is meant to be shown.
const KIND_META: Record<string, KindMeta> = {
  pr: { label: "Pull request", icon: Link2Icon },
  deploy: { label: "Deployment", icon: RocketIcon },
  app: { label: "App", icon: GlobeIcon },
};
const FALLBACK_META: KindMeta = { label: "Result", icon: ExternalLinkIcon };

// displayUrl strips the scheme for a compact label; the href keeps the
// full URL so the link still resolves.
function displayUrl(url: string): string {
  return url.replace(/^https?:\/\//, "");
}

export default function ResultLinksRow({ links }: { links: ResultLink[] }) {
  if (links.length === 0) return null;

  return (
    <div
      className="shrink-0 px-4 py-2 bg-accent-soft/50 border-b border-border-default flex flex-col gap-1.5 text-body"
      aria-label="Run result links"
    >
      {links.map((link) => {
        const meta = KIND_META[link.kind] ?? FALLBACK_META;
        const Icon = meta.icon;
        return (
          <div key={link.url} className="flex items-center gap-2 min-w-0">
            <Icon className="w-4 h-4 shrink-0 text-accent-text" />
            <span className="font-medium text-fg-default shrink-0">{meta.label}</span>
            <ArrowRightIcon className="w-3 h-3 shrink-0 text-fg-subtle" />
            <a
              href={link.url}
              target="_blank"
              rel="noopener noreferrer"
              className="inline-flex items-center gap-1 min-w-0 font-medium text-accent-text hover:underline focus-visible:ring-1 focus-visible:ring-accent rounded"
              title={`Open ${link.url}`}
            >
              <span className="truncate">{displayUrl(link.url)}</span>
              <ExternalLinkIcon className="w-3 h-3 shrink-0" />
            </a>
          </div>
        );
      })}
    </div>
  );
}
