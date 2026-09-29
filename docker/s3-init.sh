#!/bin/sh
# Creates the iterion bucket on the docker-compose.cloud.yml object store
# (SeaweedFS), then reads it back. Idempotent: `rclone mkdir` on an
# existing bucket exits 0. Nothing in iterion creates its bucket.
set -eu

BUCKET="${ITERION_S3_BUCKET:-iterion-artifacts}"

rclone mkdir "s3:${BUCKET}"
# `rclone size` exits 3 on a bucket that is not there: a real check.
rclone size "s3:${BUCKET}"
echo "s3-init: bucket ${BUCKET} ready"
