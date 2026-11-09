# EVA Online 설치 가이드

이 가이드는 지원되는 NVIDIA GPU가 장착된 Ubuntu 24.04 `linux/amd64` 서버에
EVA를 설치하는 절차입니다. 현재 서버에 설치하거나 별도 Bastion에서 여러 타겟에 각각 설치할 수 있습니다.
`online`은 설치 대상이 upstream/인터넷 Registry·Repository에 직접 접근하는 모드이며,
온프레미스/클라우드 제공자와 무관합니다. Ansible 모드는 `online_repository`입니다.
일반 전체 설치는 Infra, GPU preflight, Config, IAM, Agent, Vision, App을
하나의 `eva install` 작업으로 설치합니다. Docker, k3s, NVIDIA runtime, 호스트 영속
데이터를 변경하므로 먼저 작업 가능 시간과 백업 정책을 확인합니다.

Bastion에서 NCP NKS에 Solution만 설치할 때도 같은 configure/install을 사용합니다.
NKS의 서버 Infra는 Cloud에서 관리하며 설치자가 생성된 Values를 채웁니다.

## 1. 대상 환경

| 항목 | 필수 운영 기준 |
| --- | --- |
| OS 및 플랫폼 | Ubuntu 24.04, `linux/amd64` |
| 가속기 | 지원되는 NVIDIA GPU; 드라이버가 없으면 설치 중 준비하며 재부팅 후 재실행 |
| 컨테이너 플랫폼 | Docker, k3s, NVIDIA Container Toolkit/CDI, NVIDIA Device Plugin |
| 네트워크 | upstream/인터넷 Repository, GitHub, Docker Hub, AWS/ECR/S3 및 필요한 HTTPS endpoint |
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

다음 명령으로 관리되는 Ansible 및 Kubernetes 도구를 설치하고 검증한 뒤
`/opt/eva/runtime`에 게시합니다.

```bash
sudo eva runtime bootstrap
```

Runtime은 **Python 3.12 이상**이 필요합니다. `bootstrap`이 호환되는 Python을 자동 선택하고,
없으면 필요한 Python과 venv 패키지를 자동 설치한 뒤 Runtime을 구성합니다.

EVA Release가 바뀌었다는 이유로 bootstrap을 반복하지 않습니다. `eva install`은 작업을
시작하기 전에 Managed Runtime을 검증합니다.

Bastion 설치는 여기까지를 **현재 Bastion/controller**에서 수행합니다. 각 타겟에도
upstream/인터넷 Repository·ECR·S3 접근이 필요하며 Bastion만 인터넷에 연결된 폐쇄망 구성은 Online이 아닙니다.
Main Harbor와 설치 자산을 사용하는 환경은 [Remote Repository Runbook](remote-repository-runbook.md),
저장매체로 반입한 Bundle과 Local Harbor를 사용하는 완전 폐쇄망은 [Local Repository Runbook](local-repository-runbook.md)을 사용합니다.
Remote도 Main에서 같은 `eva install configure`로 SSH 타겟을 입력하고 `eva install --config`로 설치합니다.
Main의 Harbor·자산 준비를 먼저 완료하며 별도 타겟 등록 명령은 필요하지 않습니다.

## 4. GPU 구성 선택 (서버 설치)

Agent를 설치할 서버의 GPU를 다음 표와 맞춥니다. `eva install`은 Agent/Vision 대상의 GPU 상태를
확인하고 필요한 구성을 자동 적용합니다. `eva install configure`에서 RTX PRO 6000의 MIG 사용 여부를
선택합니다. `MIG 사용`을 선택하면 4개를 자동 구성하며, `MIG 사용 안 함`은 전체 GPU를 재사용합니다.

| Agent profile | 물리 GPU | MIG |
| --- | --- | --- |
| `A6000x1` | RTX A6000 1장 | 사용 안 함 |
| `L40sx1` | L40S 1장 | 사용 안 함 |
| `PRO5000x3` | RTX PRO 5000 Blackwell 3장 | 사용 안 함 |
| `PRO6000x1` | RTX PRO 6000 Blackwell 1장 | 사용 안 함 |
| `PRO6000-MIGx4` | RTX PRO 6000 Blackwell 1장 | 4개 자동 구성 |

선택값은 `targets.<target>.ansible.eva_gpu_mig_enabled`에 `true` 또는 `false`로 저장하며,
`infra: false`인 Solution-only 설치에도 적용합니다. 기존 설정에서 이 값을 생략하면 이전 자동 구성
동작을 유지합니다. MIG 사용 안 함을 선택했는데 이미 MIG가 켜져 있으면 기존 분할을 보존하고 중단합니다.
`PRO6000x1`은 Release의 `L40sx1` 단일 전체 GPU vLLM Values를 사용합니다.

이미 맞는 GPU/MIG 구성은 재사용합니다. 다른 MIG 분할이 있거나 변경할 GPU에 workload가
실행 중이면 중단하므로 해당 구성을 정리한 뒤 다시 실행합니다.
Display Mode 전환이 필요한 경우 설치 설정의 AWS 권한으로 전환 도구를 준비합니다.
드라이버 설치·GPU Mode 변경 후 재부팅 안내가 나오면 **해당 타겟을 재부팅하고 같은 명령을 다시 실행**합니다.

## 5. 단일 설치 설정 준비

일반 계정으로 아래 명령을 실행합니다. inventory, credential, component별 Values 폴더를
직접 만들 필요가 없습니다. 기본 결과는 현재 사용자 홈의 `eva-install.yaml`입니다.

```bash
eva install configure
```

1. 설치환경에서 **online**을 선택합니다.
2. **현재 서버 설치** 또는 **Bastion/controller**를 선택합니다.
3. Online Bastion이면 **On-premise / 서버** 또는 **Cloud (NCP NKS)**를 선택합니다.
4. 서버는 닉네임·IP·SSH 계정·숨김 비밀번호로 타겟을 등록합니다. NKS는 Bastion 계정의
   kubeconfig·context·Base Release 경로를 지정하며 worker SSH 정보는 입력하지 않습니다.
5. **Infra만 / Infra + Solution / Solution만**을 선택합니다. NKS는 **Solution만**을 사용합니다.
6. 기존과 같이 HTTP/HTTPS → 동일/별도 도메인/IP → IAM+App/외부 IAM → Agent/Vision을 선택합니다.
7. SSH 서버는 다음 타겟을 추가하거나 등록을 마칩니다. NKS는 로컬 controller 하나를 사용합니다.
8. 생성 파일을 열어 site·AWS·license·주소·SSO·스토리지 등 Values를 실제 환경에 맞게 채웁니다.

타겟마다 선택은 독립적이며 Infra-only 타겟에는 Solution Values를 넣지 않습니다.
타겟별 `components`에 따라 실제 Ansible 단계의 대상도 제한합니다. 같은 설치 옵션을 재사용해도
연결 비밀번호와 서비스 주소·Secret은 복사하지 않습니다. 여러 타겟은 하나의 cluster를 만드는 기능이 아닙니다.

`eva install configure`는 `--output FILE`로 위치를 바꿀 수 있고 새 파일만 `0600`으로 만들며 덮어쓰지 않습니다.

- **HTTP:** 격리된 테스트에만 사용합니다. 생성된 App/IAM `ingress.tls.enabled/create: false`,
  App `backendSecure: false`를 유지합니다. host 인증서 SAN 검사·TLS Secret Job·Traefik 기본
  TLSStore 생성을 실행하지 않습니다. 기존 TLS Secret/TLSStore를 삭제하는 동작은 하지 않습니다.
- **HTTPS:** 인증서 생성 방식이 hostPath일 때 SAN 검사를 수행하고 IP 접속에서는 Traefik
  기본 TLS 인증서를 구성합니다. DNS 접속은 기존 host-based Ingress TLS를 유지합니다.
  기존 Secret 사용(`create: false`) 또는 Secret 인증 방식은 hostPath 인증서를 요구하지 않습니다.
- 표준 외부 포트는 HTTP `80`, HTTPS `443`입니다. 기존 Traefik 포트를 자동 변경하지 않습니다.
  비표준 포트라면 실제 Service/외부 LB 노출과 App의 외부 포트·IAM `config.publicPort`·
  App client `redirectUris`를 함께 맞춥니다.
- AWS credential에는 제품 이미지의 ECR pull 권한과 모델용 S3 list/read 권한이 필요합니다.
  다른 AWS 계정의 자산을 사용하는 경우 해당 계정에서도 접근을 허용해야 합니다.
- 브라우저뿐 아니라 **App Pod에서도 IAM 외부 주소를 해석·접속**할 수 있어야 합니다.
  주소·license·AWS·sudo 입력을 채운 뒤 아래 입력 검증, 6절 인증서 준비, 필요한 경우 7절 preflight,
  8절 설치 순서로 진행합니다.

다음은 다른 도메인 + HTTPS의 구조 예시입니다. 선택 결과에 따라 같은 주소 틀은 App `/`,
IAM `/iam`과 공통 host를 사용합니다. `<...>` 입력이 남으면 검증이 해당 필드 경로를 안내합니다.

```yaml
schema_version: v1
site:
  id: customer-a
repository:
  mode: online
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
    aws_access_key_id: "<AWS_ACCESS_KEY_ID>" # 필수: 실제 발급값
    aws_secret_access_key: "<AWS_SECRET_ACCESS_KEY>" # 필수: 실제 발급값, 커밋 금지
    region: ap-northeast-2
targets:
  server-a:
    ansible:
      ansible_connection: local
      ansible_user: eva
      ansible_become: true
      ansible_become_password: "<SUDO_PASSWORD>" # 필수: 대상 sudo 비밀번호
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
              password: "<REALM_ADMIN_PASSWORD>" # 필수: realm 관리자 비밀번호
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
              password: "<IAM_REDIS_PASSWORD>" # IAM Redis와 동일하게 설정
              url: "" # 선택 override; 비우면 App이 SSO 변수로 조합
          license:
            activation_mode: online # Online 기본값. 사전 결정한 라이선스 방식에 맞게 변경합니다.
            product_code: "<LICENSE_PRODUCT_CODE>" # 필수: 발급된 라이선스와 일치하는 제품 코드. ex) eva-dev 또는 eva-prod
            api_key: "<LICENSE_API_KEY>" # 필수: 발급된 API key
            shared_key: "<LICENSE_SHARED_KEY>" # 필수: 발급된 shared key
```

`targets.server-a`는 inventory alias이며, `values`에는 해당 target의 Chart Values를 직접
작성합니다. 여러 대상은 `targets` 아래에 추가합니다. SSH 대상은 `ansible_connection: ssh`, `ansible_host`,
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

위 명령은 설치 설정의 필수 입력과 형식을 검증합니다. 설정 파일에는 credential이 포함되므로
Git, 채팅, Release artifact에 넣지 않습니다.

### NKS + Solution만의 설정

동일한 site·AWS·라이선스·접속 주소 틀을 사용하고 `deployment.environment: nks`와
kubeconfig 연결만 추가합니다. `components.infra`는 `false`입니다.
신규 설치와 업데이트 모두 `targets.controller.values.<component>`를 직접 채웁니다.
기존 Helm Values는 자동 병합하지 않으며 기존 역할의 백업·보정·SSO·복구 처리는 유지합니다.

- App/IAM 이미지 인증은 Kubernetes Secret을 사용합니다. AWS 키는 공통 `credentials.aws`에 입력합니다.
  ECR 변경 시 App/IAM의 `imagePullSecrets.account`·`region`, Agent의 `ecrRefresh.registry`도
  새 이미지 저장소와 맞춥니다. 어긋나면 workload 변경 전에 중단합니다.
- 기본 틀은 Cloud LB의 직접 연결(`ingress.enabled: false`)입니다. Chart Ingress를 사용할 때는
  실제 IngressClass와 TLS Secret 설정을 채웁니다. 공개 URL 변경과 DNS·LB 설정을 함께 맞춥니다.
- 새 App/IAM은 PVC·StorageClass를 채웁니다. 업데이트는 기존 PVC·hostPath 경로를 유지합니다.
  기존 IAM hostPath는 데이터가 있는 worker에 고정합니다. 새 설치에서 hostPath를 명시하면
  worker 경로를 사용하며 Bastion에서 해당 디렉터리를 준비하지는 않습니다.
- Vision은 실제 GPU resource와 모델 캐시 StorageClass를 채웁니다. NKS에 RuntimeClass가 없으면
  `runtimeClassName: ""`를 유지합니다. GPU 교체는 `Recreate` 또는 `maxSurge: 0`을 사용합니다.
  기본 `imagePullSecrets.ExistingSecret: ecr-registry`는 Deployer가 갱신합니다. 외부 관리 Secret을
  지정하면 설치자가 새 Registry의 인증 정보와 갱신을 준비해야 합니다.
  Vision은 `eva-vision-chart/main/deploy-values/eva-vision-ncp.yaml`을 NKS 기본값으로 사용합니다.
  공통 생성 Values에 NCP Values를 병합하고, 선택한 Release 이미지와 작성한 Values·CLI override를 적용합니다.
  실제 NCP URL은 Vision의 `values-sources.yaml`에 기록합니다.
- Agent 선택 시 vLLM·Qdrant도 같은 역할로 설치합니다. `values.agent.eva_agent_deploy`의
  `vllm_profile`과 `vllm_values`·`qdrant_values`에 NAS/블록 StorageClass, 용량, 모델·GPU 설정을 채웁니다.
  업데이트는 기존 PVC 이름·용량과 필요한 NodePort를 유지합니다. 서버용 NFS·StorageClass는 만들지 않습니다.
  NKS에서는 `chartmuseum/release/<version>`의 Agent·Qdrant·init `values-ncp.yaml`을 사용하며,
  `L40sx1`은 vLLM `values-ncp.L40Sx1.yaml`로 연결합니다. init 차트로 선택한 NAS/블록 StorageClass 중
  없는 것만 생성하고, 기존 클래스는 그대로 재사용합니다. NCP CSI 드라이버는 클러스터에 준비되어 있어야 합니다.
  init의 추가 설정은 `eva_agent_deploy.init_values`에 작성합니다. 예를 들어 NAS zone은
  `storageClass.fileSystem.parameters.zoneNo`로 변경하며, 생략하면 NCP 파일의 `"2"`를 사용합니다.
  실제 사용한 Agent·vLLM·Qdrant 파일명과 URL은 각 `values-sources.yaml`에서 확인합니다.
- Infra·드라이버·GPU/MIG·NFS·k3s 설정은 실행하지 않습니다. NCP 인증 프로그램과 ECR 검증용
  AWS CLI는 Bastion의 kubeconfig 소유 계정에서 사용할 수 있어야 합니다.

### Agent 이미지·모델 공급

Agent 설치에 필요한 차트·이미지·기본 Values는 선택한 Release에서 정합니다.
vLLM 모델과 Qdrant snapshot은 S3에서 자동으로 받습니다.
개별 버전을 입력하거나 파일을 별도로 다운로드할 필요가 없습니다.

- 대상 서버의 AWS 계정에 이미지 ECR pull 및 모델 S3 list/read 권한이 필요합니다.
  입력한 AWS credential로 필요한 인증을 자동 구성하므로 수동 Secret 생성은 필요하지 않습니다.
  재설치에서는 서비스 중지 전에 ECR 인증을 검증하고, 기존 Agent ECR 갱신 작업을 정리한 뒤
  관리되는 AWS Secret을 현재 설치 설정과 동기화합니다. 갱신 스케줄은 설치 후 복구합니다.
  직접 지정한 외부 AWS Secret은 유지하며, 해당 Secret의 인증이 유효해야 설치를 진행합니다.
- Agent의 Vision 기능을 사용하려면 Vision도 설치합니다.
  기본 연결 주소는 `http://eva-vision.eva-vision:8000`입니다.
- 첫 설치에는 모델 다운로드 시간이 필요합니다. 캐시를 위한 충분한 디스크 공간을 확보합니다.
  재설치·업그레이드에서 모델 캐시와 Qdrant 데이터의 PV/PVC를 삭제하지 않습니다.

### Vision 모델 공급

Vision 모델은 컨테이너 이미지와 별도로 S3에서 공급됩니다. `eva install` 과정에서
모델을 다운로드해 영구 캐시에 저장하고, 모델 준비가 끝나면 서비스를 시작합니다.
필요한 인증은 입력한 AWS credential로 자동 구성하므로 추가 명령이나 Secret 생성은 필요하지 않습니다.

- 대상 서버의 AWS 계정에 모델 S3 list/read 권한이 필요합니다.
- 첫 설치에는 모델 다운로드·준비 시간이 필요합니다. 모델과 준비 파일을 저장할 디스크 공간을 확보합니다.
- 모델 캐시는 기본 `20Gi`이며 필요하면 `values.vision.modelCache.size`로 조정합니다.
  **업그레이드·재시도에서 모델 PVC를 삭제하지 않습니다.**

## 6. 인증서 파일 준비 (서버 설치)

**HTTPS 설치 시** 각 대상 서버에 인증서와 개인 키를 준비합니다. HTTP 또는 기존 TLS Secret을
사용하는 경우에는 이 단계를 건너뜁니다.

- IAM/App의 `ingress.tls.hostPath`에 PEM 형식의 `tls.crt`(인증서)와 `tls.key`(개인 키)를 둡니다.
  위 설정 예시는 `/etc/eva/certs/tls.crt`, `/etc/eva/certs/tls.key`를 사용합니다.
- 인증서 SAN에는 실제 접속 주소인 IAM `config.host`와 App `app.backendHost`의 DNS 또는 IP가
  포함되어야 합니다. IAM/App 경로가 같다면 두 주소를 모두 포함한 인증서를 사용합니다.
- 개인 키는 YAML에 직접 넣거나 Git에 저장하지 않습니다. Bastion 설치도 파일은 각 대상 서버에 둡니다.

설치 시작 시 인증서와 접속 주소의 SAN 일치 여부를 검증합니다.

## 7. Argo CD 관리 연결 확인

기존 Argo CD로 관리하던 서버에서만 설치 전에 실행합니다. 이미 연결을 해제한 서버도
동일한 명령으로 확인할 수 있습니다. Bastion에서 여러 타겟을 설치할 때도 설정 파일 하나로 실행합니다.

```bash
sudo eva preflight argocd --config /home/eva/eva-install.yaml
```

- 명령 시작 시 Argo CD 관리 서버 주소·SSH 계정·비밀번호를 한 번 입력합니다. 설정에 포함된
  모든 타겟에 같은 관리 서버 연결을 사용합니다. 처음 접속하는 SSH host key는 fingerprint를
  확인한 뒤 승인합니다. Argo CD 계정은 필요하지 않습니다. 타겟의 SSH 접속 정보는 설치 설정을 사용합니다.
- 타겟별로 관리 상태를 확인하고, 연결 해제와 해당 클러스터의 Git 등록 변경을 별도 승인한 뒤
  요청 시 Git 인증 정보를 입력합니다. 관리 서버에서
  `kubectl`·`git`을 사용할 수 있어야 하며 저장소 읽기·push 권한이 필요합니다.
- 배포된 workload와 데이터, 다른 site는 유지합니다. 이미 해제된 경우에는 읽기 전용으로
  상태를 검증하고 타겟별로 완료를 기록합니다. 관리 metadata가 없는 타겟은 연결 해제 없이 통과합니다.
- 연결 해제 완료가 확인되지 않으면 Solution 설치를 중단합니다. `--yes`로 우회할 수 없습니다.

## 8. 설치

### On-premise / 서버

GPU preflight만 먼저 실행하려면 **설치 대상 서버에서** 실행합니다.

```bash
sudo eva preflight gpu
```

현재 서버의 GPU를 확인·준비하며 설치 설정 파일은 읽지 않습니다.
**Bastion에서** 5절의 Agent/Vision SSH 타겟을 준비하려면 설정 파일을 명시합니다.

```bash
sudo eva preflight gpu --config "$HOME/eva-install.yaml"
```

추가 실행 확인은 필요하지 않습니다.

필요한 드라이버·GPU/MIG를 준비하고 제품은 설치하지 않습니다. k3s가 아직 없으면 GPU preflight를
마친 뒤 일반 설치에서 Kubernetes 자원 노출을 확인합니다. 일반 설치에도 같은 단계가 포함되므로
별도 실행은 선택 사항입니다.

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

서버의 전체 설치 Plan 순서는 다음과 같습니다.

```text
precondition -> infra -> GPU preflight -> config -> iam -> agent -> vision -> app
```

기존 k3s 서버의 Solution-only 설치는 실행 계정에 기본 kubeconfig가 없으면 서버의 k3s 설정을 자동 준비합니다.
기존 App HTTP NodePort를 같은 공개 포트의 App `/`·IAM `/iam` Ingress로 전환할 때는
Helm 소유권과 기존 manifest를 확인해 자동 전환하며, 전환 실패 시 확인된 변경만 원래 NodePort로 복구합니다.

Agent/Vision을 선택한 타겟에는 GPU preflight가 자동 포함됩니다.
Agent, Vision, App 중 하나를 선택하면 Config가 자동으로 포함됩니다. Config를 별도로 실행하지
않습니다. Component-only 작업은 선택하지 않은 제품 component를 실행하지 않습니다. Agent는
Release 관리 values로 `eva-agent`, `eva-agent-vllm`, `eva-agent-qdrant`를 함께 설치합니다.

Runtime bootstrap과 Online Infra 설치는 APT 저장소를 자동 확인합니다. 지원하는 서명 키 문제는
자동 복구하고 재검증한 뒤 설치를 계속합니다. Bastion 설치에서도 각 타겟에서 처리하며
별도 진단·복구 명령을 실행할 필요가 없습니다. 복구 후 검증이 실패하면 기존 저장소 설정을 되돌리고
설치를 중단합니다. 지원하지 않는 오류는 설치 로그에서 원인을 확인합니다.
NVIDIA Container Toolkit의 알려진 서명 키 누락도 자동 복구합니다. Solution-only GPU 감지에서는
설치된 `lspci`를 재사용하며, 도구가 없을 때만 APT를 확인하고 패키지를 설치합니다.

인증서 SAN 검증이 실패하면 해당 IAM/App 외부 주소를 SAN에 포함한 인증서와 key를 같은 host path에
다시 준비한 뒤, 설정 값을 바꾸지 않았다면 실패한 작업을 재시도합니다.

```bash
sudo eva retry --yes
```

### NKS + Solution만

Bastion의 kubeconfig 소유 계정으로 sudo 없이 같은 명령을 실행합니다.

```bash
eva install --config "$HOME/eva-install.yaml"
```

기존 Workspace → Plan → Apply → Solution Ansible 역할에서 서버 Infra 의존 작업만 제외합니다.
설치 시작 시 API·인증·이미지 접근을 확인하고, 각 역할이 workload 변경 전에 스토리지·리소스 참조·GPU 배치를 확인합니다.
이전에 비활성화된 AWS 키는 Registry 주소만 바꿔도 사용할 수 없으므로 유효한 키를 입력합니다.

NKS의 결과 확인·재시도도 기존 `eva check --verbose`, `eva status`, `eva retry`를 sudo 없이 사용합니다.
작업 기록·로그는 계정의 `~/.local/state/eva`, IAM·Argo CD handoff는 그 아래 `sites`,
Helm Values·App DB의 private 백업은 `backups` 아래에 저장합니다.
설정이나 Values를 바꿨다면 새 `eva install --config`를 실행합니다.

새 Release로 업데이트하기 전에는 같은 Guard로 기존 6개 Helm manifest를 점검할 수 있습니다.
아래 명령은 Release를 Bastion에 풀고 Kubernetes 자원을 조회하며, 제품을 설치하거나 변경하지 않습니다.

```bash
eva workspace validate --config "$HOME/eva-install.yaml"
eva_base_release="/실제/새/eva-base-release/경로"
eva verify --release "$eva_base_release"
eva release prepare --release "$eva_base_release" --install-root "$HOME/.local/share/eva/releases"
eva_review_version=$(python3 -c 'import sys,yaml; from pathlib import Path; print(yaml.safe_load((Path(sys.argv[1]) / "release.yaml").read_text())["version"])' "$eva_base_release")
eva_guard="$HOME/.local/share/eva/releases/$eva_review_version/src/solution/roles/eva_cluster_guard/files/guard_cluster.py"
KUBECONFIG="$HOME/.kube/config" python3 "$eva_guard" --installed iam app agent vision vllm qdrant
```

`eva verify` 출력의 버전·경로가 이번 수정이 포함된 새 Release인지 먼저 확인합니다.
NKS configure에서 Release 경로를 비우면 기존 공통 선택 규칙을 사용합니다. 이미 작성한
`deployment.release`는 그대로 적용되므로, 이전 경로가 남아 있다면 고치거나 설치 시
`--release "$eva_base_release"`로 명시합니다. `--installed`를 지원하지 않는 오류는 구버전 Guard를 실행한 것입니다.

이 결과는 기존 manifest의 참조·스토리지·GPU 배치를 검증합니다. 변경할 Values의 최종 렌더링이나
새 이미지·모델 접근 성공을 보장하지는 않습니다. 미설치 Release는 `[SKIP]`하며,
Pending PVC·일시적인 GPU 배치·선택적 전역 조회 권한 부족은 경고로 안내합니다.
설치 전 Guard는 기존 데이터 경로 변경·PVC 축소·필수 참조 누락·확인 가능한 NodePort 충돌을 중단합니다.
차트는 서버 조회를 허용해 렌더링하므로 `lookup`으로 재사용하는 기존 PVC도 반영합니다.
역할이 이미 준비한 `cluster-request.json`은
`python3 "$eva_guard" --request <파일 경로>`로 다시 점검할 수 있습니다. `--request`는 여러 번 지정할 수 있고,
요청 파일에 기록된 kubeconfig·post-renderer 환경을 사용합니다. 요청 파일에는 민감값이 있을 수 있으므로 공유하지 않습니다.

## 9. 설치 결과 확인

설치 성공 시와 `eva check`의 `Access URLs`에서 설치한 App/IAM 접속 주소를 확인합니다.
n8n을 설치했다면 n8n 주소도 표시됩니다. 각 주소에는 설정한 HTTP/HTTPS, 공개 포트와 경로가 반영됩니다.

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

## 10. Rendered Artifact 및 문제 해결

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

/var/lib/eva/sites/<site>/<target>/eva-iam.yaml  # 온프레미스: root:root, 0600
# NKS: ~/.local/state/eva/sites/<site>/<target>/eva-iam.yaml (설치 계정, 0600)
```

private values 및 handoff 파일을 붙여 넣거나 커밋하지 않습니다. license, SSO, database,
registry credential이 포함될 수 있습니다.

## 11. 변경 및 재시도

단일 설정 또는 Release version을 변경하고 `eva install --config ...`를 다시 실행하면
Helm upgrade를 수행하며 기존 database와 영속 데이터를 보존합니다. 일반 upgrade는 `/eva-app`
또는 Agent/Vision persistence를 삭제하지 않습니다. Config는 선택된 component에 맞춰 자동
생성됩니다. 입력 변경 없이 외부 실패 원인을 해결했다면 해당 작업을 재시도합니다.
`retry`는 원래 작업의 Workspace snapshot을 사용하며 수정한 원본 설정을 다시
읽지 않습니다. 설정을 수정했다면 `install --config`로 새 작업을 시작합니다.

App의 기존 Helm 소유 PV/PVC는 재사용합니다. 실제 Argo CD 관리 표시 없이 과거 필드 관리 기록만
남아 있으면 handoff 파일 없이 현재 Helm 리소스의 필드를 갱신합니다. 다른 소유자의 저장소,
PVC 용량 축소·StorageClass 변경은 별도 확인이나 마이그레이션이 필요합니다.
App 소유권 검사도 서버 조회를 허용해 렌더링합니다. 차트의 `lookup`으로 기존 `keep` PVC가
저장 manifest에서 생략된 경우에도 이를 신규 인수 대상으로 판단하지 않습니다.

```bash
sudo eva retry --yes
```

## 12. 비GPU 테스트 환경

CPU-only 서버는 제한된 테스트를 위한 예외 환경입니다. 단일 설정의 `components`에서 `agent`와 `vision`을
`false`로 설정하면 Infra와 Config가 NVIDIA runtime/CDI 작업을 건너뜁니다. 이 가이드의 기본
Online 운영 환경은 아닙니다.
