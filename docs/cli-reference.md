[← Documentation index](README.md) · [← Iterion](../README.md)

# CLI Reference

All commands support `--json` for machine-readable output and `--help` for usage details.

## `iterion init`

Scaffold a new project with an example workflow:

```bash
iterion init              # Current directory
iterion init my-project   # New directory
```

Creates `pr_refine_single_model.iter`, `.env.example`, and `.gitignore`. Idempotent — won't overwrite existing files.

## `iterion validate`

Parse, compile, and validate a workflow without running it:

```bash
iterion validate workflow.iter
```

Reports errors and warnings with diagnostic codes (C001–C043), file positions, and descriptions.

## `iterion run`

Execute a workflow:

```bash
iterion run workflow.iter [flags]
```

| Flag | Description |
|------|-------------|
| `--var key=value` | Set workflow variable (repeatable) |
| `--recipe <file>` | Apply a recipe preset (JSON) |
| `--run-id <id>` | Use a specific run ID (default: auto-generated) |
| `--store-dir <dir>` | Run store directory (default: `.iterion`) |
| `--timeout <duration>` | Global timeout (e.g. `30m`, `1h`) |
| `--log-level <level>` | Log verbosity: `error`, `warn`, `info`, `debug`, `trace` |
| `--no-interactive` | Don't prompt on TTY; exit on human pause |
| `--sandbox <mode>` | Sandbox override: `auto` (read `.devcontainer/devcontainer.json`) or `none` (force off). Empty inherits `ITERION_SANDBOX_DEFAULT` then the workflow's `sandbox:` block |
| `--merge-into <target>` | For `worktree: auto` runs — `current` (default), `none` (skip merge, branch only), or a branch name |
| `--branch-name <name>` | For `worktree: auto` runs — override the storage branch name (default `iterion/run/<friendly-name>`) |
| `--merge-strategy <mode>` | For `worktree: auto` runs — `squash` (default, collapses run commits into one) or `merge` (fast-forward, preserves history) |
| `--auto-merge` | For `worktree: auto` runs — apply `--merge-strategy` at run end (default `true` on the CLI; the editor sets `false` and defers the merge to a UI action) |

## `iterion inspect`

View run state and history:

```bash
iterion inspect                          # List all runs
iterion inspect --run-id <id>            # View a specific run
iterion inspect --run-id <id> --events   # Include event log
iterion inspect --run-id <id> --full     # Show full artifact contents
```

## `iterion resume`

Resume a paused workflow run with human answers:

```bash
iterion resume --run-id <id> --file workflow.iter --answer key=value
iterion resume --run-id <id> --file workflow.iter --answers-file answers.json
```

See [resume.md](resume.md) for the full failure / resume matrix.

## `iterion diagram`

Generate a Mermaid diagram from a workflow:

```bash
iterion diagram workflow.iter              # Compact view (default)
iterion diagram workflow.iter --detailed   # Include node properties
iterion diagram workflow.iter --full       # Include templates and loop details
```

Paste the output into any Mermaid-compatible renderer (GitHub Markdown, [Mermaid Live Editor](https://mermaid.live), etc.).

## `iterion report`

Generate a chronological report for a completed run:

```bash
iterion report --run-id <id>
iterion report --run-id <id> --output report.md
```

The report includes:
- **Summary table** — workflow name, status, duration, tokens, cost, model calls
- **Artifacts table** — all published artifacts with versions
- **Timeline** — chronological reconstruction of every node execution, edge selection, verdict, branch lifecycle, and budget warning

## `iterion editor`

Launch the visual workflow editor:

```bash
iterion editor                     # Default port 4891
iterion editor --port 8080         # Custom port
iterion editor --dir ./workflows   # Custom directory
iterion editor --no-browser        # Don't auto-open browser
```

See [visual-editor.md](visual-editor.md) for features.

## `iterion sandbox`

Inspect and configure the iterion sandbox subsystem (see [sandbox.md](sandbox.md)):

```bash
iterion sandbox doctor   # Report the active driver (Docker/Podman), image cache, and capabilities
```

## `iterion server`

Start the long-running HTTP server (editor SPA + run console + cloud API). Used both for the local web editor and for cloud mode deployments — install via [`oci://ghcr.io/socialgouv/charts/iterion`](https://github.com/SocialGouv/iterion/pkgs/container/charts%2Fiterion) (chart sources in [`charts/iterion/`](../charts/iterion/)).

## `iterion runner`

Run a cloud-mode runner pod that consumes workflows from the NATS queue. Configured via `pkg/config/` env vars; deployed by the Helm chart with KEDA-based autoscaling.

## `iterion version`

Print version and commit hash.
