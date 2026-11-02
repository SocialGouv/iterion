import { ArrowUpRight, Check } from "lucide-react";
import { CopyButton } from "@/components/ui/CopyButton";

const DOCS = "https://socialgouv.github.io/iterion/";

// The cloud onboarding prompt, verbatim the block README.md,
// docs/getting-started.md and docs/index.md carry. The four copies cannot
// import one another (markdown pages, a TSX string), so
// scripts/prompt-parity.sh diffs them — a wording change here must land on
// all four or that task fails.
export const SETUP_PROMPT = `Set up Iterion Cloud (https://iterion.cloud — source: https://github.com/SocialGouv/iterion) so it can deploy AI agents on my repositories. Work in order, and pause to ask me whenever a step needs a human decision.

1. Install the CLI: \`curl -fsSL https://socialgouv.github.io/iterion/install.sh | sh\`, then run \`iterion version\`.
2. Install the skill that teaches you the .bot DSL: \`npx skills add https://github.com/SocialGouv/iterion --skill iterion-dsl\`.
3. Connect the CLI to the cloud: \`iterion remote login https://iterion.cloud\`. It opens a browser approval page — stop and ask me to approve it there. If I cannot open a browser, ask me for a personal access token and run \`iterion remote login https://iterion.cloud --token <my-token>\` instead.
4. Register the Iterion MCP server: \`claude mcp add iterion -- iterion mcp\`. If you are not Claude Code, register the same stdio server in your own MCP configuration instead (command \`iterion\`, args \`["mcp"]\`).
5. Cloud runs spend an LLM credential. First ask me whether my account is already funded — the instance runs a shared credential pool and platform credentials that let members test for free while capacity lasts (availability is not guaranteed), and my org may already hold keys. If nothing does, ask me for a key from any provider I use — Anthropic, OpenAI, xAI, ZAI, Moonshot (Kimi), OpenRouter, Bedrock, Vertex, or Azure (wire ids: \`anthropic\`, \`openai\`, \`xai\`, \`zai\`, \`moonshot\`, \`openrouter\`, \`bedrock\`, \`vertex\`, \`azure\`) — and store it with \`iterion remote api-keys create --provider <provider> --name default --from-env <MY_KEY_ENV_VAR>\`. The secret travels through the environment only, never a command argument or a file.
6. Verify, read-only: \`iterion remote status\` names the instance and my account, \`iterion remote bots list\` returns the bot catalog, and your \`remote_status\` / \`remote_bots_list\` MCP tools answer.

Do not connect any forge or git host, do not enable a repository, and do not launch a run — that is my next step, not yours. If a step fails, stop there and tell me what to fix. Finish by reporting each step's outcome and how to undo the setup (\`iterion remote logout\`, \`claude mcp remove iterion\`).`;

// The onboarding section: the copy-paste prompt that turns a visitor into a
// connected operator without leaving the page. CopyButton centralises the
// clipboard interaction; its accessible name names WHAT it copies, so a row
// of controls never collapses into one for a screen reader (RGAA 11.1).
export default function OnePrompt() {
  return (
    <section id="one-prompt" className="ch-prompt" aria-labelledby="ch-prompt-heading">
      <p className="ch-eyebrow">GET STARTED WITH ITERION CLOUD</p>
      <h2 id="ch-prompt-heading">One prompt,<br /><span>and the cloud is wired.</span></h2>
      <p>
        Paste the whole block into a fresh session of your AI coding agent — Claude Code, Codex CLI, pi, Cursor, any MCP-capable agent. It installs the CLI, connects you to Iterion Cloud, and verifies readiness: one browser pause for the login approval, nothing more. No forge connected, no repository enabled, no run launched — that is your next step.
      </p>
      <pre className="ch-prompt-block"><code>{SETUP_PROMPT}</code></pre>
      <div className="ch-prompt-actions">
        <CopyButton value={SETUP_PROMPT} label="Copy" copiedLabel="Copied" describes="the cloud setup prompt" className="ch-prompt-copy" />
        <a href={`${DOCS}getting-started.html`} target="_blank" rel="noreferrer" className="ch-prompt-docs">The full walkthrough <ArrowUpRight size={13} aria-hidden="true" /></a>
      </div>
      <div className="ch-prompt-footnote">
        <span><Check size={13} aria-hidden="true" /> Any MCP-capable coding agent</span>
        <span><Check size={13} aria-hidden="true" /> Any LLM provider — or the shared pool's free credit when it funds you</span>
        <span><Check size={13} aria-hidden="true" /> Read-only until you connect a repo</span>
      </div>
    </section>
  );
}
