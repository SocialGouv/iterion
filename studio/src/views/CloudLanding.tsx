import { useEffect, useState } from "react";
import { useLocation } from "wouter";

import { Button } from "@/components/ui/Button";
import { BrandWordmark } from "@/components/ui/BrandWordmark";
import { useServerInfoStore } from "@/store/serverInfo";
import { listMarketplace, type MarketplaceEntry } from "@/api/marketplace";

import { SignInCard } from "./Login";

const GITHUB_URL = "https://github.com/SocialGouv/iterion";
const DOCS_URL = "https://github.com/SocialGouv/iterion/tree/main/docs";

// FALLBACK_BOTS is the curated legion shown when the marketplace is empty
// or unreachable, so the hero never collapses to a blank showcase. Mirrors
// the named personas shipped in bots/ (kept short — the live marketplace is
// the real source once seeded).
const FALLBACK_BOTS: { name: string; blurb: string }[] = [
  { name: "Featurly", blurb: "Ships a feature end to end, review-gated." },
  { name: "Revi", blurb: "Reviews pull requests and replies in-thread." },
  { name: "Willy", blurb: "Whole-codebase improvement loop, converging." },
  { name: "Seki", blurb: "Source security audit (SAST) with a real floor." },
  { name: "Nexie", blurb: "Your co-CTO: surveys the repo, plans what's next." },
  { name: "Testy", blurb: "Adds real test coverage, anti-façade." },
];

// BotShowcase renders the marketplace's featured bots (public browse), with
// a graceful static fallback. Read-only and best-effort: a failed fetch
// just shows the curated roster.
function BotShowcase() {
  const [bots, setBots] = useState<MarketplaceEntry[] | null>(null);
  const [failed, setFailed] = useState(false);

  useEffect(() => {
    let alive = true;
    void listMarketplace()
      .then((list) => {
        if (alive) setBots(list);
      })
      .catch(() => {
        if (alive) setFailed(true);
      });
    return () => {
      alive = false;
    };
  }, []);

  const cards =
    bots && bots.length > 0
      ? bots.slice(0, 6).map((b) => ({
          name: b.display_name || b.name,
          blurb: b.description || "",
        }))
      : failed || bots // resolved empty or errored → curated roster
        ? FALLBACK_BOTS
        : null; // still loading

  if (!cards) {
    return (
      <div className="grid grid-cols-1 sm:grid-cols-2 lg:grid-cols-3 gap-3" aria-hidden>
        {Array.from({ length: 6 }).map((_, i) => (
          <div
            key={i}
            className="h-20 rounded-lg border border-border-subtle bg-surface-1 animate-pulse"
          />
        ))}
      </div>
    );
  }

  return (
    <div className="grid grid-cols-1 sm:grid-cols-2 lg:grid-cols-3 gap-3">
      {cards.map((b) => (
        <div
          key={b.name}
          className="rounded-lg border border-border-subtle bg-surface-1 p-4 text-left hover:border-accent transition-colors"
        >
          <div className="font-semibold text-fg-default">{b.name}</div>
          {b.blurb && <div className="mt-1 text-sm text-fg-muted line-clamp-2">{b.blurb}</div>}
        </div>
      ))}
    </div>
  );
}

// PublicTopBar is the slim header shown above the public Marketplace view
// when an anonymous visitor browses it (that route lives outside the
// authenticated AppShell). Wordmark links home (the landing); a Sign-in
// button routes to the sign-in card.
export function PublicTopBar() {
  const [, navigate] = useLocation();
  return (
    <div className="sticky top-0 z-10 flex items-center justify-between border-b border-border-subtle bg-surface-0/90 px-4 py-3 backdrop-blur">
      <button
        type="button"
        onClick={() => navigate("/")}
        className="flex items-center gap-2 text-fg-default hover:opacity-80"
        aria-label="Back to home"
      >
        <BrandWordmark />
      </button>
      <Button variant="primary" size="sm" onClick={() => navigate("/")}>
        Sign in
      </Button>
    </div>
  );
}

// LandingHero is the full-width marketing pitch above the sign-in card.
// Dark, electric-indigo, a restrained cyberpunk nod — deliberately NOT a
// pastel SaaS hero (see studio/docs/visual-identity.md). The roman-imperator
// brand voice + a live showcase of the bot legion, with links out to the
// public marketplace, GitHub, and docs.
function LandingHero() {
  const [, navigate] = useLocation();
  const marketplaceEnabled = useServerInfoStore((s) => s.info?.marketplace_enabled);

  return (
    <header className="relative overflow-hidden border-b border-border-subtle">
      {/* Indigo glow — accent-tinted, low-opacity, no pastel gradient. */}
      <div
        aria-hidden
        className="pointer-events-none absolute inset-0 opacity-60"
        style={{
          background:
            "radial-gradient(60rem 30rem at 50% -10%, var(--color-accent-soft), transparent 70%)",
        }}
      />
      <div className="relative mx-auto max-w-5xl px-6 py-16 sm:py-24 text-center">
        <BrandWordmark className="text-3xl justify-center" />
        <h1 className="mt-6 text-3xl sm:text-5xl font-semibold tracking-tight text-fg-default">
          From dev to imperator
        </h1>
        <p className="mt-4 text-lg sm:text-xl text-accent-text font-medium">
          Command a legion of bots at the next level.
        </p>
        <p className="mx-auto mt-5 max-w-2xl text-base text-fg-muted">
          Iterion is an open-source workflow engine that turns your repository
          into a battlefield of autonomous agents — they ship features, review
          PRs, audit security and keep the whole codebase converging, while you
          stay in command.
        </p>

        <div className="mt-8 flex flex-wrap items-center justify-center gap-3">
          {marketplaceEnabled && (
            <Button variant="primary" size="md" onClick={() => navigate("/marketplace")}>
              Explore the marketplace
            </Button>
          )}
          <a href={GITHUB_URL} target="_blank" rel="noreferrer">
            <Button variant="secondary" size="md">
              View on GitHub
            </Button>
          </a>
          <a href={DOCS_URL} target="_blank" rel="noreferrer">
            <Button variant="ghost" size="md">
              Read the docs
            </Button>
          </a>
        </div>

        <div className="mt-14">
          <div className="mb-4 text-xs uppercase tracking-wider text-fg-muted">
            Meet the legion
          </div>
          <BotShowcase />
        </div>
      </div>
    </header>
  );
}

// CloudLanding is the anonymous root in cloud mode: a marketing hero above
// the existing sign-in card. In any other mode (or before server-info
// resolves) it degrades to the plain centred sign-in card, so desktop/local
// never sees the hero.
export default function CloudLanding() {
  const serverInfo = useServerInfoStore((s) => s.info);
  useEffect(() => {
    if (!serverInfo) void useServerInfoStore.getState().refresh();
  }, [serverInfo]);

  const isCloud = serverInfo?.mode === "cloud";

  if (!isCloud) {
    return (
      <div className="min-h-screen flex items-center justify-center bg-surface-0 text-fg-default px-4">
        <SignInCard />
      </div>
    );
  }

  return (
    <div className="min-h-screen bg-surface-0 text-fg-default">
      <LandingHero />
      <section className="flex items-center justify-center px-4 py-12 sm:py-16">
        <SignInCard />
      </section>
    </div>
  );
}
