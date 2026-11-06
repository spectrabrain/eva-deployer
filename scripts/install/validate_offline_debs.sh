#!/usr/bin/env bash
set -euo pipefail

# Manual diagnosis entry point. The validator itself ships in the base role,
# which is what a Release install runs on the target.
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"
EVA_CACHE_ROOT="${EVA_CACHE_ROOT:-$REPO_ROOT/out/cache}"

exec bash "$REPO_ROOT/src/infra/roles/base/files/validate_offline_debs.sh" "${1:-$EVA_CACHE_ROOT/apt/debs}"
