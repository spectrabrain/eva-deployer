#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PACKAGE_DIR="${1:-$SCRIPT_DIR/apt/debs}"

if [[ "$EUID" -eq 0 ]]; then
  SUDO=()
else
  SUDO=(sudo)
fi

if [[ ! -d "$PACKAGE_DIR" ]]; then
  echo "[ERROR] offline package directory not found: $PACKAGE_DIR" >&2
  exit 1
fi

mapfile -t packages < <(find "$PACKAGE_DIR" -maxdepth 1 -type f -name '*.deb' -print | sort)
if [[ ${#packages[@]} -eq 0 ]]; then
  echo "[ERROR] no .deb files found in $PACKAGE_DIR" >&2
  exit 1
fi

"$SCRIPT_DIR/validate_offline_debs.sh" "$PACKAGE_DIR"

echo "[repair] unpacking offline packages: ${#packages[@]} files"
"${SUDO[@]}" dpkg --unpack "${packages[@]}"

echo "[repair] configuring pending packages"
"${SUDO[@]}" env DEBIAN_FRONTEND=noninteractive dpkg --configure --pending

echo "[verify] checking dpkg package state"
if ! "${SUDO[@]}" dpkg --audit; then
  echo "[ERROR] dpkg still reports incomplete packages." >&2
  exit 1
fi

echo "[ok] offline package repair completed"
