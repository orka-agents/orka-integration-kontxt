# Archived Kontxt quickstart

The original quickstart targeted Orka's pre-extraction `kontxt` profile and in-tree demo helpers. Those instructions do not run against current Orka. Use the [current smoke test](../README.md#run-the-smoke-test) and [provider configuration](provider-notes.md).

When migrating an older setup:

| Historical setting or assumption | Current equivalent |
|---|---|
| `ORKA_CONTEXT_TOKEN_PROFILE=kontxt` | `ORKA_CONTEXT_TOKEN_PROFILE=transaction-token` |
| `ORKA_CONTEXT_TOKEN_TTS_URL` containing a base URL | `ORKA_CONTEXT_TOKEN_TTS_ENDPOINT` containing the full Kontxt `/token_endpoint` URL |
| `controller.contextToken.tts.url` | `controller.contextToken.tts.endpoint` |
| Expected Task or worker profile of `kontxt` | `transaction-token`, including metadata assertions and `ORKA_TRANSACTION_PROFILE` checks |
| In-tree Kontxt installation and E2E helpers | This repository's `scripts/kind-ci.sh` and integration helper |
| Released chart assumed to match Orka `main` | `manifest_staging/charts/orka` from the selected source revision |

The old profile and TTS URL settings have no compatibility aliases. The [archived scripts](../legacy/README.md) also retain missing files and old relative paths; changing the profile alone will not make them runnable.
