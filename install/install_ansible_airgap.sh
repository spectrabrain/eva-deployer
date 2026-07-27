#!/usr/bin/env bash
set -euo pipefail

BASE_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REQ_FILE="${BASE_DIR}/requirements-airgap.txt"
WHEEL_DIR="${BASE_DIR}/wheels"
VENV_DIR="${1:-${BASE_DIR}/../.venv}"

if ! command -v python3 >/dev/null 2>&1; then
  echo "[error] python3 not found"
  exit 1
fi

if ! python3 -c "import ensurepip, venv" >/dev/null 2>&1; then
  cat <<'MSG'
[error] python3 venv/ensurepip module not available.
Airgap install:
  ./install/install_python_venv_airgap.sh

Online install:
  sudo apt update
  sudo apt install -y python3-venv
or (Ubuntu 24.04):
  sudo apt install -y python3.12-venv
MSG
  exit 1
fi

python3 -m venv "${VENV_DIR}"
source "${VENV_DIR}/bin/activate"

python -m pip install --no-index --find-links="${WHEEL_DIR}" -r "${REQ_FILE}"

echo "[done] offline ansible environment created at ${VENV_DIR}"
echo "activate with: source ${VENV_DIR}/bin/activate"
