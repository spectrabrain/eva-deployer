# EVA Remote Repository 설치 가이드

이 가이드는 인터넷에 연결된 Main 서버에서 EVA 설치 자산을 준비하고, 외부 인터넷이
차단된 Target에 Main Harbor와 검증된 Remote 자산으로 EVA를 설치하는 절차입니다.
설정 작성·설치 실행·결과 확인은 Main에서 수행하며 Target은 대화형 설정에 입력합니다.
Main bootstrap과 Target 설치는 Docker, k3s, NVIDIA runtime 및 호스트 영속 데이터를
변경할 수 있으므로 먼저 작업 가능 시간과 백업 정책을 확인합니다.

## 1. 대상 환경과 작업 위치

| 작업 위치 | 필수 운영 기준 |
| --- | --- |
| [Main] OS/platform | Ubuntu 24.04, `linux/amd64` |
| [Main] 네트워크 | AWS, ECR, Docker Hub, GitHub, package source 접근 |
| [Main] 저장 공간 | image, model, package 및 Harbor 데이터를 위한 충분한 공간 |
| [Main] Harbor | Target node와 Pod가 접근 가능한 `host:port` endpoint |
| [Target] OS/platform | Ubuntu 24.04, `linux/amd64` |
| [Target] GPU | GPU를 사용하는 구성에서는 지원 NVIDIA GPU, driver 및 `nvidia-smi` |
| [Target] 네트워크 | Main Harbor 및 Main SSH 경로만 사용; 외부 인터넷은 사용하지 않음 |
| 권한 | Main과 Target에서 `sudo eva ...`를 실행할 수 있는 사용자 |

Main은 EVA Managed Runtime, 이미지 게시용 Docker, Harbor와 private `eva` project(`public: false`)를 준비합니다.
신규 Main의 Harbor는 Docker Compose와 HTTP endpoint로 구성합니다.
기존 k3s 또는 별도 방식으로 운영하는 Harbor도 재사용할 수 있습니다.
Target에는 Main의 Runtime, package/model payload 및 Harbor image를 사용해 EVA를
설치합니다. Target에서 AWS, S3, ECR, Docker Hub 또는 Hugging Face로 fallback하지
않습니다.

## 2. [Main] EVA Tool 설치 및 Base Release 검증

설치를 시작할 파일은 `eva-base-release-<version>.zip`입니다. 압축을 푼 Release root에는
최소 다음 파일이 있어야 합니다.

```text
eva-tool_<version>_linux_amd64.tar.gz
eva-tool-installer.sh
eva-infra_<version>.tar.gz
eva-solution_<version>.tar.gz
release.yaml
checksums.sha256
```

Remote Base Release에는 `eva-offline`이 필수가 아닙니다. Remote Runtime artifact와
Target payload는 이후 Main preparation 결과로 생성됩니다.

```bash
unzip -q eva-base-release-*.zip -d eva-base-release
cd eva-base-release
sudo bash ./eva-tool-installer.sh
sudo eva verify
```

installer는 Tool archive와 Base Release checksum을 검증하고, 실행한 Release를
Current Release로 등록합니다. 따라서 `sudo eva verify`는 새 shell에서도 이 Base Release의
무결성을 확인합니다. 다른 Release를 확인할 때만 `sudo eva verify --release <path-or-version>`를
명시합니다. 아래 Main preparation/verify/install도 인자 없이 같은 Current Release를
사용합니다. 다른 Release가 필요한 경우에만 해당 Remote 명령에 `RELEASE_PATH`를 명시합니다.

## 3. [Main] Harbor 입력 확인

정상 작업에 사용할 Main Harbor endpoint를 확인합니다.

```text
10.159.57.172:32080
```

Main Harbor endpoint에는 URL scheme을 넣지 않으며 `localhost` 또는 loopback 주소를
사용하지 않습니다. Target node와 Pod 모두 이 endpoint에 연결할 수 있어야 합니다.
기본 project는 `eva`입니다.

Managed Harbor 인증은 승인된 Secret 관리 절차로 준비합니다. `EVA_HARBOR_ADMIN_PASSWORD`가
비어 있지 않으면 그 값을 우선합니다. 비밀번호 CLI option은 없고, receipt와 로그에 비밀번호를
기록하지 않습니다. 공개 기본 비밀번호로 운영하지 않습니다.
Harbor 게시는 bootstrap에서 준비한 인증을 재사용합니다. 저장된 인증이 없거나 유효하지 않으면
bootstrap 안에서 게시 계정·비밀번호 또는 robot token을 입력받습니다.
Qdrant·Vision 모델 게시도 같은 인증을 사용합니다.
credential을 Release·payload에 넣지 않습니다.

## 4. [Main] Preparation Plane bootstrap

Main의 preparation plane을 준비하거나 기존 상태를 검증합니다.

```bash
sudo eva remote bootstrap \
  --registry 10.159.57.172:32080 \
  --yes
```

이 명령은 Managed Runtime과 이미지 게시용 Docker를 준비하고, 지정한 주소에 정상 Harbor가
있으면 자동 재사용하며 없으면 Harbor를 설치합니다. Docker·k3s 등 기존 설치 방식과
HTTP/HTTPS를 자동 확인하며 정상 HTTP endpoint가 있으면 우선 사용하므로,
기존 Harbor도 같은 명령으로 진행합니다.

bootstrap은 private `eva` project(`public: false`), 이미지·OCI artifact 게시 권한과 인증을
준비·검증하고 연결 설정을 저장합니다. 기존 데이터와 artifact는 보존합니다.
HTTP 연결 설정이 추가되면 Main Docker가 재시작될 수 있습니다.
Main k3s의 Helm Harbor가 HTTPS만 제공하면 기존 차트·버전·Values와 영속 데이터를 유지하면서
같은 NodePort를 HTTP로 전환합니다. 기존 설정은
`/var/lib/eva/preparation/main/harbor-backups`에 비공개로 백업하며, 전환 후 정상 응답을
확인하지 못하면 원래 Helm revision으로 복구합니다. 이미 HTTP인 Harbor는 연결 설정을 변경하지 않습니다.
주소가 다른 서비스이거나 기존 Harbor의 정상 상태를 확인할 수 없으면 기존 서비스를 보존하고 중단합니다.

인증이 만료되었다면 `sudo eva remote bootstrap --yes`를 다시 실행하여 게시 계정을 갱신합니다.
Target의 k3s, GPU, NFS 또는 EVA component는 이후 Target 설치 단계에서 준비합니다.

## 5. [Main] Remote 자산 준비

`remote prepare`에는 Main에서 사용할 AWS 인증이 필요합니다. 명령 시작 시 저장된 인증을
검증하고, 인증이 없거나 검증에 실패하면 대화형으로 AWS Access Key ID, AWS Secret Access Key와
Region을 입력받습니다. Region의 기본값은 `ap-northeast-2`입니다.
검증된 인증은 자동 저장하여 다음 준비 작업에서 재사용하므로 인증 파일을 직접 작성할 필요가 없습니다.
Main의 AWS 계정에는 이미지 ECR pull과 모델 S3 list/read 권한이 필요합니다.
AWS 인증은 Main에서만 사용하며 Target에 전달하지 않습니다.

검증한 Base Release를 준비합니다. registry/project는 bootstrap receipt에서 자동으로
해석됩니다.

```bash
sudo eva remote prepare
```

성공하면 Remote Runtime과 Target의 패키지·Chart·helper·Agent 모델 자산이 준비되고,
제품 이미지(n8n 포함)·Infra 이미지와 Qdrant snapshot은 Main Harbor에 게시됩니다.

### Agent 이미지·모델 공급

Main은 선택한 Release에 필요한 Agent 이미지·모델·snapshot을 자동 준비합니다.
개별 차트·이미지 버전이나 snapshot 파일명을 따로 지정하지 않습니다.

- Agent 및 전송 도구 이미지는 ECR에서, vLLM 모델과 Qdrant snapshot은 S3에서 받습니다.
  Main의 AWS 계정에 ECR pull과 S3 list/read 권한이 필요합니다.
- Target에는 vLLM 모델 캐시를 전달하고 이미지와 Qdrant snapshot은 선택한 Main Harbor에서 공급합니다.
- Target에는 유효한 Harbor 인증을 준비합니다. 설치가 필요한 Secret을 자동 구성하므로
  수동 Secret 생성은 필요하지 않습니다. Target에 AWS credential을 전달하지 않습니다.
- Agent의 Vision 기능을 사용하려면 Target에 Vision도 설치되어 있어야 합니다.
  기본 연결 주소는 `http://eva-vision.eva-vision:8000`입니다.
- Target에 모델 캐시를 위한 충분한 디스크 공간을 확보합니다.
  재설치·업그레이드에서 모델 캐시와 Qdrant 데이터의 PV/PVC를 삭제하지 않습니다.

### Vision 모델 공급

Vision weights는 컨테이너 이미지와 별도로 공급하며 Target payload에 포함하지 않습니다.
Main의 `eva remote prepare`가 S3에서 모델을 다운로드·검증해 Main Harbor에 게시합니다.
Main에는 모델 S3의 list/read와 Vision 이미지 ECR pull 권한이 필요합니다.
**모델 게시가 기존 배포에 영향을 줄 수 있으므로 변경 승인을 확인합니다.**
Target의 Pod는 Main Harbor에서 모델을 받아 영구 캐시에 저장하고 서비스를 준비합니다.
Target은 S3에 접근하지 않으며 AWS credential이나 별도 weights 파일 전송이 필요하지 않습니다.
인증은 기존 EVA Infra의 Harbor 설정을 재사용하며 수동 Secret 생성이나 추가 설치 명령은 필요하지 않습니다.
첫 실행은 오래 걸릴 수 있으므로 다운로드·압축 해제와 영구 캐시를 위한 디스크 공간을 확보합니다.
**재시도·upgrade 시 모델 PVC를 삭제하지 않습니다.**
기본 모델을 사용하며, 승인된 custom 모델이 필요할 때만 준비와 설치의 모델 선택·설정을 일치시킵니다.

## 6. [Main] 대화형 설치 설정 준비

Main에서 설치 설정 YAML 하나를 작성합니다. Online Bastion 설치와 같은 대화형 절차로
타겟 닉네임·IP·SSH 계정·비밀번호를 입력하고 타겟별 설치 구성을 선택합니다.
설치 설정 파일은 Main에 보관하며 기존 파일은 덮어쓰지 않습니다.

```bash
eva install configure
```

sudo 없이 Main에서 실행하고 설치환경 **remote** → **Bastion/Main에서 SSH 설치**를 고릅니다.
타겟을 한 대씩 등록하고 Infra-only/Solution, HTTP/HTTPS, 도메인/IP, IAM 함께 설치/외부 IAM,
Agent/Vision 구성을 선택합니다. 이전 타겟의 설치 옵션을 재사용하거나 타겟을 더 추가할 수 있습니다.
SSH 비밀번호는 sudo에도 사용하며, passwordless sudo라면 생성 파일에서 `ansible_become_password`를 제거합니다.
결과는 사용자 홈의 `eva-install.yaml`입니다.
`--output`은 새 `0600` 파일만 생성합니다. `<...>` 입력을 모두 채우거나 해당 선택 항목을
제거하기 전에는 검증에 실패합니다.

다음은 다른 도메인 + HTTPS 예시입니다. 동일 주소 틀은 IAM `/iam`, App `/`를 사용합니다.

```yaml
schema_version: v1
site:
  id: customer-a
repository:
  mode: remote
  registry: harbor.example.com:32080
  project: eva
components:
  infra: true
  iam: true
  agent: true
  vision: true
  app: true
  n8n: false
targets:
  server-a:
    ansible:
      ansible_connection: ssh
      ansible_host: "<TARGET_IP>"
      ansible_user: eva
      ansible_password: "<SSH_PASSWORD>"
      ansible_become: true
      ansible_become_password: "<SUDO_PASSWORD>" # 필수: 대상 sudo 비밀번호
      repository_username: "<HARBOR_PULL_USERNAME>"
      repository_password: "<HARBOR_PULL_PASSWORD>" # Target에 기존 registry 인증이 없을 때 필요
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
          backendHost: app.example.com
          backendSecure: true
          backendSecurePort: 443
          license:
            activation_mode: offline # Remote 기본값. 사전 결정한 라이선스가 온라인이면 online으로 변경합니다.
            product_code: "<LICENSE_PRODUCT_CODE>" # 필수: 발급된 라이선스와 일치하는 제품 코드. ex) eva-dev 또는 eva-prod
            api_key: "<LICENSE_API_KEY>" # 필수: 발급된 API key
            shared_key: "<LICENSE_SHARED_KEY>" # 필수: 발급된 shared key
```

TLS certificate/key는 target의 `hostPath`에 별도로 준비합니다. `targets`는 inventory alias별
mapping이며, SSH 접속 주소는 `ansible_host`에 지정합니다. SSH private key의 상대 경로는 설정 파일 기준입니다.
필요한 Agent/Vision Chart override만 `values.agent`/`values.vision`에 추가하고,
GPU/MIG/profile과 repository-derived image 설정은 고정하지 않습니다.

Remote의 라이선스 활성화 기본값은 `offline`이며 별도 연결 질문은 하지 않습니다.
사전 발급된 온라인 라이선스를 사용하면 `activation_mode: online`으로 변경하고 App에서
라이선스 서버로 연결할 수 있도록 준비합니다. 이미지 공급 방식과 라이선스 방식은 독립적입니다.

Remote/Local 설정의 `credentials.aws`는 거부됩니다. AWS/S3/ECR credential은 Main에만
관리하며 Target 입력과 Release에 넣지 않습니다. 원본 설정과 내부 생성 Workspace는
Secret이며 커밋하지 않습니다. Deployer가 내부 Workspace를
`/var/lib/eva/workspaces/<site-id>/<content-sha256>/`에 생성하므로 폴더와 여러 파일을
직접 만들 필요가 없습니다. 디렉터리 권한은 `0700`, 파일 권한은 `0600`입니다.

### 기존 k3s에 n8n만 설치

단일 노드 Target에 정상인 k3s와 `local-path` StorageClass가 있으면 `infra: false`로
n8n만 설치할 수 있습니다. Main에서 Remote 자산 준비·검증을 먼저 완료합니다.
Target에서 외부 인터넷용 `eva runtime bootstrap`을 실행하지 않습니다.

```yaml
schema_version: v1
site:
  id: automation-a
repository:
  mode: remote
  registry: harbor.example.com:32080
  project: eva
components:
  infra: false
  iam: false
  agent: false
  vision: false
  app: false
  n8n: true
targets:
  server-a:
    ansible:
      ansible_connection: ssh
      ansible_host: "<TARGET_IP>"
      ansible_user: eva
      ansible_password: "<SSH_PASSWORD>"
      ansible_become: true
      ansible_become_password: "<SUDO_PASSWORD>"
      n8n_public_host: "<TARGET_IP>"
      n8n_node_port: 30678
      repository_protocol: http
      repository_username: "<HARBOR_PULL_USERNAME>"
      repository_password: "<HARBOR_PULL_PASSWORD>"
```

n8n 설정은 `targets.server-a.ansible`에 둡니다. 기존 Target `registries.yaml`에
해당 Harbor 인증이 있거나 n8n namespace에 사용할 pull Secret이 있으면 계정 입력을
생략할 수 있습니다. Secret 이름을 지정할 때는 `n8n_image_pull_secret_name`을 사용합니다.
기존 Secret은 덮어쓰지 않으므로 인증 만료 시 운영자가 갱신합니다.

설치는 Harbor의 n8n 이미지를 선택하고 타겟의 registry 연결 설정과 pull 인증을 준비합니다.
기존 registry·CA 설정을 보존하며, 해당 Harbor 설정이 변경된 경우에만 k3s를 재시작하고
Kubernetes API가 준비될 때까지 기다립니다.
임시 Pod로 실제 이미지 pull을 확인한 뒤 n8n Deployment와 PVC를 적용합니다.

```bash
sudo eva install --config /home/eva/eva-install.yaml --yes
sudo eva check
```

Pod `1/1 Running`, PVC `Bound`를 확인하고 `http://<TARGET_IP>:30678/`에 접속합니다.
기본 데이터 저장소는 `n8n-data` PVC이며, 재설치·업그레이드 시 삭제하지 않습니다.
수동 manifest로 설치한 n8n은 배포 이름·namespace·PVC를 맞추고 기존 owner 계정 입력도
확인한 뒤 Tool 설치로 전환합니다. EVA Alert Workflow는 Tool의 n8n 설치 단계에서 구성합니다.

## 7. 설치 전 타겟 확인

설치는 각 타겟의 `/etc/rancher/k3s/registries.yaml`에 Main에서 확인한 Harbor의 HTTP/HTTPS
endpoint를 자동 반영합니다. Infra를 선택하지 않은 기존 k3s 타겟도 설치 앞단에서 처리하므로
타겟마다 파일을 직접 작성하거나 k3s를 수동 재시작할 필요가 없습니다.
선택한 Harbor에는 `configs.<registry>.tls.insecure_skip_verify: true`를 자동 적용합니다.
다른 registry와 기존 인증·CA 설정은 보존하며 변경이 있을 때만 k3s를 재시작합니다.
HTTP endpoint에는 인증서 검증 설정이 사용되지 않습니다.

Agent/Vision 또는 GPU Infra를 설치할 타겟에는 NVIDIA driver가 준비되어 있어야 합니다.
타겟에서 `nvidia-smi`로 GPU 상태를 확인하고 MIG를 사용할 경우 의도한 구성을 적용합니다.
n8n 단독 설치에는 GPU 점검이 필요하지 않습니다. TLS certificate/key도 설정한 타겟의
`hostPath`에 준비합니다. `infra`와 Config 단계가 타겟의 GPU/MIG 자원과 Device Plugin을 확인합니다.

### Argo CD 관리 연결 확인

기존 Argo CD로 EVA를 관리하던 타겟이 있다면 Main에서 설치 전에 실행합니다.
이미 연결을 해제한 타겟도 같은 명령으로 확인할 수 있습니다.

```bash
sudo eva preflight argocd --config /home/eva/eva-install.yaml
```

- 명령 시작 시 Argo CD 관리 서버 주소·SSH 계정·비밀번호를 한 번 입력합니다.
  설정에 포함된 모든 타겟에 같은 관리 서버 연결을 사용합니다. 타겟의 SSH 접속 정보는
  설치 설정을 사용하며, 타겟마다 관리 서버 정보를 다시 입력하지 않습니다.
- 처음 접속하는 SSH host key는 fingerprint를 확인한 뒤 승인합니다. 관리 서버에서
  `kubectl`·`git`을 사용할 수 있어야 하며 등록 저장소 읽기·push 권한이 필요합니다.
- 타겟별로 관리 상태를 확인하고 연결 해제와 해당 클러스터의 Git 등록 변경을 별도 승인합니다.
  Git 변경이 필요한 경우 요청 시 Git 인증 정보를 입력합니다. 배포된 workload와 PVC, 다른 클러스터는 유지합니다.
- 완료 기록은 타겟별로 저장되며, 설치 시 해당 타겟에 자동 전달됩니다. 수동으로 복사할 필요가 없습니다.
  관리 metadata가 없는 타겟은 연결 해제 없이 통과합니다.
- 연결 해제 완료가 확인되지 않으면 Solution 설치를 중단합니다. `--yes`로 우회할 수 없습니다.

## 8. [Main] EVA 설치

Main Harbor의 준비가 끝난 Release와 설치 설정을 검증한 뒤 Main에서 실행합니다.
설정 검증은 타겟 접속이나 설치를 수행하지 않습니다.

```bash
sudo eva workspace validate --config /home/eva/eva-install.yaml
sudo eva install --config /home/eva/eva-install.yaml --yes
```

`install`은 설정에 포함된 타겟의 SSH·sudo·플랫폼·저장 공간을 확인한 뒤,
검증된 Release와 Remote Runtime/payload를 전달하고 타겟의 Tool 설치와 EVA 설치를 실행합니다.
타겟을 위한 별도의 등록 명령이나 타겟에서 설정 파일을 다시 작성하는 과정은 필요하지 않습니다.
각 타겟에는 해당 타겟의 설치 값만 전달하며 Main AWS credential과 다른 타겟의 연결 정보는 전달하지 않습니다.
여러 타겟은 각각 독립된 설치이며 하나의 Kubernetes cluster로 묶지 않습니다.
일부 component만 설치할 때는 `--component n8n`처럼 지정합니다. Chart Values 변경은
`targets.<name>.values`에 작성하며, Remote Main 설치에서는 CLI `--chart`·`--values`·`--set`을 사용하지 않습니다.

타겟의 Managed Runtime과 package/model cache는 검증된 delivery에서 준비합니다.
image·Qdrant snapshot·Vision 모델은 Main Harbor를 사용하며 타겟에서 외부 저장소로 fallback하지 않습니다.
기존 PVC와 영속 데이터는 재사용합니다. 설치 중 기존 서비스가 재시작될 수 있습니다.

다른 준비된 Release를 사용할 때만 Main의 원본 Release 경로를 명시합니다.

```bash
sudo eva install --release /path/to/eva-base-release \
  --config /home/eva/eva-install.yaml --yes
```

현재 타겟에서 직접 설치해야 하는 경우에는 대화형에서 **현재 서버에 설치**를 선택하고,
Main에서 게시한 Release의 Tool을 설치한 뒤 타겟에서 같은 `eva install --config` 명령을 사용합니다.

## 9. [Main] 설치 결과 확인

설치 성공 시와 `eva check`의 `Access URLs`에서 설치한 App/IAM과 Main Harbor 접속 주소를 확인합니다.
n8n을 설치했다면 n8n 주소도 표시됩니다. 여러 타겟에 설치한 경우 서비스 주소에 타겟 이름이 함께 표시됩니다.

Main에서 타겟별 설치 상태와 작업 결과를 확인합니다.

```bash
sudo eva check --verbose
sudo eva status
```

선택한 Agent 또는 Vision component가 있으면 GPU/MIG allocatable resource, NVIDIA Device
Plugin readiness 및 workload의 할당 상태도 확인합니다. workload image가 Main Harbor prefix를
사용하는지, Target에 AWS credential이나 public source 설정이 남지 않았는지도 확인합니다.
Vision은 모델 다운로드·준비 완료와 모델 PVC 재사용, 실제 분석 요청도 확인합니다.

## 10. [Main] 재실행 및 문제 해결

입력 수정 후에는 동일 Release와 단일 설정으로 `install`을 다시 실행합니다. 설정이 바뀌면
새 Workspace snapshot을 사용합니다. 입력 변경이 없는 실패는 `retry`로 재시도합니다.
`retry`는 원래 snapshot을 사용하며 원본 YAML을 다시 읽지 않습니다.

```bash
sudo eva install \
  --config /home/eva/eva-install.yaml \
  --yes

sudo eva retry --yes
```

동일 Runtime/payload identity와 기존 영속 데이터는 재사용합니다. Release checksum,
delivery marker 또는 Runtime/payload 검증 오류는 Target에서 임의로 고치지 말고 Main의
준비·검증·게시 절차를 다시 수행합니다.
여러 타겟 중 일부가 실패하면 Main 작업도 실패로 기록됩니다. `retry`는 선택한 모든 타겟에
원래 설치 구성을 다시 적용하므로 재실행 전 각 타겟의 서비스 상태를 확인합니다.

Vision 모델 준비 실패는 Main의 모델 게시 결과와 Target의 Harbor 접근·기존 EVA Infra 인증 설정을
확인합니다. Target에서 S3 fallback을 켜거나 실패를 우회해 workload를 중단하지 않습니다.
