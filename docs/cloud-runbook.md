# EVA Cloud 설치 가이드

Cloud Repository 기반으로 Ubuntu 서버에 EVA Infrastructure와 Solution을 설치하는 절차다.
이 문서는 Base Release artifact, system-wide `eva`, Workspace, Managed Runtime, IAM을 포함한 기본 설치 흐름을 다룬다.

이 절차는 Docker, k3s, NFS 설정을 변경한다. 설치 전에 서비스 영향과 변경 창을 확인하고, 필요한 경우 서버 backup 또는 snapshot을 준비한다.

## 범위와 전제

| 항목 | 기준 |
| --- | --- |
| 설치 방식 | `repository.mode: cloud` |
| 기본 설치 범위 | `precondition` + `infra` + `config` + `iam` + `agent` + `vision` + `app` component |
| 권장 대상 | Ubuntu 24.04, linux/amd64, 단일 control/target host |
| Ansible 대상 | 같은 VM의 `ansible_connection=local` |
| 네트워크 | GitHub, Docker Hub, AWS/ECR/S3 등 외부 HTTPS 접근 가능 |
| 권한 | installer 및 target become을 위한 `sudo` 가능 사용자 |

기본 Solution component는 IAM, Agent, Vision, App이다. n8n은 필요한 경우에만 추가한다. 사이트별 Secret, AWS key, TLS key, license는 Release source가 아닌 workspace에만 두고 source repository나 Release artifact에 포함하지 않는다.

## 0. 설치 대상 확인

```bash
lsb_release -ds
test "$(uname -m)" = x86_64
sudo -v

if ! command -v unzip >/dev/null 2>&1; then
  sudo apt-get update
  sudo apt-get install -y unzip
fi
```

## 1. EVA Tool 설치

GitHub Actions Tag Base Release artifact zip을 설치 대상 서버로 옮긴다. 압축을 푼 디렉터리가 Release root여야 한다.

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

설치 사용자는 `eva-operators` 그룹에 추가된다. 현재 shell에는 새 group membership이 없을 수 있으므로 로그아웃/로그인한 뒤 Release root를 다시 지정하고 아래를 확인한다.

```bash
RELEASE_DIR=/absolute/path/to/eva-base-release-v3.2.0
cd "$RELEASE_DIR"
id -nG | tr ' ' '\n' | grep -x eva-operators
```

## 2. Release 검증

Release metadata, platform, artifact checksum을 확인한다. `eva install`은 설치 중 검증된 Release를 `/opt/eva/releases/<version>/`에 자동으로 준비한다.

```bash
cd "$RELEASE_DIR"
eva verify .
eva release show --release .
```

일반적으로 version은 `release.yaml`의 `version`이며 설치 후 준비 경로는 `/opt/eva/releases/<version>`이다.

```bash
RELEASE_VERSION="$(awk '/^version:/{print $2; exit}' release.yaml)"
RELEASE_ROOT="/opt/eva/releases/$RELEASE_VERSION"
```

## 3. Managed Runtime 준비

Cloud Runtime bootstrap은 Ansible과 ansible-core의 version을 고정해 Python venv에 설치하고, `ansible.posix` collection도 `2.2.2`로 고정한다. standalone Helm, kubectl, kustomize, ORAS 4개 도구는 고정 SHA-256을 검증해 staging Runtime에 구성한다. 검증이 모두 성공한 경우에만 `/opt/eva/runtime`으로 원자적으로 교체한다. 따라서 bootstrap 실패는 기존 Managed Runtime을 변경하지 않는다.

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

`eva runtime show`는 descriptor와 검증된 각 실행 파일의 절대 경로를 출력한다. Runtime validation에 실패하면 Cloud 설치를 진행하지 않는다. Airgap artifact가 있는 경우에는 기존처럼 `eva runtime bootstrap --offline <eva-offline.tar.gz>`를 사용한다.

## 4. Cloud Workspace 작성

system-wide site workspace를 사용한다. `SITE_ID`는 설치 site를 식별하는 고유한 값으로 정한다.

```bash
SITE_ID=dev-infra-196
ANSIBLE_HOST=dev-infra-196
IAM_PUBLIC_HOST=iam196.eva-dev.lge.com
APP_PUBLIC_HOST=app196.eva-dev.lge.com
WORKSPACE="/etc/eva/sites/$SITE_ID"

sudo install -d -o root -g eva-operators -m 2770 \
  "$WORKSPACE" \
  "$WORKSPACE/inventory" \
  "$WORKSPACE/site-values" \
  "$WORKSPACE/credentials"

sudo tee "$WORKSPACE/site-values/site.yaml" >/dev/null <<EOF
site:
  id: $SITE_ID

repository:
  mode: cloud
  project: eva

components:
  infra: true
  iam: true
  agent: true
  vision: true
  app: true
  n8n: false
EOF

sudo tee "$WORKSPACE/inventory/inventory.ini" >/dev/null <<EOF
[local]
$ANSIBLE_HOST ansible_connection=local ansible_become=true
EOF

sudo chown -R root:eva-operators "$WORKSPACE"
sudo chmod 2770 "$WORKSPACE" "$WORKSPACE/inventory" "$WORKSPACE/site-values" "$WORKSPACE/credentials"
sudo chmod 0640 "$WORKSPACE/site-values/site.yaml" "$WORKSPACE/inventory/inventory.ini"
```

Agent와 Vision은 지원 NVIDIA GPU가 있는 서버에서 활성화한다. GPU가 없는 서버에서는 `site.yaml`의 `agent`, `vision`을 `false`로 변경한다.

Cloud 설치에 필요한 AWS credential은 Workspace에 준비한다. 실제 key는 source repository나 Release artifact에 포함하지 않는다.

```bash
sudo install -o root -g eva-operators -m 0600 /dev/null \
  "$WORKSPACE/credentials/aws_key.ini"
sudoedit "$WORKSPACE/credentials/aws_key.ini"
```

파일에는 `aws_access_key_id`, `aws_secret_access_key`, `region`을 입력한다.

## 5. IAM Workspace Values 작성

`iam.yaml`의 최상위 host key는 inventory hostname과 같아야 한다. 아래 예시에서 그 값은 `dev-infra-196`이다. 여기의 `ANSIBLE_HOST`는 shell 변수로 정한 inventory hostname이며, inventory에 별도 `ansible_host=<값>`을 설정하면 최상위 key도 그 값으로 변경해야 한다. `IAM_PUBLIC_HOST`는 IAM ingress와 Keycloak이 사용하는 공개 주소이므로 Ansible 대상 이름과 구분한다. realm administrator password는 사이트별 값으로 교체하고, 이 파일을 source repository나 Release artifact에 추가하지 않는다.

```bash
sudo install -o root -g eva-operators -m 0640 /dev/null \
  "$WORKSPACE/site-values/iam.yaml"
sudo tee "$WORKSPACE/site-values/iam.yaml" >/dev/null <<EOF
$ANSIBLE_HOST:
  config:
    host: $IAM_PUBLIC_HOST

  keycloak:
    realmPatch:
      realmAdmin:
        password: "EVAEVA123@"
      evaApp:
        redirectUris:
          - "https://$APP_PUBLIC_HOST/*"
    realmConfig:
      substitutions:
        EVA_IAM_ADMIN_EMAIL: "admin@eva.com"

  ingress:
    path: /
    tls:
      hostPath: /home/eva/certs

  imagePullSecrets:
    hostPath: /home/eva/.aws

  redis:
    tls:
      enabled: true
    external:
      enabled: true
      nodePort: 32070
EOF

```

`/home/eva/certs`에는 TLS certificate와 key가, `/home/eva/.aws`에는 IAM image pull에 필요한 AWS credential이 있어야 한다. Redis external NodePort `32070`은 cluster 전체에서 사용 중이지 않아야 한다.

## 6. EVA App Workspace Values 작성

`app.yaml`에는 사이트별 표시 이름, backend 주소, license처럼 설치자가 결정하는 값만 작성한다. IAM SSO 값, Config 단계의 worker 수, repository별 image 주소와 tag는 설치 중 자동으로 구성된다.

```bash
sudo install -o root -g eva-operators -m 0640 /dev/null \
  "$WORKSPACE/site-values/app.yaml"
sudo tee "$WORKSPACE/site-values/app.yaml" >/dev/null <<EOF
$ANSIBLE_HOST:
  app:
    browserTitleName: "EVA DEV(196)"
    backendHost: "$APP_PUBLIC_HOST"
    license:
      activation_mode: "online"
      product_code: "eva-dev"
      api_key: "<SITE_API_KEY>"
      shared_key: "<SITE_SHARED_KEY>"
EOF
```

Workspace values는 자동 생성값보다 우선한다. 필요한 경우에만 의도적인 override를 추가하며, IAM handoff나 worker 수를 중복 작성하지 않는다.

중앙 IAM이 다른 서버 또는 Workspace에 있는 경우에는 generated handoff를 찾거나 복사하지 않는다. 해당 App Workspace의 `app.yaml`에 중앙 IAM 연동값을 명시한다.

```yaml
<app-target>:
  app:
    sso:
      baseUrl: https://central-iam.example.com
      adminClientSecret: <CENTRAL_IAM_ADMIN_CLIENT_SECRET>
```

## 7. 설치 Plan 확인

현재 디렉터리가 Release root인 상태에서 Workspace를 검증하고 Plan을 확인한다. 기본 component 설정이면 순서는 `precondition → infra → config → iam → agent → vision → app`이다.

```bash
sudo -v
eva workspace validate --workspace "$WORKSPACE"
eva workspace show --workspace "$WORKSPACE"
eva plan \
  --workspace "$WORKSPACE" \
  --release .
```

## 8. Infrastructure와 Solution 설치

Plan을 확인한 뒤 같은 Workspace와 Release로 한 번만 설치를 실행한다. 이 명령은 Release와 Workspace를 검증하고 Plan을 저장한 뒤 활성화된 Infrastructure와 Solution component를 순서대로 적용한다.

```bash
sudo -v
eva install \
  --workspace "$WORKSPACE" \
  --release . \
  --yes
eva status
```

`--workspace`는 설치 입력이 있는 site workspace를 지정하고, `--release .`은 현재 Release root를 지정한다. `--yes`는 Plan 적용에 동의하는 비대화형 옵션이다. 위 예시의 `dev-infra-196`은 설치 site ID에 맞는 경로로 바꾼다.

Cloud 설치는 Operation을 `running`으로 바꾸기 전에 control node의 `apt-get update`를 진단한다. Jenkins LTS 2026 signing key 누락처럼 EVA가 지원하는 오류가 발견되면, interactive session에서만 변경 대상과 위험을 표시하고 별도 승인을 요청한다. `eva install --yes`의 `--yes`는 설치 실행 동의일 뿐 APT repository 변경 동의가 아니다.

비대화형 실행은 자동 수정 없이 중단한다. 진단과 명시적 복구는 아래 명령으로 수행한다.

```bash
eva troubleshoot apt
eva troubleshoot apt --fix-known --yes
```

`eva troubleshoot apt`의 진단에도 아래의 동등한 수동 복구 절차가 표시된다. 수동으로 수행할 때는 Jenkins 공식 키와 source를 함께 갱신한다.

```bash
sudo install -d -m 0755 /etc/apt/keyrings
curl -fsSL \
  https://pkg.jenkins.io/debian-stable/jenkins.io-2026.key \
  | sudo tee /etc/apt/keyrings/jenkins-keyring.asc >/dev/null
echo "deb [signed-by=/etc/apt/keyrings/jenkins-keyring.asc] https://pkg.jenkins.io/debian-stable binary/" \
  | sudo tee /etc/apt/sources.list.d/jenkins.list >/dev/null
sudo chmod 0644 /etc/apt/keyrings/jenkins-keyring.asc
sudo chmod 0644 /etc/apt/sources.list.d/jenkins.list
sudo apt-get update
```

`eva troubleshoot apt --fix-known --yes`는 이 수동 절차에 더해 key fingerprint 검증, 기존 keyring/source 백업, 갱신 실패 시 복원을 수행한다.

설치가 실패하면 operation ID와 log를 보존한 채 원인을 확인한다.

```bash
eva status
ls -lt /var/log/eva/operations
```

원인을 해결한 뒤에는 실패한 최신 Operation을 새 ID로 복제해 재시도한다.

```bash
eva retry --yes
eva status
```

## 9. 설치 결과 확인

설치가 성공하면 Managed Runtime을 통해 Infrastructure와 Solution 상태를 확인한다.

```bash
eva exec kubectl get nodes -o wide
eva exec kubectl get pods -A
eva exec helm version --short
eva exec kubectl get all -n eva-iam
eva exec kubectl get ingress -n eva-iam
eva exec kubectl get all -n eva-agent
eva exec kubectl get all -n eva-vision
eva exec kubectl get all -n eva-app
eva exec kubectl get ingress -n eva-app

sudo systemctl is-active docker
sudo systemctl is-active k3s
```

아래 파일은 해당 site의 precondition 결과다. 공개 IP, DNS, 외부 HTTPS 접근성, AWS CLI installer download 검증 결과를 검토한다.

```bash
find "$RELEASE_ROOT/out/work/config/$SITE_ID" -name precondition.yaml -type f -print
```

`out/work`은 Release 준비 경로가 아니라 Ansible control node의 실행 작업 경로다. 기본 CLI 설치는 prepared Release를 control node로 사용하므로, 해당 경로는 `/opt/eva/releases/<version>/out/work/` 아래에 생성된다.

## 10. 감사와 문제 분석

App의 chart defaults, Helm에 전달한 effective input values, Helm이 반환한 resolved values는 target별로 아래 경로에 보존된다.

```text
$RELEASE_ROOT/out/work/rendered/<site>/<target>/app/
  chart-defaults.yaml              # 0644
  effective-input-values.yaml      # 0600
  resolved-values.yaml             # 0600
  values-sources.yaml              # 0644, secret 없는 source metadata
```

IAM handoff는 `$RELEASE_ROOT/out/work/config/<site>/<target>/eva-iam.yaml`에 `0600`으로 보존된다. 정상 설치 중 설치자가 열거나 App values에 복사할 필요는 없으며, IAM 또는 App 장애 분석이 필요한 경우에만 권한 있는 운영자가 확인한다.

## 11. 선택 Component 설치

필요한 component만 설치할 때는 `--component`를 사용한다. 예를 들어 App만 설치하려면 다음과 같이 실행한다.

```bash
eva install \
  --workspace "$WORKSPACE" \
  --release . \
  --component app \
  --yes
```

Agent, Vision, App을 선택하면 필요한 Config 단계는 CLI가 자동으로 포함하며, 선택하지 않은 다른 제품 component는 실행하지 않는다. IAM, Agent, Vision, App도 같은 방식으로 각각 `--component iam`, `--component agent`, `--component vision`, `--component app`을 지정한다. n8n은 필요한 경우에만 `site.yaml`에서 활성화해 선택한다.

## 12. 변경 관리와 정리

현재 CLI에는 Infrastructure 또는 Solution uninstall이 없다. 설치를 제거해야 하는 경우 개별 k3s/Docker 파일을 수동 삭제하는 대신, 설치 전 준비한 backup 또는 snapshot으로 복원하거나 서버 교체 절차를 사용한다. 이 방법이 Docker, k3s, package, systemd 변경을 일관되게 되돌리는 방법이다.
