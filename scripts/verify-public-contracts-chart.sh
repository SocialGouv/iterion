#!/usr/bin/env bash
set -euo pipefail

chart_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/charts/iterion"
render_dir="$(mktemp -d)"
trap 'rm -rf "$render_dir"' EXIT

helm template normal "$chart_dir" --namespace ordinary --set image.tag=edge > "$render_dir/normal.yaml"
if grep -Fq 'ITERION_CONTRACTS_DISTRIBUTED_SYSTEM_NATS_URL' "$render_dir/normal.yaml"; then
  echo 'default chart exposed privileged system NATS configuration' >&2
  exit 1
fi

helm template trusted "$chart_dir" --namespace trusted --set image.tag=edge \
  --set serviceAccount.name=trusted-sa --set runner.enabled=false \
  --set server.distributedAuthority.enabled=true \
  --set server.distributedAuthority.systemNatsSecretName=system-nats \
  --set server.distributedAuthority.authorityRef=trusted/authority \
  '--set=server.distributedAuthority.kubernetesNamespaces[0]=trusted' \
  '--set=server.distributedAuthority.kubernetesNamespaces[1]=workers' \
  --set-string config.nats.url=nats://shared-broker:4222 \
  --set-string config.mongo.uri=mongodb://shared-store:27017 > "$render_dir/trusted.yaml"
grep -Fq 'name: "system-nats"' "$render_dir/trusted.yaml"
grep -Fq 'value: "trusted/authority"' "$render_dir/trusted.yaml"
grep -Fq 'value: "trusted,workers"' "$render_dir/trusted.yaml"
grep -Fq 'serviceAccountName: trusted-sa' "$render_dir/trusted.yaml"
if grep -Eq '^kind: (Role|RoleBinding)$' "$render_dir/trusted.yaml"; then
  echo 'trusted server release rendered sandbox RBAC' >&2
  exit 1
fi
if [ "$(grep -Fc 'ITERION_CONTRACTS_DISTRIBUTED_SYSTEM_NATS_URL' "$render_dir/trusted.yaml")" -ne 1 ]; then
  echo 'system NATS URL was not wired exactly once to the server' >&2
  exit 1
fi

helm template workers "$chart_dir" --namespace workers --set image.tag=edge \
  --set serviceAccount.name=worker-sa --set server.replicas=0 \
  --set server.hpa.enabled=false --set runner.enabled=true \
  --set-string config.nats.url=nats://shared-broker:4222 \
  --set-string config.mongo.uri=mongodb://shared-store:27017 > "$render_dir/workers.yaml"
grep -Fq 'serviceAccountName: worker-sa' "$render_dir/workers.yaml"
grep -Fq 'replicas: 0' "$render_dir/workers.yaml"
if grep -Fq 'ITERION_CONTRACTS_DISTRIBUTED_SYSTEM_NATS_URL' "$render_dir/workers.yaml"; then
  echo 'worker release received the system NATS URL' >&2
  exit 1
fi
if grep -Fq 'kind: HorizontalPodAutoscaler' "$render_dir/workers.yaml"; then
  echo 'worker release rendered a dormant server HPA' >&2
  exit 1
fi
if helm template unsafe "$chart_dir" --set image.tag=edge \
  --set server.distributedAuthority.enabled=true \
  --set server.distributedAuthority.systemNatsSecretName=system-nats \
  --set server.distributedAuthority.authorityRef=trusted/authority \
  '--set=server.distributedAuthority.kubernetesNamespaces[0]=trusted' >/dev/null 2>&1; then
  echo 'shared server/runner release accepted privileged authority wiring' >&2
  exit 1
fi

echo 'public-contract authority chart profiles passed'
