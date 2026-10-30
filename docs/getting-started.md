# Get started in one prompt

Paste one prompt into your AI coding agent and it installs Iterion, connects it to iterion.cloud, and verifies readiness to deploy.

The prompt is agent-agnostic: Claude Code is documented first, and any MCP-capable agent that can run shell commands works. It stops deliberately short of your repositories — no forge connection, no run launched. The [walkthrough](#deploy-your-first-agent-on-a-repo) continues from there.

## Before you start

You only need four things the agent cannot get for itself:

- an account on [iterion.cloud](https://iterion.cloud) — sign up directly or ask your org admin for an invitation (see [cloud-user.md](cloud-user.md))
- a browser, for the one-time login approval in step 3
- an Anthropic API key, *only if* your org has no pooled or platform credentials yet (see [byok.md](byok.md)) — the prompt asks for it when needed
- an AI coding agent that can run shell commands (Claude Code, Codex CLI, pi, Cursor, …)

## The prompt

Start a fresh session in your agent, paste the whole block, and expect exactly one browser pause (the login approval):

```text
Set up Iterion (https://github.com/SocialGouv/iterion) so it can deploy AI agents on my repositories, connected to https://iterion.cloud. Work in order, and pause to ask me whenever a step needs a human decision.

1. Install the CLI: `curl -fsSL https://socialgouv.github.io/iterion/install.sh | sh`, then run `iterion version`.
2. Install the skill that teaches you the .bot DSL: `npx skills add https://github.com/SocialGouv/iterion --skill iterion-dsl`.
3. Connect the CLI to the cloud: `iterion remote login https://iterion.cloud`. It opens a browser approval page — stop and ask me to approve it there. If I cannot open a browser, ask me for a personal access token and run `iterion remote login https://iterion.cloud --token <my-token>` instead.
4. Register the Iterion MCP server: `claude mcp add iterion -- iterion mcp`. If you are not Claude Code, register the same stdio server in your own MCP configuration instead (command `iterion`, args `["mcp"]`).
5. Cloud runs need an LLM credential. If my account has none yet, ask me for an Anthropic API key and store it with `iterion remote api-keys create --provider anthropic --name default --from-env ANTHROPIC_API_KEY` — the key travels through the environment only, never a command argument or a file.
6. Verify, read-only: `iterion remote status` names the instance and my account, `iterion remote bots list` returns the bot catalog, and your `remote_status` / `remote_bots_list` MCP tools answer.

Do not connect any forge or git host, do not enable a repository, and do not launch a run — that is my next step, not yours. If a step fails, stop there and tell me what to fix. Finish by reporting each step's outcome and how to undo the setup (`iterion remote logout`, `claude mcp remove iterion`).
```

## What the agent does

| Step | Command | Details |
|---|---|---|
| Install the CLI | `curl -fsSL https://socialgouv.github.io/iterion/install.sh \| sh` | Downloads a release binary and verifies its sha256. Homebrew, Windows and manual downloads: [install.md](install.md#cli). |
| Install the agent skill | `npx skills add https://github.com/SocialGouv/iterion --skill iterion-dsl` | Teaches the agent to author and debug `.bot` workflows. The skill authors, the MCP server operates — [skill.md](skill.md#iterion-dsl--author-workflows). |
| Connect to iterion.cloud | `iterion remote login https://iterion.cloud` | Browser approval at `/cli-auth`; a personal access token (`iap_…`) is stored in `~/.iterion/cli-auth.json`. Headless alternatives: [cloud-cli.md](cloud-cli.md#setup). |
| Register the MCP server | `claude mcp add iterion -- iterion mcp` | Any MCP client can spawn the same stdio server (`command: iterion`, `args: ["mcp"]`): [mcp-server.md](mcp-server.md#setup). |
| Bind an LLM credential | `iterion remote api-keys create … --from-env` | Cloud runs spend a BYOK credential. The value travels through the environment, never a command argument: [secrets hygiene](cloud-cli.md#secrets-hygiene), [byok.md](byok.md). |
| Verify readiness | `iterion remote status` · `iterion remote bots list` | Both are read-only probes; from the MCP side the same answers come from `remote_status` / `remote_bots_list`. |

Deliberately not done by the prompt: connecting a forge, enabling a repository, launching a run. That is the next section.

## Deploy your first agent on a repo

One-time setup, then every pull request triggers the bot on its own.

1. **Connect a forge** (once): in the studio, open `https://iterion.cloud/integrations` and connect GitHub/GitLab/Forgejo (OAuth or a PAT). Headless equivalent: `iterion remote forge connections create --data '{"mode":"pat", …}'`. The mechanics and the three connection modes: [forge-integrations.md](forge-integrations.md).
2. **Enable a bot on a repo** — one command provisions the webhook, the webhook secret and the managed `forge_token` binding:

   ```sh
   iterion remote forge repo-bots create --data '{"connection_id":"conn_…","repo":"org/app","bot_ids":["review-pr"]}'
   ```

3. From now on, opening a PR runs the bot and the result lands back on the forge — a review with inline comments for `review-pr`. Watch the run at `https://iterion.cloud/studio/runs/<id>`.

No webhook needed to try it once — launch by hand against any repo the connection can read:

```sh
iterion remote runs launch --bot review-pr --repo-url org/app --connection-id conn_… --follow
```

Or launch your own workflow, source uploaded inline: `iterion remote runs launch ./my.bot --var repo=org/app --follow`. The full operator surface (follow, logs, artifacts, merge) is in [cloud-cli.md](cloud-cli.md); the big picture in [cloud-overview.md](cloud-overview.md).

## Undo and troubleshooting

- `iterion remote logout` — forgets the stored cloud credential.
- `claude mcp remove iterion` — unregisters the MCP server.
- The installed skill is inert markdown guidance; remove it with your agent's skill tooling when desired.
- MCP symptoms and fixes: [mcp-server.md](mcp-server.md#troubleshooting).

## No cloud yet? Go local first

The same engine runs entirely on your machine with no account: the six-step [quickstart](quickstart.md) installs the CLI, runs a workflow with zero credentials, and lands a real catalog bot on a local repo. You can add the cloud later with step 3 above.
