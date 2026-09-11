#!/usr/bin/env bash
set -euo pipefail
umask 077

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# shellcheck source=versions.env
source "${repo_root}/versions.env"

if [[ "${1:-}" == --help ]]; then
  cat <<'USAGE'
Build and validate Kontxt against an exact Orka checkout on disposable Kind.

ORKA_REPOSITORY      Git URL or local repository path (default: Orka upstream)
ORKA_REF             Branch, tag, or commit (default: main)
ORKA_CHART_PATH      Matching chart override (default: selected staging chart)
ORKA_IMAGE           Existing controller image; otherwise build selected source
ORKA_PUBLISHER_IMAGE Existing publisher image; otherwise build selected source
KIND_NODE_IMAGE      Kind node image, preferably pinned by digest
KIND_CLUSTER_NAME    Cluster name (default: a unique name for this run)
KIND_USE_EXISTING=1  Use a disposable Kind cluster and its explicit KUBECONFIG
KEEP_CLUSTER=1       Preserve a script-created cluster and registry for inspection

The script removes only clusters and registries it creates. Existing cluster
mode preserves the installation and registry; use it through kindctl exec.
USAGE
  exit 0
fi
[[ $# == 0 ]] || { echo 'error: only --help is supported' >&2; exit 2; }

log() { printf '==> %s\n' "$*" >&2; }
die() { printf 'error: %s\n' "$*" >&2; exit 1; }

for command in docker git helm kind kubectl jq openssl curl; do
  command -v "${command}" >/dev/null || die "missing required command: ${command}"
done
docker info >/dev/null 2>&1 || die 'Docker is not available'
for setting in KIND_USE_EXISTING KEEP_CLUSTER; do
  [[ "${!setting:-0}" == 0 || "${!setting:-0}" == 1 ]] || die "${setting} must be 0 or 1"
done

workdir="$(mktemp -d "${TMPDIR:-/tmp}/orka-kontxt.XXXXXXXX")"
run_id="$(basename "${workdir}" | tr '[:upper:]' '[:lower:]')"
cluster="${KIND_CLUSTER_NAME:-${run_id//./-}}"
namespace=orka-system
cluster_owned=0
cluster_ready=0
registry_attempted=0
registry_owner="${run_id}"

cleanup() {
  local status=$?
  trap - EXIT
  if (( status != 0 )); then
    log "Validation failed (exit ${status})"
    if (( cluster_ready )); then
      kubectl -n "${namespace}" get pods --request-timeout=10s >&2 || true
    fi
  fi
  if [[ "${KEEP_CLUSTER:-0}" == 1 || "${KIND_USE_EXISTING:-0}" == 1 ]]; then
    log "Preserved cluster ${cluster} and run directory ${workdir}"
    log "Scoped kubeconfig: ${KUBECONFIG:-not created}"
    if [[ -n "${ORKA_KIND_REGISTRY_NAME:-}" ]]; then
      log "Local registry: ${ORKA_KIND_REGISTRY_NAME} (owner ${registry_owner})"
    fi
  else
    if (( cluster_owned )); then
      kind delete cluster --name "${cluster}" || status=1
    fi
    if (( registry_attempted )); then
      orka_kind_registry_stop "${cluster}" "${registry_owner}" || status=1
    fi
    rm -rf "${workdir}"
  fi
  exit "${status}"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

log "Checking out Orka ref ${ORKA_REF}"
git clone --quiet --filter=blob:none --no-checkout -- "${ORKA_REPOSITORY}" "${workdir}/orka"
orka_source="${workdir}/orka"
if ! orka_commit="$(git -C "${orka_source}" rev-parse --verify --end-of-options "${ORKA_REF}^{commit}" 2>/dev/null)"; then
  orka_commit="$(git -C "${orka_source}" rev-parse --verify --end-of-options "origin/${ORKA_REF}^{commit}")"
fi
git -C "${orka_source}" checkout --quiet --detach "${orka_commit}"
chart="${ORKA_CHART_PATH:-${orka_source}/manifest_staging/charts/orka}"
[[ -f "${chart}/Chart.yaml" ]] || die "matching staging chart not found: ${chart}"
[[ -f "${orka_source}/scripts/lib/kind-local-registry.sh" ]] || die 'selected Orka ref lacks the Kind registry helper'
# Reuse the selected Orka revision's digest publishing and ownership checks.
# shellcheck disable=SC1091
source "${orka_source}/scripts/lib/kind-local-registry.sh"
log "Orka commit ${orka_commit}; chart ${chart}"

if [[ "${KIND_USE_EXISTING:-0}" == 1 ]]; then
  [[ -n "${KIND_CLUSTER_NAME:-}" ]] || die 'KIND_USE_EXISTING requires KIND_CLUSTER_NAME'
  [[ -n "${KUBECONFIG:-}" && "${KUBECONFIG}" != *:* && -f "${KUBECONFIG}" ]] || \
    die 'KIND_USE_EXISTING requires one explicit, existing scoped KUBECONFIG'
  [[ "${KUBECONFIG}" != "${HOME}/.kube/config" && ! "${KUBECONFIG}" -ef "${HOME}/.kube/config" ]] || \
    die 'KIND_USE_EXISTING refuses the global kubeconfig; use a scoped KUBECONFIG'
  [[ "$(kubectl config current-context)" == "kind-${cluster}" ]] || die 'KUBECONFIG does not select the requested Kind cluster'
  kind get nodes --name "${cluster}" | awk 'END { exit (NR == 0) }' || die 'requested Kind cluster has no nodes'
else
  existing_clusters="$(kind get clusters)"
  if awk -v target="${cluster}" '$0 == target { found=1 } END { exit !found }' <<<"${existing_clusters}"; then
    die "cluster ${cluster} already exists; refusing to replace or delete it"
  fi
  export KUBECONFIG="${workdir}/kubeconfig"
  cluster_owned=1
  kind create cluster --name "${cluster}" --kubeconfig "${KUBECONFIG}" --image "${KIND_NODE_IMAGE}" --wait 120s
fi
cluster_ready=1

for required_namespace in "${namespace}" vekil-system; do
  existing_namespace="$(kubectl get namespace "${required_namespace}" --ignore-not-found -o name)"
  [[ -z "${existing_namespace}" ]] || die "namespace ${required_namespace} already exists"
done
registry_attempted=1
orka_kind_registry_start "${cluster}" "${registry_owner}"

build_image() {
  local image="$1" dockerfile="$2" context="$3"
  docker build --tag "${image}" --file "${dockerfile}" "${context}"
}
ensure_image() {
  docker image inspect "$1" >/dev/null 2>&1 || docker pull "$1"
}

controller_image="${ORKA_IMAGE:-orka-kontxt-controller:${run_id}}"
publisher_image="${ORKA_PUBLISHER_IMAGE:-orka-kontxt-publisher:${run_id}}"
helper_image="orka-kontxt-helper:${run_id}"
if [[ -z "${ORKA_IMAGE:-}" ]]; then
  log 'Building the selected Orka controller source'
  build_image "${controller_image}" "${orka_source}/Dockerfile" "${orka_source}"
else
  ensure_image "${controller_image}"
fi
if [[ -z "${ORKA_PUBLISHER_IMAGE:-}" ]]; then
  log 'Building the selected Orka publisher source'
  build_image "${publisher_image}" "${orka_source}/workers/publisher/Dockerfile" "${orka_source}"
else
  ensure_image "${publisher_image}"
fi
log 'Building the Kontxt service and smoke client'
build_image "${helper_image}" "${repo_root}/cmd/live-kontxt-e2e/Dockerfile" "${repo_root}"
controller_ref="$(orka_kind_registry_push "${controller_image}" orka/controller)"
publisher_ref="$(orka_kind_registry_push "${publisher_image}" orka/publisher)"
helper_ref="$(orka_kind_registry_push "${helper_image}" kontxt/helper)"
log "Controller ${controller_ref}"
log "Publisher ${publisher_ref}"
log "Kontxt helper ${helper_ref}"

kubectl create namespace "${namespace}"
kubectl label namespace "${namespace}" orka.ai/controller-mode=harness-v2
# The chart installs its provider-proxy ingress policy in this namespace.
kubectl create namespace vekil-system
openssl rand 32 >"${workdir}/snapshot.key"
kubectl -n "${namespace}" create secret generic orka-agent-snapshot-key --from-file="key=${workdir}/snapshot.key"
openssl req -x509 -newkey rsa:2048 -nodes -days 1 \
  -keyout "${workdir}/webhook.key" -out "${workdir}/webhook.crt" \
  -subj "/CN=orka-webhook.${namespace}.svc" \
  -addext "subjectAltName=DNS:orka-webhook.${namespace}.svc,DNS:orka-webhook.${namespace}.svc.cluster.local" \
  2>"${workdir}/openssl.log"
kubectl -n "${namespace}" create secret generic orka-webhook-tls --type=kubernetes.io/tls \
  --from-file="tls.crt=${workdir}/webhook.crt" --from-file="tls.key=${workdir}/webhook.key" --from-file="ca.crt=${workdir}/webhook.crt"

jq -n --arg controller "${controller_ref%@*}" --arg controllerDigest "${controller_ref##*@}" \
  --arg publisher "${publisher_ref%@*}" --arg publisherDigest "${publisher_ref##*@}" \
  --arg ca "$(openssl base64 -A <"${workdir}/webhook.crt")" --arg namespace "${namespace}" '
  {
    controller: {
      mode: "harness-v2", watchNamespace: $namespace,
      image: {repository: $controller, digest: $controllerDigest},
      agentExecutionSnapshot: {existingSecret: "orka-agent-snapshot-key", key: "key"}
    },
    publisher: {image: {repository: $publisher, digest: $publisherDigest}},
    webhooks: {tls: {existingSecret: "orka-webhook-tls"}, caBundle: $ca},
    providerProxy: {enabled: true}
  }' >"${workdir}/runtime-values.json"

log 'Installing Kontxt with Kubernetes issuer trust'
kubernetes_issuer="$(kubectl get --raw /.well-known/openid-configuration | jq -er '.issuer')"
kubectl -n "${namespace}" apply -f "${repo_root}/manifests/kontxt/resources.yaml"
kubectl set image --local -f "${repo_root}/manifests/kontxt/deployment.yaml" \
  "tts=${helper_ref}" "downstream=${helper_ref}" -o yaml |
  kubectl set env --local -f - --containers=tts "KUBERNETES_ISSUER=${kubernetes_issuer}" -o yaml |
  kubectl -n "${namespace}" apply -f -
kubectl -n "${namespace}" rollout status deployment/kontxt-tts --timeout=3m

log 'Installing the current Orka chart with transaction-token authentication'
helm install orka "${chart}" --namespace "${namespace}" \
  --values "${workdir}/runtime-values.json" \
  --values "${repo_root}/manifests/orka/transaction-token-values.yaml" \
  --wait --timeout 10m

log 'Running projected ServiceAccount, authorization, replacement, and Task checks'
kubectl set image --local -f "${repo_root}/manifests/kontxt/smoke-job.yaml" "smoke=${helper_ref}" -o yaml |
  kubectl set env --local -f - "TASK_IMAGE=${helper_ref}" -o yaml |
  kubectl -n "${namespace}" apply -f -
deadline=$((SECONDS + 360))
while :; do
  job="$(kubectl -n "${namespace}" get job kontxt-smoke -o json)"
  if jq -e 'any(.status.conditions[]?; .type == "Complete" and .status == "True")' <<<"${job}" >/dev/null; then
    kubectl -n "${namespace}" logs job/kontxt-smoke
    break
  fi
  if jq -e 'any(.status.conditions[]?; (.type == "Failed" or .type == "FailureTarget") and .status == "True")' <<<"${job}" >/dev/null; then
    kubectl -n "${namespace}" logs job/kontxt-smoke >&2 || true
    die 'Kontxt smoke Job failed'
  fi
  (( SECONDS < deadline )) || die 'timed out waiting for the Kontxt smoke Job'
  sleep 2
done
log "Kontxt compatibility passed against Orka ${orka_commit}"
