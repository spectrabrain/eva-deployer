# EVA Cloud Infra E2E Runbook

개발용 Ubuntu 서버에서 Cloud Repository 기반 Infra 설치를 처음부터 검증하는 절차다.
이 문서는 Base Release artifact, system-wide `eva`, Workspace, Managed Runtime, Plan/Apply 흐름을 확인한다.

이 절차는 Docker, k3s, NFS 설정을 변경한다. 재사용할 서버가 아니라 폐기하거나 snapshot으로 되돌릴 수 있는 개발 VM에서만 실행한다.

## 범위와 전제

| 항목 | 기준 |
| --- | --- |
| 설치 방식 | `repository.mode: cloud` |
| 첫 E2E 범위 | `precondition` + `infra` component |
| 권장 대상 | Ubuntu 24.04, linux/amd64, 단일 개발 VM |
| Ansible 대상 | 같은 VM의 `ansible_connection=local` |
| 네트워크 | GitHub, Docker Hub, AWS/ECR/S3 등 외부 HTTPS 접근 가능 |
| 권한 | installer 및 target become을 위한 `sudo` 가능 사용자 |

IAM, App, Agent, Vision, n8n은 Infra 검증이 성공한 뒤 별도 run에서 추가한다. 실제 고객 Secret, AWS key, TLS key, license는 이 문서의 Workspace에 넣지 않는다.

## 0. 개발 VM 확인

```bash
lsb_release -ds
test "$(uname -m)" = x86_64
sudo -v

if ! command -v unzip >/dev/null 2>&1; then
  sudo apt-get update
  sudo apt-get install -y unzip
fi
```

## 1. Release 설치

GitHub Actions Tag Base Release artifact zip을 개발 VM으로 옮긴다. 압축을 푼 디렉터리가 Release root여야 한다.

```bash
RELEASE_TAG=v3.2.0
RELEASE_ZIP="eva-base-release-${RELEASE_TAG}.zip"
RELEASE_DIR="${RELEASE_ZIP%.zip}"

unzip -q "$RELEASE_ZIP" -d "$RELEASE_DIR"
RELEASE_DIR="$(cd "$RELEASE_DIR" && pwd)"
cd "$RELEASE_DIR"

sudo bash ./eva-tool-installer.sh
command -v eva
eva version
```

Installer는 같은 디렉터리의 단일 `eva-tool_*_linux_amd64.tar.gz`와 `checksums.sha256` matching entry를 자동 검증한다. archive가 여러 개이거나 checksum이 없으면 중단하므로, 다른 Release 파일을 이 디렉터리에 섞지 않는다.

설치 사용자는 `eva-operators` 그룹에 추가된다. 현재 shell에는 새 group membership이 없을 수 있으므로 로그아웃/로그인한 뒤 아래를 확인한다.

```bash
id -nG | tr ' ' '\n' | grep -x eva-operators
```

## 2. Release 검증 및 준비

Release metadata, platform, artifact checksum을 확인하고 `/opt/eva/releases/<version>/`에 Infra/Solution source를 준비한다.

```bash
cd "$RELEASE_DIR"
eva verify .
eva release show --release .
eva release prepare --release .
```

`release prepare`가 출력한 경로를 이후 `RELEASE_ROOT`로 사용한다. 일반적으로 version은 `release.yaml`의 `version`이며 준비 경로는 `/opt/eva/releases/<version>`이다.

```bash
RELEASE_VERSION="$(awk '/^version:/{print $2; exit}' release.yaml)"
RELEASE_ROOT="/opt/eva/releases/$RELEASE_VERSION"
test -f "$RELEASE_ROOT/ansible.cfg"
test -f "$RELEASE_ROOT/src/infra/playbooks/site_infra.yaml"
```

## 3. Managed Runtime 준비

Cloud Runtime bootstrap은 Ansible과 ansible-core의 version을 고정해 Python venv에 설치하고, standalone Helm, kubectl, kustomize, ORAS 4개 도구만 고정 SHA-256을 검증해 staging Runtime에 구성한다. 검증이 모두 성공한 경우에만 `/opt/eva/runtime`으로 원자적으로 교체한다. 따라서 bootstrap 실패는 기존 Managed Runtime을 변경하지 않는다.

Bootstrap은 linux/amd64 Cloud 환경을 지원하며 `python3-venv`가 필요하다. Runtime 안의 Ansible 의존성은 PyPI에서 다운로드하므로, 이 단계에서는 외부 HTTPS 연결이 필요하다. 시스템 PATH의 동명 도구는 Apply에 사용하지 않는다.

```bash
sudo apt-get update
sudo apt-get install -y ca-certificates python3 python3-venv

sudo eva runtime bootstrap
eva runtime validate
eva runtime show
eva exec ansible-playbook --version
eva exec helm version --short
eva exec kubectl version --client
eva exec kustomize version
eva exec oras version
```

`eva runtime show`는 descriptor와 검증된 각 실행 파일의 절대 경로를 출력한다. Runtime validation에 실패하면 Infra Apply를 진행하지 않는다. Airgap artifact가 있는 경우에는 기존처럼 `eva runtime bootstrap --offline <eva-offline.tar.gz>`를 사용한다.

## 4. Infra 전용 Workspace 작성

이번 검증은 system-wide site workspace를 사용한다. `SITE_ID`는 다른 E2E 실행과 겹치지 않는 값으로 정한다.

```bash
SITE_ID=dev-infra-e2e
WORKSPACE="/etc/eva/sites/$SITE_ID"

sudo install -d -o root -g eva-operators -m 2770 \
  "$WORKSPACE" \
  "$WORKSPACE/inventory" \
  "$WORKSPACE/site-values"

sudo tee "$WORKSPACE/site-values/site.yaml" >/dev/null <<EOF
site:
  id: $SITE_ID

repository:
  mode: cloud
  project: eva

components:
  infra: true
  iam: false
  agent: false
  vision: false
  app: false
  n8n: false
EOF

sudo tee "$WORKSPACE/inventory/inventory.ini" >/dev/null <<EOF
[local]
$SITE_ID ansible_connection=local ansible_become=true
EOF

sudo chown -R root:eva-operators "$WORKSPACE"
sudo chmod 2770 "$WORKSPACE" "$WORKSPACE/inventory" "$WORKSPACE/site-values"
sudo chmod 0640 "$WORKSPACE/site-values/site.yaml" "$WORKSPACE/inventory/inventory.ini"
```

로컬 대상의 privilege escalation은 현재 사용자의 sudo timestamp를 사용한다. Apply 직전에 인증을 갱신한다.

```bash
sudo -v
eva workspace validate --site "$SITE_ID"
eva workspace show --site "$SITE_ID"
```

## 5. Plan 확인

Infra 범위의 Plan은 `precondition`을 먼저 넣고 그 다음 `infra`를 실행해야 한다. 먼저 stdout Plan을 검토한 뒤 저장 가능한 operation을 만든다.

```bash
eva plan \
  --site "$SITE_ID" \
  --release "$RELEASE_ROOT" \
  --component infra

eva plan \
  --site "$SITE_ID" \
  --release "$RELEASE_ROOT" \
  --component infra \
  --save

eva status
```

Plan에 `site_precondition.yaml`, `site_infra.yaml` 외의 Solution playbook이 들어 있으면 여기서 중단하고 `site-values/site.yaml`의 component 설정과 `--component infra` 입력을 확인한다.

## 6. Infra Apply

Apply는 저장된 최신 operation을 실행한다. 비대화형 E2E에서는 `--yes`가 필요하다.

```bash
sudo -v
eva apply --yes
eva status
```

실패하면 operation ID와 log를 보존한 채 원인을 확인한다. 같은 문제가 해결되기 전에는 새 Plan을 만들지 않는다.

```bash
eva status
ls -lt /var/log/eva/operations
```

## 7. Infra 결과 확인

성공한 Apply 뒤 Managed Runtime을 통해 k3s 상태를 확인한다.

```bash
eva exec kubectl get nodes -o wide
eva exec kubectl get pods -A
eva exec helm version --short

sudo systemctl is-active docker
sudo systemctl is-active k3s
```

아래 파일은 해당 site의 precondition 결과다. 공개 IP, DNS, 외부 HTTPS 접근성, AWS CLI installer download 검증 결과를 검토한다.

```bash
find "$RELEASE_ROOT/out/work/config/$SITE_ID" -name precondition.yaml -type f -print
```

`out/work`은 Release 준비 경로가 아니라 Ansible control node의 실행 작업 경로다. 기본 CLI Apply는 prepared Release를 control node로 사용하므로, 해당 경로는 `/opt/eva/releases/<version>/out/work/` 아래에 생성된다.

## 8. 다음 E2E 범위

Infra가 성공하면 같은 Release와 Workspace에 필요한 component만 하나씩 활성화한다.

1. `iam: true`로 변경 후 `eva install --site "$SITE_ID" --release "$RELEASE_ROOT" --component iam`
2. `agent` 또는 `vision` 전에 `config` 선행 단계가 Plan에 포함되는지 확인
3. `app`은 IAM handoff와 license 입력이 준비된 뒤 검증
4. `n8n`은 마지막에 별도 component로 검증

Cloud 모드에서 target AWS credential이 필요한 Solution component를 검증할 때만 `$WORKSPACE/credentials/aws_key.ini`를 `0600`으로 별도 제공한다. Infra-only E2E에는 이 credential을 만들지 않는다.

## 9. 종료와 정리

현재 CLI에는 Infra uninstall이 없다. 개발 E2E 종료 후에는 개별 k3s/Docker 파일을 수동 삭제하는 대신 VM을 폐기하거나 실행 전 snapshot으로 되돌린다. 이 방법이 Docker, k3s, package, systemd 변경을 남기지 않는 유일한 재현 가능한 정리 방식이다.
