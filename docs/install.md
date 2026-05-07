[← Documentation index](README.md) · [← Iterion](../README.md)

# Install

Iterion ships in six delivery flavours. Pick the one that fits your workflow — the DSL, runtime, persistence and observability are identical across all of them.

| Mode | Best for | Install |
|---|---|---|
| 🖥️ [**CLI**](#cli) | Scripted runs, CI/CD pipelines, quick iteration | `curl -fsSL https://socialgouv.github.io/iterion/install.sh \| sh` |
| 🌐 [**Web editor**](visual-editor.md) | Visual workflow design on your dev machine | Bundled with the CLI: `iterion editor` |
| 🪟 [**Desktop app**](desktop.md) | Native window with multi-project, OS keychain, auto-update | Download `iterion-desktop` from [Releases](https://github.com/SocialGouv/iterion/releases/latest) |
| 🐳 [**Docker**](#docker) | Zero-install runs, reproducible CI, isolated environments | `docker run --rm ghcr.io/socialgouv/iterion:latest` |
| ☁️ [**Cloud / server**](cloud.md) | Multi-tenant deployment, shared run store, REST/WS API | `helm install iterion oci://ghcr.io/socialgouv/charts/iterion` |
| 📦 [**TypeScript SDK**](../sdks/typescript/) | Programmatic invocation from Node/Deno/Bun apps | `npm install @iterion/sdk` |

All six invoke the same Go core. Pick CLI for automation, the web editor for daily editing, the desktop app if you want a one-click install with managed credentials, Docker when you want to run iterion without installing it on the host, cloud mode when teams need a shared always-on instance, or the SDK to embed iterion inside another Node/Deno/Bun application.

---

## CLI

```bash
curl -fsSL https://socialgouv.github.io/iterion/install.sh | sh
```

Or install to a custom directory (no sudo needed):

```bash
INSTALL_DIR=. curl -fsSL https://socialgouv.github.io/iterion/install.sh | sh
```

### Homebrew (macOS, Linux)

```bash
brew tap socialgouv/iterion https://github.com/SocialGouv/iterion
brew install iterion
# Desktop app (macOS only):
brew install --cask iterion-desktop
```

Updates: `brew upgrade iterion` (and/or `brew upgrade --cask iterion-desktop`).

### Windows (PowerShell)

```powershell
Invoke-WebRequest -Uri "https://github.com/socialgouv/iterion/releases/latest/download/iterion-windows-amd64.exe" -OutFile iterion.exe
```

### Manual download

You can also download binaries from the [latest release](https://github.com/socialgouv/iterion/releases/latest). Builds are available for Linux, macOS (Intel + Apple Silicon), and Windows.

> **Want a native window instead of a CLI + browser?** See [Desktop App](desktop.md) — it ships an installable `.AppImage` / `.app` / `.exe` with the editor pre-wired, OS-keychain credentials and auto-update.

---

## Docker

The published image (`ghcr.io/socialgouv/iterion:latest`) bundles the `iterion` binary, `git`, Node 22 and the pinned `claude` / `codex` CLIs. Override the default `server` command for ad-hoc runs:

```bash
# One-off workflow run, mounting your project at /work
docker run --rm \
  -v "$PWD:/work" -w /work \
  -e ANTHROPIC_API_KEY \
  ghcr.io/socialgouv/iterion:latest \
  run workflow.iter --var pr_title="..."

# Editor/server on http://localhost:4891
docker run --rm -p 4891:4891 -v "$PWD:/work" -w /work \
  -e ANTHROPIC_API_KEY \
  ghcr.io/socialgouv/iterion:latest
```

The image is built by `.github/workflows/image.yml` on every main push and tag, and scanned by `.github/workflows/trivy.yml` post-build and weekly (non-blocking, findings land in the repo Security tab).

---

## Cloud (Helm)

```bash
helm upgrade --install iterion \
  oci://ghcr.io/socialgouv/charts/iterion \
  --version <semver> \
  -f values.yaml
```

Pick a `--version` from the [iterion releases](https://github.com/SocialGouv/iterion/releases). The chart bundles server + runner Deployments, KEDA-based runner autoscaling on queue depth, and optional sandbox RBAC for per-run pods.

For the full operator runbook (secrets, NetworkPolicy, observability, resume, migration), see [cloud-deployment.md](cloud-deployment.md). For a quick architectural overview, see [cloud.md](cloud.md).

---

## TypeScript SDK

```bash
npm install @iterion/sdk
```

`@iterion/sdk` wraps the CLI with typed `run` / `resume` / `events` streaming for Node, Deno, and Bun apps. See [`sdks/typescript/`](../sdks/typescript/) for usage and API reference.
