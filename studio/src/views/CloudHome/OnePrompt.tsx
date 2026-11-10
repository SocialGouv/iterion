import { ArrowUpRight } from "lucide-react";
import { CopyButton } from "@/components/ui/CopyButton";

const DOCS = "https://socialgouv.github.io/iterion/";

// The cloud onboarding prompt. This string is the reference; the walkthrough
// in docs/getting-started.md carries the same block verbatim. The two copies
// cannot import one another (a markdown page, a TSX string), so
// scripts/prompt-parity.sh diffs them — a wording change here lands on both or
// that task fails.
export const SETUP_PROMPT = `Set up Iterion Cloud (https://iterion.cloud) so I can run AI agents on my repositories:

1. Install the CLI: \`curl -fsSL https://socialgouv.github.io/iterion/install.sh | sh\`
2. Add the .bot authoring skill: \`npx skills add https://github.com/SocialGouv/iterion --skill iterion-dsl\`
3. Log in: \`iterion remote login https://iterion.cloud\` — I approve it in my browser.
4. Register the MCP server: \`claude mcp add iterion -- iterion mcp\` (or the same stdio server in your own MCP config).
5. Check it works: \`iterion remote status\` and \`iterion remote bots list\`.

An LLM key is optional — the shared pool or my org may already fund runs. If I want my own, ask me which env var holds it, then run \`iterion remote api-keys create --provider <provider> --name default --from-env <VAR>\`.

Once it works, propose a next step — such as the \`review-pr\` bot on one of my repositories (https://socialgouv.github.io/iterion/getting-started.html#deploy-your-first-agent-on-a-repo) — and, once I agree, carry it through with me.`;

// The onboarding band: a short pitch beside the prompt a visitor copies into
// their coding agent. CopyButton centralises the clipboard interaction; its
// accessible name names WHAT it copies, so the control never collapses into
// another "copy" for a screen reader (RGAA 11.1).
export default function OnePrompt() {
  return (
    <section id="one-prompt" className="ch-prompt" aria-labelledby="ch-prompt-heading">
      <div className="ch-prompt-intro">
        <p className="ch-eyebrow">GET STARTED WITH ITERION CLOUD</p>
        <h2 id="ch-prompt-heading">One prompt,<br /><span>and the cloud is wired.</span></h2>
        <p>
          Paste it into a fresh session of your coding agent — Claude Code, Codex CLI, pi, Cursor. It installs the CLI, logs you in and checks the connection, then offers to put a first bot on one of your repositories.
        </p>
        <a href={`${DOCS}getting-started.html`} target="_blank" rel="noreferrer" className="ch-prompt-docs">The full walkthrough <ArrowUpRight size={13} aria-hidden="true" /></a>
      </div>
      <div className="ch-prompt-card">
        <div className="ch-prompt-bar">
          <span>SETUP PROMPT</span>
          <CopyButton value={SETUP_PROMPT} label="Copy" copiedLabel="Copied" describes="the cloud setup prompt" className="ch-prompt-copy" />
        </div>
        <pre className="ch-prompt-block"><code>{SETUP_PROMPT}</code></pre>
      </div>
    </section>
  );
}
