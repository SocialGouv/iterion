#!/bin/sh
# Bootstrap a MinIO bucket for the docker-compose.cloud.yml stack so
# iterion server + runner can PUT artifacts on first boot. Idempotent:
# safe to re-run after `task cloud:up`.
#
# Cloud-ready plan §F (T-38).

set -eu

ENDPOINT="${ITERION_S3_ENDPOINT:-http://minio:9000}"
ACCESS_KEY="${ITERION_S3_ACCESS_KEY_ID:-minioadmin}"
SECRET_KEY="${ITERION_S3_SECRET_ACCESS_KEY:-minioadmin}"
BUCKET="${ITERION_S3_BUCKET:-iterion-artifacts}"

# `mc` is shipped in the minio/mc image used by the init service. Wait
# for the MinIO API to accept connections before issuing any commands;
# the docker-compose healthcheck guards that already, but a couple of
# retries here make the init resilient against slow-starting hosts.
attempt=0
until mc alias set local "$ENDPOINT" "$ACCESS_KEY" "$SECRET_KEY" >/dev/null 2>&1; do
  attempt=$((attempt + 1))
  if [ "$attempt" -ge 30 ]; then
    echo "minio-init: gave up waiting for $ENDPOINT after $attempt attempts" >&2
    exit 1
  fi
  sleep 1
done

if mc ls "local/$BUCKET" >/dev/null 2>&1; then
  echo "minio-init: bucket $BUCKET already exists"
else
  mc mb --quiet "local/$BUCKET"
  echo "minio-init: created bucket $BUCKET"
fi

# Public-read is irrelevant for iterion; we keep the default private
# policy. If a future workflow needs presigned URLs they'll opt in
# explicitly per-key.

echo "minio-init: ready"
