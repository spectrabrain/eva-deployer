#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"
EVA_CACHE_ROOT="${EVA_CACHE_ROOT:-$REPO_ROOT/out/cache}"
DEB_DIR="${1:-${EVA_CACHE_ROOT}/python-debs}"

if [[ ! -d "${DEB_DIR}" ]]; then
  echo "[error] deb directory not found: ${DEB_DIR}"
  echo "run on online host first: ./scripts/download/download_python_venv_debs.sh"
  exit 1
fi

shopt -s nullglob
debs=("${DEB_DIR}"/*.deb)
shopt -u nullglob

if [[ ${#debs[@]} -eq 0 ]]; then
  echo "[error] no .deb files in: ${DEB_DIR}"
  echo "run on online host first: ./scripts/download/download_python_venv_debs.sh"
  exit 1
fi

echo "[info] installing python venv prerequisites from ${DEB_DIR}"
sudo dpkg -i "${debs[@]}" || true

if ! python3 -c "import ensurepip, venv" >/dev/null 2>&1; then
  echo "[info] first pass unresolved deps, retrying dpkg once more"
  sudo dpkg -i "${debs[@]}" || true
fi

if python3 -c "import ensurepip, venv" >/dev/null 2>&1; then
  echo "[done] python3 venv/ensurepip is ready"
  python3 --version
else
  echo "[error] python3 venv/ensurepip still unavailable."
  echo "Please confirm all required deb files are downloaded in ${DEB_DIR}."
  exit 1
fi
