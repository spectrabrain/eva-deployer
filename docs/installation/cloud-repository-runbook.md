# EVA Cloud 설치 가이드

이 가이드는 지원되는 NVIDIA GPU가 장착된 Ubuntu 24.04 `linux/amd64` 서버에
EVA를 설치하는 절차입니다. 현재 서버에 설치하거나 별도 Bastion에서 여러 타겟에 각각 설치할 수 있습니다.
일반 전체 설치는 Infra, Config, IAM, Agent, Vision, App을
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
sudo eva verify
```

installer는 하나의 `eva-tool_*_linux_amd64.tar.gz` archive와 Release checksum을
검증한 뒤, 실행한 Release를 root 관리 Current Release로 등록합니다. 이후 새 shell에서도
`sudo eva verify`와 `sudo eva install`은 이 Release를 사용하므로 Release version 변수를
따로 설정할 필요가 없습니다. 다른 Release가 필요할 때만 `--release /path/to/release`를
명시합니다.

## 3. Managed Runtime 준비

서버에 EVA Runtime이 아직 없을 때만 bootstrap을 실행합니다. 관리되는 Ansible 및 Kubernetes
도구를 설치하고 검증한 뒤 `/opt/eva/runtime`에 게시합니다.

```bash
sudo eva runtime bootstrap
```

EVA Release가 바뀌었다는 이유로 bootstrap을 반복하지 않습니다. `eva install`은 작업을
시작하기 전에 Managed Runtime을 검증합니다.

Bastion 설치는 여기까지를 **현재 Bastion/controller**에서 수행합니다. 각 타겟에도
Cloud Repository/ECR/S3 접근이 필요하며 Bastion만 인터넷에 연결된 폐쇄망 구성은 Cloud가 아닙니다.
그 환경에는 Remote Repository Runbook을 사용합니다.

## 4. GPU 사전 조건 확인

설치 전 EVA GPU 사전 조건 검사를 실행합니다. 이 명령은 NVIDIA driver를 확인하고 감지된
GPU 및 MIG 상태를 표시하며 driver가 없을 때 필요한 조치를 안내합니다.
Bastion에서는 이 명령이 원격 타겟을 검사하지 않으므로 각 타겟에서 따로 확인합니다.

```bash
sudo eva preflight gpu
```

사이트에서 MIG를 사용할 경우 설치 전에 의도한 MIG 구성을 적용합니다. `infra`가 Docker,
CDI, k3s, NVIDIA Device Plugin, allocatable resource를 구성합니다. 이후 Config가
`nvidia.com/gpu` 또는 양수 `nvidia.com/mig-*` resource를 자동 선택합니다. 양수인 MIG
resource type이 여러 개면 임의로 선택하지 않고 설치를 중단합니다.

## 5. 단일 설치 설정 준비

일반 계정으로 아래 명령을 실행합니다. inventory, credential, component별 Values 폴더를
직접 만들 필요가 없습니다. 기본 결과는 현재 사용자 홈의 `eva-install.yaml`입니다.

```bash
eva install configure
```

1. 설치환경에서 **cloud**를 선택합니다.
2. **현재 서버 설치** 또는 **Bastion/controller**를 선택합니다.
3. Bastion이면 닉네임·IP·SSH 계정·숨김 비밀번호로 타겟 한 대를 등록합니다.
  sudo도 같은 비밀번호를 사용하며 private key는 이 wizard에서 받지 않습니다.
4. 해당 타겟의 **Infra-only / Infra + Solution**을 선택합니다.
5. Solution이면 HTTP/HTTPS → 동일/별도 도메인/IP 직접 접속 → IAM+APP/APP만(외부 IAM) →
  추가 Agent/Vision 구성을 선택합니다.
6. Bastion이면 다음 타겟을 추가하고 설치 옵션 재사용 여부를 고르거나 등록을 마칩니다.
7. 생성 파일을 열어 남은 site·AWS·license·인증서·주소·SSO 입력을 채웁니다.

타겟마다 선택은 독립적이며 Infra-only 타겟에는 Solution Values를 넣지 않습니다.
타겟별 `components`에 따라 실제 Ansible 단계의 대상도 제한합니다. 같은 설치 옵션을 재사용해도
연결 비밀번호와 서비스 주소·Secret은 복사하지 않습니다. 여러 타겟은 하나의 cluster를 만드는 기능이 아닙니다.
기존 단일 서버용 `eva config sample --mode cloud`와 비대화형 옵션도 유지합니다.

샘플은 v3.3 대상 로컬 App/IAM 소스 기준입니다. HTTP도 입력 검증은 가능하지만 실제 설치에는
Release Chart 전환과 Deployer TLS 실행 연동이 별도로 필요합니다. 기존 Release 설치는 **HTTPS**를
선택합니다. 두 명령 모두 `--output FILE`로 위치를 바꿀 수 있고 새 파일만 `0600`으로 만들며 덮어쓰지 않습니다.

다음은 다른 도메인 + HTTPS의 구조 예시입니다. 선택 결과에 따라 같은 주소 틀은 App `/`,
IAM `/iam`과 공통 host를 사용합니다. `<...>` 입력이 남으면 검증이 해당 필드 경로를 안내합니다.

```yaml
schema_version: v1
site:
  id: customer-a
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
credentials:
  aws:
    aws_access_key_id: "<AWS_ACCESS_KEY_ID>" # 필수: 실제 발급값. ex) AKIAEXAMPLEONLY000000 (예시 전용)
    aws_secret_access_key: "<AWS_SECRET_ACCESS_KEY>" # 필수: 실제 발급값, 커밋 금지. ex) demo-aws-secret-only (예시 전용)
    region: ap-northeast-2
targets:
  server-a:
    ansible:
      ansible_connection: local
      ansible_user: eva
      ansible_become: true
      ansible_become_password: "<SUDO_PASSWORD>" # 필수: 대상 sudo 비밀번호. ex) demo-sudo-only (예시 전용)
    values:
      iam:
        config:
          host: iam.example.com
        keycloak:
          realmConfig:
            substitutions:
              EVA_IAM_ADMIN_EMAIL: "admin@eva.com"
          realmPatch:
            realmAdmin:
              password: "<REALM_ADMIN_PASSWORD>" # 필수: realm 관리자 비밀번호. ex) Demo-Realm-Only!42 (예시 전용)
            evaApp:
              redirectUris:
                - "https://app.example.com/*"
        ingress:
          tls:
            enabled: true
            create: true
            hostPath: /etc/eva/certs
        redis:
          tls:
            enabled: true # HTTPS 구성에 맞춰 IAM Redis TLS도 사용합니다.
          external:
            enabled: true
      app:
        ingress:
          path: /
          tls:
            enabled: true
            create: true
            hostPath: /etc/eva/certs
        app:
          browserTitleName: "EVA"
          backendHost: app.example.com
          backendSecure: true
          backendSecurePort: 443
          sso:
            brokerMode: false # 외부 SSO(IdP) broker 연동 시 true
            brokerIdpHint: "" # 외부 SSO 연동 시 IAM Identity Provider alias 입력
            redis:
              port: 32079
              db: 0
              password: eva-redis-pass # IAM Redis와 동일하게 맞추고 변경
              url: "" # 선택 override; 비우면 App이 SSO 변수로 조합
          license:
            activation_mode: online
            product_code: "<LICENSE_PRODUCT_CODE>" # 필수: 승인된 제품 코드. ex) eva
            api_key: "<LICENSE_API_KEY>" # 필수: 발급된 API key. ex) demo-license-api-key (예시 전용)
            shared_key: "<LICENSE_SHARED_KEY>" # 필수: 발급된 shared key. ex) demo-license-shared-key (예시 전용)
```

`targets.server-a`는 inventory alias이며, `values`에는 target별 Chart Values를 그대로
씁니다. 기존처럼 target 이름을 Values 안에 한 번 더 감싸지 않습니다. 여러 대상은
`targets` 아래에 추가합니다. SSH 대상은 `ansible_connection: ssh`, `ansible_host`,
`ansible_user`와 password 또는 private key 경로를 `ansible`에 지정합니다.
상대 SSH key 경로는 설정 파일 위치 기준이며, 대상 서버의 TLS·데이터 경로는 절대 경로로 씁니다.

`values.agent`와 `values.vision`은 의도적인 Chart override가 필요할 때만 추가합니다.
Agent/vLLM/Qdrant 기준 Values는 Release에서 제공하고 GPU/MIG/vLLM profile은 Config가
자동 선택하므로 이 설정에 복사하거나 고정하지 않습니다.

IAM과 App을 함께 설치하면 SSO 값은 자동 전달됩니다. 별도 중앙 IAM은 외부 IAM 샘플의
`targets.server-a.values.app.app.sso`에 URL·realm·client·Secret·Redis 연결값을 지정합니다.
`awscli`는 생성된 Workspace credential을 사용해 target 사용자의 AWS CLI를 구성합니다.

```bash
sudo eva workspace validate --config /home/eva/eva-install.yaml
```

위 명령은 입력만 검증하고 파일을 생성하지 않습니다. 설치 시에는
`/var/lib/eva/workspaces/<site-id>/<content-sha256>/`에 내부 Workspace를 원자적으로 생성합니다.
폴더는 `0700`, 파일은 `0600`이며 설정이 바뀌면 새 snapshot을 사용합니다.
기존 Workspace와 `--workspace` 방식은 계속 지원하지만 `--config`와 동시에 지정하지 않습니다.

기존 `config sample`에서 메뉴 없이 틀을 지정하려면 `--address shared|separate --protocol https`를 추가합니다.
필수 입력, 여러 target과 HTTP 지원 범위는 [단일 설치 설정](single-file-configuration.md)을 참고합니다.

Bastion은 설치 대상이 아니며 생성 Workspace와 실행 로그를 보관하는 controller입니다.
`eva check`는 현재 서버의 Docker/k3s를 확인하므로 Bastion에서 타겟 전체 점검으로 사용하지 않습니다.
타겟별 상태·방화벽·SSH host key를 따로 확인하고, 검증된 Release의 원격 실행 경로를 사전에 시험합니다.
기존 APT 진단도 controller 기준이므로 타겟 APT 상태를 대신 검사하지 않습니다.

원본 설정과 생성 Workspace에는 credential이 포함되므로 Git, 채팅, Release artifact에 넣지
않습니다. 인증서와 SSH private key는 YAML에 inline하지 않고 기존 Secret 관리 절차로 준비합니다.
TLS certificate와 key는 위 `hostPath` 예시의 `/etc/eva/certs`에 둡니다.
`eva install`의 precondition은 선택된 IAM `config.host`와 App `app.backendHost`가 각 인증서의
SAN에 포함되는지 변경 전에 검증합니다. IP와 DNS 모두 같은 검증 대상이며, DNS 기반 설치는 기존
host-based Ingress TLS 흐름을 유지하고 Traefik `TLSStore`를 만들지 않습니다.

Agent Release values, Config 생성 설정, role k3s/repository override는 자동 적용됩니다. 선택
Agent 및 Vision Chart override는 target별로 적용되며 GPU, MIG, vLLM profile을 선택하지 않습니다.

## 6. Argo CD 관리 연결 확인

기존 본사 Argo CD가 같은 site의 EVA resource를 관리했을 수 있는 환경에서는 설치 전에
아래 preflight를 실행합니다. 대상 cluster의 tracking metadata를 다시 수집하고, 발견된 경우에만
연결 해제 여부를 묻습니다.

```bash
sudo eva preflight argocd --config /home/eva/eva-install.yaml
```

승인하면 Argo CD 관리 서버 주소, SSH 사용자, password를 입력받습니다. 관리 서버의
`kubectl`로 감지된 Application의 공통 destination과 ApplicationSet ownership을 확인합니다.
cluster registration Secret의 tracking ID가 `Application/argocd/registration`임을 확인한 뒤,
Git source에서 해당 site manifest만 제거하는 별도 승인을 표시합니다. 이 화면에는 repository,
동적으로 확인한 default branch, target manifest 및 다음 commit 제목이 표시됩니다.

```text
[DEPLOYER] remove <cluster-name> from Argo CD registration
```

Git push와 `registration` Application이 pushed commit을 관측한 것이 확인된 뒤에만 live Secret을
삭제하고, 이후 해당 cluster prefix의 legacy Application을
non-cascade로 제거합니다. global `registration` Application의 Auto-Sync와 모든 ApplicationSet은
변경하지 않으며 다른 site의 manifest도 수정하지 않습니다. push 실패 또는 remote HEAD 변경은
live Secret/Application 삭제 없이 실패합니다. Argo CD CLI 로그인이나 Argo CD 계정 정보는 필요하지
않지만 SSH 사용자는 관리 서버에서 repository clone 및 push 권한을 이미 가져야 합니다.

완료된 handoff는
`/var/lib/eva/sites/<site-id>/argocd-handoff.yaml`에 원자적으로 기록됩니다.
receipt에는 registration Application, repository, branch, manifest, pushed commit SHA도 기록합니다.
Git-backed handoff에는 이 다섯 metadata가 모두 필요하며, 이전 형식 receipt는 계속 읽을 수 있습니다.
receipt 복구는 `registration`이 target Secret을 원하지 않고 live Secret/Application이 안정적으로
계속 부재한 경우에만 허용됩니다. 그렇지 않으면 full Git-backed preflight가 필요합니다.
거절하면 설치를 시작하지 않으므로, 필요한 경우 현재 상태를 그대로 둔 채 직접 조치할 수 있습니다.
명령은 installer가 등록한 Current Release를 사용해 `/opt/eva/releases`에 준비합니다.
별도의 `--release` 또는 `--install-root` 입력은 필요하지 않습니다.
관리 서버 SSH host key는 `/home/eva/.ssh/known_hosts`에서 검증합니다.
파일이 없으면 생성하며, 처음 보는 host key는 SHA256 fingerprint를 표시하고 별도 승인을 받습니다.

`eva install` 또는 `eva apply`에서 tracking metadata가 남아 있으면 handoff를 수행하지 않고 위
preflight 명령을 안내하며 Solution 설치 전에 중단합니다. `--yes`도 이 규칙을 우회하지 않습니다.

## 7. 설치

installer가 등록한 Current Release로 명령 하나를 실행합니다.

```bash
sudo eva install --config /home/eva/eva-install.yaml --yes
```

다른 검증된 Release를 사용할 때만 다음처럼 명시적으로 override합니다. 이 override는
Current Release를 변경하지 않습니다.

```bash
sudo eva verify --release /path/to/another/release
sudo eva install --release /path/to/another/release --config /home/eva/eva-install.yaml --yes
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
다시 준비한 뒤, 설정 값을 바꾸지 않았다면 실패한 작업을 재시도합니다.

```bash
sudo eva retry --yes
```

## 8. 설치 결과 확인

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

## 9. Rendered Artifact 및 문제 해결

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

## 10. 변경 및 재시도

단일 설정 또는 Release version을 변경하고 `eva install --config ...`를 다시 실행하면
Helm upgrade를 수행하며 기존 database와 영속 데이터를 보존합니다. 일반 upgrade는 `/eva-app`
또는 Agent/Vision persistence를 삭제하지 않습니다. Config는 선택된 component에 맞춰 자동
생성됩니다. 입력 변경 없이 외부 실패 원인을 해결했다면 해당 작업을 재시도합니다.
`retry`는 원래 작업의 Workspace snapshot을 사용하며 수정한 원본 설정을 다시
읽지 않습니다. 설정을 수정했다면 `install --config`로 새 작업을 시작합니다.

```bash
sudo eva retry --yes
```

## 11. 비GPU 테스트 환경

CPU-only 서버는 제한된 테스트를 위한 예외 환경입니다. 단일 설정의 `components`에서 `agent`와 `vision`을
`false`로 설정하면 Infra와 Config가 NVIDIA runtime/CDI 작업을 건너뜁니다. 이 가이드의 기본
Cloud 운영 환경은 아닙니다.
