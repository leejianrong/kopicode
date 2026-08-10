#!/usr/bin/env bash
# Install kopicode's git hooks. Idempotent.
set -euo pipefail

root="$(git rev-parse --show-toplevel)"
hooks="$(git rev-parse --git-path hooks)"

install -m 0755 "$root/scripts/pre-push" "$hooks/pre-push"
echo "installed: $hooks/pre-push"
echo "bypass with: git push --no-verify"
