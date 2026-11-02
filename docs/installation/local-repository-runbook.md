# EVA Local Repository 설치 가이드

`eva airgap prepare`와 `eva local prepare`가 포함된 Tool·Release를 사용합니다.

## 1. Airgap Bundle 준비 — 인터넷 준비 서버에서 먼저 실행

폐쇄망 반입 전에 **인터넷에 연결된 별도 준비 서버**에서 `eva airgap prepare`로 Bundle을 만듭니다.
Base Release 압축을 풀고 포함된 installer로 EVA Tool을 설치한 뒤, 해당 Release로 준비합니다.
대상 서버에서 자산을 다운로드하거나 `eva remote prepare`를 대신 실행하지 않습니다.

### 준비 서버의 필수 조건과 승인

| 항목 | 진행 조건 |
| --- | --- |
| OS·권한 | Ubuntu 24.04, `linux/amd64`, **root 실행(`sudo`)** |
| Release | 준비할 **Base Release ZIP**; 압축 해제 후 포함된 installer로 Tool 설치·Release 검증 |
| Docker | 동작하는 `linux/amd64` Docker daemon; 이미지 pull·save 가능 |
| 외부 접근 | Release가 참조하는 패키지·Chart·Registry·모델·snapshot 다운로드 경로와 AWS STS 접근 |
| APT | Ubuntu·Docker·NVIDIA 저장소와 최신 인덱스 구성; 필요한 패키지 candidate 및 대상 kernel 호환성 확인 |
| 공간 | 출력 파일 외에도 다운로드 cache, Runtime, staging 복사본과 검증용 압축 해제를 수용할 여유 공간 |
| 승인 | 다운로드, Docker image pull/export, 누락 Runtime의 온라인 bootstrap에 따른 준비 서버 APT/pip 변경 승인 |

필수 명령은 `bash`, `curl`, `docker`, `aws`(AWS CLI), `sudo`, `apt-get`, `apt-cache`, `apt`,
`dpkg`, `dpkg-scanpackages`(`dpkg-dev`), `gpg`, `tar`, `gzip`, `unzip`, `sha256sum`,
`find`, `awk`, `sed`, `sort`, `python3`입니다. **Docker와 AWS CLI는 이 명령이 설치하지 않습니다.**
준비 서버와 대상 서버의 Ubuntu·패키지 의존성·kernel을 맞춥니다. 출력 파일시스템과 임시
검증 공간(`/tmp` 또는 `TMPDIR`)을 모두 확인하며, 고정된 용량·소요 시간을 전제로 계획하지 않습니다.

AWS credential은 준비 서버의 `/var/lib/eva/credentials/aws_key.ini`를 재사용하고 STS로 검증합니다.
사용 가능한 credential이 없으면 대화형 터미널에서 key를 **숨김 입력**받습니다. 비대화형 실행은
승인된 Secret 관리 절차로 해당 파일을 먼저 준비합니다. `--yes`는 credential 입력을 대신하지
않으며, Secret을 명령행·환경 설정 예시·로그에 직접 적지 않습니다.

### 준비 서버의 Tool 설치와 Release 검증

준비할 `eva-base-release-<version>.zip` 하나를 사용자 홈에 둡니다.
기존 `eva-base-release` 폴더가 있다면 별도 보관하고, 다른 Release 파일을 섞지 않습니다.

```bash
cd "$HOME"
unzip -q eva-base-release-*.zip -d eva-base-release
cd eva-base-release
sudo bash ./eva-tool-installer.sh
sudo eva verify
```

installer는 Release를 검증하고 **Current Release**로 등록합니다. 이후 명령은 해당 Release를
자동으로 사용하므로 `--release`를 지정할 필요가 없습니다. 준비가 끝날 때까지 압축을 푼
폴더를 보존합니다. prepared Release, 반입(import)한 Release, Bundle archive는 준비 입력으로
사용할 수 없습니다. 원본의 모든 선언 artifact가 유효해야 하며 원본은 수정하지 않습니다.

### Bundle 생성과 검증

**GPU profile만 선택하면 됩니다.** 준비 서버가 아니라 **설치할 대상 서버의 GPU 구성**을
기준으로 아래 표에서 선택합니다. `PRO6000-MIGx4`는 GPU 1장을 MIG 4개로 나눈 구성입니다.

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

선택한 profile의 vLLM 이미지만 포함하므로 설치 시 대상의 GPU·MIG 구성도 표와 같아야 합니다.
다른 profile의 대상에도 설치하려면 Bundle을 따로 준비합니다.

드라이버 옵션을 생략하면 모든 profile에서 **`nvidia-driver-580`과 의존 패키지**를 준비합니다.
GPU profile과 드라이버 패키지의 1:1 매핑은 없습니다. 대상에서 `nvidia-smi`가 정상 동작하면
설치 시 기존 드라이버를 유지하며, 드라이버가 없으면 기본 설치 패키지는 `nvidia-driver-580`입니다.
다른 드라이버가 필요한 대상은 신규 설치 전에 호환 드라이버를 준비합니다.
`--driver-package`는 **Bundle에 담을 패키지**만 변경하며 대상의 설치 패키지 설정까지 바꾸지는 않습니다.

추가 옵션은 필요한 경우에만 지정합니다. 전체 옵션은 `eva airgap prepare --help`로 확인합니다.

| 옵션 | 생략 시 | 지정할 때 |
| --- | --- | --- |
| `--driver-package PACKAGE` | `nvidia-driver-580` | 다른 드라이버 패키지를 반입해야 할 때. 예: `nvidia-driver-580-open` |
| `--output FILE.tar.gz` | 사용자 홈의 기본 파일명 | 저장 위치를 바꾸거나 같은 Release의 다른 profile용 Bundle을 만들 때 |
| `--display-mode-selector FILE.zip` | 포함 안 함 | 대상 GPU의 Display Mode 전환에 필요한 NVIDIA 배포 ZIP을 반입할 때 |
| `--release PATH` | installer가 등록한 Current Release | 다른 Base Release로 준비할 때 |

Bundle에는 해당 Release의 설치 패키지·차트·이미지·모델·Qdrant snapshot과 Runtime,
Harbor 설치 파일이 포함됩니다. 필요한 Runtime이 준비 서버에 없으면 온라인으로 설치합니다.
대상 서버의 Harbor 설치·이미지 게시·GPU 설정은 반입 후 진행합니다.

실행 시 선택한 profile·드라이버 패키지와 저장 경로를 표시합니다. Bundle은 `sudo`를 실행한
원래 사용자 홈에 `eva-airgap-bundle_<version>_<os>_<arch>.tar.gz`로 저장하며 해당 사용자가
복사할 수 있게 소유권을 맞춥니다. 기존 파일은 덮어쓰지 않습니다.
중단·실패하면 원인을 해결한 뒤 처음부터 다시 실행하고, 검증된 최종 Bundle만 반입합니다.

`prepare`는 Bundle을 검증한 뒤 최종 파일을 저장하고 위치·파일명·반입용 SHA-256을 표시합니다.
사용자가 `eva`, Release가 `3.3.0`인 경우 출력 예시는 다음과 같습니다.

```text
[OK] Airgap Bundle verified: /home/eva/eva-airgap-bundle_3.3.0_linux_amd64.tar.gz
[OK] Bundle SHA-256: <생성된-Bundle의-SHA256>
[OK] Bundle directory: /home/eva
[OK] Bundle filename: eva-airgap-bundle_3.3.0_linux_amd64.tar.gz
```

출력된 SHA-256을 기록해 대상 서버의 반입 검증에 사용합니다.
Release 버전에 `v`가 있으면 파일명에도 포함됩니다. 실패한 명령 이후에는 다음 단계로 진행하지 않습니다.

Bundle은 다음처럼 **Release root가 평탄한 구조**입니다. installer와 Tool archive가 같은
디렉터리에 있어야 하며, 다른 선언 artifact도 함께 보존합니다.

```text
release.yaml
checksums.sha256
README.md
eva-tool-installer.sh
eva-tool_<version>_linux_amd64.tar.gz
eva-infra_…tar.gz
eva-solution_…tar.gz
eva-offline_<version>_linux_amd64.tar.gz
```

`eva-offline` 내부에는 `runtime/`과 `out/cache/`가 있습니다. Bundle용 metadata와 checksum은
새로 생성하며 원본 Release는 그대로 둡니다. Bundle과 함께 Release/version, GPU profile,
driver package, 대상 OS/kernel 조건 및 SHA-256을 인계합니다. **기대 checksum은 별도의 신뢰할 수
있는 경로로 전달**합니다. AWS·Docker·SSH credential, 고객 설치 YAML·TLS private key는
Bundle에 넣지 않습니다. 준비 서버의 인터넷 credential은 대상에 반입하지 않으며,
대상용 설치 설정·TLS 키는 별도 승인된 Secret 관리 경로로 전달합니다.

## 2. 준비 범위와 담당 구분

인터넷과 Main Harbor에 접근할 수 없는 폐쇄망에 EVA를 설치하는 절차입니다.
검증된 Release와 오프라인 자산을 반입하고, 내부 Harbor를 공급원으로 사용합니다.
대상 서버에서 Tool 설치 → 설정 작성 → `eva local prepare` → `eva install` 순서로 진행합니다.

Main 서버의 Harbor와 게시 경로에 접근할 수 있다면
[Remote Repository 설치 가이드](remote-repository-runbook.md)를 사용합니다.
Local은 Repository 공급 방식이며, 서비스의 HTTP/HTTPS나 IAM 함께 설치 여부와는 별개입니다.

| 담당 | 설치 전 완료할 작업 |
| --- | --- |
| 자산 준비 담당자 | 대상 OS·플랫폼·GPU와 설치할 component에 맞는 Release, Runtime, 패키지, Chart, 이미지, 모델 및 snapshot 준비·검증·반입 |
| 내부 Repository 담당자 | 사용할 Harbor 주소·project·게시 권한 인계; 별도 서버의 Harbor를 사용한다면 해당 Harbor 준비 |
| 설치 담당자 | 반입·Tool 설치, 설정·인증서·라이선스 준비, Local 준비·설치 명령 실행과 결과 확인 |

`eva local prepare`가 Bundle의 캐시·Runtime 배치, 필요한 Docker·Harbor 설치와 이미지·snapshot 게시를
처리합니다. 기존 Harbor가 있으면 설정과 데이터를 유지하며, 인터넷에서 자산을 추가로 받지 않습니다.
Local 준비가 성공한 뒤 `eva install`을 실행합니다.

## 3. 설치 전 확인

| 항목 | 진행 조건 |
| --- | --- |
| 서버 | Ubuntu 24.04, `linux/amd64`, 설치 계정과 sudo 권한, 충분한 디스크·메모리 |
| 작업 승인 | Docker·k3s·GPU 실행 환경·영구 저장소 변경과 서비스 재시작이 가능한 시간 확보, 기존 데이터 백업 |
| Release | Tool·Infra·Solution·오프라인 자산의 버전과 checksum 일치, 설치할 모든 component의 자산 포함 |
| OS 패키지 | 대상 Ubuntu·Python 환경에 맞는 의존 패키지와 필요한 GPU driver·Container Toolkit 자산 포함 |
| Harbor | 실제 Harbor IP/DNS:port, 사용할 project와 게시 계정; 별도 서버를 사용한다면 Harbor 준비 완료 |
| GPU | Agent/Vision 설치 시 지원 GPU·driver·`nvidia-smi`, 의도한 MIG 구성과 해당 모델·vLLM 이미지 준비 |
| 서비스 | App/IAM 공개 주소, 내부 DNS·라우팅·방화벽, 인증서, IAM·Redis 연결값과 발급된 라이선스 준비 |
| 데이터 | 영구 저장소 경로·용량·권한과 보존 정책 확인 |

### Harbor 주소 입력

설치 설정의 `repository.registry`에는 **대상 서버에서 접근 가능한 실제 Harbor IP/DNS:port**를
입력합니다. 예: `192.0.2.20:32080`. `http://`·`https://` 같은 URL scheme은 넣지 않습니다.

### 설치 구성·데이터 확인

차트·이미지·모델·snapshot은 앞의 `eva airgap prepare`에서 Bundle에 포함됩니다.
반입 후 캐시 배치와 Harbor 적재는 6절의 `eva local prepare`로 진행합니다.

- Agent의 Vision 기능을 사용하려면 설치 구성에서 Vision도 선택합니다.
- Harbor project와 인증 정보는 준비 담당자가 인계한 값을 사용합니다.
  Vision 설치 시 모델을 자동 게시하므로 해당 project의 게시 권한도 필요합니다.
- Local 설정에는 `credentials.aws`를 넣지 않으며, 대상 서버에 AWS credential을 반입하지 않습니다.
- 재시도·업그레이드 시 기존 모델 캐시와 Qdrant 데이터의 **PV/PVC를 삭제하지 않습니다.**

## 4. 대상 서버 반입·Tool 설치

준비한 Bundle을 대상 서버의 사용자 홈에 복사합니다. `<version>`은 준비 완료 시 출력된
파일명의 버전으로 바꿉니다. 처음에는 EVA Tool이 없으므로 반입 파일 확인과 압축 해제만 OS 명령으로 진행합니다.

```bash
cd "$HOME"
sha256sum eva-airgap-bundle_<version>_linux_amd64.tar.gz
```

출력값이 준비 서버에서 기록한 SHA-256과 일치하면 Tool을 설치합니다.

```bash
(
  set -e
  mkdir eva-base-release
  tar -xzf eva-airgap-bundle_<version>_linux_amd64.tar.gz \
    -C eva-base-release --no-same-owner --keep-old-files
  cd eva-base-release
  sudo bash ./eva-tool-installer.sh
  sudo eva verify
  eva local prepare --help
)
```

기존 `eva-base-release` 폴더가 있다면 별도 보관한 뒤 새 폴더에 풉니다.
installer가 이 폴더를 Current Release로 등록하므로 설치 후에도 보존합니다.
이후 5절에서 설정을 작성하고, 6절의 `eva local prepare`로 캐시·Runtime·Harbor를 준비합니다.
캐시 경로나 helper를 직접 지정할 필요는 없습니다.

## 5. 단일 설치 설정 작성

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

| 설정 | 입력할 내용 |
| --- | --- |
| `site.id` | 고객사·설치 환경의 고유 Site ID |
| `repository` | `mode: local`, 실제 내부 Harbor의 `registry`, 승인된 `project` |
| `targets.server-a.ansible` | 대상 사용자, local 접속·sudo 정보 |
| `targets.server-a.values.iam` | 함께 설치할 IAM의 주소·관리자 설정·TLS·Redis |
| `targets.server-a.values.app` | App 공개 주소·TLS·라이선스, 외부 IAM 사용 시 SSO·Redis 연결값 |
| `values.agent` / `values.vision` | 승인된 Chart override가 있을 때만 추가 |

`<...>` 필수 입력은 모두 실제 값으로 바꿉니다. 대상의 인증서·데이터 경로는 절대 경로로 씁니다.
Values는 생성된 `values.<component>` 아래에 그대로 작성하며 `localhost`나 IP key로 다시
감싸지 않습니다. Release 기본 Values나 GPU profile 전체를 복사해 고정하지 않습니다.

### 기본으로 처리되는 설정

기본 Local 설치에서는 아래 항목을 설치 YAML에 추가할 필요가 없습니다.

| 항목 | 기본 동작 |
| --- | --- |
| Harbor 계정 | 새 Harbor는 Deployer 기본 계정으로 준비하며, 기존 Harbor는 저장된 인증을 재사용합니다. `eva local prepare`가 k3s 인증도 구성합니다. |
| Qdrant snapshot | Local 모드에서는 `harbor` 방식과 `values-k3s.harbor.yaml`을 자동 선택합니다. |
| Agent vLLM profile | 설치 시 실제 GPU·MIG 구성으로 자동 결정합니다. 대상 구성이 Bundle 준비 시 선택한 profile과 같아야 합니다. |

기존 Harbor의 인증을 사용할 수 없을 때만 `eva local prepare`가 계정을 숨김 입력으로 받습니다.
자동화 실행에서 별도 계정을 사용하려면 `targets.server-a.ansible`의
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

## 6. Local 준비와 사전 조건 확인

```bash
sudo eva workspace validate --config "$HOME/eva-install.yaml"
```

설정 검증이 성공하면 같은 설정 파일로 Local 설치 자산과 Harbor를 준비합니다.

```bash
sudo eva local prepare --config "$HOME/eva-install.yaml" --yes
```

이 명령은 Current Release의 캐시·Runtime을 자동 배치하고 필요한 Docker·Harbor를 준비한 뒤,
Bundle의 이미지를 적재·게시합니다. Agent를 선택하면 Qdrant snapshot도 게시합니다.
기존 Harbor의 인증을 재사용하며, 게시 계정이 필요하면 터미널에서 숨김 입력으로 받습니다.
다른 서버의 Harbor를 사용한다면 해당 Harbor가 먼저 실행 중이어야 합니다.

`[OK] Local preparation completed`를 확인한 뒤 설치를 진행합니다.
재실행 시 같은 Bundle의 캐시는 checksum 검증 후 재사용합니다. 다른 Bundle의 캐시나 손상된
캐시는 덮어쓰지 않으므로 오류 내용을 확인하고 조치합니다.
외부 IAM·Redis 접근, GPU, 인증서의 실제 유효성은 대상 환경에서 확인합니다.

기존 드라이버가 준비된 GPU 서버에서는 다음으로 GPU/MIG 구성을 확인합니다.
신규 드라이버 설치가 필요하면 `eva install`의 Infra 단계에서 진행하고 재부팅 안내를 따릅니다.

```bash
sudo eva preflight gpu
```

기존 Argo CD가 관리하던 서버라면 연결 해제를 먼저 확인합니다. Git 등록 변경에는 별도 승인이
필요하며 `--yes`로 우회할 수 없습니다. 필요한 관리 서버 접근이 없으면 먼저 운영 담당자와 조치합니다.

```bash
sudo eva preflight argocd --config "$HOME/eva-install.yaml"
```

## 7. 설치 실행

준비가 완료된 Current Release로 설치합니다.

```bash
sudo eva install --config "$HOME/eva-install.yaml" --yes
```

Managed Runtime이 없으면 원본 Release의 `eva-offline`에서 구성합니다.
인터넷용 `eva runtime bootstrap`이나 별도 pip/Ansible 설치를 정상 절차에 추가하지 않습니다.
검증된 다른 Release·Bundle을 쓸 때만 `--release <절대 경로>`를 명시합니다.
Release를 바꿨다면 해당 Bundle을 선택하고 **`eva local prepare`를 먼저 실행**합니다.

선택한 component에 따라 precondition → Infra → Config → IAM → Agent → Vision → App 순으로
실행합니다. Agent/Vision/App을 선택하면 Config가 자동 포함되며, Infra-only는 Solution을
배포하지 않습니다. Workspace와 component별 Values는 내부에서 생성하므로 별도 작성하지 않습니다.
IAM/App 재설치 시 workload 재시작에 따른 일시 중단이 발생할 수 있습니다.

## 8. 결과 확인과 인계

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

## 9. 실패 처리와 변경 설치

| 상황 | 다음 조치 |
| --- | --- |
| cache·Chart·모델 누락 또는 OS 패키지 불일치 | 준비 담당자가 같은 Release 기준의 자산·배치를 보완; 인터넷 fallback이나 버전 혼합 금지 |
| image pull 실패 | Harbor endpoint·인증·적재 태그·mirror 경로와 node/Pod 접근 확인 |
| Vision 모델 준비 실패 | 캐시 무결성·모델 선택, Harbor 접근·게시 권한·기존 EVA Infra 인증 설정 확인; 실패를 우회해 workload를 중단하지 않음 |
| Qdrant artifact 조회 실패 | 일반 이미지와 별도로 snapshot 게시 여부·profile·tag·인증 확인 |
| IAM Secret 조회 실패 | IAM 단계 로그와 기동·관리자 인증·realm/client 구성 확인; 빈 handoff 수동 작성 금지 |
| 인증서·로그인·Redis 오류 | 실제 공개 URL·SAN·redirect URI·Redis endpoint/TLS·방화벽 정합성 확인 |
| 원본 설정 또는 Release 변경 | 자산을 맞춘 뒤 `install --config`로 새 Operation 시작 |

원인은 해당 Operation의 상태·로그에서 확인합니다. 다른 작업과 혼동하지 않도록 ID를 지정합니다.
입력을 바꾸지 않고 외부 원인만 해결했다면 재시도합니다.

```bash
operation_id="<operation-id>"
sudo eva status "$operation_id"
sudo eva inspect logs --operation "$operation_id" --tail 200
sudo eva retry --yes "$operation_id"
```

`retry`는 원래 Workspace snapshot과 Release를 재사용합니다. 수정한 원본 설정이나 새 Current
Release를 다시 읽지 않으며, 실패 task부터 이어가는 resume·전체 rollback 기능도 아닙니다.
설정 수정이나 수정 Release 적용은 새 설치 작업으로 진행합니다.

**일반 재설치·upgrade에서 DB, PVC, hostPath나 라이선스 데이터를 삭제하지 않습니다.**
데이터 초기화·DB 변경은 설치 재시도와 별개이며, 백업·복구 계획과 명시 승인이 필요합니다.
관련 작업은 [App DB 이전과 사용자 백업](../operations/app-database-migration.md),
[Helm 사용자 Values 백업](../operations/helm-user-values-backup.md)을 따릅니다.
