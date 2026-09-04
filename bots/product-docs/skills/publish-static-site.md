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
- `TOOLS_REF` — the SocialGouv/iterion ref the converter is fetched from.

## Credentials — by reference only

`$REGISTRY_TOKEN` is the PATH of a read-only file holding the registry
token. NEVER cat, echo, print or interpolate it; the login below reads it on
stdin. `$DEPLOY_CREDENTIAL` belongs to the deploy-target skill — do not touch
it here.

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

```sh
set -e
STAGE="$SCRATCH/layer"; rm -rf "$STAGE"; mkdir -p "$STAGE/usr/share/nginx/html"
cp -R "$SCRATCH/mk/site/." "$STAGE/usr/share/nginx/html/"
tar -C "$STAGE" -cf "$SCRATCH/site-layer.tar" usr
TAG="$(git rev-parse --short=12 HEAD)"            # the docs commit the site was built from
REGISTRY_HOST="${IMAGE%%/*}"                       # e.g. ghcr.io
crane auth login "$REGISTRY_HOST" -u "${REGISTRY_USER:-iterion}" --password-stdin < "$REGISTRY_TOKEN"
crane --platform linux/amd64 append \
  -b nginxinc/nginx-unprivileged:1.27-alpine \
  -f "$SCRATCH/site-layer.tar" \
  -t "${IMAGE}:${TAG}"
crane manifest "${IMAGE}:${TAG}" >/dev/null       # the push is not done until the registry serves it
```

- ghcr.io accepts any non-empty username with a token; `iterion` is the
  default when the workflow passes none. Other registries may require the
  real account name — that is what `REGISTRY_USER` is for.
- The tag is the docs commit: the same content always yields the same
  reference, and a redeploy of an unchanged corpus is a no-op rollout.
- Never `docker`, never `buildx`, never a curl-installed daemon: the
  measured failure mode of a bot that improvises container tooling is a
  live URL that delivers nothing.

## 3. Hand-off to the deploy-target skill

Set, for the deploy skill's manifest:

- `IMAGE="${IMAGE}:${TAG}"` (the exact reference `crane manifest` served)
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
  token.
- **A base URL the platform will not serve** (the host the deploy skill
  derives from the slug differs from `BASE_URL`): report the two hosts as a
  mismatch instead of returning a URL the workflow's truth gate refuses.
