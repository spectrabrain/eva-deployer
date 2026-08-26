#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
BASE_DIR="${BASE_DIR:-$SCRIPT_DIR}"
source "$SCRIPT_DIR/load_versions.sh"
load_deploy_versions

if [[ "$EUID" -eq 0 ]]; then
  SUDO_CMD=()
else
  command -v sudo >/dev/null 2>&1 || {
    echo "[ERROR] Docker package download requires sudo when this script is not run as root." >&2
    exit 1
  }
  if ! sudo -n true 2>/dev/null; then
    echo "[ERROR] Docker package download requires non-interactive sudo access." >&2
    echo "        Run 'sudo -v' in a terminal, then rerun this script, or run it as root." >&2
    exit 1
  fi
  SUDO_CMD=(sudo)
fi

EVA_AGENT_RELEASE="${EVA_AGENT_RELEASE:?missing EVA_AGENT_RELEASE (set in versions.json)}"
EVA_APP_CHART_VERSION="${EVA_APP_CHART_VERSION:?missing EVA_APP_CHART_VERSION (set in versions.json)}"
EVA_VISION_CHART_VERSION="${EVA_VISION_CHART_VERSION:?missing EVA_VISION_CHART_VERSION (set in versions.json)}"
EVA_AGENT_CHART_VERSION="${EVA_AGENT_CHART_VERSION:?missing EVA_AGENT_CHART_VERSION (set in versions.json)}"
EVA_AGENT_VLLM_CHART_VERSION="${EVA_AGENT_VLLM_CHART_VERSION:?missing EVA_AGENT_VLLM_CHART_VERSION (set in versions.json)}"
EVA_AGENT_INIT_CHART_VERSION="${EVA_AGENT_INIT_CHART_VERSION:?missing EVA_AGENT_INIT_CHART_VERSION (set in versions.json)}"
QDRANT_CHART_VERSION="${QDRANT_CHART_VERSION:?missing QDRANT_CHART_VERSION (set in versions.json)}"
EVA_IAM_CHART_VERSION="${EVA_IAM_CHART_VERSION:?missing EVA_IAM_CHART_VERSION (set in versions.json)}"
KUSTOMIZE_VERSION="${KUSTOMIZE_VERSION:?missing KUSTOMIZE_VERSION (set in versions.json)}"
K3S_DEFAULT_VERSION="${K3S_DEFAULT_VERSION:?missing K3S_DEFAULT_VERSION (set in versions.json)}"

mkdir -p "$BASE_DIR"/{aws,apt,docker,nvidia,cuda,helm,k3s,k8s,nfs,eva-app,eva-vision,eva-agent,eva-iam,qdrant,tools,images}
mkdir -p "$BASE_DIR/docker/debs"
mkdir -p "$BASE_DIR/apt/debs"
mkdir -p "$BASE_DIR/nvidia/container-toolkit-debs"
cp "$SCRIPT_DIR/install_docker.sh" "$BASE_DIR/docker/install_docker.sh" 2>/dev/null || true
mkdir -p "$BASE_DIR/eva-agent/release/$EVA_AGENT_RELEASE"/{eva-agent,eva-agent-init,eva-agent-qdrant,eva-agent-vllm,plugins/eva-agent-qdrant}
mkdir -p "$BASE_DIR/display_mode_selector"

fetch() {
  local url="$1"
  local out="$2"
  echo "[download] $url -> $out"
  curl -fL --retry 3 --retry-delay 2 --connect-timeout 20 --max-time 1800 "$url" -o "$out"
}

fetch_optional() {
  local url="$1"
  local out="$2"
  echo "[download-optional] $url -> $out"
  if ! curl -fL --retry 3 --retry-delay 2 --connect-timeout 20 --max-time 1800 "$url" -o "$out"; then
    echo "[warn] optional download failed: $url"
    return 1
  fi
  return 0
}

download_deb_packages() {
  local output_dir="$1"
  shift

  (
    cd "$output_dir"
    "${SUDO_CMD[@]}" apt-get download "$@"
  )
}

resolve_deb_packages() {
  local -a pending=("$@")
  local -A resolved=()
  local package candidate_version dependency

  while [[ ${#pending[@]} -gt 0 ]]; do
    package="${pending[0]}"
    pending=("${pending[@]:1}")
    package="${package%%:*}"

    [[ -n "$package" && -z "${resolved[$package]:-}" ]] || continue
    candidate_version="$(apt-cache "${APT_CACHE_OPTIONS[@]}" policy "$package" | awk '/Candidate:/ { print $2; exit }')"
    [[ -n "$candidate_version" && "$candidate_version" != "(none)" ]] || continue

    resolved["$package"]=1
    while IFS= read -r dependency; do
      dependency="${dependency%%:*}"
      [[ "$dependency" != \<* && -n "$dependency" ]] && pending+=("$dependency")
    done < <(
      apt-cache "${APT_CACHE_OPTIONS[@]}" depends "$package" \
        | sed -nE 's/^[[:space:]]+\|?(PreDepends|Depends):[[:space:]]+([^[:space:]]+).*/\2/p'
    )
  done

  printf '%s\n' "${!resolved[@]}" | sort
}

prepare_display_mode_selector() {
  local dms_dir="$BASE_DIR/display_mode_selector"
  local dms_zip="${DISPLAY_MODE_SELECTOR_ZIP_PATH:-$dms_dir/NVIDIA-Display-Mode-Selector-Tool-1.72.0-July25.zip}"
  local dms_url="${DISPLAY_MODE_SELECTOR_URL:-}"
  local dms_target_dir="$dms_dir/mode_selector_1.72.0"
  local dms_binary="$dms_target_dir/linux/x64/displaymodeselector"

  if [[ ! -f "$dms_zip" && -n "$dms_url" ]]; then
    fetch "$dms_url" "$dms_zip"
  fi

  if [[ -f "$dms_zip" && ! -f "$dms_binary" ]]; then
    if command -v unzip >/dev/null 2>&1; then
      local tmp_dir
      tmp_dir="$(mktemp -d)"
      unzip -q -o "$dms_zip" -d "$tmp_dir"

      local extracted_root
      extracted_root="$(find "$tmp_dir" -type f -path '*/linux/x64/displaymodeselector' | head -n1 | sed 's#/linux/x64/displaymodeselector$##')"
      if [[ -n "$extracted_root" && -d "$extracted_root" ]]; then
        rm -rf "$dms_target_dir"
        mkdir -p "$dms_target_dir"
        cp -a "$extracted_root/"* "$dms_target_dir/"
      fi
      rm -rf "$tmp_dir"
    else
      echo "[warn] unzip not found, skip extracting display mode selector archive."
    fi
  fi

  if [[ -f "$dms_binary" ]]; then
    chmod +x "$dms_binary" || true
    echo "[ok] display mode selector prepared: $dms_binary"
  else
    echo "[warn] display mode selector not prepared."
    echo "       - expected zip: $dms_zip"
    echo "       - expected binary: $dms_binary"
    echo "       If needed, set DISPLAY_MODE_SELECTOR_URL and rerun this script."
  fi
}

# Direct URLs used by current Ansible infra roles
fetch "https://awscli.amazonaws.com/awscli-exe-linux-x86_64.zip" "$BASE_DIR/aws/awscli-exe-linux-x86_64.zip"
fetch "https://download.docker.com/linux/ubuntu/gpg" "$BASE_DIR/docker/docker.gpg"
fetch "https://nvidia.github.io/libnvidia-container/gpgkey" "$BASE_DIR/nvidia/libnvidia-container.gpgkey"
fetch "https://nvidia.github.io/libnvidia-container/stable/deb/nvidia-container-toolkit.list" "$BASE_DIR/nvidia/nvidia-container-toolkit.list"
fetch "https://developer.download.nvidia.com/compute/cuda/repos/wsl-ubuntu/x86_64/cuda-keyring_1.1-1_all.deb" "$BASE_DIR/cuda/cuda-keyring_1.1-1_all.deb"
fetch "https://raw.githubusercontent.com/helm/helm/main/scripts/get-helm-4" "$BASE_DIR/helm/get-helm-4"
chmod +x "$BASE_DIR/helm/get-helm-4"

# Download Docker Engine and all packages apt resolves for an airgap install.
# The prepared host must be Ubuntu with apt package indexes available.
if command -v apt-get >/dev/null 2>&1 && command -v dpkg >/dev/null 2>&1; then
  APT_CACHE_OPTIONS=()
  DOCKER_ARCH="$(dpkg --print-architecture)"
  DOCKER_CODENAME="$(. /etc/os-release && echo "${VERSION_CODENAME:-}")"
  if [[ -n "$DOCKER_CODENAME" ]]; then
    DOCKER_KEYRING="$(mktemp)"
    DOCKER_LIST="$(mktemp)"
    DOCKER_SOURCEPARTS="$(mktemp -d)"
    trap 'rm -rf "$DOCKER_KEYRING" "$DOCKER_LIST" "$DOCKER_SOURCEPARTS"' EXIT
    gpg --dearmor < "$BASE_DIR/docker/docker.gpg" > "$DOCKER_KEYRING"
    chmod 0644 "$DOCKER_KEYRING"
    printf 'deb [arch=%s signed-by=%s] https://download.docker.com/linux/ubuntu %s stable\n' \
      "$DOCKER_ARCH" "$DOCKER_KEYRING" "$DOCKER_CODENAME" > "$DOCKER_SOURCEPARTS/docker.list"
    if [[ -d /etc/apt/sources.list.d ]]; then
      find /etc/apt/sources.list.d -maxdepth 1 -type f \
        ! -name 'docker.list' -exec cp -a {} "$DOCKER_SOURCEPARTS/" \;
    fi
    "${SUDO_CMD[@]}" apt-get \
      -o Dir::Etc::sourceparts="$DOCKER_SOURCEPARTS" \
      -o APT::Get::List-Cleanup="0" update
    DOCKER_PACKAGES=(
      docker-ce
      docker-ce-cli
      containerd.io
      docker-buildx-plugin
      docker-compose-plugin
    )
    find "$BASE_DIR/docker/debs" -maxdepth 1 -type f -name '*.deb' -delete
    APT_CACHE_OPTIONS=(
      -o Dir::Etc::sourcelist=-
      -o "Dir::Etc::sourceparts=$DOCKER_SOURCEPARTS"
    )
    DOCKER_DEPENDENCIES="$(resolve_deb_packages "${DOCKER_PACKAGES[@]}")"
    mapfile -t DOCKER_DOWNLOAD_PACKAGES < <(
      printf '%s\n' "${DOCKER_PACKAGES[@]}" "$DOCKER_DEPENDENCIES" | sort -u
    )
    if [[ ${#DOCKER_DOWNLOAD_PACKAGES[@]} -eq 0 ]]; then
      echo "[ERROR] could not resolve Docker package dependencies from apt." >&2
      exit 1
    fi
    echo "[info] downloading Docker packages and dependencies: ${#DOCKER_DOWNLOAD_PACKAGES[@]} packages"
    download_deb_packages "$BASE_DIR/docker/debs" \
      -o Dir::Etc::sourcelist='-' \
      -o Dir::Etc::sourceparts="$DOCKER_SOURCEPARTS" \
      "${DOCKER_DOWNLOAD_PACKAGES[@]}"
    "${SUDO_CMD[@]}" rm -rf \
      "$BASE_DIR/docker/debs/partial" \
      "$BASE_DIR/docker/debs/lock"
    find "$BASE_DIR/docker/debs" -maxdepth 1 -type f -name '*.deb' -print \
      | sort > "$BASE_DIR/docker/debs/manifest.txt"
    rm -rf "$DOCKER_KEYRING" "$DOCKER_LIST" "$DOCKER_SOURCEPARTS"
    APT_CACHE_OPTIONS=()
    echo "[ok] Docker packages prepared under $BASE_DIR/docker/debs"
  else
    echo "[warn] Ubuntu VERSION_CODENAME not found; Docker package download skipped."
  fi
else
  echo "[warn] apt-get/dpkg not found; Docker package download skipped."
fi

# Download Ubuntu packages required by the base and NFS Ansible roles.
# These packages are installed from install/apt/debs on an airgap host.
if command -v apt-get >/dev/null 2>&1 && command -v dpkg >/dev/null 2>&1; then
  APT_CACHE_OPTIONS=()
  BASE_PACKAGES=(
    unzip
    curl
    gnupg
    nfs-kernel-server
    nfs-common
    keyutils
  )
  BASE_EXCLUDED_PACKAGES=(
    systemd
    systemd-dev
    systemd-sysv
    systemd-resolved
    systemd-timesyncd
    systemd-oomd
    udev
    libsystemd-shared
    libsystemd0
    libudev1
    libnss-systemd
    libpam-systemd
    dpkg
    libc6
  )
  find "$BASE_DIR/apt/debs" -maxdepth 1 -type f -name '*.deb' -delete
  BASE_DEPENDENCIES="$(resolve_deb_packages "${BASE_PACKAGES[@]}")"
  mapfile -t BASE_DOWNLOAD_PACKAGES < <(
    printf '%s\n' "$BASE_DEPENDENCIES" \
      | grep -F -x -v -f <(printf '%s\n' "${BASE_EXCLUDED_PACKAGES[@]}")
  )
  if [[ ${#BASE_DOWNLOAD_PACKAGES[@]} -eq 0 ]]; then
    echo "[ERROR] could not resolve base/NFS package dependencies from apt." >&2
    exit 1
  fi
  echo "[info] downloading base/NFS packages and dependencies: ${#BASE_DOWNLOAD_PACKAGES[@]} packages"
  download_deb_packages "$BASE_DIR/apt/debs" "${BASE_DOWNLOAD_PACKAGES[@]}"
  rm -rf "$BASE_DIR/apt/debs/partial" "$BASE_DIR/apt/debs/lock"
  find "$BASE_DIR/apt/debs" -maxdepth 1 -type f -name '*.deb' -print \
    | sort > "$BASE_DIR/apt/debs/manifest.txt"
  echo "[ok] base/NFS packages prepared under $BASE_DIR/apt/debs"
fi

# Download NVIDIA Container Toolkit packages for airgap installation.
if command -v apt-get >/dev/null 2>&1 && command -v dpkg >/dev/null 2>&1; then
  APT_CACHE_OPTIONS=()
  NVIDIA_TOOLKIT_PACKAGES=(
    libnvidia-container1
    libnvidia-container-tools
    nvidia-container-toolkit-base
    nvidia-container-toolkit
  )
  NVIDIA_TOOLKIT_DEPENDENCIES="$(resolve_deb_packages "${NVIDIA_TOOLKIT_PACKAGES[@]}")"
  find "$BASE_DIR/nvidia/container-toolkit-debs" -maxdepth 1 -type f -name '*.deb' -delete
  echo "[info] downloading NVIDIA Container Toolkit packages and dependencies"
  mapfile -t NVIDIA_TOOLKIT_DOWNLOAD_PACKAGES < <(
    printf '%s\n' "${NVIDIA_TOOLKIT_PACKAGES[@]}" "$NVIDIA_TOOLKIT_DEPENDENCIES" | sort -u
  )
  download_deb_packages "$BASE_DIR/nvidia/container-toolkit-debs" \
    "${NVIDIA_TOOLKIT_DOWNLOAD_PACKAGES[@]}"
  "${SUDO_CMD[@]}" rm -rf "$BASE_DIR/nvidia/container-toolkit-debs/partial" "$BASE_DIR/nvidia/container-toolkit-debs/lock"
  find "$BASE_DIR/nvidia/container-toolkit-debs" -maxdepth 1 -type f -name '*.deb' -print \
    | sort > "$BASE_DIR/nvidia/container-toolkit-debs/manifest.txt"
fi
fetch "https://get.k3s.io" "$BASE_DIR/k3s/install_k3s.sh"
chmod +x "$BASE_DIR/k3s/install_k3s.sh"
fetch "https://raw.githubusercontent.com/NVIDIA/k8s-device-plugin/v0.18.0/deployments/static/nvidia-device-plugin.yml" "$BASE_DIR/k8s/nvidia-device-plugin-v0.18.0.yml"
fetch "https://dl.k8s.io/release/stable.txt" "$BASE_DIR/k8s/stable.txt"

# Resolve and download stable kubectl binaries
KUBECTL_STABLE="$(tr -d '\n\r' < "$BASE_DIR/k8s/stable.txt")"
fetch "https://dl.k8s.io/release/${KUBECTL_STABLE}/bin/linux/amd64/kubectl" "$BASE_DIR/k8s/kubectl-${KUBECTL_STABLE}-linux-amd64"
fetch "https://dl.k8s.io/release/${KUBECTL_STABLE}/bin/linux/arm64/kubectl" "$BASE_DIR/k8s/kubectl-${KUBECTL_STABLE}-linux-arm64"
chmod +x "$BASE_DIR/k8s/kubectl-${KUBECTL_STABLE}-linux-amd64" "$BASE_DIR/k8s/kubectl-${KUBECTL_STABLE}-linux-arm64"

# NFS CSI chart sources used by helm role
fetch "https://raw.githubusercontent.com/kubernetes-csi/csi-driver-nfs/master/charts/index.yaml" "$BASE_DIR/nfs/index.yaml"
NFS_VER="4.11.0"
fetch "https://raw.githubusercontent.com/kubernetes-csi/csi-driver-nfs/master/charts/v${NFS_VER}/csi-driver-nfs-${NFS_VER}.tgz" "$BASE_DIR/nfs/csi-driver-nfs-${NFS_VER}.tgz"

# Helpful extras for offline k3s binary install
# GitHub API can return 403 when rate-limited; do not fail whole script.
K3S_VER="${K3S_VERSION:-$K3S_DEFAULT_VERSION}"
if [[ -z "${K3S_VER}" ]]; then
  # Prefer stable channel API (no GitHub API rate-limit issue)
  if fetch_optional "https://update.k3s.io/v1-release/channels/stable" "$BASE_DIR/k3s/stable-channel.json"; then
    K3S_VER="$(
      python3 - <<'PY' "$BASE_DIR/k3s/stable-channel.json"
import json
import sys

path = sys.argv[1]
try:
    with open(path, "r", encoding="utf-8") as f:
        data = json.load(f)
    print(data.get("data") or data.get("latest") or "")
except Exception:
    print("")
PY
    )"
    if [[ -z "${K3S_VER}" ]]; then
      K3S_VER="$(grep -m1 -oE '"(data|latest)"\s*:\s*"[^"]*"' "$BASE_DIR/k3s/stable-channel.json" | sed -E 's/.*:\s*"//;s/"$//')"
    fi
  fi
fi
if [[ -z "${K3S_VER}" ]]; then
  # Fallback: GitHub API
  if fetch_optional "https://api.github.com/repos/k3s-io/k3s/releases/latest" "$BASE_DIR/k3s/latest-release.json"; then
    K3S_VER="$(grep -m1 -o '"tag_name": *"[^"]*"' "$BASE_DIR/k3s/latest-release.json" | sed 's/.*"tag_name": *"//;s/"$//')"
  fi
fi

if [[ -z "${K3S_VER}" ]]; then
  echo "[warn] could not resolve k3s version automatically."
  echo "       rerun with explicit version, for example:"
  echo "       K3S_VERSION=v1.36.2+k3s1 ./install/download_offline_assets.sh"
  exit 1
fi
K3S_AIRGAP_IMAGES_ARCHIVE="k3s-airgap-images-${DOCKER_ARCH:-amd64}.tar.zst"
fetch "https://github.com/k3s-io/k3s/releases/download/${K3S_VER//+/%2B}/${K3S_AIRGAP_IMAGES_ARCHIVE}" "$BASE_DIR/k3s/$K3S_AIRGAP_IMAGES_ARCHIVE"

cat > "$BASE_DIR/k3s/latest-release.json" <<JSON
{"tag_name":"${K3S_VER}"}
JSON

fetch "https://github.com/k3s-io/k3s/releases/download/${K3S_VER}/k3s" "$BASE_DIR/k3s/k3s-${K3S_VER}-linux-amd64"
fetch "https://github.com/k3s-io/k3s/releases/download/${K3S_VER}/k3s-arm64" "$BASE_DIR/k3s/k3s-${K3S_VER}-linux-arm64"
chmod +x "$BASE_DIR/k3s/k3s-${K3S_VER}-linux-amd64" "$BASE_DIR/k3s/k3s-${K3S_VER}-linux-arm64"

fetch "https://get.helm.sh/helm4-latest-version" "$BASE_DIR/helm/helm4-latest-version.txt"
HELM_VER="$(tr -d '\n\r' < "$BASE_DIR/helm/helm4-latest-version.txt")"
if [[ -n "${HELM_VER}" ]]; then
  fetch "https://get.helm.sh/helm-${HELM_VER}-linux-amd64.tar.gz" "$BASE_DIR/helm/helm-${HELM_VER}-linux-amd64.tar.gz"
  fetch "https://get.helm.sh/helm-${HELM_VER}-linux-amd64.tar.gz.sha256" "$BASE_DIR/helm/helm-${HELM_VER}-linux-amd64.tar.gz.sha256"
  fetch "https://get.helm.sh/helm-${HELM_VER}-linux-arm64.tar.gz" "$BASE_DIR/helm/helm-${HELM_VER}-linux-arm64.tar.gz"
  fetch "https://get.helm.sh/helm-${HELM_VER}-linux-arm64.tar.gz.sha256" "$BASE_DIR/helm/helm-${HELM_VER}-linux-arm64.tar.gz.sha256"
fi

# EVA App / Vision / Agent / IAM charts
fetch "https://mellerikat.github.io/eva-app/eva-app-${EVA_APP_CHART_VERSION}.tgz" "$BASE_DIR/eva-app/eva-app-${EVA_APP_CHART_VERSION}.tgz"
fetch "https://mellerikat.github.io/eva-iam/eva-iam-${EVA_IAM_CHART_VERSION}.tgz" "$BASE_DIR/eva-iam/eva-iam-${EVA_IAM_CHART_VERSION}.tgz"
fetch "https://raw.githubusercontent.com/mellerikat/eva-vision/chartmuseum/eva-vision-${EVA_VISION_CHART_VERSION}.tgz" "$BASE_DIR/eva-vision/eva-vision-${EVA_VISION_CHART_VERSION}.tgz"
fetch "https://mellerikat.github.io/eva-agent/eva-agent-${EVA_AGENT_CHART_VERSION}.tgz" "$BASE_DIR/eva-agent/eva-agent-${EVA_AGENT_CHART_VERSION}.tgz"
fetch "https://mellerikat.github.io/eva-agent/eva-agent-vllm-${EVA_AGENT_VLLM_CHART_VERSION}.tgz" "$BASE_DIR/eva-agent/eva-agent-vllm-${EVA_AGENT_VLLM_CHART_VERSION}.tgz"
fetch "https://mellerikat.github.io/eva-agent/eva-agent-init-${EVA_AGENT_INIT_CHART_VERSION}.tgz" "$BASE_DIR/eva-agent/eva-agent-init-${EVA_AGENT_INIT_CHART_VERSION}.tgz"
fetch "https://github.com/qdrant/qdrant-helm/releases/download/qdrant-${QDRANT_CHART_VERSION}/qdrant-${QDRANT_CHART_VERSION}.tgz" "$BASE_DIR/qdrant/qdrant-${QDRANT_CHART_VERSION}.tgz"

# EVA Agent release values/templates/scripts
AGENT_RELEASE_BASE="https://raw.githubusercontent.com/mellerikat/eva-agent/chartmuseum/release/${EVA_AGENT_RELEASE}"
fetch "${AGENT_RELEASE_BASE}/eva-agent/values-k3s.yaml" "$BASE_DIR/eva-agent/release/${EVA_AGENT_RELEASE}/eva-agent/values-k3s.yaml"
fetch "${AGENT_RELEASE_BASE}/eva-agent/values-secret.yaml" "$BASE_DIR/eva-agent/release/${EVA_AGENT_RELEASE}/eva-agent/values-secret.yaml"
fetch "${AGENT_RELEASE_BASE}/eva-agent-init/values-k3s.yaml" "$BASE_DIR/eva-agent/release/${EVA_AGENT_RELEASE}/eva-agent-init/values-k3s.yaml"
fetch "${AGENT_RELEASE_BASE}/eva-agent-qdrant/values-k3s.yaml" "$BASE_DIR/eva-agent/release/${EVA_AGENT_RELEASE}/eva-agent-qdrant/values-k3s.yaml"

# Download all supported k3s GPU profile values for eva-agent-vllm
VLLM_K3S_VALUES_FILES=(
  "values-k3s.A6000x1.yaml"
  "values-k3s.L40sx1.yaml"
  "values-k3s.PRO5000x3.yaml"
  "values-k3s.PRO6000-MIGx4.yaml"
)
for name in "${VLLM_K3S_VALUES_FILES[@]}"; do
  fetch "${AGENT_RELEASE_BASE}/eva-agent-vllm/${name}" "$BASE_DIR/eva-agent/release/${EVA_AGENT_RELEASE}/eva-agent-vllm/${name}"
done
fetch "${AGENT_RELEASE_BASE}/plugins/eva-agent-qdrant/post-renderer.sh" "$BASE_DIR/eva-agent/release/${EVA_AGENT_RELEASE}/plugins/eva-agent-qdrant/post-renderer.sh"
fetch "${AGENT_RELEASE_BASE}/plugins/eva-agent-qdrant/plugin.yaml" "$BASE_DIR/eva-agent/release/${EVA_AGENT_RELEASE}/plugins/eva-agent-qdrant/plugin.yaml"
fetch "https://raw.githubusercontent.com/mellerikat/eva-agent/chartmuseum/install_eva_agent.sh" "$BASE_DIR/eva-agent/install_eva_agent.sh"
fetch "https://raw.githubusercontent.com/mellerikat/eva-agent/chartmuseum/install_eva_agent_dependencies.sh" "$BASE_DIR/eva-agent/install_eva_agent_dependencies.sh"
chmod +x "$BASE_DIR/eva-agent/install_eva_agent.sh" "$BASE_DIR/eva-agent/install_eva_agent_dependencies.sh" "$BASE_DIR/eva-agent/release/${EVA_AGENT_RELEASE}/plugins/eva-agent-qdrant/post-renderer.sh"

# kustomize offline binary
KUSTOMIZE_ARCHIVE="kustomize_v${KUSTOMIZE_VERSION}_linux_amd64.tar.gz"
fetch "https://github.com/kubernetes-sigs/kustomize/releases/download/kustomize%2Fv${KUSTOMIZE_VERSION}/${KUSTOMIZE_ARCHIVE}" "$BASE_DIR/tools/${KUSTOMIZE_ARCHIVE}"
tar -xzf "$BASE_DIR/tools/${KUSTOMIZE_ARCHIVE}" -C "$BASE_DIR/tools"
chmod +x "$BASE_DIR/tools/kustomize"

# Optional: build NVIDIA driver offline apt repo
if [[ "${DOWNLOAD_NVIDIA_DRIVER_REPO:-true}" == "true" ]]; then
  chmod +x "$BASE_DIR/build_nvidia_driver_repo.sh"
  "$BASE_DIR/build_nvidia_driver_repo.sh" "${NVIDIA_DRIVER_PACKAGE:-nvidia-driver-580}"
fi

# Optional: NVIDIA display mode selector archive download/extract
prepare_display_mode_selector

cat > "$BASE_DIR/manifest.txt" <<MANIFEST
Generated: $(date -Iseconds)
kubectl_stable: ${KUBECTL_STABLE}
k3s_stable: ${K3S_VER:-unknown}
helm4_stable: ${HELM_VER:-unknown}
eva_app_chart: ${EVA_APP_CHART_VERSION}
eva_vision_chart: ${EVA_VISION_CHART_VERSION}
eva_agent_chart: ${EVA_AGENT_CHART_VERSION}
eva_agent_vllm_chart: ${EVA_AGENT_VLLM_CHART_VERSION}
eva_agent_init_chart: ${EVA_AGENT_INIT_CHART_VERSION}
eva_iam_chart: ${EVA_IAM_CHART_VERSION}
qdrant_chart: ${QDRANT_CHART_VERSION}
eva_agent_release_values: ${EVA_AGENT_RELEASE}
kustomize: ${KUSTOMIZE_VERSION}

Files:
$(find "$BASE_DIR" -maxdepth 5 -type f | sort)
MANIFEST

echo "[done] Offline assets downloaded under $BASE_DIR"
echo "[hint] EVA 이미지는 ./install/download_eva_images.sh 실행 후 Harbor로 push하세요"
echo "[hint] EVA model cache는 ./install/download_eva_models.sh 로 다운로드하세요"
