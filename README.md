# Orka integration: Kontxt

Versioned, out-of-tree integration assets for using [Kontxt](https://github.com/aramase/kontxt) as a Transaction Token Service with Orka's vendor-neutral `transaction-token` profile.

## Supported

- Kubernetes projected ServiceAccount and optional OIDC subjects exchanged at Kontxt TTS.
- Strict `Txn-Token` ingress to Orka (`typ: txntoken+jwt`).
- Scope authorization, signed `tctx` constraints, child delegation, safe transaction audit metadata, and outbound transaction-token replacement.
- Kind-based validation against a configurable Orka repository/ref, chart path, or image.

## Not supported

- Provider-specific behavior in Orka core.
- Raw TxTokens in Task specs/status/logs.
- Unscoped child delegation or resource credentials masquerading as transaction tokens.
- Long-lived credentials committed to this repository.

## Quick start

```bash
export ORKA_REF=main                 # or an exact candidate commit
export ORKA_IMAGE=ghcr.io/orka-agents/orka:latest
./scripts/kind-ci.sh
```

The controller must use the exact endpoint configuration:

```yaml
controller:
  contextToken:
    profile: transaction-token
    tts:
      endpoint: http://kontxt-tts.default.svc.cluster.local:8080/token_endpoint
```

See `docs/compatibility.md`, `manifests/orka/transaction-token-values.yaml`, and `legacy/` for the pre-extraction demo/E2E entrypoints retained as migration material.
