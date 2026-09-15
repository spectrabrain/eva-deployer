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

## 1. EVA Tool 설치

GitHub Actions Tag Base Release artifact zip 하나를 빈 작업 디렉터리에 옮긴다. 압축을 푼 디렉터리가 Release root여야 한다.

```bash
unzip -q eva-base-release-*.zip -d eva-base-release
cd eva-base-release

sudo bash ./eva-tool-installer.sh
command -v eva
eva version
```

Installer는 같은 디렉터리의 단일 `eva-tool_*_linux_amd64.tar.gz`와 `checksums.sha256` matching entry를 자동 검증한다. archive가 여러 개이거나 checksum이 없으면 중단하므로, 다른 Release 파일을 이 디렉터리에 섞지 않는다.

Installer는 계정이나 group membership을 변경하지 않는다. Tool, Runtime, Release는 일반 사용자가 조회할 수 있으며, Workspace의 Secret과 Operation 상태·로그는 `sudo` 권한으로만 읽고 변경한다. 따라서 이미 `sudo` 권한이 있는 `eva` 또는 DevOps 계정은 재로그인 없이 바로 사용할 수 있다.

## 2. Release 검증

Release metadata, platform, artifact checksum을 확인한다. `eva install`은 설치 중 검증된 Release를 `/opt/eva/releases/<version>/`에 자동으로 준비한다.

```bash
eva verify .
```

`eva install`은 검증된 Release를 `/opt/eva/releases/<version>/`에 자동으로 준비한다. 정상 설치에는 Release 경로 또는 version을 별도 변수로 설정할 필요가 없다.

## 3. Managed Runtime 준비

Cloud Runtime bootstrap은 필요한 APT package index를 갱신하고 `ca-certificates`, `python3`, `python3-venv`를 준비한 뒤 Ansible과 ansible-core의 version을 고정해 Python venv에 설치한다. `ansible.posix` collection은 `2.2.2`로 고정하며, standalone Helm, kubectl, kustomize, ORAS 4개 도구는 고정 SHA-256을 검증해 staging Runtime에 구성한다. staging Runtime 검증이 모두 성공한 경우에만 `/opt/eva/runtime`으로 원자적으로 교체하므로 bootstrap 실패는 기존 Managed Runtime을 변경하지 않는다.

Bootstrap은 linux/amd64 Cloud 환경을 지원한다. Runtime 안의 Ansible 의존성은 PyPI에서 다운로드하므로, 이 단계에서는 외부 HTTPS 연결이 필요하다. 시스템 PATH의 동명 도구는 Apply에 사용하지 않는다.

```bash
sudo eva runtime bootstrap
```

`sudo eva runtime bootstrap`은 Runtime descriptor와 모든 managed executable을 검증한 뒤에만 설치를 완료한다. Airgap artifact가 있는 경우에는 기존처럼 `sudo eva runtime bootstrap --offline <eva-offline.tar.gz>`를 사용한다.

## 4. Workspace 입력 파일 준비

설치자는 원하는 절대 Workspace 경로에 사이트별 입력 파일 다섯 개만 준비한다. 표준 경로는 `/etc/eva/sites/<site-id>/`이며, `/home/eva/site-dev-196/`처럼 외부 경로도 사용할 수 있다. 아래 예시는 site ID가 `site-dev-196`인 경우다.

```text
<workspace>/
├── credentials/
│   └── aws_key.ini
├── inventory/
│   └── inventory.ini
└── site-values/
    ├── site.yaml
    ├── iam.yaml
    └── app.yaml
```

아래 예시는 `site-dev-196` site와 같은 이름의 local Ansible target을 기준으로 한다. site ID, hostname, public host, Secret은 설치 환경의 값으로 바꾼다.

### `site-values/site.yaml`

기본 설치 component를 선택한다. GPU가 없는 서버에서는 `agent`, `vision`을 `false`로 설정한다.

```yaml
site:
  id: site-dev-196

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
```

### `inventory/inventory.ini`

단일 서버 설치는 local inventory를 사용한다. `site-dev-196`은 아래 IAM/App values의 최상위 key와 같아야 한다.

```ini
[eva]
site-dev-196 ansible_connection=local ansible_user=eva ansible_password='!234qwer' ansible_become=true ansible_become_password='!234qwer'
```

### `credentials/aws_key.ini`

Cloud repository와 image pull에 사용할 사이트별 AWS credential을 작성한다.

```ini
aws_access_key_id = <AWS_ACCESS_KEY_ID>
aws_secret_access_key = <AWS_SECRET_ACCESS_KEY>
region = ap-northeast-2
```

`credentials/aws_key.ini`는 EVA Workspace의 입력 파일이다. `awscli` role은 선택된 Workspace의 이 파일만 읽어 inventory target user의 `~/.aws/credentials`와 `~/.aws/config`을 구성한다. Workspace root나 `/home/eva/.aws`에 별도의 `aws_key.ini`를 둘 필요는 없다.

### `site-values/iam.yaml`

inventory hostname을 최상위 key로 사용한다. IAM public host, TLS 경로, realm administrator password, App redirect URI와 image pull 경로를 설치 환경에 맞게 작성한다.

```yaml
site-dev-196:
  config:
    host: iam196.eva-dev.lge.com

  keycloak:
    realmPatch:
      realmAdmin:
        password: "<REALM_ADMIN_PASSWORD>"
      evaApp:
        redirectUris:
          - "https://app196.eva-dev.lge.com/*"
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
      nodePort: 32079
```

`/home/eva/certs`에는 TLS certificate와 key가, `/home/eva/.aws`에는 IAM image pull에 필요한 AWS credential이 있어야 한다. Redis external NodePort `32070`은 cluster 전체에서 사용 중이지 않아야 한다.

### `site-values/app.yaml`

inventory hostname을 최상위 key로 사용한다. App 표시명, backend host, license처럼 사이트별 입력만 작성한다. IAM SSO 값과 Config worker 수는 같은 Workspace에서 IAM과 App을 함께 설치하면 자동으로 구성된다.

```yaml
site-dev-196:
  app:
    browserTitleName: "EVA DEV(196)"
    backendHost: "app196.eva-dev.lge.com"
    license:
      activation_mode: "online"
      product_code: "eva-dev"
      api_key: "<SITE_API_KEY>"
      shared_key: "<SITE_SHARED_KEY>"
    sso:
      redis:
        port: 32079
        db: 0
        password: eva-redis-pass
```

`aws_key.ini`, `iam.yaml`, `app.yaml`에는 credential 또는 Secret이 포함될 수 있으므로 조직의 승인된 Secret 관리 절차로 작성하고 Release artifact나 source repository에 넣지 않는다. Runtime과 CLI는 `sudo eva ...`로 실행하므로 root가 이 파일을 읽을 수 있어야 한다.

중앙 IAM을 별도 서버 또는 Workspace에 두는 경우에는 해당 App Workspace의 `app.yaml`에 `app.sso.baseUrl`과 `app.sso.adminClientSecret`을 명시한다. 다른 Workspace의 generated handoff를 복사하거나 탐색하지 않는다.

## 5. Infrastructure와 Solution 설치

현재 디렉터리가 Release root인 상태에서 한 번만 설치를 실행한다. 이 명령은 Workspace와 Release를 검증하고 Plan 요약을 출력한 뒤 `precondition → infra → config → iam → agent → vision → app` 순서로 적용한다. Workspace 위치에 맞는 명령 하나만 사용한다.

```bash
sudo eva install . --workspace /home/eva/site-dev-196 --yes
```

외부 Workspace는 해당 절대 경로를 `--workspace`에 전달한다. `--yes`는 Plan 적용에 동의하는 비대화형 옵션이다.
Workspace를 /etc/eva/sites/<site-id>에 배치한 경우에만 --workspace 대신 --site <site-id>를 사용할 수 있다.

Cloud 설치는 Operation을 `running`으로 바꾸기 전에 control node의 `apt-get update`를 진단한다. Jenkins LTS 2026 signing key 누락처럼 EVA가 지원하는 오류가 발견되면, interactive session에서만 변경 대상과 위험을 표시하고 별도 승인을 요청한다. `eva install --yes`의 `--yes`는 설치 실행 동의일 뿐 APT repository 변경 동의가 아니다.

비대화형 실행은 자동 수정 없이 중단한다. 진단과 명시적 복구는 아래 명령으로 수행한다.

```bash
sudo eva troubleshoot apt
sudo eva troubleshoot apt --fix-known --yes
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
sudo eva status
sudo ls -lt /var/log/eva/operations
```

원인을 해결한 뒤에는 실패한 최신 Operation을 새 ID로 복제해 재시도한다.

```bash
sudo eva retry --yes
sudo eva status
```

## 6. 설치 결과 확인

설치 결과는 아래 한 명령으로 확인한다. 최신 Operation에서 선택한 EVA component만 확인하며, 정상 상태에서는 핵심 요약만 출력한다.

```bash
sudo eva check
```

`eva check`는 Runtime version, Docker, k3s, Kubernetes node readiness, 각 선택 component의 workload/Pod/Ingress readiness, 최신 Operation 상태를 확인한다. 문제가 있으면 실패 항목과 다음 확인 명령만 출력한다.

```bash
sudo eva check --verbose
sudo eva status
```

`--verbose`는 실패한 component에 한해 unhealthy Pod, workload readiness, service, 최근 Kubernetes event를 출력한다. 정상 상태에서는 raw Kubernetes 객체 목록을 출력하지 않는다.

아래 파일은 해당 site의 precondition 결과다. 공개 IP, DNS, 외부 HTTPS 접근성, AWS CLI installer download 검증 결과를 검토한다.

```bash
sudo find /opt/eva/releases -path "*/out/work/config/$SITE_ID/*" \
  -name precondition.yaml -type f -print
```

`out/work`은 Release 준비 경로가 아니라 Ansible control node의 실행 작업 경로다. 기본 CLI 설치는 prepared Release를 control node로 사용하므로, 해당 경로는 `/opt/eva/releases/<version>/out/work/` 아래에 생성된다.

## 7. 감사와 문제 분석

App의 chart defaults, Helm에 전달한 effective input values, Helm이 반환한 resolved values는 target별로 아래 경로에 보존된다.

```text
/opt/eva/releases/<version>/out/work/rendered/<site>/<target>/app/
  chart-defaults.yaml              # 0644
  effective-input-values.yaml      # 0600
  resolved-values.yaml             # 0600
  values-sources.yaml              # 0644, secret 없는 source metadata
```

IAM handoff는 `/opt/eva/releases/<version>/out/work/config/<site>/<target>/eva-iam.yaml`에 `0600`으로 보존된다. 정상 설치 중 설치자가 열거나 App values에 복사할 필요는 없으며, IAM 또는 App 장애 분석이 필요한 경우에만 권한 있는 운영자가 확인한다.

## 8. 선택 Component 설치

필요한 component만 설치할 때는 `--component`를 사용한다. 예를 들어 App만 설치하려면 다음과 같이 실행한다.

```bash
sudo eva install . --workspace /home/eva/site-dev-196 --component app --yes
```

외부 Workspace를 사용하는 경우 `--workspace /home/eva/site-dev-196`와 같이 사용한다. Agent, Vision, App을 선택하면 필요한 Config 단계는 CLI가 자동으로 포함하며, 선택하지 않은 다른 제품 component는 실행하지 않는다. IAM, Agent, Vision, App도 같은 방식으로 각각 `--component iam`, `--component agent`, `--component vision`, `--component app`을 지정한다. n8n은 필요한 경우에만 `site.yaml`에서 활성화해 선택한다.

## 9. 변경 관리와 정리

현재 CLI에는 Infrastructure 또는 Solution uninstall이 없다. 설치를 제거해야 하는 경우 개별 k3s/Docker 파일을 수동 삭제하는 대신, 설치 전 준비한 backup 또는 snapshot으로 복원하거나 서버 교체 절차를 사용한다. 이 방법이 Docker, k3s, package, systemd 변경을 일관되게 되돌리는 방법이다.
