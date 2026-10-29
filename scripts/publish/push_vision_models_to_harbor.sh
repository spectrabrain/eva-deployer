#!/usr/bin/env bash
# Requires the offline tool ORAS 1.3.3 at $EVA_CACHE_ROOT/tools/oras (or ORAS_CMD).
# Uses the existing repository project, not a separate model project.
set +x
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
exec "${PYTHON:-python3}" -B "$SCRIPT_DIR/../lib/vision_models.py" publish "$@"