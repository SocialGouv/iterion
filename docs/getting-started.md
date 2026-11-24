# Get started with Iterion Cloud in one prompt

Paste one prompt into your AI coding agent and it installs Iterion, connects it to iterion.cloud, checks the connection, then offers to deploy a first agent with you. This is the **cloud onboarding** — for a fully local setup with no account, take the [quickstart](quickstart.md) instead, and add the cloud later with step 3 below.

The prompt is agent-agnostic: Claude Code is documented first, and any MCP-capable agent that can run shell commands works. It ends by proposing a next step — a first bot on one of your repositories, the [walkthrough](#deploy-your-first-agent-on-a-repo) below — which the agent carries out with you.

## Before you start

You only need four things the agent cannot get for itself:

- an account on [iterion.cloud](https://iterion.cloud) — sign up directly or ask your org admin for an invitation (see [cloud-user.md](cloud-user.md))
- a browser, for the one-time login approval in step 3
- an LLM credential, *only if* nothing already funds your account — the instance runs a [shared credential pool](credential-pool.md) and platform credentials that let members test for free while capacity lasts (availability is not guaranteed); otherwise bring a key from **any supported provider** — Anthropic, OpenAI, xAI, ZAI, Moonshot (Kimi), OpenRouter, Bedrock, Vertex, or Azure (see [byok.md](byok.md)). OAuth forfaits (ChatGPT/Codex, Claude) are bound by the operator, not by this prompt. The prompt treats the key as optional.
- an AI coding agent that can run shell commands (Claude Code, Codex CLI, pi, Cursor, …)

## The prompt

The same prompt sits on the [iterion.cloud](https://iterion.cloud) home, with a copy button. Start a fresh session in your agent, paste the whole block, and expect a browser pause for the login approval — and one more if you then connect a forge through OAuth:

```text
Set up Iterion Cloud (https://iterion.cloud) so I can run AI agents on my repositories:

1. Install the CLI: `curl -fsSL https://socialgouv.github.io/iterion/install.sh | sh`
2. Add the .bot authoring skill: `npx skills add https://github.com/SocialGouv/iterion --skill iterion-dsl`
3. Log in: `iterion remote login https://iterion.cloud` — I approve it in my browser.
4. Register the MCP server: `claude mcp add iterion -- iterion mcp` (or the same stdio server in your own MCP config).
5. Check it works: `iterion remote status` and `iterion remote bots list`.

An LLM key is optional — the shared pool or my org may already fund runs. If I want my own, ask me which env var holds it, then run `iterion remote api-keys create --provider <provider> --name default --from-env <VAR>`.

Once it works, propose a next step — such as the `review-pr` bot on one of my repositories (https://socialgouv.github.io/iterion/getting-started.html#deploy-your-first-agent-on-a-repo) — and, once I agree, carry it through with me.
```

## What the agent does

| Step | Command | Details |
|---|---|---|
| Install the CLI | `curl -fsSL https://socialgouv.github.io/iterion/install.sh \| sh` | Downloads a release binary and verifies its sha256. Homebrew, Windows and manual downloads: [install.md](install.md#cli). |
| Install the agent skill | `npx skills add https://github.com/SocialGouv/iterion --skill iterion-dsl` | Teaches the agent to author and debug `.bot` workflows. The skill authors, the MCP server operates — [skill.md](skill.md#iterion-dsl--author-workflows). |
| Connect to iterion.cloud | `iterion remote login https://iterion.cloud` | Browser approval at `/cli-auth`; a personal access token (`iap_…`) is stored in `~/.iterion/cli-auth.json`. Headless alternatives: [cloud-cli.md](cloud-cli.md#setup). |
| Register the MCP server | `claude mcp add iterion -- iterion mcp` | Any MCP client can spawn the same stdio server (`command: iterion`, `args: ["mcp"]`): [mcp-server.md](mcp-server.md#setup). |
| Bind an LLM credential (optional) | `iterion remote api-keys create … --from-env` | Cloud runs spend an LLM credential — any vendor provider, or nothing at all when the shared pool or platform credentials already fund the account. `--provider` takes a wire id (wire ids: `anthropic`, `openai`, `xai`, `zai`, `moonshot`, `openrouter`, `bedrock`, `vertex`, `azure`); `moonshot` is Kimi. `api-keys create` stores a team-wide key, which takes a team admin or owner; a member stores a personal one with `--scope me`, which funds only the runs they own — their launches, and resumes or forks of them, whoever in the team starts those — in the team active when they stored it, never the runs a webhook, a schedule or the board starts. The value travels through the environment, never a command argument: [secrets hygiene](cloud-cli.md#secrets-hygiene), [byok.md](byok.md), [credential-pool.md](credential-pool.md). |
| Verify readiness | `iterion remote status` · `iterion remote bots list` | Both are read-only probes; from the MCP side the same answers come from `remote_status` / `remote_bots_list`. |

The prompt connects no forge and enables no repository on its own: it ends by proposing that step — the next section — and the agent carries it out with you once you agree.

## Deploy your first agent on a repo

One-time setup, then every pull request triggers the bot on its own. Steps 1 and 2 take a team admin or owner; on an org that requires provisioning approval, step 2 can also wait for an org admin.

1. **Connect a forge** (once): in the studio, open `https://iterion.cloud/integrations` and connect GitHub/GitLab/Forgejo (OAuth or a PAT). Headless equivalent — `jq` reads the token from the exported `FORGE_PAT` itself and the body travels on stdin, so the token is on no command line: `jq -n '{mode:"pat",provider:"github",pat:env.FORGE_PAT}' | iterion remote forge connections create --data @-`. The mechanics and the three connection modes: [forge-integrations.md](forge-integrations.md).
2. **Enable a bot on a repo** — one command provisions the webhook, the webhook secret and the managed `forge_token` binding:

   ```sh
   iterion remote forge repo-bots create --data '{"connection_id":"<connection-id>","repo":"org/app","bot_ids":["review-pr"]}'
   ```

3. From now on, opening a PR runs the bot and the result lands back on the forge — a review with inline comments for `review-pr`. Watch the run at `https://iterion.cloud/studio/runs/<id>`.

No webhook needed to try it once — launch by hand against a branch of any repo the connection can read. `review-pr` reviews the branch against `base_ref` (default `main`), so point `--repo-ref` at a branch with changes; `--var pr_url=<PR URL>` also posts the review on that pull request:

```sh
iterion remote runs launch --bot review-pr --repo-url https://github.com/org/app --repo-ref my-branch --connection-id <connection-id> --follow
```

Or launch your own workflow, source uploaded inline: `iterion remote runs launch ./my.bot --var repo=org/app --follow`. The full operator surface (follow, logs, artifacts, merge) is in [cloud-cli.md](cloud-cli.md); the big picture in [cloud-overview.md](cloud-overview.md).

## Undo and troubleshooting

- `iterion remote logout` — forgets the stored cloud credential.
- `claude mcp remove iterion` — unregisters the MCP server.
- The installed skill is inert markdown guidance; remove it with your agent's skill tooling when desired.
- MCP symptoms and fixes: [mcp-server.md](mcp-server.md#troubleshooting).

## No cloud yet? Go local first

The same engine runs entirely on your machine with no account: the six-step [quickstart](quickstart.md) installs the CLI, runs a workflow with zero credentials, and lands a real catalog bot on a local repo. You can add the cloud later with step 3 above.
