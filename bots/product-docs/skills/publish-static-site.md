---
name: publish-static-site
description: Build the product documentation into a static site (GitBook blocks → MkDocs Material) and package it as a non-root nginx container image pushed to the operator's registry — the platform-agnostic half of Prody's publish tail. The attached deploy-target skill takes the image from there.
---

# publish-static-site — build, package, push

You turn the committed docs tree into ONE container image that serves the
site on port 8080 as a non-root user, and push it to the image repository
the workflow names. Where that image runs is the `deploy-target` skill's
business, not yours: you hand it an image reference, a port and a numeric
user, and it hands back a live URL.

Every step below was validated end-to-end. The non-obvious constraints are
called out — they are the ones that silently cost a pass when ignored.

## Inputs (from the calling workflow)

- `PRODUCT_DIR` — the GitBook-flavoured docs tree (never modified).
- `BASE_URL` — the URL the site will be served at; the site is built for
  `${BASE_URL}/` (root), one deployment per product.
- `IMAGE` — the image repository, no tag (e.g. `ghcr.io/<org>/prody-<product>`).
- `REGISTRY_USER` — the login the token authenticates as; may be empty.
- `SLUG` — the deploy slug the deploy-target skill derives its scope and host
  from; empty means `prody-<product id>`. You only pass it on (§3).
- `TOOLS_REF` — the SocialGouv/iterion ref the converter is fetched from.

## Credentials — by reference only

The registry token is a read-only FILE; you need its PATH, never its
content. **The authoritative path is the one your system prompt renders**
(it appears again in the prompt's "Mounted secret files" list).
`$REGISTRY_TOKEN` is only the SANDBOX's shortcut to that same path: iterion
writes a file secret's `env:` into the container spec, so on a host run
nothing sets it. An unset `$REGISTRY_TOKEN` means "not in a sandbox" — it
never means "no token". Step 2 opens by pinning the path from the variable
when it exists and from your system prompt when it does not, then proving it
readable — so a path problem surfaces as a path problem and never as a 401.

NEVER cat, echo, print or interpolate the file. `$DEPLOY_CREDENTIAL` follows
the same rule and the same fallback, and belongs to the deploy-target skill —
do not touch it here.

## 1. Build

```sh
set -e
SCRATCH=/tmp/iterion-scratch/publish; rm -rf "$SCRATCH"; mkdir -p "$SCRATCH"
curl -fsSL "https://raw.githubusercontent.com/SocialGouv/iterion/${TOOLS_REF}/bots/product-docs/deploy/gitbook_to_mkdocs.py" -o "$SCRATCH/gitbook_to_mkdocs.py"
python3 -m venv "$SCRATCH/venv" && "$SCRATCH/venv/bin/pip" -q install 'mkdocs-material' 'mkdocs>=1.6,<2'
"$SCRATCH/venv/bin/python" "$SCRATCH/gitbook_to_mkdocs.py" --src "$PRODUCT_DIR" --out "$SCRATCH/mk" --site-name "<title from the docs README, fallback: the product id>" --site-url "${BASE_URL}/"
( cd "$SCRATCH/mk" && "$SCRATCH/venv/bin/mkdocs" build --strict )
test -f "$SCRATCH/mk/site/index.html"
```

A converter or build failure is a stop-and-report: the converter fails
closed on constructs it does not know, and that is a finding to surface, not
something to patch around.

## 2. Package — one layer on a public non-root nginx base, no daemon

The sandbox has no container daemon. `crane` (from the bundle's
`devbox.json`) appends the built site as a single layer onto a public base
image and pushes the result — no Dockerfile, no build service.

Base image: `nginxinc/nginx-unprivileged:1.27-alpine` — serves
`/usr/share/nginx/html` on **port 8080** as **uid 101**. Both numbers are
what the deploy manifest must carry (a cluster that enforces `runAsNonRoot`
refuses a root image, and a non-root process cannot bind 80).

`crane auth login` PERSISTS: it writes the token through Docker's config
store, which is `$HOME/.docker/config.json` unless `DOCKER_CONFIG` says
otherwise. Under a sandbox that file dies with the container, but a host run
(`sandbox_skipped`) would leave a reusable `write:packages` token in the
operator's home — outside the scratch directory this bot is allowed to
touch. So point `DOCKER_CONFIG` at scratch BEFORE the login, and delete it
when the push is done. Every `crane` call in this block must run with that
export in scope.

```sh
set -e
: "${SCRATCH:?re-export it (step 1): an empty SCRATCH aims every path below at /}"
REGISTRY_TOKEN_PATH="${REGISTRY_TOKEN:-<the registry_token path from your system prompt>}"
test -r "$REGISTRY_TOKEN_PATH"                    # a path problem surfaces HERE, not as a 401
export DOCKER_CONFIG="$SCRATCH/.docker"           # the login must not escape scratch
rm -rf "$DOCKER_CONFIG"; mkdir -p "$DOCKER_CONFIG"; chmod 700 "$DOCKER_CONFIG"
trap 'rm -rf "$DOCKER_CONFIG"' EXIT               # …even if a step below fails
STAGE="$SCRATCH/layer"; rm -rf "$STAGE"; mkdir -p "$STAGE/usr/share/nginx/html"
cp -R "$SCRATCH/mk/site/." "$STAGE/usr/share/nginx/html/"
tar -C "$STAGE" -cf "$SCRATCH/site-layer.tar" usr
TAG="$(git -C "$PRODUCT_DIR" rev-parse --short=12 HEAD)"   # -C: your cwd is not the repo
REGISTRY_HOST="${IMAGE%%/*}"                       # e.g. ghcr.io
crane auth login "$REGISTRY_HOST" -u "${REGISTRY_USER:-iterion}" --password-stdin < "$REGISTRY_TOKEN_PATH"
crane --platform linux/amd64 append \
  -b nginxinc/nginx-unprivileged:1.27-alpine \
  -f "$SCRATCH/site-layer.tar" \
  -t "${IMAGE}:${TAG}"
DIGEST="$(crane digest "${IMAGE}:${TAG}")"        # the registry's own answer — and the push
IMAGE_REF="${IMAGE}@${DIGEST}"                    # is not done until it can give one
rm -rf "$DOCKER_CONFIG"                            # the token outlives nothing
echo "IMAGE_REF=${IMAGE_REF}"                      # you need to SEE it: it goes in the manifest
```

- ghcr.io accepts any non-empty username with a token; `iterion` is the
  default when the workflow passes none. Other registries may require the
  real account name — that is what `REGISTRY_USER` is for.
- If you run the block in pieces, keep the `DOCKER_CONFIG` export in scope
  for every `crane` call and remove the directory yourself at the end: an
  unscoped login is a credential left behind, not a convenience.
- **Deploy the DIGEST, not the tag.** `${IMAGE}:${TAG}` is a human-readable
  alias and nothing more. The tag is the docs commit, but the image's content
  is the docs *plus* the converter (`TOOLS_REF`) *plus* the floating
  `nginx-unprivileged` base — so an unchanged docs commit can carry different
  bytes under the same tag. Hand a deploy target that tag and the pod spec
  does not change, so nothing rolls out: the cluster keeps serving the image
  it pulled last time, and the workflow's truth gate (a 200 with a title)
  cannot tell a fresh site from a stale one. `${IMAGE}@${DIGEST}` moves
  whenever any of the three do, so the rollout is forced by construction and
  `imagePullPolicy` stops mattering.
- Never `docker`, never `buildx`, never a curl-installed daemon: the
  measured failure mode of a bot that improvises container tooling is a
  live URL that delivers nothing.

## 3. Hand-off to the deploy-target skill

Set, for the deploy skill's manifest:

- `IMAGE="${IMAGE_REF}"` — the DIGEST reference (`${IMAGE}@sha256:…`) the
  registry served, NEVER the `:${TAG}` alias. Report the tag in your summary
  for humans; deploy the digest.
- `PORT=8080`
- `runAsUser: 101` (nginx-unprivileged's numeric user — replace the skill
  manifest's default user, never leave a mismatch)
- `SLUG` = the workflow's deploy slug (`prody-<product id>` when empty)
- readiness probe path `/`

Then follow the deploy-target skill to the letter (namespace model,
manifest, rollout, URL validation) and return `${BASE_URL}/` only once the
rollout is healthy and the URL answers 200 with the site's title.

## Known refusals and their remedies (report, do not retry blindly)

- **`ImagePullBackOff` on a freshly created repository**: a first push to
  ghcr.io creates the package PRIVATE, and the deploy identity cannot create
  image-pull secrets. Report `deployed=false` with the remedy: make the
  package public once (GitHub → organisation → Packages → the package →
  Package settings → Change visibility → Public), then re-run the publish.
- **`401` on `crane auth login`**: the registry token is wrong, expired or
  lacks `write:packages`. Report which registry refused it; never print the
  token. This is a REFUSAL by the registry — do not confuse it with the login
  failing to read its input at all (an unset `$REGISTRY_TOKEN` on a host run,
  caught by the `test -r` above). Reporting "the token lacks `write:packages`"
  when the shell simply could not open a file sends the operator to rotate a
  credential that was never wrong.
- **A base URL the platform will not serve** (the host the deploy skill
  derives from the slug differs from `BASE_URL`): report the two hosts as a
  mismatch instead of returning a URL the workflow's truth gate refuses.
