# Orka integration: Kontxt

Integration assets and tests for using [Kontxt](https://github.com/aramase/kontxt) as a Transaction Token Service with Orka's vendor-neutral `transaction-token` profile.

## Run the smoke test

Run from the repository root with Docker, Git, kind v0.33.0, kubectl, Helm, curl, jq, and OpenSSL installed. Docker builds the Go binaries and Orka UI.

```bash
export ORKA_REF=main # Use an exact commit for a reproducible run.
./scripts/kind-ci.sh
```

The script checks out the selected Orka source, uses its `manifest_staging/charts/orka` chart, and builds the controller, workspace publisher, and integration helper. It publishes the images to a local registry by digest and installs Orka and Kontxt into an isolated kind cluster with a private kubeconfig. The script removes the cluster and registry when it exits; set `KEEP_CLUSTER=1` to retain them for inspection.

The smoke test checks projected ServiceAccount token exchange, unauthenticated request rejection, allowed requests, scope and namespace denials, and a container Task that completes with safe transaction metadata. It also checks token replacement with narrower scopes, transaction ID continuity, rejection of broader scopes, and downstream token verification. It needs no model credentials. Model execution and Orka's internal `delegate_task` path require separate validation.

See [compatibility and verification status](docs/compatibility.md) for the versions, coverage, and source/image overrides.

## Use a local Orka checkout

With kindctl installed, create a cluster scoped to this repository and run against the local Orka `main` branch:

```bash
kindctl create --tag kontxt-main --k8s-version v1.36.4
kontxt_context="$(kindctl kubectl --tag kontxt-main config current-context)"

ORKA_REPOSITORY="$HOME/projects/orka" \
ORKA_REF=main \
KIND_USE_EXISTING=1 \
KIND_CLUSTER_NAME="${kontxt_context#kind-}" \
kindctl exec --tag kontxt-main -- ./scripts/kind-ci.sh
```

This uses the committed source at the selected ref. Use a fresh cluster; setup refuses existing `orka-system` or `vekil-system` namespaces. Existing-cluster mode preserves the cluster, local registry, and run directory for inspection. Use the same tag for later commands:

```bash
kindctl kubectl --tag kontxt-main get pods -A
kindctl delete --tag kontxt-main
```

After deleting the cluster, also delete the registry container and run directory named in the script's final output. The run directory includes temporary test keys.

For another installation, follow [current provider configuration](docs/provider-notes.md) and the [sample Helm values](manifests/orka/transaction-token-values.yaml). Orka requires `profile: transaction-token` and an exact TTS endpoint ending in `/token_endpoint` for Kontxt.
