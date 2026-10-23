#!/usr/bin/env bash
# Download only the selected Vision 3.3 model directories; never write to S3.
# EVA_VISION_MODELS is a comma-separated, case-sensitive list (seven by default).
set +x
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
exec "${PYTHON:-python3}" -B "$SCRIPT_DIR/../lib/vision_models.py" download "$@"