import { useEffect, useState } from "react";
import { Link } from "wouter";
import { ArrowRight, ArrowUpRight, Check, ChevronRight, Clock3, Code2, GitBranch, GitPullRequest, ListChecks, Menu, Moon, ShieldCheck, Star, Sun, X } from "lucide-react";
import { GitHubLogoIcon } from "@radix-ui/react-icons";
import { BrandMark } from "@/components/ui/BrandMark";
import { useThemeStore } from "@/store/theme";
import StackCompatibility from "./StackCompatibility";
import PlatformFeatures from "./PlatformFeatures";
import "./cloud-home.css";

const GITHUB = "https://github.com/SocialGouv/iterion";
const DOCS = "https://socialgouv.github.io/iterion/";

function Brand() {
  return <Link href="/" className="ch-brand" aria-label="Iterion Cloud home"><BrandMark className="ch-brand-mark" /><span>iterion<span className="ch-cloud-label">cloud</span></span></Link>;
}

function GitHubStar() {
  return (
    <div className="ch-star-invite">
      <span>Like what we’re building?</span>
      <a href={GITHUB} target="_blank" rel="noreferrer" className="ch-star-link">
        <Star size={15} aria-hidden="true" /> Star on GitHub <ArrowUpRight size={13} aria-hidden="true" />
      </a>
    </div>
  );
}

function HeroVisual() {
  return (
    <div className="ch-orbit" role="img" aria-label="Iterion coordinates a mission through planning, execution, review and delivery.">
      <div className="ch-orbit-caption"><span className="ch-tiny-cross">+</span> COMPLEXITY, COORDINATED <span>01—04</span></div>
      <div className="ch-orbit-ring ch-ring-one" /><div className="ch-orbit-ring ch-ring-two" /><div className="ch-orbit-ring ch-ring-three" />
      <svg className="ch-orbit-paths" viewBox="0 0 560 450" fill="none" aria-hidden="true">
        <path d="M112 125 C180 125 180 218 279 218 S380 108 447 108 M279 218 C370 218 379 340 446 340 M279 218 C192 218 198 349 98 349" stroke="currentColor" strokeWidth="1" />
        <path className="ch-flow-path" d="M112 125 C180 125 180 218 279 218 S380 108 447 108" stroke="var(--ch-accent)" strokeWidth="2" />
        <circle cx="279" cy="218" r="97" stroke="currentColor" strokeDasharray="2 8" />
      </svg>
      <div className="ch-core"><BrandMark className="ch-core-mark" /><span>iterion</span><small>ORCHESTRATE</small></div>
      <div className="ch-orbit-node ch-node-plan"><span className="ch-node-icon"><ListChecks size={17} /></span><div><small>01 / UNDERSTAND</small><strong>A clear plan</strong></div><Check size={13} className="ch-mint" /></div>
      <div className="ch-orbit-node ch-node-build"><span className="ch-node-icon"><Code2 size={17} /></span><div><small>02 / EXECUTE</small><strong>Agents at work</strong></div><span className="ch-live-dot" /></div>
      <div className="ch-orbit-node ch-node-review"><span className="ch-node-icon"><ShieldCheck size={17} /></span><div><small>03 / VERIFY</small><strong>Review & refine</strong></div></div>
      <div className="ch-orbit-node ch-node-ship"><span className="ch-node-icon"><GitPullRequest size={17} /></span><div><small>04 / DELIVER</small><strong>Ready to ship</strong></div><ArrowUpRight size={14} /></div>
      <div className="ch-orbit-bottom"><GitBranch size={13} /><span>One workflow. Every step connected.</span><span className="ch-tiny-cross">+</span></div>
    </div>
  );
}

function UseCases() {
  return (
    <section className="ch-section ch-use-cases" id="workflows" aria-labelledby="ch-use-cases-heading">
      <div className="ch-section-heading">
        <div>
          <p className="ch-eyebrow">CONNECTED TO YOUR WORK.</p>
          <h2 id="ch-use-cases-heading">Connect your repos.<br />React to events.<br />Run on schedule.</h2>
        </div>
        <p>A pull request, a webhook, or a recurring task.<br />Get the results where your team works.</p>
      </div>
      <div className="ch-trigger-columns">
        <article className="ch-trigger-story" aria-labelledby="ch-repo-heading">
          <p className="ch-trigger-label"><GitBranch size={17} /> REPOSITORY EVENTS</p>
          <h3 id="ch-repo-heading">Put bots on your repositories.</h3>
          <p className="ch-trigger-description">
            Connect a GitHub, GitLab or Forgejo repository and enable the bots
            you need. Pull requests trigger their work automatically.
          </p>
          <div className="ch-bot-stories">
            <div className="ch-bot-story">
              <h4><a href={`${GITHUB}/tree/main/bots/review-pr`} target="_blank" rel="noreferrer">Revi <ArrowUpRight size={13} /></a><span>Code review</span></h4>
              <p>Reviews pull requests and posts findings directly on the changed lines, so the discussion stays with the code.</p>
            </div>
            <div className="ch-bot-story">
              <h4><a href={`${GITHUB}/tree/main/bots/dep-update-guard`} target="_blank" rel="noreferrer">Vetty <ArrowUpRight size={13} /></a><span>Dependency maintenance</span></h4>
              <p>Checks Dependabot and Renovate updates, adapts the code when needed, and runs the build and tests before reporting back on the PR.</p>
            </div>
          </div>
          <p className="ch-trigger-summary">Repository event <ArrowRight size={13} /> Bot <ArrowRight size={13} /> PR feedback</p>
        </article>
        <article className="ch-trigger-story" aria-labelledby="ch-schedule-heading">
          <p className="ch-trigger-label"><Clock3 size={17} /> RECURRING SCHEDULES</p>
          <h3 id="ch-schedule-heading">Give recurring work a rhythm.</h3>
          <p className="ch-trigger-description">
            Choose a cadence and configure your bot. Iterion runs it on
            schedule, whether the work is about your code or the world outside it.
          </p>
          <div className="ch-bot-stories">
            <div className="ch-bot-story">
              <h4><a href={`${GITHUB}/tree/main/bots/feed-watch`} target="_blank" rel="noreferrer">Vigie <ArrowUpRight size={13} /></a><span>News & technology watch</span></h4>
              <p>Collects RSS/Atom feeds, removes duplicates and writes a sourced digest. Publishes it to your Slack or Mattermost channel at the cadence you choose.</p>
            </div>
            <div className="ch-bot-story">
              <h4><a href={`${GITHUB}/tree/main/bots/docs-refresh`} target="_blank" rel="noreferrer">Doki <ArrowUpRight size={13} /></a><span>Documentation maintenance</span></h4>
              <p>Aligns your documentation with the code, then keeps it current with scheduled incremental updates based on what changed. Delivers the changes as reviewable pull requests.</p>
            </div>
          </div>
          <p className="ch-trigger-summary">Schedule <ArrowRight size={13} /> Bot <ArrowRight size={13} /> Published results</p>
        </article>
      </div>
    </section>
  );
}

export default function CloudHome({ marketplaceEnabled = false }: { marketplaceEnabled?: boolean }) {
  const [menuOpen, setMenuOpen] = useState(false);
  const resolved = useThemeStore(s => s.resolved);
  const setTheme = useThemeStore(s => s.setMode);
  useEffect(() => {
    const previousTitle = document.title;
    document.title = "Iterion Cloud — The control plane for AI agents";
    return () => { document.title = previousTitle; };
  }, []);
  return (
    <div className="cloud-home">
      <a href="#ch-main" className="ch-skip">Skip to content</a>
      <header className="ch-nav-wrap">
        <div className="ch-nav ch-container">
          <Brand />
          <nav className="ch-nav-links" aria-label="Main navigation"><a href="#workflows">Use cases</a><a href="#control">Platform</a><a href={DOCS} target="_blank" rel="noreferrer">Documentation <ArrowUpRight size={12} /></a></nav>
          <div className="ch-nav-actions"><button className="ch-theme" type="button" onClick={() => setTheme(resolved === "dark" ? "light" : "dark")} aria-label={`Switch to ${resolved === "dark" ? "light" : "dark"} theme`}>{resolved === "dark" ? <Sun size={17} /> : <Moon size={17} />}</button><a className="ch-github-icon" href={GITHUB} target="_blank" rel="noreferrer" aria-label="Iterion on GitHub"><GitHubLogoIcon width={19} height={19} /></a><Link href="/login" className="ch-nav-signin">Sign in <ArrowUpRight size={14} /></Link><button className="ch-mobile-toggle" type="button" onClick={() => setMenuOpen(!menuOpen)} aria-expanded={menuOpen} aria-controls="ch-mobile-nav" aria-label={menuOpen ? "Close navigation" : "Open navigation"}>{menuOpen ? <X size={21} /> : <Menu size={21} />}</button></div>
        </div>
        {menuOpen && <nav id="ch-mobile-nav" className="ch-mobile-nav" aria-label="Mobile navigation"><a href="#workflows" onClick={() => setMenuOpen(false)}>Use cases</a><a href="#control" onClick={() => setMenuOpen(false)}>Platform</a><a href={DOCS} target="_blank" rel="noreferrer">Documentation <ArrowUpRight size={13} /></a></nav>}
      </header>
      <main id="ch-main">
        <section className="ch-hero ch-container" aria-labelledby="ch-title">
          <div className="ch-hero-copy"><a className="ch-open-source" href={GITHUB} target="_blank" rel="noreferrer"><span className="ch-open-dot" /> OPEN SOURCE. OPEN POSSIBILITIES. <ChevronRight size={13} /></a><h1 id="ch-title">Your agents called.<br /><span>They need an<br />orchestrator.</span></h1><p className="ch-hero-description">Linux runs apps. Kubernetes orchestrates containers.<br /><strong>Iterion orchestrates agents.</strong></p><div className="ch-hero-ctas"><Link href="/login" className="ch-button ch-button-primary">Open Iterion Cloud <ArrowUpRight size={17} /></Link><a href="#workflows" className="ch-button ch-button-text"><ArrowRight size={15} /> Explore use cases</a></div><div className="ch-hero-footnote"><span><Check size={13} /> Git native</span><span><Check size={13} /> Model agnostic</span><span><Check size={13} /> MIT licensed</span></div><GitHubStar /></div>
          <HeroVisual />
        </section>
        <StackCompatibility />
        <div className="ch-container"><UseCases />
          <PlatformFeatures />
          <section className="ch-final-cta" aria-labelledby="ch-deployment-heading">
            <p className="ch-eyebrow">DEPLOYMENT OPTIONS</p>
            <h2 id="ch-deployment-heading">Cloud, local or <span className="ch-no-wrap">self-hosted.</span></h2>
            <p>The same workflow engine, wherever you run it.<br />Use Iterion Cloud, work locally, or deploy on your own infrastructure.</p>
            <div><Link href="/login" className="ch-button ch-button-primary">Open Iterion Cloud <ArrowUpRight size={17} /></Link><a href={`${DOCS}quickstart.html`} target="_blank" rel="noreferrer" className="ch-button ch-button-secondary">Run locally <ArrowUpRight size={14} /></a><a href={`${DOCS}cloud-deployment.html`} target="_blank" rel="noreferrer" className="ch-button ch-button-text">Self-host Iterion <ArrowUpRight size={14} /></a></div>
          </section>
        </div>
      </main>
      <footer className="ch-footer ch-container"><div><Brand /><p>Complex work. Beautifully orchestrated.</p></div><nav aria-label="Footer navigation"><a href={DOCS} target="_blank" rel="noreferrer">Docs <ArrowUpRight size={12} /></a><a href={GITHUB} target="_blank" rel="noreferrer">GitHub <ArrowUpRight size={12} /></a>{marketplaceEnabled && <Link href="/marketplace">Marketplace <ArrowRight size={12} /></Link>}<span>Open source · MIT license</span></nav></footer>
    </div>
  );
}
