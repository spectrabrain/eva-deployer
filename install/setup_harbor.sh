#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
HARBOR_VERSION="${HARBOR_VERSION:-v2.15.2}"
HARBOR_PORT="32080"
# Harbor's `hostname` is its canonical client-facing address, not the Linux
# hostname.  Leave it unset by default so a standalone Harbor server gets the
# IPv4 selected by its default route.  `localhost` is not usable by Pods or
# another k3s node.
HARBOR_HOSTNAME="${HARBOR_HOSTNAME:-}"
if [[ ${HARBOR_ADMIN_PASSWORD+x} ]]; then
  HARBOR_ADMIN_PASSWORD_EXPLICIT="true"
else
  HARBOR_ADMIN_PASSWORD_EXPLICIT="false"
fi
HARBOR_ADMIN_PASSWORD="${HARBOR_ADMIN_PASSWORD:-EVA123@}"
HARBOR_DATA_VOLUME="${HARBOR_DATA_VOLUME:-}"
HARBOR_PROJECT="${HARBOR_PROJECT:-eva}"
HARBOR_REGISTRY_ENDPOINT="${HARBOR_REGISTRY_ENDPOINT:-}"
HARBOR_ASSET_DIR="${HARBOR_ASSET_DIR:-$SCRIPT_DIR/harbor}"
HARBOR_INSTALL_ROOT="${HARBOR_INSTALL_ROOT:-$HOME/.local/share/eva-harbor}"
HARBOR_ENDPOINT_FILE="${HARBOR_ENDPOINT_FILE:-$SCRIPT_DIR/harbor-endpoint.yaml}"
DOWNLOAD_ONLY="false"
SKIP_INSTALL="false"
SKIP_PROJECT="false"
SKIP_LOGIN="false"

usage() {
  cat <<USAGE
Usage: HARBOR_VERSION=v2.15.2 ./install/setup_harbor.sh [options]

Options:
  --hostname <name>          Harbor canonical client address (DNS name or IP).
                             Default: IPv4 selected from the default route
  --admin-password <value>   Harbor admin password. Default: EVA123@ on first install
  --password <value>         Alias for --admin-password
  --data-volume <path>       Override Harbor data_volume. Default: keep Harbor template default
  --install-root <path>      Harbor runtime/extract path. Default: ${HARBOR_INSTALL_ROOT}
  --asset-dir <path>         Harbor offline installer path. Default: ${HARBOR_ASSET_DIR}
  --project <name>           Harbor project to create. Default: ${HARBOR_PROJECT}
  --registry-endpoint <host:port>
                             Address that k3s nodes and Pods use for Harbor
  --endpoint-file <path>     Write portable Harbor endpoint metadata here
  --download-only            Download offline installer only
  --skip-install             Configure harbor.yml but do not run install.sh
  --skip-project             Do not create Harbor project
  --skip-login               Do not run docker login after project creation
  -h, --help                 Show this help

Environment:
  HARBOR_VERSION             Harbor version to install/download. Default: v2.15.2
  HARBOR_HOSTNAME            Same as --hostname
  HARBOR_ADMIN_PASSWORD      Same as --admin-password
  HARBOR_DATA_VOLUME         Same as --data-volume
  HARBOR_INSTALL_ROOT        Same as --install-root
  HARBOR_ASSET_DIR           Same as --asset-dir
  HARBOR_PROJECT             Same as --project
  HARBOR_REGISTRY_ENDPOINT   Same as --registry-endpoint
  HARBOR_ENDPOINT_FILE       Same as --endpoint-file

Notes:
  - Harbor HTTP port is fixed to ${HARBOR_PORT}.
  - --hostname is written to harbor.yml; it is not the Linux hostname.
    Use a DNS name or IP reachable from all registry clients. If omitted, the
    server's default-route IPv4 is used. Do not use localhost/127.0.0.1.
  - If --data-volume is omitted, the data_volume from harbor.yml.tmpl is preserved.
  - Keep the runtime path outside eva-deployer to avoid rsync overwriting Harbor configuration.
  - On existing Harbor installs, harbor_admin_password is preserved unless explicitly provided.
USAGE
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --hostname)
      HARBOR_HOSTNAME="${2:?--hostname requires a value}"
      shift 2
      ;;
    --admin-password|--password)
      HARBOR_ADMIN_PASSWORD="${2:?--admin-password requires a value}"
      HARBOR_ADMIN_PASSWORD_EXPLICIT="true"
      shift 2
      ;;
    --data-volume)
      HARBOR_DATA_VOLUME="${2:?--data-volume requires a value}"
      shift 2
      ;;
    --install-root)
      HARBOR_INSTALL_ROOT="${2:?--install-root requires a value}"
      shift 2
      ;;
    --asset-dir)
      HARBOR_ASSET_DIR="${2:?--asset-dir requires a value}"
      shift 2
      ;;
    --project)
      HARBOR_PROJECT="${2:?--project requires a value}"
      shift 2
      ;;
    --registry-endpoint)
      HARBOR_REGISTRY_ENDPOINT="${2:?--registry-endpoint requires a value}"
      shift 2
      ;;
    --endpoint-file)
      HARBOR_ENDPOINT_FILE="${2:?--endpoint-file requires a value}"
      shift 2
      ;;
    --download-only)
      DOWNLOAD_ONLY="true"
      shift
      ;;
    --skip-install)
      SKIP_INSTALL="true"
      shift
      ;;
    --skip-project)
      SKIP_PROJECT="true"
      shift
      ;;
    --skip-login)
      SKIP_LOGIN="true"
      shift
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      echo "[ERROR] unknown argument: $1" >&2
      usage
      exit 1
      ;;
  esac
done

if [[ "$HARBOR_VERSION" != v* ]]; then
  echo "[ERROR] HARBOR_VERSION must include the leading 'v'. Example: v2.15.2" >&2
  exit 1
fi

HARBOR_HOSTNAME="${HARBOR_HOSTNAME#http://}"
HARBOR_HOSTNAME="${HARBOR_HOSTNAME#https://}"
HARBOR_HOSTNAME="${HARBOR_HOSTNAME%/}"
if [[ -n "$HARBOR_HOSTNAME" && "$HARBOR_HOSTNAME" == *:* ]]; then
  host_part="${HARBOR_HOSTNAME%%:*}"
  port_part="${HARBOR_HOSTNAME##*:}"
  if [[ "$port_part" == "$HARBOR_PORT" ]]; then
    echo "[info] stripping fixed Harbor port from hostname: $HARBOR_HOSTNAME -> $host_part"
    HARBOR_HOSTNAME="$host_part"
  else
    echo "[ERROR] --hostname must not include a port other than ${HARBOR_PORT}: $HARBOR_HOSTNAME" >&2
    exit 1
  fi
fi

detect_registry_host() {
  local detected
  detected="$(ip -4 route show default 2>/dev/null | awk '
    / src / {
      for (i = 1; i <= NF; i++) if ($i == "src") { print $(i + 1); exit }
    }
  ')"
  if [[ -z "$detected" ]]; then
    detected="$(hostname -I 2>/dev/null | awk '{ print $1 }')"
  fi
  [[ -n "$detected" ]] || {
    echo "[ERROR] internal registry address could not be detected; use --registry-endpoint <host:port>" >&2
    exit 1
  }
  printf '%s\n' "$detected"
}

if [[ -z "$HARBOR_HOSTNAME" ]]; then
  HARBOR_HOSTNAME="$(detect_registry_host)"
  echo "[info] Harbor hostname not specified; using default-route IPv4: $HARBOR_HOSTNAME"
fi

if [[ "$HARBOR_HOSTNAME" == "localhost" || "$HARBOR_HOSTNAME" == "127.0.0.1" ]]; then
  echo "[ERROR] --hostname must be a DNS name or IP reachable by registry clients, not $HARBOR_HOSTNAME" >&2
  echo "        Omit --hostname to auto-detect this server's default-route IPv4." >&2
  exit 1
fi

if [[ -z "$HARBOR_REGISTRY_ENDPOINT" ]]; then
  HARBOR_REGISTRY_ENDPOINT="${HARBOR_HOSTNAME}:${HARBOR_PORT}"
fi
HARBOR_REGISTRY_ENDPOINT="${HARBOR_REGISTRY_ENDPOINT#http://}"
HARBOR_REGISTRY_ENDPOINT="${HARBOR_REGISTRY_ENDPOINT#https://}"
HARBOR_REGISTRY_ENDPOINT="${HARBOR_REGISTRY_ENDPOINT%/}"
[[ "$HARBOR_REGISTRY_ENDPOINT" == *:* ]] || {
  echo "[ERROR] --registry-endpoint must include host:port: $HARBOR_REGISTRY_ENDPOINT" >&2
  exit 1
}

if [[ $EUID -eq 0 ]]; then
  SUDO_CMD=""
elif [[ ${SUDO_CMD+x} ]]; then
  SUDO_CMD="$SUDO_CMD"
else
  SUDO_CMD="sudo"
fi

require_cmd() {
  local cmd="$1"
  command -v "$cmd" >/dev/null 2>&1 || { echo "[ERROR] required command not found: $cmd" >&2; exit 1; }
}

require_cmd curl
require_cmd tar
require_cmd python3

require_docker() {
  require_cmd docker
  run_cmd docker info >/dev/null 2>&1 || {
    echo "[ERROR] Docker daemon is not available. Install and start Docker before Harbor." >&2
    exit 1
  }
  run_cmd docker compose version >/dev/null 2>&1 || {
    echo "[ERROR] Docker Compose plugin is not available. Install docker-compose-plugin before Harbor." >&2
    exit 1
  }
}

load_harbor_images_if_needed() {
  local prepare_image="goharbor/prepare:${HARBOR_VERSION}"
  local image_archive="$HARBOR_DIR/harbor.${HARBOR_VERSION}.tar.gz"

  if run_cmd docker image inspect "$prepare_image" >/dev/null 2>&1; then
    return 0
  fi

  if [[ ! -f "$image_archive" ]]; then
    echo "[ERROR] Harbor prepare image is not available: $prepare_image" >&2
    echo "        Harbor image archive not found: $image_archive" >&2
    exit 1
  fi

  echo "[load] Harbor images: $image_archive"
  run_cmd docker load -i "$image_archive"
}

run_cmd() {
  if [[ -n "$SUDO_CMD" ]]; then
    "$SUDO_CMD" "$@"
  else
    "$@"
  fi
}

read_data_volume() {
  local file="$1"
  python3 - "$file" <<'PY'
import re
import sys
from pathlib import Path

path = Path(sys.argv[1])
if not path.exists():
    sys.exit(0)

for line in path.read_text().splitlines():
    match = re.match(r'^data_volume:\s*(.+?)\s*$', line)
    if match:
        print(match.group(1).strip().strip('"\''))
        break
PY
}

read_harbor_admin_password() {
  local file="$1"
  python3 - "$file" <<'PY'
import re
import sys
from pathlib import Path

path = Path(sys.argv[1])
if not path.exists():
    sys.exit(0)

for line in path.read_text().splitlines():
    match = re.match(r'^harbor_admin_password:\s*(.+?)\s*$', line)
    if match:
        print(match.group(1).strip().strip('"\''))
        break
PY
}

write_harbor_endpoint_metadata() {
  local endpoint_dir
  endpoint_dir="$(dirname "$HARBOR_ENDPOINT_FILE")"
  mkdir -p "$endpoint_dir"
  python3 - "$HARBOR_ENDPOINT_FILE" "$HARBOR_REGISTRY_ENDPOINT" "$HARBOR_PROJECT" <<'PY'
import json
import sys
from pathlib import Path

path = Path(sys.argv[1])
registry = sys.argv[2]
project = sys.argv[3]
path.write_text(
    '# Generated by install/setup_harbor.sh. Copy this file with the USB bundle '
    'to the k3s deployment controller.\n'
    f'harbor_registry: {json.dumps(registry)}\n'
    f'repository_project: {json.dumps(project)}\n'
)
PY
  chmod 0644 "$HARBOR_ENDPOINT_FILE"
  echo "[config] Harbor endpoint metadata: $HARBOR_ENDPOINT_FILE"
  echo "         harbor_registry: $HARBOR_REGISTRY_ENDPOINT"
  echo "         repository_project: $HARBOR_PROJECT"
}

mkdir -p "$HARBOR_ASSET_DIR" "$HARBOR_INSTALL_ROOT"

INSTALLER_NAME="harbor-offline-installer-${HARBOR_VERSION}.tgz"
INSTALLER_PATH="$HARBOR_ASSET_DIR/$INSTALLER_NAME"
INSTALLER_URL="https://github.com/goharbor/harbor/releases/download/${HARBOR_VERSION}/${INSTALLER_NAME}"
HARBOR_DIR="$HARBOR_INSTALL_ROOT/harbor"
HARBOR_YML="$HARBOR_DIR/harbor.yml"
HARBOR_TMPL="$HARBOR_DIR/harbor.yml.tmpl"
HARBOR_COMPOSE="$HARBOR_DIR/docker-compose.yml"
HARBOR_URL="http://${HARBOR_HOSTNAME}:${HARBOR_PORT}"

if [[ ! -f "$INSTALLER_PATH" ]]; then
  echo "[download] $INSTALLER_URL -> $INSTALLER_PATH"
  curl -fL --retry 3 --retry-delay 2 --connect-timeout 20 --max-time 1800 \
    -o "$INSTALLER_PATH" "$INSTALLER_URL"
else
  echo "[skip] installer exists: $INSTALLER_PATH"
fi

if [[ "$DOWNLOAD_ONLY" == "true" ]]; then
  echo "[done] downloaded installer: $INSTALLER_PATH"
  exit 0
fi

require_docker

if [[ ! -x "$HARBOR_DIR/install.sh" ]]; then
  echo "[extract] $INSTALLER_PATH -> $HARBOR_INSTALL_ROOT"
  tar -xzf "$INSTALLER_PATH" -C "$HARBOR_INSTALL_ROOT"
else
  echo "[skip] harbor already extracted: $HARBOR_DIR"
fi

if [[ ! -f "$HARBOR_TMPL" ]]; then
  echo "[ERROR] harbor template not found: $HARBOR_TMPL" >&2
  exit 1
fi

EXISTING_INSTALL="false"
OLD_DATA_VOLUME=""
OLD_HARBOR_ADMIN_PASSWORD=""
if [[ -f "$HARBOR_COMPOSE" ]]; then
  EXISTING_INSTALL="true"
fi
if [[ -f "$HARBOR_YML" ]]; then
  OLD_DATA_VOLUME="$(read_data_volume "$HARBOR_YML")"
  OLD_HARBOR_ADMIN_PASSWORD="$(read_harbor_admin_password "$HARBOR_YML")"
fi

if [[ "$HARBOR_ADMIN_PASSWORD_EXPLICIT" != "true" && -n "$OLD_HARBOR_ADMIN_PASSWORD" ]]; then
  HARBOR_ADMIN_PASSWORD="$OLD_HARBOR_ADMIN_PASSWORD"
  echo "[config] preserving existing harbor_admin_password"
fi

if [[ "$SKIP_INSTALL" != "true" && "$EXISTING_INSTALL" == "true" ]]; then
  require_cmd docker
  load_harbor_images_if_needed
  echo "[down] stopping existing Harbor stack"
  (cd "$HARBOR_DIR" && run_cmd docker compose down)
fi

if [[ -n "$HARBOR_DATA_VOLUME" && -n "$OLD_DATA_VOLUME" && "$OLD_DATA_VOLUME" != "$HARBOR_DATA_VOLUME" ]]; then
  echo "[data] data_volume change detected: $OLD_DATA_VOLUME -> $HARBOR_DATA_VOLUME"
  echo "[data] existing Harbor data is not copied by this script"
  echo "[data] new data_volume will be used after Harbor restarts: $HARBOR_DATA_VOLUME"
  run_cmd mkdir -p "$HARBOR_DATA_VOLUME"
fi

if [[ -f "$HARBOR_YML" ]]; then
  backup="$HARBOR_YML.bak.$(date +%Y%m%d%H%M%S)"
  cp "$HARBOR_YML" "$backup"
  echo "[backup] $backup"
else
  cp "$HARBOR_TMPL" "$HARBOR_YML"
fi

python3 - "$HARBOR_YML" "$HARBOR_HOSTNAME" "$HARBOR_PORT" "$HARBOR_ADMIN_PASSWORD" "$HARBOR_DATA_VOLUME" <<'PY'
import json
import re
import sys
from pathlib import Path

path = Path(sys.argv[1])
hostname = sys.argv[2]
port = sys.argv[3]
admin_password = sys.argv[4]
data_volume = sys.argv[5]

lines = path.read_text().splitlines()
out = []
in_http = False
in_https = False
http_port_written = False
hostname_written = False
password_written = False
data_volume_written = False

key_re = re.compile(r'^[A-Za-z0-9_]+:')

def yaml_string(value: str) -> str:
    return json.dumps(value)

def plain_scalar(value: str) -> str:
    return value

for line in lines:
    is_top_level = bool(key_re.match(line))

    if in_https:
        if not is_top_level:
            continue
        in_https = False

    if is_top_level and line.startswith('https:'):
        # This deployer uses fixed HTTP port 32080. Remove the template HTTPS block
        # so dummy certificate paths do not break Harbor installation.
        in_https = True
        continue

    if is_top_level:
        if in_http and not http_port_written:
            out.append(f'  port: {port}')
            http_port_written = True
        in_http = False

    if line.startswith('hostname:'):
        out.append(f'hostname: {plain_scalar(hostname)}')
        hostname_written = True
        continue

    if line.startswith('http:'):
        out.append('http:')
        in_http = True
        continue

    if in_http and re.match(r'^\s+port:', line):
        out.append(f'  port: {port}')
        http_port_written = True
        continue

    if line.startswith('harbor_admin_password:'):
        out.append(f'harbor_admin_password: {yaml_string(admin_password)}')
        password_written = True
        continue

    if data_volume and line.startswith('data_volume:'):
        out.append(f'data_volume: {plain_scalar(data_volume)}')
        data_volume_written = True
        continue

    out.append(line)

if in_http and not http_port_written:
    out.append(f'  port: {port}')

if not hostname_written:
    out.insert(0, f'hostname: {plain_scalar(hostname)}')

if not password_written:
    out.append(f'harbor_admin_password: {yaml_string(admin_password)}')

if data_volume and not data_volume_written:
    out.append(f'data_volume: {plain_scalar(data_volume)}')

path.write_text('\n'.join(out) + '\n')
PY

echo "[config] harbor.yml updated"
echo "         hostname: $HARBOR_HOSTNAME"
echo "         http.port: $HARBOR_PORT"
echo "         harbor_admin_password: ********"
if [[ -n "$HARBOR_DATA_VOLUME" ]]; then
  echo "         data_volume: $HARBOR_DATA_VOLUME"
else
  echo "         data_volume: keep template default"
fi

if [[ "$SKIP_INSTALL" == "true" ]]; then
  echo "[skip] install.sh skipped"
else
  load_harbor_images_if_needed
  require_cmd docker
  if [[ "$EXISTING_INSTALL" == "true" ]]; then
    echo "[prepare] regenerating Harbor compose files"
    (cd "$HARBOR_DIR" && run_cmd ./prepare)
    echo "[up] starting Harbor stack"
    (cd "$HARBOR_DIR" && run_cmd docker compose up -d)
  else
    echo "[install] running Harbor installer"
    (cd "$HARBOR_DIR" && run_cmd ./install.sh)
  fi
  echo "[status] docker compose ps"
  (cd "$HARBOR_DIR" && run_cmd docker compose ps)
fi

write_harbor_endpoint_metadata

if [[ "$SKIP_PROJECT" == "true" ]]; then
  echo "[skip] project creation skipped"
  exit 0
fi

echo "[wait] Harbor API: $HARBOR_URL"
ready="false"
for _ in $(seq 1 60); do
  if curl -fsS "$HARBOR_URL/api/v2.0/ping" >/dev/null 2>&1; then
    ready="true"
    break
  fi
  sleep 2
done

if [[ "$ready" != "true" ]]; then
  echo "[ERROR] Harbor API did not become ready: $HARBOR_URL" >&2
  exit 1
fi

tmp_response="$(mktemp)"
trap 'rm -f "$tmp_response"' EXIT
status="$(curl -sS -o "$tmp_response" -w '%{http_code}' \
  -u "admin:${HARBOR_ADMIN_PASSWORD}" \
  -H 'Content-Type: application/json' \
  -X POST "$HARBOR_URL/api/v2.0/projects" \
  -d "{\"project_name\":\"${HARBOR_PROJECT}\",\"public\":false,\"metadata\":{\"public\":\"false\"}}")"

if [[ "$status" == "201" || "$status" == "409" ]]; then
  echo "[project] ready: $HARBOR_PROJECT"
else
  echo "[ERROR] failed to create Harbor project: HTTP $status" >&2
  cat "$tmp_response" >&2
  exit 1
fi

if [[ "$SKIP_LOGIN" == "true" ]]; then
  echo "[skip] docker login skipped"
else
  registry="${HARBOR_HOSTNAME}:${HARBOR_PORT}"
  echo "[login] docker login $registry -u admin"
  if ! printf '%s' "$HARBOR_ADMIN_PASSWORD" | run_cmd docker login "$registry" -u admin --password-stdin; then
    echo "[warn] docker login failed. Check Docker insecure registry/certificate settings if Harbor uses HTTP or self-signed TLS."
  fi
fi

echo "[done] Harbor is ready: $HARBOR_URL"
echo "       project: $HARBOR_PROJECT"
echo "       repository_registry: $HARBOR_REGISTRY_ENDPOINT"
