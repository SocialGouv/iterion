#!/bin/sh
# Creates the buckets of the docker-compose.cloud.yml object store
# (SeaweedFS), then reads each back: iterion's, and the one of the S3
# compatibility bench (task cloud:test:s3), which writes and deletes objects
# and so never runs against iterion's. Idempotent: `rclone mkdir` on an
# existing bucket exits 0. Nothing in iterion creates its bucket.
set -eu

for BUCKET in "${ITERION_S3_BUCKET:-iterion-artifacts}" "${ITERION_TEST_S3_BUCKET:-iterion-compat-bench}"; do
  rclone mkdir "s3:${BUCKET}"
  # `rclone size` exits 3 on a bucket that is not there: a real check.
  rclone size "s3:${BUCKET}"
  echo "s3-init: bucket ${BUCKET} ready"
done
