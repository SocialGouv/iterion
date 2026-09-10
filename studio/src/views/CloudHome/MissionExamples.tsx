import { Activity, ArrowRight, ArrowUpRight, Check, Code2, FlaskConical, GitPullRequest, Layers3, MessagesSquare, RefreshCw, ShieldCheck, Zap, type LucideIcon } from "lucide-react";

const GITHUB = "https://github.com/SocialGouv/iterion/tree/main/bots/";
const DOCS = "https://socialgouv.github.io/iterion/";

type Mission = {
  bot: string;
  path: string;
  category: string;
  title: string;
  description: string;
  outcome: string;
  Icon: LucideIcon;
  prerequisite?: { bot: string; path: string; description: string };
};

const missions: Mission[] = [
  {
    bot: "Seki", path: "sec-audit-source", category: "SECURITY AUDIT", Icon: ShieldCheck,
    title: "Audit the repo. Triage the findings.",
    description: "Scan source code and secrets, investigate findings, and remember reviewed false positives. Get actionable security issues; remediation is opt-in.",
    outcome: "Findings with evidence and fix guidance",
  },
  {
    bot: "Endy", path: "e2e-coverage", category: "END-TO-END TESTS", Icon: FlaskConical,
    title: "Turn coverage gaps into real E2E tests.",
    description: "Map the app’s features to coverage, add tests in your repo’s own harness, and check that they catch regressions. Rerun the suite as each gap is closed.",
    outcome: "E2E tests + a feature coverage map",
  },
  {
    bot: "Obsy", path: "instrument", category: "OBSERVABILITY", Icon: Activity,
    title: "Sentry wired. Logs in order.",
    description: "Add Sentry or GlitchTip error tracking, structure logs around your existing logger, and test the integration. Enable performance tracing when you need it.",
    outcome: "Instrumented code, tests and setup docs",
  },
  {
    bot: "Morphy", path: "modernize", category: "CODEBASE MODERNIZATION", Icon: RefreshCw,
    title: "Change the stack. Keep the behavior.",
    description: "Upgrade runtimes, frameworks or data stores in verified stages. Check each stage against a behavioral baseline before moving on.",
    outcome: "A migration verified at every stage",
    prerequisite: { bot: "Goldy", path: "golden-master", description: "builds the required non-regression safety net first." },
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
        <p>From the first brief to the next migration.<br />Bots that plan, build, test and deliver.</p>
      </div>
      <div className="ch-build-missions">
        <article className="ch-mission-card ch-appy-story" aria-labelledby="ch-appy-title">
          <div className="ch-mission-topline"><span><Layers3 size={18} aria-hidden="true" /> APPLICATION DEVELOPMENT</span><BotLink name="Appy" path="app-dev" /></div>
          <h3 id="ch-appy-title">One brief. A whole app.</h3>
          <p>Appy turns an idea into working software: scaffold the app, write the code, run the tests and iterate on the result. Enable deployment to take it all the way to your configured target.</p>
          <div className="ch-appy-modes">
            <div>
              <p><MessagesSquare size={15} aria-hidden="true" /> INTERACTIVE</p>
              <h4>“Grill me.”</h4>
              <span>Challenge the brief together. Appy asks the missing questions and turns your answers into a specification before building.</span>
            </div>
            <div>
              <p><Zap size={15} aria-hidden="true" /> ONE PROMPT</p>
              <h4>“Here’s the brief. Go.”</h4>
              <span>Skip the interview and switch off draft approval. Appy plans, builds and tests from your initial prompt.</span>
            </div>
          </div>
          <div className="ch-appy-proof">
            <p className="ch-proof-label"><Check size={15} aria-hidden="true" /> TESTED END TO END <span>JUL 2026</span></p>
            <h4>Next.js + PostgreSQL <ArrowRight size={19} aria-hidden="true" /> Kubernetes</h4>
            <p>A DSFR app with a persistent database: repository created, code tested, image built by CI and deployed. No manual step in the recorded run.</p>
            <a href={`${DOCS}bot-runs/app-dev.html`} target="_blank" rel="noreferrer">Read the run report <ArrowUpRight size={13} aria-hidden="true" /></a>
          </div>
        </article>
        <article className="ch-mission-card ch-featurly-story" aria-labelledby="ch-featurly-title">
          <div className="ch-mission-topline"><span><Code2 size={18} aria-hidden="true" /> FEATURE DEVELOPMENT</span><BotLink name="Featurly" path="feature-dev" /></div>
          <h3 id="ch-featurly-title">Ship the whole feature.</h3>
          <p>Give Featurly a feature and an existing repo. It carries the change through implementation, tests and review, one verified commit at a time.</p>
          <ol className="ch-feature-steps">
            <li><span>01</span><div><strong>Plan the change</strong><p>Read the codebase and work out the steps.</p></div></li>
            <li><span>02</span><div><strong>Implement & test</strong><p>Build the feature in meaningful commits.</p></div></li>
            <li><span>03</span><div><strong>Verify & refine</strong><p>Work through build failures and review findings.</p></div></li>
          </ol>
          <p className="ch-feature-delivery"><GitPullRequest size={17} aria-hidden="true" /> Push the commits and open a pull request when enabled.</p>
        </article>
      </div>
      <div className="ch-maintenance-missions">
        {missions.map(({ bot, path, category, title, description, outcome, Icon, prerequisite }) => (
          <article className="ch-mission-card ch-maintenance-story" key={bot} aria-labelledby={`ch-mission-${path}`}>
            <div className="ch-mission-topline"><span><Icon size={18} aria-hidden="true" /> {category}</span><BotLink name={bot} path={path} /></div>
            <h3 id={`ch-mission-${path}`}>{title}</h3>
            <p>{description}</p>
            {prerequisite && <p className="ch-mission-prerequisite"><BotLink name={prerequisite.bot} path={prerequisite.path} /> {prerequisite.description}</p>}
            <p className="ch-mission-outcome"><ArrowRight size={14} aria-hidden="true" /> {outcome}</p>
          </article>
        ))}
      </div>
      <div className="ch-missions-footer"><p>Run these bots, inspect their workflows, or fork them for your team.</p><a href={`${DOCS}examples.html`} target="_blank" rel="noreferrer">Explore the full catalogue <ArrowUpRight size={14} aria-hidden="true" /></a></div>
    </section>
  );
}
