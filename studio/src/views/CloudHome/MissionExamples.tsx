import { Activity, ArrowRight, ArrowUpRight, BookOpen, Check, Code2, FlaskConical, GitBranch, GitPullRequest, Layers3, MessagesSquare, PackageCheck, Radio, RefreshCw, ShieldCheck, Zap, type LucideIcon } from "lucide-react";

const GITHUB = "https://github.com/SocialGouv/iterion/tree/main/bots/";
const DOCS = "https://socialgouv.github.io/iterion/";

type Mission = {
  bot: string;
  path: string;
  category: string;
  title: string;
  description: string;
  Icon: LucideIcon;
  prerequisite?: { bot: string; path: string; description: string };
};

const missions: Mission[] = [
  {
    bot: "Featurly", path: "feature-dev", category: "FEATURES", Icon: Code2,
    title: "Ship the whole feature.",
    description: "From an existing repo to a tested feature: plan, implement and review, one verified commit at a time. Push and open a PR when enabled.",
  },
  {
    bot: "Seki", path: "sec-audit-source", category: "SECURITY", Icon: ShieldCheck,
    title: "Find risks. Cut the noise.",
    description: "Scan source code and secrets, investigate findings and remember false positives. Get evidence and fix guidance; remediation is opt-in.",
  },
  {
    bot: "Endy", path: "e2e-coverage", category: "E2E TESTS", Icon: FlaskConical,
    title: "Close the coverage gaps.",
    description: "Map features to coverage, add E2E tests in your repo’s own harness, and verify that they catch regressions. Rerun the suite as gaps close.",
  },
  {
    bot: "Obsy", path: "instrument", category: "OBSERVABILITY", Icon: Activity,
    title: "Sentry wired. Logs in order.",
    description: "Add Sentry or GlitchTip, structure logs around your existing logger, and test the integration. With setup docs and optional performance tracing.",
  },
  {
    bot: "Morphy", path: "modernize", category: "MODERNIZATION", Icon: RefreshCw,
    title: "New stack. Same behavior.",
    description: "Upgrade runtimes, frameworks or data stores in stages, each checked against a behavioral baseline.",
    prerequisite: { bot: "Goldy", path: "golden-master", description: "builds the required safety net first." },
  },
  {
    bot: "Revi", path: "review-pr", category: "CODE REVIEW", Icon: GitPullRequest,
    title: "Every PR gets a review.",
    description: "React to pull request events with a code review. Post findings directly on the changed lines, where your team already discusses the code.",
  },
  {
    bot: "Vetty", path: "dep-update-guard", category: "DEPENDENCIES", Icon: PackageCheck,
    title: "Keep dependency PRs moving.",
    description: "Check incoming Dependabot and Renovate updates, adapt the code when needed, then run the build and tests before reporting back on the PR.",
  },
  {
    bot: "Doki", path: "docs-refresh", category: "DOCUMENTATION", Icon: BookOpen,
    title: "Docs that keep up.",
    description: "Align docs with the code, then schedule incremental updates based on what changed. Get reviewable pull requests as your documentation evolves.",
  },
  {
    bot: "Vigie", path: "feed-watch", category: "NEWS & TECH WATCH", Icon: Radio,
    title: "Your daily signal, delivered.",
    description: "Collect RSS/Atom feeds, deduplicate and write a sourced digest. Publish to Slack or Mattermost on the schedule you choose.",
  },
];

function BotLink({ name, path }: { name: string; path: string }) {
  return <a href={`${GITHUB}${path}`} target="_blank" rel="noreferrer">{name} <ArrowUpRight size={13} aria-hidden="true" /></a>;
}

export default function MissionExamples() {
  return (
    <section id="workflows" className="ch-section ch-missions" aria-labelledby="ch-missions-heading">
      <div className="ch-section-heading">
        <div>
          <p className="ch-eyebrow">GIVE YOUR BOTS THE BIG JOBS.</p>
          <h2 id="ch-missions-heading">New apps. Big features.<br />Better codebases.</h2>
        </div>
        <div id="connected-work" className="ch-missions-connect">
          <p><GitBranch size={18} aria-hidden="true" /> Connect your repos.</p>
          <p>React to events. Run on schedule.</p>
          <span>GitHub, GitLab or Forgejo. A brief, a pull request, a webhook or a recurring task: give your bots the work.</span>
        </div>
      </div>
      <div className="ch-mission-collection">
        <article className="ch-appy-story" aria-labelledby="ch-appy-title">
          <div className="ch-mission-topline"><span><Layers3 size={18} aria-hidden="true" /> APPLICATION DEVELOPMENT</span><BotLink name="Appy" path="app-dev" /></div>
          <div className="ch-appy-overview">
            <div>
              <h3 id="ch-appy-title">One brief. A whole app.</h3>
              <p>Appy scaffolds, codes, tests and iterates. Enable deployment to take the result all the way to your configured target.</p>
            </div>
            <div className="ch-appy-modes">
              <div>
                <h4><MessagesSquare size={16} aria-hidden="true" /> “Grill me.”</h4>
                <p>Work through the missing questions together, then build from a clear specification.</p>
              </div>
              <div>
                <h4><Zap size={16} aria-hidden="true" /> “Here’s the brief. Go.”</h4>
                <p>Skip the interview and draft approval. Plan, build and test from one prompt.</p>
              </div>
            </div>
          </div>
          <div className="ch-appy-proof">
            <p><Check size={16} aria-hidden="true" /><span><strong>Next.js + PostgreSQL <ArrowRight size={15} aria-hidden="true" /> Kubernetes</strong><span>Tested end to end · July 2026 · No manual step in the recorded run.</span></span></p>
            <a href={`${DOCS}bot-runs/app-dev.html`} target="_blank" rel="noreferrer">Read the run report <ArrowUpRight size={13} aria-hidden="true" /></a>
          </div>
        </article>
        <div className="ch-mission-grid">
          {missions.map(({ bot, path, category, title, description, Icon, prerequisite }) => (
            <article className="ch-mission-card" key={bot} aria-labelledby={`ch-mission-${path}`}>
              <div className="ch-mission-topline"><span><Icon size={17} aria-hidden="true" /> {category}</span><BotLink name={bot} path={path} /></div>
              <h3 id={`ch-mission-${path}`}>{title}</h3>
              <p>{description}</p>
              {prerequisite && <p className="ch-mission-prerequisite"><BotLink name={prerequisite.bot} path={prerequisite.path} /> {prerequisite.description}</p>}
            </article>
          ))}
        </div>
      </div>
      <div className="ch-missions-footer"><p>Run these bots, inspect their workflows, or fork them for your team.</p><a href={`${DOCS}examples.html`} target="_blank" rel="noreferrer">Explore the full catalogue <ArrowUpRight size={14} aria-hidden="true" /></a></div>
    </section>
  );
}
