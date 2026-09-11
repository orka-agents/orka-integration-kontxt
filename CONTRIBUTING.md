# Contributing

Keep provider installation and policy in this repository, using Orka's generic `transaction-token` contract. Update version pins in [versions.env](versions.env) and record verification against an exact Orka commit in [docs/compatibility.md](docs/compatibility.md).

With Go matching [go.mod](go.mod) installed, run these checks before submitting a change:

```bash
go test ./...
for kontxt_script in scripts/*.sh; do
  bash -n "$kontxt_script"
done
./scripts/check-redaction.sh
```

For changes to deployment or token behavior, also run the [kind smoke test](README.md#run-the-smoke-test). Use a private kubeconfig or the documented kindctl workflow. A passing fixture test alone does not establish compatibility with a deployed controller.

Use synthetic test identities. Never commit tokens, private keys, kubeconfigs, generated binaries, or live endpoint credentials.
