# EVA Cloud 설치 가이드

이 가이드는 지원되는 NVIDIA GPU가 장착된 Ubuntu 24.04 `linux/amd64` 서버 한 대에
EVA를 설치하는 절차입니다. 일반 설치는 Infra, Config, IAM, Agent, Vision, App을
하나의 `eva install` 작업으로 설치합니다. Docker, k3s, NVIDIA runtime, 호스트 영속
데이터를 변경하므로 먼저 작업 가능 시간과 백업 정책을 확인합니다.

## 1. 대상 환경

| 항목 | 필수 운영 기준 |
| --- | --- |
| OS 및 플랫폼 | Ubuntu 24.04, `linux/amd64` |
| 가속기 | 지원되는 NVIDIA GPU, 설치된 NVIDIA driver, `nvidia-smi` |
| 컨테이너 플랫폼 | Docker, k3s, NVIDIA Container Toolkit/CDI, NVIDIA Device Plugin |
| 네트워크 | Cloud Repository, GitHub, Docker Hub, AWS/ECR/S3 및 필요한 HTTPS endpoint |
| 사이트 입력 | 대상 호스트의 AWS credential 및 TLS certificate/key |
| 권한 | `sudo eva ...`를 실행할 수 있는 사용자 |

기본 component는 `infra`, `iam`, `agent`, `vision`, `app`이며 `n8n`은 선택 사항입니다.
Agent 설치는 EVA Agent, `eva-agent-init`, vLLM, Qdrant를 함께 배포합니다.

## 2. EVA Tool 설치 및 Release 검증

Tag Base Release artifact의 압축을 풀고 Release root에서 포함된 installer를 실행합니다.

```bash
unzip -q eva-base-release-*.zip -d eva-base-release
cd eva-base-release
sudo bash ./eva-tool-installer.sh
eva verify .
```

installer는 하나의 `eva-tool_*_linux_amd64.tar.gz` archive를 `checksums.sha256`로
검증합니다. 이후 `eva install`이 검증된 Release를 `/opt/eva/releases/<version>/`에
준비하므로 Release version 변수를 따로 설정할 필요가 없습니다.

## 3. Managed Runtime 준비

서버에 EVA Runtime이 아직 없을 때만 bootstrap을 실행합니다. 관리되는 Ansible 및 Kubernetes
도구를 설치하고 검증한 뒤 `/opt/eva/runtime`에 게시합니다.

```bash
sudo eva runtime bootstrap
```

EVA Release가 바뀌었다는 이유로 bootstrap을 반복하지 않습니다. `eva install`은 작업을
시작하기 전에 Managed Runtime을 검증합니다.

## 4. GPU 사전 조건 확인

설치 전 EVA GPU 사전 조건 검사를 실행합니다. 이 명령은 NVIDIA driver를 확인하고 감지된
GPU 및 MIG 상태를 표시하며 driver가 없을 때 필요한 조치를 안내합니다.

```bash
sudo eva preflight gpu
```

사이트에서 MIG를 사용할 경우 설치 전에 의도한 MIG 구성을 적용합니다. `infra`가 Docker,
CDI, k3s, NVIDIA Device Plugin, allocatable resource를 구성합니다. 이후 Config가
`nvidia.com/gpu` 또는 양수 `nvidia.com/mig-*` resource를 자동 선택합니다. 양수인 MIG
resource type이 여러 개면 임의로 선택하지 않고 설치를 중단합니다.

## 5. Workspace 입력 준비

절대 Workspace 경로를 선택합니다. `/etc/eva/sites/<site-id>`가 관례이지만
`--workspace`에 지정하면 `/home/eva/site-dev-196` 같은 경로도 사용할 수 있습니다.
inventory target 이름은 `iam.yaml` 및 `app.yaml`의 최상위 key입니다. 의도적인 Chart override가
필요할 때만 `agent.yaml` 또는 `vision.yaml`을 만듭니다.

```bash
mkdir -p /home/eva/site-dev-196/{credentials,inventory,site-values}
```

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

# 의도적인 Chart override가 필요할 때만 선택적으로 추가:
# site-values/agent.yaml
# site-values/vision.yaml
```

### `site-values/site.yaml`

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

```ini
[eva]
site-dev-196 ansible_connection=local ansible_user=eva ansible_password='<SSH_PASSWORD>' ansible_become=true ansible_become_password='<SUDO_PASSWORD>'
```

### `credentials/aws_key.ini`

```ini
aws_access_key_id = <AWS_ACCESS_KEY_ID>
aws_secret_access_key = <AWS_SECRET_ACCESS_KEY>
region = ap-northeast-2
```

`awscli`는 이 Workspace 파일만 읽어 inventory target 사용자의 AWS CLI credential을
구성합니다. 이 파일을 Git, 채팅, Release artifact에 포함하지 않습니다.

### `site-values/iam.yaml`

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
  ingress:
    tls:
      hostPath: /home/eva/certs
  imagePullSecrets:
    hostPath: /home/eva/.aws
```

### 선택 `site-values/agent.yaml`

의도적인 EVA Agent main chart override가 필요할 때만 이 파일을 만듭니다. Agent, vLLM,
Qdrant의 기준 values는 Agent Release가 제공하므로 Workspace에 복사하거나 수정하지 않습니다.
Config가 GPU 모델, 물리 GPU 수, MIG 상태, Kubernetes allocatable resource에서 vLLM profile을
자동 선택합니다.

### 선택 `site-values/vision.yaml`

이 파일은 persistent storage 크기, CPU/memory, image policy, rollout timeout처럼 의도적인
Vision Chart override가 필요할 때만 사용합니다. Config와 Kubernetes가 GPU 또는 MIG resource를
자동 선택하므로 GPU 수, MIG profile, resource 이름을 지정하려고 이 파일을 만들지 않습니다.

### `site-values/app.yaml`

```yaml
site-dev-196:
  app:
    browserTitleName: "EVA DEV(196)"
    backendHost: "app196.eva-dev.lge.com"
    license:
      activation_mode: online
      product_code: eva-dev
      api_key: "<SITE_API_KEY>"
      shared_key: "<SITE_SHARED_KEY>"
```

IAM과 App을 하나의 작업으로 설치하면 IAM SSO 값이 App에 자동으로 전달됩니다. 중앙 IAM
서버가 별도로 있다면 이 Workspace 파일에 `app.sso.baseUrl` 및
`app.sso.adminClientSecret`을 명시합니다.

`iam.yaml`, 선택 `agent.yaml` 또는 `vision.yaml`, `app.yaml`, `aws_key.ini`에는 credential이
포함될 수 있습니다. 실제 파일은 승인된 Secret 관리 절차로 관리하고 커밋하지 않습니다. 대상 TLS
certificate와 key는 위에서 구성한 host path, 일반적으로 `/home/eva/certs`에 둡니다.
`eva install`의 precondition은 선택된 IAM `config.host`와 App `app.backendHost`가 각 인증서의
SAN에 포함되는지 변경 전에 검증합니다. IP와 DNS 모두 같은 검증 대상이며, DNS 기반 설치는 기존
host-based Ingress TLS 흐름을 유지하고 Traefik `TLSStore`를 만들지 않습니다.

Agent Release values, Config 생성 설정, role k3s/repository override는 자동 적용됩니다. 선택
Agent 및 Vision Workspace Chart override는 target별로 적용되며 GPU, MIG, vLLM profile을 선택하지 않습니다.

## 6. 설치

검증된 Release root에서 명령 하나를 실행합니다.

```bash
sudo eva install . --workspace /home/eva/site-dev-196 --yes
```

Plan 순서는 항상 다음과 같습니다.

```text
precondition -> infra and GPU/MIG -> config -> iam -> agent -> vision -> app
```

Agent, Vision, App 중 하나를 선택하면 Config가 자동으로 포함됩니다. Config를 별도로 실행하지
않습니다. Component-only 작업은 선택하지 않은 제품 component를 실행하지 않습니다. Agent는
Release 관리 values로 `eva-agent`, `eva-agent-vllm`, `eva-agent-qdrant`를 함께 설치합니다.

precondition이 지원되는 APT repository 문제를 감지하면 별도로 진단합니다.
`eva install --yes`는 외부 APT repository 변경을 승인하지 않습니다.

```bash
sudo eva troubleshoot apt
sudo eva troubleshoot apt --fix-known --yes
```

인증서 SAN 검증이 실패하면 해당 IAM/App 외부 주소를 SAN에 포함한 인증서와 key를 같은 host path에
다시 준비한 뒤, Workspace 값을 바꾸지 않았다면 실패한 작업을 재시도합니다.

```bash
sudo eva retry --yes
```

## 7. 설치 결과 확인

설치 상태 점검으로 시작합니다. 선택된 Agent 또는 Vision component가 있으면 node의 GPU 또는
MIG allocatable resource, NVIDIA Device Plugin readiness, Agent/Vision Pod의 GPU 또는
MIG 할당도 함께 확인합니다.

```bash
sudo eva check --verbose
```

점검에 실패하면 Kubernetes 또는 private rendered artifact를 열기 전에 작업 요약을 확인합니다.

```bash
sudo eva status
```

App이 사용하는 Agent 및 Vision endpoint는 내부 Service인 `eva-agent.eva-agent` 및
`eva-vision.eva-vision`입니다. 연결 문제를 진단할 때 App Pod에서 선택된 endpoint를 확인합니다.

## 8. Rendered Artifact 및 문제 해결

선택된 각 Helm component는 준비된 Release 아래에 chart default, effective Helm input, resolved
Helm values, source metadata를 보관합니다. effective 및 resolved 파일에는 Secret 값이 포함될 수
있으므로 private 파일입니다.

```text
/opt/eva/releases/<version>/out/work/config/<site>/<target>/eva.yaml

/opt/eva/releases/<version>/out/work/rendered/<site>/<target>/agent/
  chart-defaults.yaml              # 0644
  effective-input-values.yaml      # 0600
  resolved-values.yaml             # 0600
  values-sources.yaml              # 0644, Secret 값 없음

/opt/eva/releases/<version>/out/work/rendered/<site>/<target>/vllm/
  chart-defaults.yaml              # 0644
  effective-input-values.yaml      # 0600
  resolved-values.yaml             # 0600
  values-sources.yaml              # 0644, Secret 값 없음

/opt/eva/releases/<version>/out/work/rendered/<site>/<target>/qdrant/
  chart-defaults.yaml              # 0644
  effective-input-values.yaml      # 0600
  resolved-values.yaml             # 0600
  values-sources.yaml              # 0644, Secret 값 없음

/opt/eva/releases/<version>/out/work/rendered/<site>/<target>/vision/
  chart-defaults.yaml              # 0644
  effective-input-values.yaml      # 0600
  resolved-values.yaml             # 0600
  values-sources.yaml              # 0644, Secret 값 없음

/opt/eva/releases/<version>/out/work/rendered/<site>/<target>/app/
  chart-defaults.yaml              # 0644
  effective-input-values.yaml      # 0600
  resolved-values.yaml             # 0600
  values-sources.yaml              # 0644, Secret 값 없음

/var/lib/eva/sites/<site>/<target>/eva-iam.yaml  # root:root, 0600
```

private values 및 handoff 파일을 붙여 넣거나 커밋하지 않습니다. license, SSO, database,
registry credential이 포함될 수 있습니다.

## 9. 변경 및 재시도

Workspace values 또는 Release version을 변경하면 Helm upgrade를 수행하며 기존 database와
영속 데이터를 보존합니다. 일반 upgrade는 `/eva-app` 또는 Agent/Vision persistence를 삭제하지
않습니다. Config는 선택된 component에 맞춰 자동 생성됩니다. 실패한 작업을 수정한 뒤 해당 작업만
재시도합니다.

```bash
sudo eva retry --yes
```

## 10. 비GPU 테스트 환경

CPU-only 서버는 제한된 테스트를 위한 예외 환경입니다. `site.yaml`에서 `agent`와 `vision`을
`false`로 설정하면 Infra와 Config가 NVIDIA runtime/CDI 작업을 건너뜁니다. 이 가이드의 기본
Cloud 운영 환경은 아닙니다.
