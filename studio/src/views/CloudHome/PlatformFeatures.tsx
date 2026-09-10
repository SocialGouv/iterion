import {
  ArrowUpRight, BookOpen, Box, BrainCircuit, CalendarClock, ChartNoAxesCombined,
  ClipboardCheck, FileText, Gauge, GitBranch, GitPullRequest, KeyRound, Layers3,
  MessageSquare, Network, PanelsTopLeft, Puzzle, RotateCcw, ScanEye, ScrollText,
  ShieldCheck, SlidersHorizontal, SquarePen, Terminal, Users, Workflow,
} from "lucide-react";

const highlights = [
  {
    label: "LIVE AGENT SUPERVISION",
    title: "Agents can have a coach, too.",
    description: "Attach a supervisor that watches live activity and sends course corrections during a run. Define when it should step in, with its own evaluation budget.",
    icon: ScanEye,
    href: "supervisors.html",
    link: "Meet the supervisors",
  },
  {
    label: "MEMORY ACROSS RUNS",
    title: "Give the next run a head start.",
    description: "Let bots save decisions, findings and handover notes in persistent knowledge spaces. Reuse that context across runs, bots or projects, with scoped access.",
    icon: BookOpen,
    href: "memory-and-knowledge.html",
    link: "Explore shared memory",
  },
  {
    label: "WORKFLOW BENCHMARKS",
    title: "Put your workflow to the test.",
    description: "Compare judge verdicts across repeated runs and recipe variants. Measure approval rates and how review loops converge before extending your automation.",
    icon: ChartNoAxesCombined,
    href: "asymptote-bench.html",
    link: "Explore quality benchmarks",
  },
];

const capabilities = [
  {
    label: "01 / ORCHESTRATE",
    title: "Define how agents work.",
    icon: Workflow,
    features: [
      { name: "Multi-agent, multi-model", description: "Mix specialist agents and model families in one workflow. Let one model implement and another review.", icon: Workflow },
      { name: "Visual + code authoring", description: "Build workflows on a canvas or edit the source. Both stay in sync, with validation as you work.", icon: SquarePen },
      { name: "Reusable, versioned bots", description: "Keep workflows in Git. Review changes, share bot bundles and reuse what works across projects.", icon: GitBranch },
      { name: "Model choice & fallback", description: "Choose models for each task, including sovereign endpoints. Set fallback routes for provider outages or usage limits.", icon: BrainCircuit },
      { name: "Skills, plugins & MCP", description: "Give agents your team's knowledge, connect tools and extend their capabilities.", icon: Puzzle },
      { name: "CLI, API & SDK", description: "Start workflows from your terminal, your applications or an existing delivery pipeline.", icon: Terminal },
    ],
  },
  {
    label: "02 / OPERATE",
    title: "Connect work to action.",
    icon: Layers3,
    features: [
      { name: "Repositories & events", description: "Trigger bots from pull requests, board changes and webhooks. Chain workflows when a run completes.", icon: GitPullRequest },
      { name: "Recurring automation", description: "Schedule maintenance, documentation and monitoring. Set the cadence and let the workflow run.", icon: CalendarClock },
      { name: "Dependencies between pipelines", description: "Chain tasks across bots and hold downstream work until its prerequisites succeed.", icon: Network },
      { name: "One place to follow work", description: "Track repositories, boards, pipelines and runs in the Studio. Inspect progress and generated artifacts.", icon: PanelsTopLeft },
      { name: "Ask questions. Keep working.", description: "Let agents ask for input while independent work continues, and wait when an answer is needed. Keep approvals and live steering available.", icon: MessageSquare },
      { name: "Checkpoint & resume", description: "Resume interrupted workflows from saved progress, without repeating completed steps.", icon: RotateCcw },
    ],
  },
  {
    label: "03 / GOVERN",
    title: "Keep control of execution.",
    icon: ShieldCheck,
    features: [
      { name: "Budgets & limits", description: "Set limits on cost, tokens, time and iterations. Adjust a running workflow's budget when needed.", icon: Gauge },
      { name: "Tests & review gates", description: "Require tests, checks or a reviewer's approval before progressing. Bound review-and-fix loops with explicit limits.", icon: ClipboardCheck },
      { name: "Isolated workspaces", description: "Run work in dedicated Git worktrees and sandboxes. Configure tool permissions and network access.", icon: Box },
      { name: "Your keys & secrets", description: "Bring your own provider credentials and bind secrets to the workflows that need them.", icon: KeyRound },
      { name: "Teams & access", description: "Organize shared work across teams, manage membership and connect your organization's SSO.", icon: Users },
      { name: "Traceable execution", description: "Follow run events, inspect outputs and review audit records to understand what happened.", icon: ScrollText },
    ],
  },
];

export default function PlatformFeatures() {
  return (
    <section className="ch-section ch-control" id="control" aria-labelledby="ch-control-heading">
      <div className="ch-section-heading">
        <div>
          <p className="ch-eyebrow">THE AGENT CONTROL PLANE.</p>
          <h2 id="ch-control-heading">Let it run.<br /><span>Keep the reins.</span></h2>
        </div>
        <p>Supervise the work. Carry context forward.<br />Measure results and keep control.</p>
      </div>
      <div className="ch-highlight-grid">
        {highlights.map(({ label, title, description, icon: Icon, href, link }) => (
          <article className="ch-highlight" key={label}>
            <p className="ch-highlight-label"><Icon size={21} strokeWidth={1.5} aria-hidden="true" />{label}</p>
            <h3>{title}</h3>
            <p>{description}</p>
            <a href={`https://socialgouv.github.io/iterion/${href}`} target="_blank" rel="noreferrer">{link} <ArrowUpRight size={13} /></a>
          </article>
        ))}
      </div>
      <div className="ch-experience-grid">
        <article className="ch-experience-card">
          <p className="ch-experience-label"><FileText size={19} strokeWidth={1.5} aria-hidden="true" /> TAILORED OUTPUT</p>
          <h3>The right information, up front.</h3>
          <p>Each bot defines what to highlight in its final output: key findings, a summary, or links to deliverables. Shape the result around what people need to understand and act on.</p>
          <p className="ch-experience-example">A review's findings. A published digest. A pull request.</p>
        </article>
        <article className="ch-experience-card">
          <p className="ch-experience-label"><SlidersHorizontal size={19} strokeWidth={1.5} aria-hidden="true" /> FOCUSED CONFIGURATION</p>
          <h3>Settings your whole team can use.</h3>
          <p>Bots can expose custom configuration fields in a dedicated editor. Choose exactly what users can see and change, so non-technical teammates get only the settings relevant to their work.</p>
          <p className="ch-experience-example">For Vigie: choose feed sources and adjust the editorial brief.</p>
          <a href="https://socialgouv.github.io/iterion/config-share.html" target="_blank" rel="noreferrer">Explore scoped configuration <ArrowUpRight size={13} /></a>
        </article>
      </div>
      <div className="ch-capability-grid">
        {capabilities.map(({ label, title, icon: Icon, features }) => (
          <article className="ch-capability-group" key={label}>
            <p className="ch-capability-label">{label}</p>
            <Icon className="ch-capability-icon" size={28} strokeWidth={1.3} aria-hidden="true" />
            <h3>{title}</h3>
            <ul>
              {features.map(({ name, description, icon: FeatureIcon }) => (
                <li key={name}>
                  <h4><FeatureIcon size={16} strokeWidth={1.6} aria-hidden="true" /><span>{name}</span></h4>
                  <p>{description}</p>
                </li>
              ))}
            </ul>
          </article>
        ))}
      </div>
      <a className="ch-capability-docs" href="https://socialgouv.github.io/iterion/current-state.html" target="_blank" rel="noreferrer">
        Explore the platform documentation <ArrowUpRight size={14} />
      </a>
    </section>
  );
}
