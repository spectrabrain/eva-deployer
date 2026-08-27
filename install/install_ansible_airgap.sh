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

# 다른 airgap bundle 은 ansible 이 "Assert ... offline packages are available" 로 검사하지만
# (roles/base, roles/docker, roles/eva_app 등) wheelhouse 는 ansible 이 아직 없는 단계라
# 여기서 직접 검사합니다. 없으면 pip 의 "No matching distribution found" 로만 드러납니다.
shopt -s nullglob
WHEELS=("${WHEEL_DIR}"/*.whl)
if (( ${#WHEELS[@]} == 0 )); then
  cat >&2 <<MSG
[ERROR] wheelhouse 가 비어 있습니다: ${WHEEL_DIR}

이 서버(airgap)에서는 wheel 을 받을 수 없습니다. 인터넷이 되는 준비 서버에서
아래를 실행한 뒤 install/wheels/ 전체를 이 서버의 같은 경로로 복사하세요.

  TARGET_PYTHON=$(python3 -c 'import sys; print(f"{sys.version_info.major}.{sys.version_info.minor}")') ./install/download_ansible_wheels.sh
  rsync -a install/wheels/ <this-host>:${WHEEL_DIR}/

주의: download_ansible_wheels.sh 를 다른 명령과 '&&' 로 묶지 마세요.
앞 명령(download_python_venv_debs.sh 는 sudo 필요)이 실패하면 조용히 건너뜁니다.
MSG
  exit 1
fi

if ! printf '%s\n' "${WHEELS[@]##*/}" | grep -qiE '^ansible[_-]core-'; then
  echo "[ERROR] ${WHEEL_DIR} 에 ansible-core wheel 이 없습니다. 준비 서버에서 다시 받으세요." >&2
  exit 1
fi

# Python 버전이 다르면 platform wheel(cryptography/pyyaml/cffi/markupsafe)이 안 맞습니다.
LOCAL_PYTHON="$(python3 -c 'import sys; print(f"{sys.version_info.major}.{sys.version_info.minor}")')"
MANIFEST="${WHEEL_DIR}/manifest.txt"
if [[ -f "$MANIFEST" ]]; then
  BUILT_FOR="$(awk -F': *' '/^target_python:/{print $2; exit}' "$MANIFEST")"
  if [[ -n "$BUILT_FOR" && "$BUILT_FOR" != "$LOCAL_PYTHON" ]]; then
    echo "[ERROR] wheelhouse 는 Python ${BUILT_FOR} 용인데 이 서버는 ${LOCAL_PYTHON} 입니다." >&2
    echo "        준비 서버에서 TARGET_PYTHON=${LOCAL_PYTHON} ./install/download_ansible_wheels.sh 로 다시 받으세요." >&2
    exit 1
  fi
else
  echo "[warn] ${MANIFEST} 가 없습니다. 어느 Python 용 wheelhouse 인지 확인할 수 없습니다."
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

# 전체 requirements(ansible 메타패키지 + ansible-lint)가 과할 때는 최소 구성만 넣을 수 있습니다.
#   ANSIBLE_AIRGAP_REQUIREMENTS="ansible-core==2.20.5" ./install/install_ansible_airgap.sh
if [[ -n "${ANSIBLE_AIRGAP_REQUIREMENTS:-}" ]]; then
  echo "[info] requirements 대신 지정된 spec 을 설치합니다: ${ANSIBLE_AIRGAP_REQUIREMENTS}"
  # shellcheck disable=SC2086
  python -m pip install --no-index --find-links="${WHEEL_DIR}" ${ANSIBLE_AIRGAP_REQUIREMENTS}
else
  python -m pip install --no-index --find-links="${WHEEL_DIR}" -r "${REQ_FILE}"
fi

echo "[done] offline ansible environment created at ${VENV_DIR}"
echo "activate with: source ${VENV_DIR}/bin/activate"
