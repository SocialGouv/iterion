## Minimal Dockerfile that BuildKit (V2-6) materializes for the
## `examples/sandbox_cloud_build.iter` fixture. The marker file is the
## ground truth the workflow's tool node reads back to assert that the
## image was actually built (and not silently substituted).
FROM alpine:3.20

RUN echo "built-by-buildkit" > /etc/iterion-build-marker \
 && adduser -u 1000 -D iterion

USER 1000
WORKDIR /workspace
