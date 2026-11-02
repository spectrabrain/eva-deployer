# EVA Local Repository 설치 가이드

폐쇄망 대상 서버 한 대에 EVA를 설치하는 절차입니다. 인터넷 준비 서버에서 Bundle을 만들고,
대상 서버에서 **Tool 설치 → 설정 작성 → Local 준비 → 설치 → 결과 확인** 순서로 진행합니다.
`eva airgap prepare`와 `eva local prepare`가 포함된 Tool·Release를 사용합니다.
Main Harbor에서 자산을 받을 수 있는 환경은 [Remote 설치 가이드](remote-repository-runbook.md)를 따릅니다.

## 1. [Airgap Bundle 준비 서버] Tool 설치 및 Bundle 생성

폐쇄망 반입 전에 **인터넷에 연결된 별도 준비 서버**에서 `eva airgap prepare`로 Bundle을 만듭니다.
Base Release 압축을 풀고 포함된 installer로 EVA Tool을 설치한 뒤, 해당 Release로 준비합니다.

### 준비 서버 확인

| 항목 | 진행 조건 |
| --- | --- |
| OS·권한 | Ubuntu 24.04, `linux/amd64`, sudo 권한 |
| Tool·Release | 설치할 버전의 Base Release ZIP |
| Docker·AWS CLI | Docker daemon과 AWS CLI 설치 완료; `prepare`는 이 두 도구를 설치하지 않습니다. |
| 다운로드 접근 | 패키지·차트·Registry·모델·snapshot 경로와 AWS STS 접근, 이미지 ECR pull·모델 및 snapshot S3 list/read 권한 |
| OS 패키지 | Ubuntu·Docker·NVIDIA APT 저장소와 인덱스 준비, 대상 OS·kernel에 맞는 의존 패키지 |
| 공간 | Bundle 파일, 다운로드 캐시와 검증용 압축 해제를 수용할 디스크 공간; 사용자 홈과 임시 공간 모두 확인 |

`prepare`가 필요한 도구와 자산을 확인하며, 부족한 항목이 있으면 중단합니다.
Runtime이 없으면 준비 서버에 온라인으로 설치하므로 APT/pip 변경이 가능한 서버를 사용합니다.
AWS 인증은 `/var/lib/eva/credentials/aws_key.ini`를 재사용하고, 사용할 수 없으면 터미널에서
숨김 입력으로 받습니다. 비대화형 실행은 해당 파일을 미리 준비합니다.

### 준비 서버의 Tool 설치와 Release 검증

준비할 `eva-base-release-<version>.zip` 하나를 사용자 홈에 둡니다.
아래 `<version>`을 전달받은 ZIP 파일명의 버전으로 바꿉니다.
기존 `eva-base-release` 폴더가 있다면 별도 보관한 뒤 새 폴더에 풉니다.

```bash
(
  set -e
  cd "$HOME"
  unzip -q 'eva-base-release-<version>.zip' -d eva-base-release
  cd eva-base-release
  sudo bash ./eva-tool-installer.sh
  sudo eva verify
  eva airgap prepare --help
)
```

installer는 Release를 검증하고 **Current Release**로 등록합니다. 이후 명령은 해당 Release를
자동으로 사용하므로 `--release`를 지정할 필요가 없습니다. 압축을 푼 폴더는 보존합니다.
Bundle 생성에는 **Base Release 원본**을 사용합니다.

### Bundle 생성과 검증

Agent를 설치할 대상 서버의 GPU 구성으로 **GPU profile만 선택하면 됩니다.**
준비 서버의 GPU와는 무관합니다. `PRO6000-MIGx4`는 GPU 1장을 MIG 4개로 나눈 구성입니다.

| 대상 GPU | 물리 GPU 수 | MIG 구성 | `--vllm-profile` |
| --- | --- | --- | --- |
| NVIDIA RTX A6000 | 1장 | 사용 안 함 | `A6000x1` |
| NVIDIA L40S | 1장 | 사용 안 함 | `L40sx1` |
| NVIDIA RTX PRO 5000 Blackwell | 3장 | 사용 안 함 | `PRO5000x3` |
| NVIDIA RTX PRO 6000 Blackwell | 1장 | 4개 | `PRO6000-MIGx4` |

예를 들어 대상 서버가 L40S 1장이고 MIG를 사용하지 않으면 다음과 같이 실행합니다.

```bash
sudo eva airgap prepare --vllm-profile L40sx1 --yes
```

선택한 profile의 vLLM 이미지만 포함하므로 Agent 설치 시 대상의 GPU·MIG 구성도 표와 같아야 합니다.
다른 profile용 Bundle은 `--output`으로 다른 파일명을 지정해 준비합니다.
Agent를 설치하지 않는 경우에는 예시의 `L40sx1`을 사용해도 됩니다.

드라이버 옵션을 생략하면 모든 profile에서 **`nvidia-driver-580`과 의존 패키지**를 준비합니다.
대상에서 `nvidia-smi`가 정상 동작하면 기존 드라이버를 유지하고, 없으면 Infra 단계에서
`nvidia-driver-580`을 설치합니다. GPU profile과 드라이버 패키지의 1:1 매핑은 없습니다.

추가 옵션은 필요한 경우에만 지정합니다. 전체 옵션은 `eva airgap prepare --help`로 확인합니다.

| 옵션 | 생략 시 | 지정할 때 |
| --- | --- | --- |
| `--driver-package PACKAGE` | `nvidia-driver-580` | 다른 드라이버를 반입할 때. 신규 설치 시 `targets.server-a.ansible.gpu_driver_package`도 같은 패키지로 지정합니다. 예: `nvidia-driver-580-open` |
| `--output FILE.tar.gz` | 사용자 홈의 기본 파일명 | 저장 위치를 바꾸거나 같은 Release의 다른 profile용 Bundle을 만들 때 |
| `--display-mode-selector FILE.zip` | 포함 안 함 | 대상 GPU의 Display Mode 전환에 필요한 NVIDIA 배포 ZIP을 반입할 때 |
| `--release PATH` | installer가 등록한 Current Release | 다른 Base Release로 준비할 때 |

Bundle에는 해당 Release의 설치 패키지·차트·이미지·모델·Qdrant snapshot과 Runtime,
Harbor 설치 파일이 포함됩니다. 실행 시 선택한 profile·드라이버 패키지와 저장 경로를 표시합니다.
Bundle은 `sudo`를 실행한 원래 사용자 홈에
`eva-airgap-bundle_<version>_<os>_<arch>.tar.gz`로 저장하며 해당 사용자가 복사할 수 있게
소유권을 맞춥니다. 기존 파일은 덮어쓰지 않습니다.
중단·실패하면 원인을 해결한 뒤 처음부터 다시 실행하고, 검증된 최종 Bundle만 반입합니다.

`prepare`는 Bundle을 검증한 뒤 최종 파일을 저장하고 위치·파일명·반입용 SHA-256을 표시합니다.
사용자가 `eva`, Release가 `3.3.0`인 경우 출력 예시는 다음과 같습니다.

```text
[OK] Airgap Bundle verified: /home/eva/eva-airgap-bundle_3.3.0_linux_amd64.tar.gz
[OK] Bundle SHA-256: <생성된-Bundle의-SHA256>
[OK] Bundle directory: /home/eva
[OK] Bundle filename: eva-airgap-bundle_3.3.0_linux_amd64.tar.gz
```

Bundle과 함께 Release 버전·GPU profile·드라이버 패키지·대상 OS/kernel 조건을 인계합니다.
출력된 SHA-256은 별도의 신뢰할 수 있는 경로로 전달해 대상 서버에서 비교합니다.
Release 버전에 `v`가 있으면 파일명에도 포함되므로 실제 출력 파일명을 사용합니다.
AWS 인증과 고객 설치 설정·TLS 개인 키는 Bundle에 포함하지 않습니다.

## 2. [공통] 준비 범위와 담당 구분

| 담당 | 작업 서버 | 완료할 작업 |
| --- | --- | --- |
| 준비 담당자 | Airgap Bundle 준비 서버 | 1절의 Bundle 생성·검증, 파일과 SHA-256 인계 |
| 설치 담당자 | Local 설치 서버 | 3~8절의 환경 확인·Tool 설치·설정·Local 준비·설치·결과 확인 |
| 운영 담당자 | Local 설치 서버 및 기존 Harbor 서버 | Harbor 주소와 기존 인증, 서비스 주소·인증서·라이선스·데이터 보존 조건 인계 |

## 3. [Local 설치 서버] 설치 전 확인

| 항목 | 진행 조건 |
| --- | --- |
| 서버 | Ubuntu 24.04, `linux/amd64`, 설치 계정과 sudo 권한, 충분한 디스크·메모리 |
| 작업 승인 | Docker·k3s·GPU 실행 환경·영구 저장소 변경과 서비스 재시작이 가능한 시간 확보, 기존 데이터 백업 |
| Bundle | 준비 담당자가 전달한 파일·SHA-256·GPU profile·드라이버 패키지 확인 |
| Harbor | 사용할 실제 IP/DNS:port와 project 확인; 다른 서버의 Harbor를 사용하면 실행 중이어야 함 |
| GPU | Agent/Vision 설치 시 지원 GPU 확인, Agent는 Bundle과 동일한 GPU·MIG 구성 필요 |
| 서비스 | App/IAM 접속 주소, DNS·방화벽, TLS 인증서, 발급된 라이선스; 외부 IAM이면 SSO·Redis 연결값 |
| 데이터 | 영구 저장소 경로·용량·권한, 기존 데이터 백업 |

### Harbor 주소 입력

설치 설정의 `repository.registry`에는 **대상 서버에서 접근 가능한 실제 Harbor IP/DNS:port**를
입력합니다. `http://`·`https://` 같은 URL scheme은 넣지 않습니다.

| Harbor 구성 | 입력할 주소 |
| --- | --- |
| 대상 서버에 새로 설치 | 대상 서버 IP/DNS와 `32080`. 예: `192.0.2.20:32080` |
| 기존 또는 다른 서버의 Harbor 사용 | 해당 Harbor의 실제 IP/DNS:port. node와 Pod에서 모두 접근 가능해야 합니다. |

### 설치 구성·데이터 확인

- Agent의 Vision 기능을 사용하려면 설치 구성에서 Vision도 선택합니다.
- 기존 Harbor를 사용하면 이미지·모델을 게시할 권한이 있는 인증을 준비합니다.
- Local 설정에는 `credentials.aws`를 넣지 않으며, 대상 서버에 AWS credential을 반입하지 않습니다.
- 재시도·업그레이드 시 기존 모델 캐시와 Qdrant 데이터의 **PV/PVC를 삭제하지 않습니다.**

## 4. [Local 설치 서버] Bundle 반입·Tool 설치

준비한 Bundle을 대상 서버의 사용자 홈에 복사합니다. `<version>`은 준비 완료 시 출력된
파일명의 버전으로 바꿉니다. 처음에는 EVA Tool이 없으므로 반입 파일 확인과 압축 해제만 OS 명령으로 진행합니다.

```bash
cd "$HOME"
sha256sum 'eva-airgap-bundle_<version>_linux_amd64.tar.gz'
```

출력값이 준비 서버에서 기록한 SHA-256과 일치하면 Tool을 설치합니다.

```bash
(
  set -e
  cd "$HOME"
  mkdir eva-base-release
  tar -xzf 'eva-airgap-bundle_<version>_linux_amd64.tar.gz' \
    -C eva-base-release --no-same-owner --keep-old-files
  cd eva-base-release
  sudo bash ./eva-tool-installer.sh
  sudo eva verify
  eva local prepare --help
)
```

installer가 이 폴더를 Current Release로 등록하므로 설치 후에도 보존합니다.
기존 폴더가 있거나 압축 해제에 실패하면 내용을 확인·보관한 뒤 새 폴더에 다시 풉니다.
이후 5절에서 설정을 작성하고, 6절의 `eva local prepare`로 캐시·Runtime·Harbor를 준비합니다.
캐시 경로나 helper를 직접 지정할 필요는 없습니다.

## 5. [Local 설치 서버] 단일 설치 설정 작성

대상 서버의 **일반 계정**으로 실행합니다. 기본 결과는 사용자 홈의
`eva-install.yaml`이며 새 파일만 권한 `0600`으로 생성합니다.

```bash
eva install configure
```

1. 설치환경 **local**을 선택합니다.
2. **Infra-only / Infra + Solution**을 선택합니다.
3. Solution이면 HTTP/HTTPS, 동일 주소·별도 도메인·IP 직접 접속을 선택합니다.
4. IAM+App 함께 설치 또는 App+외부 IAM을 선택하고, 필요한 Agent/Vision을 고릅니다.
5. 생성 파일을 열어 남은 필수 입력과 고객사 설정을 채웁니다.

기존 설정 파일이 있으면 내용을 확인해 사용하거나 `--output FILE`로 새 파일을 생성합니다.
Local은 현재 서버에 설치하므로 `targets`에는 `ansible_connection: local`인 대상 하나를 둡니다.

| 설정 | 입력할 내용 |
| --- | --- |
| `site.id` | 고객사·설치 환경의 고유 Site ID |
| `repository` | `mode: local`, 실제 내부 Harbor의 `registry`, 승인된 `project` |
| `targets.server-a.ansible` | 대상 사용자, local 접속·sudo 정보 |
| `targets.server-a.values.iam` | 함께 설치할 IAM의 주소·관리자 설정·TLS·Redis |
| `targets.server-a.values.app` | App 공개 주소·TLS·라이선스, 외부 IAM 사용 시 SSO·Redis 연결값 |
| `targets.server-a.values.agent` / `targets.server-a.values.vision` | 기본값을 변경할 때만 추가 |

`<...>` 필수 입력은 모두 실제 값으로 바꿉니다. 대상의 인증서·데이터 경로는 절대 경로로 씁니다.
App/IAM만 설치할 때는 `components.agent`·`components.vision`을 선택하지 않습니다.
Values는 생성된 `targets.server-a.values.<component>` 아래에 작성합니다.

### 기본으로 처리되는 설정

기본 Local 설치에서는 아래 항목을 설치 YAML에 추가할 필요가 없습니다.

| 항목 | 기본 동작 |
| --- | --- |
| Harbor 계정 | 새 Harbor는 Deployer 기본 계정으로 준비하며, 기존 Harbor는 저장된 인증을 재사용합니다. `eva local prepare`가 k3s 인증도 구성합니다. |
| Qdrant snapshot | Local 모드에서는 `harbor` 방식과 `values-k3s.harbor.yaml`을 자동 선택합니다. |
| Agent vLLM profile | 설치 시 실제 GPU·MIG 구성으로 자동 결정합니다. 대상 구성이 Bundle 준비 시 선택한 profile과 같아야 합니다. |

기존 Harbor의 인증을 사용할 수 없을 때만 `eva local prepare`가 계정을 숨김 입력으로 받습니다.
비대화형 실행에서 인증을 지정해야 하면 `targets.server-a.ansible`의
`harbor_admin_user`·`harbor_admin_password`에 지정합니다.
`k3s_dockerhub_mirror_enabled: true`는 Docker Hub 주소를 그대로 참조하는 구형·custom 차트가
필요한 경우에만 사용하며, 기본 3.3.0 차트 설치에는 지정하지 않습니다.

전체 schema와 자동화용 샘플 옵션은 [단일 설치 설정](single-file-configuration.md)을 참고합니다.
설정과 비밀번호를 Git·Release·Bundle·채팅에 넣지 않습니다.

### 주소·TLS·IAM·라이선스

- **HTTPS 권장:** hostPath 인증서를 사용할 때 대상의 `ingress.tls.hostPath` 디렉터리에 PEM 형식의
  `tls.crt`와 `tls.key`를 준비합니다. 인증서 SAN은 실제 접속 DNS/IP와 일치해야 합니다.
  개인 키는 제한된 권한으로 관리합니다. 기존 TLS Secret 방식은 승인된 Secret을 먼저 준비합니다.
- **HTTP:** 격리된 테스트에만 사용합니다. wizard가 생성한 HTTP 설정을 사용하며 TLS 생성 여부만
  꺼서 전환하지 않습니다. Repository 모드와 TLS 설정은 별개입니다.
- **동일 주소:** App은 `/`, IAM은 `/iam`을 사용합니다. 비표준 외부 포트는 실제 Ingress/LB 노출,
  App 주소, IAM 공개 URL과 client redirect URI를 함께 맞춥니다.
- **IAM 함께 설치:** 같은 target의 SSO URL·client Secret은 Deployer가 자동 전달합니다.
  Secret을 터미널에 출력하거나 App 명령행으로 복사하지 않습니다.
- **외부 IAM:** App의 SSO URL·realm·client·관리 API Secret·Redis 연결값을 입력합니다.
  App Pod에서도 IAM/Redis에 접근할 수 있어야 하며, 외부 IAM의 App redirect URI·권한은 별도로
  승인·구성합니다. Deployer가 외부 IAM을 변경하지는 않습니다.
- **라이선스:** Local 기본 활성화 방식은 `offline`입니다. 발급된 제품 코드·API key·shared key와
  방식을 맞춥니다. `online` 라이선스를 의도적으로 사용한다면 App의 라이선스 서버 접근이 필요합니다.
  차트 공개 기본 비밀번호는 운영 정책에 맞게 변경하고 IAM Redis와 App 연결값도 함께 맞춥니다.

## 6. [Local 설치 서버] Local 준비와 사전 조건 확인

### Local 자산 준비

```bash
sudo eva workspace validate --config "$HOME/eva-install.yaml"
```

설정 검증이 성공하면 같은 설정 파일로 Local 설치 자산과 Harbor를 준비합니다.

```bash
sudo eva local prepare --config "$HOME/eva-install.yaml" --yes
```

이 명령은 Current Release의 캐시·Runtime을 자동 배치하고 필요한 Docker·Harbor를 준비한 뒤,
Bundle의 이미지를 적재·게시합니다. Agent를 선택하면 Qdrant snapshot도 게시합니다.
Vision 모델은 이후 `eva install`의 Vision 단계에서 게시하고 서비스 캐시에 배치합니다.
기존 Harbor의 설정과 데이터를 재사용하며, 필요한 자산은 Bundle에서 가져옵니다.

`[OK] Local preparation completed`를 확인한 뒤 설치를 진행합니다.
재실행 시 같은 Bundle의 캐시는 checksum 검증 후 재사용합니다. 다른 Bundle의 캐시나 손상된
캐시는 덮어쓰지 않으므로 오류 내용을 확인하고 조치합니다.
외부 IAM·Redis 접근, GPU, 인증서의 실제 유효성은 대상 환경에서 확인합니다.

### GPU preflight

Agent/Vision을 선택하면 `eva install`이 GPU 상태를 확인하고 필요한 구성을 적용합니다.
RTX PRO 6000 1장은 `PRO6000-MIGx4`에 맞춰 MIG 4개를 생성하며, 이미 맞는 구성은 재사용합니다.
`A6000x1`, `L40sx1`, `PRO5000x3`에는 MIG를 적용하지 않습니다.
Agent는 1절에서 준비한 Bundle profile과 대상 구성이 같아야 합니다.

설치 전에 GPU preflight만 실행하려면 `eva local prepare`를 완료한 뒤 같은 설정으로 실행합니다.

```bash
sudo eva preflight gpu
```

사용자 홈의 `eva-install.yaml`을 자동으로 읽으며, `sudo`로 실행해도 원래 사용자의 홈을 사용합니다.
다른 설정 파일을 사용할 때만 `--config FILE`을 지정합니다. 추가 실행 확인은 필요하지 않습니다.

필요한 드라이버와 GPU/MIG를 준비하며 제품은 설치하지 않습니다. k3s가 아직 없으면 GPU preflight를
마친 뒤 일반 설치에서 Kubernetes 자원 노출을 확인합니다. 다른 MIG 분할이 있거나 변경할 GPU에
workload가 실행 중이면 중단하므로 해당 구성을 정리한 뒤 다시 실행합니다.
재부팅 안내가 나오면 **해당 설치 서버를 재부팅하고 같은 명령을 다시 실행**합니다.
Display Mode 전환이 필요한 RTX PRO 6000은 Bundle 준비 시 `--display-mode-selector FILE.zip`을 포함합니다.

### 기존 Argo CD 관리 연결 확인

기존 Argo CD가 관리하던 Local 설치 서버라면 연결 해제를 먼저 확인합니다. Git 등록 변경에는 별도 승인이
필요하며 `--yes`로 우회할 수 없습니다. 필요한 관리 서버 접근이 없으면 먼저 운영 담당자와 조치합니다.

```bash
sudo eva preflight argocd --config "$HOME/eva-install.yaml"
```

## 7. [Local 설치 서버] EVA 설치

준비가 완료된 Current Release로 설치합니다.

```bash
sudo eva install --config "$HOME/eva-install.yaml" --yes
```

캐시·Runtime·Harbor는 앞의 `eva local prepare`에서 준비한 구성을 사용합니다.
Release를 바꾸면 새 Bundle의 Tool 설치·검증부터 진행하고 `eva local prepare`를 다시 실행합니다.

선택한 component에 따라 precondition → Infra → GPU preflight → Config → IAM → Agent → Vision → App 순으로
실행합니다. GPU preflight는 Agent/Vision을 선택한 대상에 자동 포함됩니다.
Agent/Vision/App을 선택하면 Config가 자동 포함되며, n8n을 선택하면 마지막에 설치합니다.
Workspace와 component별 Values는 내부에서 생성합니다.
IAM/App 재설치 시 workload 재시작에 따른 일시 중단이 발생할 수 있습니다.

## 8. [Local 설치 서버] 결과 확인과 인계

설치 성공 시와 `eva check`의 `Access URLs`에서 설치한 App/IAM과 Local Harbor 접속 주소를 확인합니다.
n8n을 설치했다면 n8n 주소도 표시됩니다. 각 주소에는 설정한 HTTP/HTTPS, 공개 포트와 경로가 반영됩니다.

```bash
sudo eva status
sudo eva check --verbose
sudo eva inspect operations
sudo eva inspect logs --tail 200
```

완료 조건은 명령 종료뿐 아니라 다음 서비스 검증까지 포함합니다.

- 설정한 App 주소로 접속하고 새 브라우저 세션에서 로그인·로그아웃 확인
- IAM/Redis 연결과 발급된 라이선스의 활성 상태 확인
- 선택한 Agent/Vision의 GPU 할당과 실제 카메라·분석 요청 확인
- Vision 모델 다운로드·준비 완료와 모델 PVC 재사용 확인
- Qdrant snapshot 사용 시 대상 collection·복원 데이터 확인
- 데이터 저장·조회, 영구 저장소와 재기동 후 보존 확인

설치 Site·Release·Operation ID, 서비스 주소와 검증 결과를 운영 담당자에게 인계합니다.
`inspect values`와 `inspect config`는 **Secret을 마스킹하지 않고 출력**하므로 일반 로그 수집이나
채팅 공유에 사용하지 않습니다. 상세 조회는 [EVA Tool 명령어 가이드](../operations/eva-cli-command-guide.md)를 따릅니다.

## 9. [Local 설치 서버] 실패 처리와 변경 설치

| 상황 | 다음 조치 |
| --- | --- |
| `local prepare` 실패 | 터미널의 실패 단계와 원인을 확인하고 같은 준비 명령 재실행; 기존 캐시 checksum 오류는 준비 담당자가 확인 |
| cache·Chart·모델 누락 또는 OS 패키지 불일치 | 준비 담당자가 대상 환경에 맞는 Bundle을 다시 준비·검증 |
| image pull 실패 | Harbor 주소·인증·적재 태그와 node/Pod 접근 확인 |
| Vision 모델 준비 실패 | 캐시 무결성·모델 선택, Harbor 접근·게시 권한과 Vision 단계 로그 확인 |
| Qdrant artifact 조회 실패 | `local prepare`의 snapshot 게시 결과와 tag·인증 확인 |
| IAM Secret 조회 실패 | IAM 단계 로그와 기동·관리자 인증·realm/client 구성 확인 |
| 인증서·로그인·Redis 오류 | 실제 공개 URL·SAN·redirect URI·Redis endpoint/TLS·방화벽 정합성 확인 |
| 원본 설정 또는 Release 변경 | 설정 변경은 새 `install --config` 실행; Release 변경은 Bundle 검증·`local prepare`부터 진행 |

설치 실패는 해당 Operation의 상태·로그에서 확인합니다. `local prepare`에는 설치 Operation이
생성되지 않으므로 준비 실패는 터미널 출력으로 확인합니다.
설치 입력을 바꾸지 않고 외부 원인만 해결했다면 해당 Operation을 재시도합니다.

```bash
operation_id="<operation-id>"
sudo eva status "$operation_id"
sudo eva inspect logs --operation "$operation_id" --tail 200
sudo eva retry --yes "$operation_id"
```

`retry`는 원래 설정 snapshot과 Release로 설치 단계를 다시 실행합니다.
설정 수정이나 새 Release 적용은 `eva install --config`로 새 설치 작업을 시작합니다.

**일반 재설치·upgrade에서 DB, PVC, hostPath나 라이선스 데이터를 삭제하지 않습니다.**
데이터 초기화·DB 변경은 설치 재시도와 별개이며, 백업·복구 계획과 명시 승인이 필요합니다.
관련 작업은 [App DB 이전과 사용자 백업](../operations/app-database-migration.md),
[Helm 사용자 Values 백업](../operations/helm-user-values-backup.md)을 따릅니다.
