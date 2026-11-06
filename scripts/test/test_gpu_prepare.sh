#!/usr/bin/env bash
set -euo pipefail
repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
PYTHONDONTWRITEBYTECODE=1 "${PYTHON:-python3}" "$repo_root/scripts/test/test_gpu_prepare.py"
