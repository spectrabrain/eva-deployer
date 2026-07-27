#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PACKAGE_DIR="${DOCKER_PACKAGE_DIR:-$SCRIPT_DIR/docker/debs}"
GPG_URL="${DOCKER_GPG_URL:-https://download.docker.com/linux/ubuntu/gpg}"
GPG_PATH="${DOCKER_GPG_PATH:-$SCRIPT_DIR/docker/docker.gpg}"
REPO_URL="${DOCKER_REPO_URL:-https://download.docker.com/linux/ubuntu}"
AIRGAP="${AIRGAP_MODE:-false}"

usage() {
  cat <<USAGE
Usage: ./install/install_docker.sh [options]

Options:
  --airgap              Install Docker from install/docker/debs/*.deb
  --package-dir <dir>   Override offline package directory
  --help                Show this help

Environment:
  AIRGAP_MODE=true      Same as --airgap
  DOCKER_PACKAGE_DIR    Offline package directory
USAGE
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --airgap) AIRGAP=true; shift ;;
    --package-dir) PACKAGE_DIR="${2:?--package-dir requires a value}"; shift 2 ;;
    -h|--help) usage; exit 0 ;;
    *) echo "[ERROR] unknown argument: $1" >&2; usage; exit 1 ;;
  esac
done

if [[ "$EUID" -eq 0 ]]; then
  SUDO=()
else
  SUDO=(sudo)
fi

require_cmd() {
  command -v "$1" >/dev/null 2>&1 || { echo "[ERROR] required command not found: $1" >&2; exit 1; }
}

require_cmd apt-get
require_cmd dpkg
require_cmd systemctl
mkdir -p "$(dirname "$GPG_PATH")"

if command -v docker >/dev/null 2>&1; then
  echo "[skip] Docker already installed: $(docker --version)"
else
  if [[ "$AIRGAP" == "true" ]]; then
    mapfile -t packages < <(find "$PACKAGE_DIR" -maxdepth 1 -type f -name '*.deb' -print | sort)
    if [[ ${#packages[@]} -eq 0 ]]; then
      echo "[ERROR] no Docker .deb packages found in $PACKAGE_DIR" >&2
      echo "        Run ./install/download_offline_assets.sh on an internet-connected Ubuntu host first." >&2
      exit 1
    fi
    echo "[install] Docker from offline packages: ${#packages[@]} files"
    mapfile -t package_names < <(for package in "${packages[@]}"; do dpkg-deb -f "$package" Package; done | sort -u)
    if ! "${SUDO[@]}" dpkg --unpack "${packages[@]}"; then
      echo "[ERROR] offline Docker installation could not satisfy package dependencies." >&2
      echo "        The package bundle is incomplete or was prepared for a different Ubuntu release/architecture." >&2
      echo "        Re-run AWS_PROFILE=default ./install/download_offline_assets.sh on a matching Ubuntu host," >&2
      echo "        then copy the complete install/docker/debs/ directory to this Airgap server." >&2
      exit 1
    fi
    if ! "${SUDO[@]}" dpkg --configure "${package_names[@]}"; then
      echo "[ERROR] offline Docker package configuration failed." >&2
      echo "        Verify that install/docker/debs contains all packages from the prepared bundle." >&2
      exit 1
    fi
  else
    require_cmd curl
    require_cmd lsb_release
    echo "[install] Docker from official repository"
    "${SUDO[@]}" apt-get update
    "${SUDO[@]}" apt-get install -y apt-transport-https ca-certificates curl gnupg lsb-release
    "${SUDO[@]}" install -m 0755 -d /etc/apt/keyrings
    if [[ ! -f "$GPG_PATH" ]]; then
      curl -fsSL "$GPG_URL" -o "$GPG_PATH"
    fi
    "${SUDO[@]}" install -m 0644 "$GPG_PATH" /etc/apt/keyrings/docker.asc
    ARCH="$(dpkg --print-architecture)"
    CODENAME="$(. /etc/os-release && echo "${VERSION_CODENAME:-}")"
    [[ -n "$CODENAME" ]] || { echo "[ERROR] Ubuntu VERSION_CODENAME is unavailable" >&2; exit 1; }
    printf 'deb [arch=%s signed-by=/etc/apt/keyrings/docker.asc] %s %s stable\n' "$ARCH" "$REPO_URL" "$CODENAME" \
      | "${SUDO[@]}" tee /etc/apt/sources.list.d/docker.list >/dev/null
    "${SUDO[@]}" apt-get update
    "${SUDO[@]}" apt-get install -y docker-ce docker-ce-cli containerd.io docker-buildx-plugin docker-compose-plugin
  fi
fi

if ! docker info >/dev/null 2>&1; then
  "${SUDO[@]}" systemctl enable --now docker
fi

TARGET_USER="${SUDO_USER:-${USER}}"
if ! id -nG "$TARGET_USER" 2>/dev/null | tr ' ' '\n' | grep -qx docker; then
  "${SUDO[@]}" usermod -aG docker "$TARGET_USER"
fi

echo "[ok] Docker installed and running: $(docker --version 2>/dev/null || sudo docker --version)"
if docker compose version >/dev/null 2>&1 || sudo docker compose version >/dev/null 2>&1; then
  echo "[ok] Docker Compose plugin is available"
else
  echo "[ERROR] Docker Compose plugin is not available" >&2
  exit 1
fi
echo "[info] Log out and back in for docker group membership to take effect."
