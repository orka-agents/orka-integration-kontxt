#!/usr/bin/env bash
set -euo pipefail
repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# shellcheck disable=SC1091
source "${repo_root}/versions.env"
ORKA_REPOSITORY="${ORKA_REPOSITORY:-https://github.com/orka-agents/orka.git}"
ORKA_REF="${ORKA_REF:-main}"
ORKA_IMAGE="${ORKA_IMAGE:-ghcr.io/orka-agents/orka:latest}"
cluster="${KIND_CLUSTER_NAME:-orka-kontxt-integration}"
workdir="$(mktemp -d)"
trap 'kind delete cluster --name "${cluster}" >/dev/null 2>&1 || true; rm -rf "${workdir}"' EXIT
kind create cluster --name "${cluster}" --wait 120s
if [[ -n "${ORKA_CHART_PATH:-}" ]]; then
  chart="${ORKA_CHART_PATH}"
else
  git clone --filter=blob:none "${ORKA_REPOSITORY}" "${workdir}/orka"
  git -C "${workdir}/orka" checkout --detach "${ORKA_REF}"
  chart="${workdir}/orka/charts/orka"
fi
helm upgrade --install orka "${chart}" --namespace orka-system --create-namespace \
  --set controller.image.repository="${ORKA_IMAGE%:*}" \
  --set controller.image.tag="${ORKA_IMAGE##*:}" \
  --wait --timeout 5m
kubectl -n orka-system rollout status deployment/orka --timeout=5m 2>/dev/null || kubectl -n orka-system get deployments
kubectl apply --server-side --dry-run=server -f "${repo_root}/manifests/orka/transaction-token-values.yaml" >/dev/null 2>&1 || true
