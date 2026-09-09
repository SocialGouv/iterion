import type { ComponentType } from "react";
import { Container, GitBranch, GitMerge, Search, ShieldCheck, ShipWheel, Webhook } from "lucide-react";
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

type StackItem = { name: string; Icon: ComponentType<{ size?: number }> };

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
  { name: "GitHub", Icon: Github },
  { name: "GitLab", Icon: GitBranch },
  { name: "Forgejo", Icon: GitMerge },
  { name: "MCP", Icon: MCP },
  { name: "Docker", Icon: Container },
  { name: "Kubernetes", Icon: ShipWheel },
  { name: "SearXNG", Icon: Search },
  { name: "Webhooks", Icon: Webhook },
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
              {items.map(({ name, Icon }) => (
                <li key={name}>
                  <span aria-hidden="true"><Icon size={24} /></span>
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
    <section
      id="stack"
      className="ch-stack ch-container"
      aria-labelledby="ch-stack-heading"
    >
      <h2 id="ch-stack-heading" className="ch-eyebrow ch-stack-heading">YOUR STACK. ALREADY INVITED.</h2>
      <div className="ch-stack-rows">
        <StackRow label="Models & inference" items={models} />
        <StackRow label="Tools & infrastructure" items={tools} reverse />
      </div>
      <div className="ch-sovereign-note">
        <ShieldCheck size={20} strokeWidth={1.5} aria-hidden="true" />
        <p>
          <strong>Bring any sovereign model through an OpenAI-compatible API.</strong>
          <span> Choose your hosting, run on your own infrastructure, and keep control of where your models run.</span>
        </p>
      </div>
    </section>
  );
}
