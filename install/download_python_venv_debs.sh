#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
OUT_DIR="${OUT_DIR:-$SCRIPT_DIR/python-debs}"

ROOT_PKGS=(
  "${ROOT_PKG1:-python3-venv}"
  "${ROOT_PKG2:-python3.12-venv}"
  "${ROOT_PKG3:-python3-pip-whl}"
  "${ROOT_PKG4:-python3-setuptools-whl}"
)

if ! command -v apt-cache >/dev/null 2>&1 || ! command -v apt-get >/dev/null 2>&1; then
  echo "[ERROR] apt tools not found. This script must run on Debian/Ubuntu."
  exit 1
fi

mkdir -p "$OUT_DIR"

echo "[info] refreshing apt metadata"
sudo apt-get update

deps="$(
  apt-cache depends --recurse --no-recommends --no-suggests --no-conflicts --no-breaks --no-replaces --no-enhances "${ROOT_PKGS[@]}" \
    | sed -n 's/.*PreDepends: //p; s/.*Depends: //p' \
    | tr -d '<>' \
    | awk '{print $1}' \
    | grep -E '^[a-zA-Z0-9.+-]+$' \
    | sort -u
)"

all_pkgs="$(printf "%s\n%s\n" "${ROOT_PKGS[*]}" "$deps" | tr ' ' '\n' | grep -E '^[a-zA-Z0-9.+-]+$' | sort -u)"

has_candidate() {
  local pkg="$1"
  local candidate
  candidate="$(apt-cache policy "$pkg" | sed -n 's/^[[:space:]]*Candidate:[[:space:]]*//p' | head -n1)"
  [[ -n "$candidate" && "$candidate" != "(none)" ]]
}

echo "[info] downloading deb packages to $OUT_DIR"
(
  cd "$OUT_DIR"
  while IFS= read -r pkg; do
    [[ -z "$pkg" ]] && continue
    if ! has_candidate "$pkg"; then
      echo "[skip] no install candidate: $pkg"
      continue
    fi
    echo "[download] $pkg"
    apt-get download "$pkg"
  done <<< "$all_pkgs"
)

cat > "$OUT_DIR/manifest.txt" <<MANIFEST
Generated: $(date -Iseconds)
Root packages: ${ROOT_PKGS[*]}
Files:
$(find "$OUT_DIR" -maxdepth 1 -type f | sort)
MANIFEST

echo "[done] python venv deb bundle prepared: $OUT_DIR"
