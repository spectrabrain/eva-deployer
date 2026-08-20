#!/usr/bin/env bash
set -euo pipefail

BASE_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
WHEEL_DIR="${BASE_DIR}/wheels"
REQ_FILE="${BASE_DIR}/requirements-airgap.txt"
MANIFEST="${WHEEL_DIR}/manifest.txt"

command -v python3 >/dev/null 2>&1 || { echo "[ERROR] python3 not found"; exit 1; }
python3 -m pip --version >/dev/null 2>&1 || {
  echo "[ERROR] python3 -m pip 을 쓸 수 없습니다. 준비 서버에 python3-pip 을 설치하세요."
  exit 1
}

# 이 스크립트는 인터넷이 되는 '준비 서버' 전용입니다. airgap 서버에서 실행하면 pip 이
# 5회 재시도한 뒤에야 실패해서, 정작 원인(여기서 실행하면 안 됨)이 잘 안 보입니다.
if ! python3 -c "import socket; socket.create_connection(('pypi.org', 443), timeout=5).close()" 2>/dev/null; then
  cat >&2 <<'MSG'
[ERROR] pypi.org 에 연결할 수 없습니다.

이 스크립트는 인터넷이 되는 준비 서버에서 실행하는 것입니다.
airgap 서버에서는 wheel 을 받을 수 없으니, 준비 서버에서 아래를 실행한 뒤
install/wheels/ 전체를 airgap 서버의 같은 경로로 복사하세요.

  TARGET_PYTHON=<airgap 서버의 Python 버전> ./install/download_ansible_wheels.sh
  rsync -a install/wheels/ <airgap-host>:<repo>/install/wheels/

airgap 서버에서는 install/install_ansible_airgap.sh 만 실행합니다.
MSG
  exit 1
fi

LOCAL_PYTHON="$(python3 -c 'import sys; print(f"{sys.version_info.major}.{sys.version_info.minor}")')"

# 대상(airgap) 서버의 Python 버전. 준비 서버와 다르면 반드시 지정하세요.
# cryptography / pyyaml / cffi / markupsafe 는 Python 버전별로 다른 wheel 이라,
# 안 맞는 wheelhouse 를 옮기면 대상 서버에서 "No matching distribution found" 가 납니다.
#   TARGET_PYTHON=3.12 ./install/download_ansible_wheels.sh
TARGET_PYTHON="${TARGET_PYTHON:-$LOCAL_PYTHON}"
TARGET_PLATFORM="${TARGET_PLATFORM:-manylinux_2_17_x86_64}"
TARGET_ABI="${TARGET_ABI:-cp${TARGET_PYTHON//./}}"

mkdir -p "$WHEEL_DIR"

pip_args=(--dest "$WHEEL_DIR" --requirement "$REQ_FILE" --only-binary=:all:)
if [[ "$TARGET_PYTHON" != "$LOCAL_PYTHON" ]]; then
  echo "[info] cross-download: 준비 서버=$LOCAL_PYTHON, 대상=$TARGET_PYTHON ($TARGET_PLATFORM/$TARGET_ABI)"
  pip_args+=(
    --python-version "$TARGET_PYTHON"
    --implementation cp
    --abi "$TARGET_ABI"
    --platform "$TARGET_PLATFORM"
  )
else
  echo "[info] 준비 서버와 대상 서버 Python 이 같다고 가정합니다 ($LOCAL_PYTHON)."
  echo "       다르면 TARGET_PYTHON=<대상 버전> 으로 다시 실행하세요."
fi

python3 -m pip download "${pip_args[@]}"

# 여기부터가 이 스크립트에 없던 부분입니다. pip 이 조용히 아무것도 받지 않거나,
# 이 스크립트 자체가 실행되지 않은 채로 install/wheels 가 빈 상태로 대상 서버에
# 넘어가는 사고를 막습니다 (대상 서버에서는 pip 의 모호한 에러로만 드러납니다).
shopt -s nullglob
wheels=("$WHEEL_DIR"/*.whl)
if (( ${#wheels[@]} == 0 )); then
  echo "[ERROR] wheel 이 하나도 없습니다: $WHEEL_DIR" >&2
  exit 1
fi

if ! printf '%s\n' "${wheels[@]##*/}" | grep -qiE '^ansible[_-]core-'; then
  echo "[ERROR] ansible-core wheel 이 없습니다. requirements-airgap.txt 를 확인하세요." >&2
  exit 1
fi

cat > "$MANIFEST" <<MANIFEST_EOF
Generated: $(date -Iseconds)
prep_python: ${LOCAL_PYTHON}
target_python: ${TARGET_PYTHON}
target_platform: ${TARGET_PLATFORM}
target_abi: ${TARGET_ABI}
wheel_count: ${#wheels[@]}

Requirements:
$(cat "$REQ_FILE")

Files:
$(printf '%s\n' "${wheels[@]##*/}" | sort)
MANIFEST_EOF

echo "[done] wheel ${#wheels[@]}개 → ${WHEEL_DIR} (target python ${TARGET_PYTHON})"
echo "[done] manifest: ${MANIFEST}"
echo "[next] install/wheels/ 전체를 대상 서버의 같은 경로로 복사하세요."
