# Configure Kontxt for current Orka

Kontxt validates the caller's configured subject identity and issues a transaction token. Orka validates that TxToken using the vendor-neutral `transaction-token` profile. For a complete local setup, run the [smoke test](../README.md#run-the-smoke-test).

## Ingress verification and authorization

Configure the controller with the issuer, audience, and JWKS endpoint used by your Kontxt installation:

```bash
ORKA_CONTEXT_TOKEN_PROFILE=transaction-token
ORKA_CONTEXT_TOKEN_ISSUER=https://kontxt-tts.example.test
ORKA_CONTEXT_TOKEN_AUDIENCE=orka-api
ORKA_CONTEXT_TOKEN_JWKS_URL=https://kontxt-tts.example.test/.well-known/jwks.json
ORKA_CONTEXT_TOKEN_AUTHZ_MODE=enforce
```

Pass the raw TxToken in `Txn-Token`. Orka requires an RS256 JWT with `typ: txntoken+jwt`, matching issuer and audience, valid time claims, and the required transaction claims. Bearer-token support is opt-in through `ORKA_CONTEXT_TOKEN_HEADERS=Txn-Token,Authorization:Bearer`; the default leaves `Authorization: Bearer` available for ServiceAccount and OIDC authentication.

`enforce` rejects requests that lack required scopes or violate signed `tctx` constraints. `audit` records the authorization decision while allowing the request, and `off` disables these authorization checks. Token verification still applies in each mode.

Common default scopes are:

| Operation | Scope |
|---|---|
| Create a Task | `orka:tasks:create` |
| Get a Task or its result | `orka:tasks:get` |
| List Tasks | `orka:tasks:list` |
| Delete a Task | `orka:tasks:delete` |
| Read a Tool definition | `orka:tools:read` |
| Use an Orka-managed Tool | `orka:tools:use` |
| Use a model provider | `orka:providers:use` |

Use the matching `ORKA_CONTEXT_TOKEN_*_SCOPES` environment variable or `--context-token-*-scopes` flag to configure scope aliases. Kontxt policy must allow the requested scope and sign the intended constraints. For example, a token with `tctx.namespace=default` cannot authorize a request targeting another namespace in `enforce` mode.

## TTS replacement and delegation

For Orka-side replacement, configure the exact OAuth endpoint:

```bash
ORKA_CONTEXT_TOKEN_TTS_ENDPOINT=https://kontxt-tts.example.test/token_endpoint
ORKA_CONTEXT_TOKEN_TTS_AUDIENCE=orka-api
ORKA_CONTEXT_TOKEN_TTS_TOKEN_SOURCE=incoming
```

The equivalent Helm values are:

```yaml
controller:
  contextToken:
    profile: transaction-token
    issuer: https://kontxt-tts.example.test
    audience: orka-api
    jwksUrl: https://kontxt-tts.example.test/.well-known/jwks.json
    authzMode: enforce
    tts:
      endpoint: https://kontxt-tts.example.test/token_endpoint
      audience: orka-api
      tokenSource: incoming
```

Orka sends requests to the configured endpoint without appending a path. The former `ORKA_CONTEXT_TOKEN_TTS_URL`, `--context-token-tts-url`, and `controller.contextToken.tts.url` settings have no compatibility aliases.

Configure requested child and outbound scopes through `ORKA_CONTEXT_TOKEN_CHILD_SCOPE` and `ORKA_CONTEXT_TOKEN_OUTBOUND_SCOPE`. Child scopes must be a subset of the parent transaction scopes. Orka stores delegated raw tokens in owner-referenced Secrets; Tasks contain the Secret reference and safe transaction metadata. Replacement uses RFC 8693 and requires the response to contain the transaction `issued_token_type` and `token_type=N_A`.

The smoke test checks replacement against Kontxt directly and verifies the resulting token downstream. It does not exercise Orka's internal `delegate_task` or worker-initiated replacement paths.

## Identity trust and metadata

For projected Kubernetes ServiceAccount subjects, configure Kontxt to trust the cluster's issuer/JWKS and the token's audience. Orka then validates the resulting TxToken. Other OIDC subjects require their own Kontxt issuer and policy configuration; the smoke test covers Kubernetes ServiceAccount subjects.

The disposable kind setup mounts the cluster CA into Kontxt and grants unauthenticated reads of Kubernetes' issuer discovery document and public JWKS through `system:service-account-issuer-discovery`. This lets Kontxt fetch the signing keys directly from Kubernetes. See [Kubernetes' issuer discovery guidance](https://kubernetes.io/docs/tasks/configure-pod-container/configure-service-account/#service-account-issuer-discovery) when configuring another cluster.

Tasks created through the authenticated REST API receive verified `spec.requestedBy` and `spec.transaction` fields. Transaction metadata uses `profile: transaction-token`, a transaction ID, and safe context digests. The controller propagates the metadata to container Jobs, Pods, and worker environment variables such as `ORKA_TRANSACTION_ID` and `ORKA_TRANSACTION_PROFILE`. Raw TxTokens must stay out of Task specs/status and logs.

See [compatibility and verification status](compatibility.md) and the [archived configuration migration](legacy-quickstart.md).
