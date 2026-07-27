#!/usr/bin/env bash
set -euo pipefail

BASE_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
DEB_DIR="${1:-${BASE_DIR}/python-debs}"

if [[ ! -d "${DEB_DIR}" ]]; then
  echo "[error] deb directory not found: ${DEB_DIR}"
  echo "run on online host first: ./install/download_python_venv_debs.sh"
  exit 1
fi

shopt -s nullglob
debs=("${DEB_DIR}"/*.deb)
shopt -u nullglob

if [[ ${#debs[@]} -eq 0 ]]; then
  echo "[error] no .deb files in: ${DEB_DIR}"
  echo "run on online host first: ./install/download_python_venv_debs.sh"
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
