#!/bin/sh
# Serve-side companion of the `deploy-onyxia-sspcloud` skill.
#
# Runs as the Onyxia personal init script of a long-lived datalab service
# (any interactive chart works — vscode-python is the tested one). It keeps a
# local mirror of s3://$SITES_BUCKET/$SITES_PREFIX/ and serves it over HTTP on
# the chart's user port, so the service's ingress URL publishes every site
# pushed under that prefix:
#
#   s3://<bucket>/<prefix>/<site>/index.html  →  https://<service-url>/<site>/
#
# The bot side (the skill) only ever pushes to S3 — it never talks to Onyxia.
#
# No secrets in this file: S3 credentials are the AWS_* env vars Onyxia
# injects into every service. They are STS tokens that expire (~7 days), so
# the sync loop logs — but keeps serving the last good mirror — once they do;
# relaunching the service re-injects fresh ones.
set -u

SITES_BUCKET="${SITES_BUCKET:-devthejo}"
SITES_PREFIX="${SITES_PREFIX:-sites}"
SERVE_PORT="${SERVE_PORT:-5000}"
SYNC_INTERVAL="${SYNC_INTERVAL:-60}"
SITES_DIR="${SITES_DIR:-$HOME/sites}"

ENDPOINT_HOST="${AWS_S3_ENDPOINT:-minio.lab.sspcloud.fr}"
# MC_HOST_* carries the STS session token, which `mc alias set` cannot.
export MC_HOST_sites="https://${AWS_ACCESS_KEY_ID}:${AWS_SECRET_ACCESS_KEY}:${AWS_SESSION_TOKEN}@${ENDPOINT_HOST}"

# mc ships in the Onyxia images; install it only when absent (plain images).
if ! command -v mc >/dev/null 2>&1; then
  curl -fsSLo /tmp/mc https://dl.min.io/client/mc/release/linux-amd64/mc
  chmod +x /tmp/mc
  mkdir -p "$HOME/.local/bin" && mv /tmp/mc "$HOME/.local/bin/mc"
  export PATH="$HOME/.local/bin:$PATH"
fi

mkdir -p "$SITES_DIR"

# Initial mirror is synchronous so the first HTTP response already has content.
mc mirror --remove --overwrite --quiet "sites/${SITES_BUCKET}/${SITES_PREFIX}" "$SITES_DIR" \
  || echo "onyxia-serve: initial mirror failed — serving empty dir" >&2

nohup sh -c "while :; do
  mc mirror --remove --overwrite --quiet 'sites/${SITES_BUCKET}/${SITES_PREFIX}' '$SITES_DIR' \
    || echo \"onyxia-serve: sync failed at \$(date -u +%FT%TZ) — credentials expired?\"
  sleep '${SYNC_INTERVAL}'
done" >"$HOME/sites-sync.log" 2>&1 &

nohup python3 -m http.server "$SERVE_PORT" --directory "$SITES_DIR" \
  >"$HOME/sites-http.log" 2>&1 &

echo "onyxia-serve: mirroring s3://${SITES_BUCKET}/${SITES_PREFIX} → ${SITES_DIR}, serving on :${SERVE_PORT}"
