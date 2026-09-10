import type { ComponentType } from "react";
import { ArrowUpRight, Box, Container, FileCode2, GitBranch, GitMerge, Pin, Search, ShieldCheck, ShipWheel, Webhook } from "lucide-react";
import Claude from "@lobehub/icons/es/Claude";
import OpenAI from "@lobehub/icons/es/OpenAI";
import Gemini from "@lobehub/icons/es/Gemini";
import Mistral from "@lobehub/icons/es/Mistral";
import Ollama from "@lobehub/icons/es/Ollama";
import Vllm from "@lobehub/icons/es/Vllm";
import OpenRouter from "@lobehub/icons/es/OpenRouter";
import Azure from "@lobehub/icons/es/Azure";
import Bedrock from "@lobehub/icons/es/Bedrock";
import Github from "@lobehub/icons/es/Github";
import MCP from "@lobehub/icons/es/MCP";

type StackItem = { name: string } & (
  | { Icon: ComponentType<{ size?: number }>; mark?: never }
  | { mark: string; Icon?: never }
);

const DOCS = "https://socialgouv.github.io/iterion/";

// Compatibility surfaces, not an exhaustive catalog of accepted model IDs.
// Custom model endpoints use the engine's OpenAI-compatible API support.
const models: StackItem[] = [
  { name: "Mistral", Icon: Mistral },
  { name: "Ollama", Icon: Ollama },
  { name: "vLLM", Icon: Vllm },
  { name: "Claude", Icon: Claude },
  { name: "OpenAI", Icon: OpenAI },
  { name: "Gemini", Icon: Gemini },
  { name: "OpenRouter", Icon: OpenRouter },
  { name: "Azure OpenAI", Icon: Azure },
  { name: "AWS Bedrock", Icon: Bedrock },
];

const tools: StackItem[] = [
  { name: "Devbox", Icon: Box },
  { name: "GitHub", Icon: Github },
  { name: "GitLab", Icon: GitBranch },
  { name: "Forgejo", Icon: GitMerge },
  { name: "MCP", Icon: MCP },
  { name: "Docker", Icon: Container },
  { name: "Kubernetes", Icon: ShipWheel },
  { name: "SearXNG", Icon: Search },
  { name: "Webhooks", Icon: Webhook },
];

// Examples of project toolchains, provisioned through Devbox when needed.
// These are toolchain examples, not native script-language integrations.
const languages: StackItem[] = [
  { name: "JavaScript", mark: "JS" },
  { name: "TypeScript", mark: "TS" },
  { name: "Python", mark: "Py" },
  { name: "Go", mark: "Go" },
  { name: "Rust", mark: "Rs" },
  { name: "Java", mark: "Jv" },
  { name: "C# / .NET", mark: "C#" },
  { name: "PHP", mark: "php" },
  { name: "Ruby", mark: "Rb" },
  { name: "C / C++", mark: "C++" },
  { name: "Shell", mark: "$_" },
];

function StackRow({ label, items, reverse = false }: {
  label: string;
  items: StackItem[];
  reverse?: boolean;
}) {
  return (
    <div className="ch-stack-row">
      <p className="ch-stack-row-label">{label}</p>
      <div className="ch-stack-window" tabIndex={0} role="group" aria-label={label}>
        <div className={`ch-stack-track${reverse ? " ch-stack-reverse" : ""}`}>
          {[false, true].map(duplicate => (
            <ul
              className={`ch-stack-list${duplicate ? " ch-stack-duplicate" : ""}`}
              key={String(duplicate)}
              aria-label={duplicate ? undefined : label}
              aria-hidden={duplicate || undefined}
            >
              {items.map(({ name, Icon, mark }) => (
                <li key={name}>
                  <span aria-hidden="true">{Icon ? <Icon size={24} /> : <span className="ch-language-mark">{mark}</span>}</span>
                  <span>{name}</span>
                </li>
              ))}
            </ul>
          ))}
        </div>
      </div>
    </div>
  );
}

export default function StackCompatibility() {
  return (
    <div id="stack" className="ch-compatibility">
      <section className="ch-devbox-support" aria-labelledby="ch-devbox-heading">
        <div className="ch-devbox-overview">
          <p className="ch-runtime-label"><Box size={18} aria-hidden="true" /> DEVBOX, FIRST-CLASS.</p>
          <h2 id="ch-devbox-heading">Your bot’s tools. Your repo’s versions.</h2>
          <p>Give your agents the runtimes, compilers and CLIs your project needs. Pin their versions per bot and per repository, alongside your code.</p>
          <ul className="ch-devbox-tools" aria-label="Toolchain components"><li>Runtimes</li><li>Build tools</li><li>Package managers</li><li>Test runners</li></ul>
        </div>
        <div className="ch-devbox-config">
          <dl className="ch-devbox-scopes">
            <div><dt>Per bot</dt><dd>Package its own toolchain with <code>devbox.json</code> and <code>devbox.lock</code>.</dd></div>
            <div><dt>Per project / repo</dt><dd>Pick up the repository’s Devbox config and lockfile. Its versions take precedence.</dd></div>
          </dl>
          <div className="ch-devbox-usage"><FileCode2 size={19} aria-hidden="true" /><p><strong>From agent commands to build & test.</strong><span>Use the configured tools across your workflow: scripted steps, application builds, test suites and the commands your agents run.</span></p></div>
        </div>
        <div className="ch-devbox-footer"><span><Pin size={13} aria-hidden="true" /> Pinned versions. Committed lockfiles.</span><a href={`${DOCS}sandbox.html#devbox-tools-devbox-json`} target="_blank" rel="noreferrer">Explore the Devbox integration <ArrowUpRight size={13} aria-hidden="true" /></a></div>
      </section>
      <section className="ch-stack" aria-labelledby="ch-stack-heading">
        <h2 id="ch-stack-heading" className="ch-eyebrow ch-stack-heading">YOUR STACK. ALREADY INVITED.</h2>
        <div className="ch-stack-rows">
          <StackRow label="Models & inference" items={models} />
          <StackRow label="Tools & infrastructure" items={tools} reverse />
          <StackRow label="Toolchains via Devbox" items={languages} />
        </div>
        <div className="ch-sovereign-note">
          <ShieldCheck size={20} strokeWidth={1.5} aria-hidden="true" />
          <p>
            <strong>Bring any sovereign model through an OpenAI-compatible API.</strong>
            <span> Choose your hosting, run on your own infrastructure, and keep control of where your models run.</span>
          </p>
        </div>
      </section>
    </div>
  );
}
