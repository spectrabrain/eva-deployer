#!/usr/bin/env bash
set -euo pipefail

BASE_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
WHEEL_DIR="${BASE_DIR}/wheels"
REQ_FILE="${BASE_DIR}/requirements-airgap.txt"

mkdir -p "${WHEEL_DIR}"

python3 -m pip download \
  --dest "${WHEEL_DIR}" \
  --requirement "${REQ_FILE}" \
  --only-binary=:all:

echo "[done] downloaded ansible wheelhouse into ${WHEEL_DIR}"
