#!/usr/bin/env bash
set -euo pipefail
if git grep -nE -- ': Bearer eyJ[A-Za-z0-9_-]{20,}|-----BEGIN (RSA |EC |)PRIVATE KEY-----|Txn-Token: [A-Za-z0-9_-]{24,}' -- ':!scripts/check-redaction.sh'; then
  echo 'credential-like material found' >&2
  exit 1
fi
