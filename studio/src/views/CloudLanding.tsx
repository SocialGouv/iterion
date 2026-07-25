import { useEffect } from "react";
import { useLocation } from "wouter";
import { useQuery } from "@tanstack/react-query";
import { GitHubLogoIcon, ArrowRightIcon } from "@radix-ui/react-icons";

import { Button } from "@/components/ui/Button";
import { BrandWordmark } from "@/components/ui/BrandWordmark";
import { BrandMark } from "@/components/ui/BrandMark";
import { ThemeToggle } from "@/components/ui/ThemeToggle";
import { useServerInfoStore } from "@/store/serverInfo";
import { listMarketplace, type MarketplaceEntry } from "@/api/marketplace";

import { SignInCard } from "./Login";

const GITHUB_URL = "https://github.com/SocialGouv/iterion";
const DOCS_URL = "https://socialgouv.github.io/iterion/";

// FALLBACK_BOTS is the curated agent set shown when the marketplace is empty
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

type ShowcaseBot = { name: string; blurb: string; installs?: number };

// gridGlowStyle layers a faint accent grid under a radial indigo glow — a
// restrained cyberpunk backdrop (see studio/docs/visual-identity.md), masked
// to fade out before it reaches the content. Token-driven, so it tracks the
// active theme.
const gridGlowStyle: React.CSSProperties = {
  backgroundImage:
    "radial-gradient(48rem 26rem at 50% -8%, var(--color-accent-soft), transparent 70%)," +
    "linear-gradient(to right, color-mix(in srgb, var(--color-accent) 8%, transparent) 1px, transparent 1px)," +
    "linear-gradient(to bottom, color-mix(in srgb, var(--color-accent) 8%, transparent) 1px, transparent 1px)",
  backgroundSize: "100% 100%, 44px 44px, 44px 44px",
  WebkitMaskImage: "radial-gradient(80% 60% at 50% 0%, black, transparent 78%)",
  maskImage: "radial-gradient(80% 60% at 50% 0%, black, transparent 78%)",
};

// LandingTopBar is the sticky header of the marketing landing: brand on the
// left, theme toggle + GitHub + Sign-in on the right.
function LandingTopBar() {
  const [, navigate] = useLocation();
  return (
    <div className="sticky top-0 z-20 border-b border-border-subtle bg-surface-0/80 backdrop-blur">
      <div className="mx-auto flex max-w-5xl items-center justify-between px-4 py-3 sm:px-6">
        <div className="flex items-center gap-2.5">
          <BrandMark className="h-7 w-7" />
          <BrandWordmark />
        </div>
        <div className="flex items-center gap-2 sm:gap-3">
          <ThemeToggle />
          <a
            href={GITHUB_URL}
            target="_blank"
            rel="noreferrer"
            aria-label="iterion on GitHub"
            title="iterion on GitHub"
            className="hidden h-8 w-8 items-center justify-center rounded-full text-fg-muted transition-colors hover:bg-surface-2 hover:text-fg-default sm:flex"
          >
            <GitHubLogoIcon className="h-4 w-4" />
          </a>
          <Button variant="primary" size="sm" onClick={() => navigate("/login")}>
            Sign in
          </Button>
        </div>
      </div>
    </div>
  );
}

// PersonaAvatar is the accent-tinted monogram tile fronting each showcase
// card — gives the agent grid a face without shipping per-bot artwork.
function PersonaAvatar({ name }: { name: string }) {
  return (
    <span className="flex h-9 w-9 shrink-0 items-center justify-center rounded-lg bg-accent-soft text-sm font-semibold text-accent-text">
      {name.slice(0, 1).toUpperCase()}
    </span>
  );
}

// BotShowcase renders the marketplace's featured bots (public browse), with
// a graceful static fallback. Read-only and best-effort: a failed fetch
// just shows the curated roster.
function BotShowcase() {
  // Same key + request as the Marketplace view's default unfiltered
  // browse, so landing → marketplace navigation shares the cache. No
  // error surface by design: failed just means the curated roster.
  const query = useQuery<MarketplaceEntry[]>({
    queryKey: ["marketplace", "", "", "", "popular"],
    queryFn: () => listMarketplace("", "", "", "popular"),
  });
  const bots = query.data ?? null;
  const failed = query.isError;

  const cards: ShowcaseBot[] | null =
    bots && bots.length > 0
      ? bots.slice(0, 6).map((b) => ({
          name: b.display_name || b.name,
          blurb: b.description || "",
          installs: b.installs,
        }))
      : failed || bots // resolved empty or errored → curated roster
        ? FALLBACK_BOTS
        : null; // still loading

  if (!cards) {
    return (
      <div className="grid grid-cols-1 gap-3 sm:grid-cols-2 lg:grid-cols-3" aria-hidden>
        {Array.from({ length: 6 }).map((_, i) => (
          <div
            key={i}
            className="h-24 animate-pulse rounded-xl border border-border-subtle bg-surface-1"
          />
        ))}
      </div>
    );
  }

  return (
    <div className="grid grid-cols-1 gap-3 sm:grid-cols-2 lg:grid-cols-3">
      {cards.map((b) => (
        <div
          key={b.name}
          className="group flex items-start gap-3 rounded-xl border border-border-subtle bg-surface-1 p-4 text-left transition-all hover:-translate-y-0.5 hover:border-accent hover:shadow-[var(--shadow-md)]"
        >
          <PersonaAvatar name={b.name} />
          <div className="min-w-0">
            <div className="flex items-baseline gap-2">
              <span className="truncate font-semibold text-fg-default">{b.name}</span>
              {typeof b.installs === "number" && b.installs > 0 && (
                <span className="shrink-0 font-mono text-micro text-fg-subtle">
                  {b.installs}↓
                </span>
              )}
            </div>
            {b.blurb && (
              <div className="mt-1 line-clamp-2 text-sm text-fg-muted">{b.blurb}</div>
            )}
          </div>
        </div>
      ))}
    </div>
  );
}

// PublicTopBar is the slim header shown above the public Marketplace view
// when an anonymous visitor browses it (that route lives outside the
// authenticated AppShell). Brand links home (the landing); a theme toggle +
// Sign-in button sit on the right.
export function PublicTopBar() {
  const [, navigate] = useLocation();
  return (
    <div className="sticky top-0 z-10 flex items-center justify-between border-b border-border-subtle bg-surface-0/90 px-4 py-3 backdrop-blur sm:px-6">
      <button
        type="button"
        onClick={() => navigate("/")}
        className="flex items-center gap-2.5 rounded text-fg-default hover:opacity-80 focus:outline-none focus-visible:ring-1 focus-visible:ring-accent"
        aria-label="Back to home"
      >
        <BrandMark className="h-7 w-7" />
        <BrandWordmark />
      </button>
      <div className="flex items-center gap-2 sm:gap-3">
        <ThemeToggle />
        <Button variant="primary" size="sm" onClick={() => navigate("/login")}>
          Sign in
        </Button>
      </div>
    </div>
  );
}

// LandingHero is the full-width marketing pitch above the sign-in card.
// Dark/light theme-aware, electric-indigo, a restrained cyberpunk nod —
// deliberately NOT a pastel SaaS hero. The infrastructure/control-plane brand
// voice is paired with a live agent showcase and links out to the public
// marketplace, GitHub, and docs.
function LandingHero() {
  const [, navigate] = useLocation();
  const marketplaceEnabled = useServerInfoStore((s) => s.info?.marketplace_enabled);

  return (
    <header className="relative overflow-hidden border-b border-border-subtle">
      <div aria-hidden className="pointer-events-none absolute inset-0" style={gridGlowStyle} />
      <div className="relative mx-auto max-w-5xl px-6 py-16 text-center sm:py-24">
        <div className="mb-6 flex justify-center">
          <BrandMark className="h-16 w-16 drop-shadow-[0_4px_24px_var(--color-accent-soft)]" />
        </div>

        <span className="inline-flex items-center gap-1.5 rounded-full border border-border-subtle bg-surface-1/60 px-3 py-1 text-xs text-fg-muted backdrop-blur">
          <span className="h-1.5 w-1.5 rounded-full bg-accent" />
          Open-source control plane for AI agents
        </span>

        <h1
          className="mt-5 bg-clip-text text-4xl font-bold tracking-tight text-transparent sm:text-6xl"
          style={{
            backgroundImage:
              "linear-gradient(92deg, var(--color-fg-default) 30%, var(--color-accent-text))",
          }}
        >
          The control plane for AI agents.
        </h1>
        <p className="mt-4 text-lg font-medium text-accent-text sm:text-2xl">
          Apps have Linux. The cloud has Kubernetes. AI agents have Iterion.
        </p>
        <p className="mx-auto mt-5 max-w-2xl text-base text-fg-muted">
          Define agent workflows as code. Iterion schedules, coordinates, and
          governs agents across parallel branches, review gates, tools, and
          budgets — from one auditable control plane.
        </p>

        <div className="mt-8 flex flex-wrap items-center justify-center gap-3">
          {marketplaceEnabled && (
            <Button variant="primary" size="md" onClick={() => navigate("/marketplace")}>
              <span className="flex items-center gap-1.5">
                Explore the marketplace
                <ArrowRightIcon className="h-4 w-4" />
              </span>
            </Button>
          )}
          <a href={GITHUB_URL} target="_blank" rel="noreferrer">
            <Button variant="secondary" size="md">
              <span className="flex items-center gap-1.5">
                <GitHubLogoIcon className="h-4 w-4" />
                View on GitHub
              </span>
            </Button>
          </a>
          <a href={DOCS_URL} target="_blank" rel="noreferrer">
            <Button variant="ghost" size="md">
              Read the docs
            </Button>
          </a>
        </div>

        <div className="mt-16">
          <div className="mb-4 text-xs uppercase tracking-[0.2em] text-fg-subtle">
            Featured agent workflows
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
      <LandingTopBar />
      <LandingHero />
      <section className="relative flex items-center justify-center px-4 py-14 sm:py-20">
        <div className="w-full max-w-md">
          <div className="mb-4 text-center text-xs uppercase tracking-[0.2em] text-fg-subtle">
            Get started
          </div>
          <SignInCard />
        </div>
      </section>
      <footer className="border-t border-border-subtle px-6 py-8 text-center text-xs text-fg-subtle">
        <div className="mx-auto flex max-w-5xl flex-wrap items-center justify-center gap-x-4 gap-y-2">
          <span>© iterion — MIT-licensed</span>
          <a href={GITHUB_URL} target="_blank" rel="noreferrer" className="text-accent-text hover:underline">
            GitHub
          </a>
          <a href={DOCS_URL} target="_blank" rel="noreferrer" className="text-accent-text hover:underline">
            Docs
          </a>
        </div>
      </footer>
    </div>
  );
}
