# Contributing

Use pinned upstream versions, kind-scoped clusters, and synthetic credentials only. Run `go test ./...`, `bash -n scripts/*.sh legacy/**/*.sh`, and `scripts/check-redaction.sh` before opening a pull request. Never commit tokens, private keys, kubeconfigs, generated binaries, or live endpoint credentials.
