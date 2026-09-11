# Compatibility and verification

Compatibility applies to an Orka source revision, chart, and images tested together. `main` moves; use an exact commit in `ORKA_REF` when reproducing a result.

| Integration | Orka target | Kontxt | Cluster | Verification status |
|---|---|---|---|---|
| This revision | `main` at `55cb3d5232b4a9b697e72471e346c0a6493d4c21` | v0.0.1 | kind v0.33.0, Kubernetes v1.36.4 | Live smoke passed on 2026-09-11 |

The complete runner passed on a fresh ARM64 kind cluster with Helm v3.22.0. It built the controller and publisher from the local Orka checkout at the commit above and used no model credentials.

The pins are in [versions.env](../versions.env). The [smoke test](../scripts/kind-ci.sh) uses `manifest_staging/charts/orka` from the selected Orka checkout and builds its controller and workspace publisher by default.

## Smoke coverage

The smoke test exercises:

- A projected Kubernetes ServiceAccount token exchanged at Kontxt TTS for a TxToken.
- Orka API responses of `401` for an unauthenticated request, `200` for an authorized task list, and `403` for missing scope or a signed namespace constraint violation.
- Creation, retrieval, and successful completion of a container Task with transaction metadata and checks against raw token leakage.
- Token replacement with narrower scopes, the same transaction ID, rejection of broader scopes, and downstream signature/claim verification.

It does not validate model execution, GitHub Actions OIDC, Orka's internal `delegate_task`, or worker-initiated outbound replacement. Those paths need their own live tests before claiming compatibility.

## Select the source and images

| Variable | Default and behavior |
|---|---|
| `ORKA_REPOSITORY` | `https://github.com/orka-agents/orka.git`; a local repository path is also accepted. |
| `ORKA_REF` | `main`; selects the committed branch, tag, or commit to check out. Local uncommitted changes are excluded. |
| `ORKA_CHART_PATH` | Optional chart override; otherwise uses `manifest_staging/charts/orka` from the selected source. An override must match that source revision. |
| `ORKA_IMAGE` | Optional existing local or pullable controller image; otherwise builds from the selected source. |
| `ORKA_PUBLISHER_IMAGE` | Optional existing local or pullable workspace publisher image; otherwise builds from the selected source. |
| `KIND_USE_EXISTING` | Set to `1` to use the cluster named by `KIND_CLUSTER_NAME` with the supplied private `KUBECONFIG`. Preserves the cluster, registry, and run directory. |
| `KIND_CLUSTER_NAME` | Optional cluster name; required when using an existing cluster. |
| `KEEP_CLUSTER` | Set to `1` to keep a script-created cluster, registry, and run directory after the run. |

Controller and publisher image overrides must be compatible with the selected chart and source. An image tag alone does not establish that compatibility. See the [local checkout example](../README.md#use-a-local-orka-checkout) for kindctl usage and cleanup.
