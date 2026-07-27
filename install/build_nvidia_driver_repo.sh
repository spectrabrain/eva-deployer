#!/usr/bin/env bash
set -euo pipefail

BASE_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
NVIDIA_DIR="${BASE_DIR}/nvidia"
REPO_DIR="${NVIDIA_DIR}/nvidia-driver-repo"
TMP_DIR="$(mktemp -d)"
DRIVER_PACKAGE="${1:-nvidia-driver-580}"
DRIVER_REPO_MODE="${DRIVER_REPO_MODE:-nvidia-only}"   # nvidia-only | full
STRICT_MISSING="${STRICT_MISSING:-false}"             # true | false

cleanup() {
  rm -rf "${TMP_DIR}"
}
trap cleanup EXIT

mkdir -p "${REPO_DIR}"
rm -f "${REPO_DIR}"/*.deb "${REPO_DIR}/Packages.gz" "${REPO_DIR}/Packages"

echo "[info] resolve dependency set for ${DRIVER_PACKAGE}"
mapfile -t unique_packages < <(
  apt-cache depends --recurse --no-recommends --no-suggests \
    --no-conflicts --no-breaks --no-replaces --no-enhances \
    "${DRIVER_PACKAGE}" \
    | awk '/^[[:alnum:]][[:alnum:]+.-]*$/ {print $0}' \
    | grep -v ':i386$' \
    | sort -u
)

if [[ ${#unique_packages[@]} -eq 0 ]]; then
  echo "[error] failed to resolve dependency packages for ${DRIVER_PACKAGE}"
  exit 1
fi

if [[ "${DRIVER_REPO_MODE}" == "nvidia-only" ]]; then
  mapfile -t unique_packages < <(
    printf '%s\n' "${unique_packages[@]}" \
      | grep -E '^(nvidia|libnvidia|xserver-xorg-video-nvidia|linux-modules-nvidia|cuda-drivers)'
  )
fi

if [[ ${#unique_packages[@]} -eq 0 ]]; then
  echo "[error] package set is empty after filtering (mode=${DRIVER_REPO_MODE})"
  exit 1
fi

echo "[info] download $((${#unique_packages[@]})) packages"
pushd "${TMP_DIR}" >/dev/null
missing_packages=()
for pkg in "${unique_packages[@]}"; do
  echo "  - ${pkg}"
  if ! apt download "${pkg}" >/dev/null 2>&1; then
    missing_packages+=("${pkg}")
  fi
done
popd >/dev/null

if ls "${TMP_DIR}"/*.deb >/dev/null 2>&1; then
  mv "${TMP_DIR}"/*.deb "${REPO_DIR}/"
else
  echo "[error] no deb packages downloaded"
  exit 1
fi

pushd "${REPO_DIR}" >/dev/null
dpkg-scanpackages . /dev/null | gzip -9c > Packages.gz
popd >/dev/null

cat > "${NVIDIA_DIR}/nvidia-driver-repo.manifest.txt" <<MANIFEST
Generated: $(date -Iseconds)
Driver package: ${DRIVER_PACKAGE}
Deb count: $(find "${REPO_DIR}" -maxdepth 1 -name '*.deb' | wc -l)

Deb files:
$(find "${REPO_DIR}" -maxdepth 1 -name '*.deb' -printf '%f\n' | sort)

Missing packages:
$(printf '%s\n' "${missing_packages[@]:-}" | sed '/^$/d')
MANIFEST

tar -C "${REPO_DIR}" -czf "${NVIDIA_DIR}/nvidia-driver-repo.tar.gz" .

if [[ ${#missing_packages[@]} -gt 0 ]]; then
  echo "[warn] missing packages: ${#missing_packages[@]}"
  if [[ "${STRICT_MISSING}" == "true" ]]; then
    echo "[error] STRICT_MISSING=true and some packages were not downloaded."
    exit 1
  fi
fi

echo "[done] NVIDIA offline driver repo created"
echo "       repo dir : ${REPO_DIR}"
echo "       tar file : ${NVIDIA_DIR}/nvidia-driver-repo.tar.gz"
