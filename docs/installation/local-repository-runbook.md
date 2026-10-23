# EVA Local Repository 설치 가이드

## 1. Airgap Bundle 준비 — 인터넷 준비 서버에서 먼저 실행

폐쇄망 반입 전에 **인터넷에 연결된 별도 준비 서버**에서 `eva airgap prepare`로 Bundle을 만듭니다.
이 명령이 포함된 Tool을 설치하고 `eva airgap prepare --help`로 지원 여부를 확인합니다.
대상 서버에서 자산을 다운로드하거나 `eva remote prepare`를 대신 실행하지 않습니다.

### 준비 서버의 필수 조건과 승인

| 항목 | 진행 조건 |
| --- | --- |
| OS·권한 | Ubuntu 24.04, `linux/amd64`, **root 실행(`sudo`)** |
| Release | checksum 검증을 통과한 완전한 **원본 Release 디렉터리**; metadata에 installer artifact 포함 |
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

### Bundle 생성과 검증

다음 경로·GPU profile은 실제 반입 계획에 맞게 바꿉니다. `--release`를 생략하면 준비 서버의
**Current Release**를 사용합니다. prepared Release, 반입(import)한 Release, Bundle archive는
입력으로 사용할 수 없습니다. 원본의 모든 선언 artifact가 유효해야 하며 원본은 수정하지 않습니다.

```bash
original_release="/data/eva/base-releases/<version>"
bundle="/data/eva/export/eva-airgap-bundle_<version>_linux_amd64.tar.gz"

sudo eva verify --release "$original_release"
sudo eva airgap prepare --release "$original_release" \
  --output "$bundle" --vllm-profile L40sx1 \
  --driver-package nvidia-driver-580 --yes
```

- `--output FILE.tar.gz`, `--vllm-profile`, `--yes`는 필수입니다. 기존 출력은 덮어쓰지 않습니다.
- profile은 `A6000x1`, `L40sx1`, `PRO5000x3`, `PRO6000-MIGx4` 중 하나입니다.
  **해당 profile의 vLLM 이미지만** 포함하며 Values는 모든 profile용을 받습니다. 대상의 실제
  GPU/MIG와 설치 시 선택되는 profile이 같아야 합니다. 다른 profile이면 새 Bundle을 준비합니다.
- `--driver-package` 기본값은 `nvidia-driver-580`이며 `nvidia-driver-NNN` 또는
  `nvidia-driver-NNN-open` 형식을 지원합니다. 전체 의존성 repo를 준비하며 누락은 오류로 처리합니다.
  패키지 이름을 지정하는 것만으로 대상 kernel/GPU 호환성이 보장되지는 않습니다.
- 특정 GPU의 Display Mode 전환에 필요한 경우에만 승인된 NVIDIA 독점 배포 ZIP을
  `--display-mode-selector /secure/approved.zip`으로 추가합니다. 생략 가능한 옵션이며,
  포함 여부와 관계없이 모든 하드웨어의 Display Mode·GPU·MIG 준비 완료를 보장하지 않습니다.

명령은 선택한 원본 Release의 Infra/Solution version catalog를 기준으로 **Infra·App·IAM·Agent·Vision의
패키지·Chart·helper, n8n 이미지, 선택한 vLLM 이미지, Agent 모델, Vision 모델 캐시, Harbor용 Qdrant snapshot,
Harbor offline installer, NVIDIA driver 의존성 repo**를 준비합니다. 유효한 Managed Runtime은
재사용하고 없으면 온라인으로 bootstrap합니다. 손상된 기존 Runtime은 먼저 복구해야 합니다.

**Harbor 배포·이미지/Qdrant snapshot 게시, GPU driver 설치·Display Mode/MIG 전환, Docker daemon 설정
변경은 수행하지 않습니다.** 출력 옆의 격리된 임시 작업 디렉터리는 성공·실패 시 정리하며
resume을 지원하지 않습니다. 중단 후에는 원인을 해결하고 처음부터 다시 실행합니다.
강제 종료·서버 장애 시에는 private 임시 디렉터리가 남을 수 있으므로 담당자가 실행 중인 작업이
없는지와 내용을 확인한 뒤 정리합니다. 남은 staging 파일을 완성된 Bundle로 사용하지 않습니다.
다운로드 backend 출력은 숨기고 단계명과 최종 결과를 표시합니다. 누락 Runtime을 bootstrap할 때는
기존 Runtime 설치기의 APT/pip 진행 출력이 추가될 수 있습니다. credential을 기록하려고
shell trace나 backend 디버그 출력을 켜지 않습니다. 검증이 끝난 Bundle만 최종 출력 경로에 나타납니다.

완료 후 archive와 반입용 SHA-256을 확인합니다. 실패한 명령 이후에는 다음 단계로 진행하지 않습니다.

```bash
sudo eva verify --release "$bundle"
sudo sha256sum "$bundle"
```

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
이 가이드는 **대상 서버에서 EVA Tool과 단일 설치 설정으로 설치**하는 흐름을 따릅니다.
수동 Ansible·Helm 배포를 같은 작업에 섞지 않습니다.

Main 서버의 Harbor와 게시 경로에 접근할 수 있다면
[Remote Repository 설치 가이드](remote-repository-runbook.md)를 사용합니다.
Local은 Repository 공급 방식이며, 서비스의 HTTP/HTTPS나 IAM 함께 설치 여부와는 별개입니다.

| 담당 | 설치 전 완료할 작업 |
| --- | --- |
| 자산 준비 담당자 | 대상 OS·플랫폼·GPU와 설치할 component에 맞는 Release, Runtime, 패키지, Chart, 이미지, 모델 및 snapshot 준비·검증·반입 |
| 내부 Repository 담당자 | private Harbor 설치, 이미지 적재, 필요한 Qdrant snapshot OCI artifact 게시, endpoint·기존 Harbor 인증 설정 인계 |
| 설치 담당자 | 대상 서버 사전 확인, 설정·인증서·라이선스 준비, CLI 설치 실행과 결과 검증 |

**현재 `eva install`은 Local Harbor 생성, 제품 이미지 load/push, Qdrant snapshot artifact 게시,
전체 오프라인 캐시 배치를 대신하지 않습니다.** 이 준비가 끝나지 않았다면 설치를 시작하지 않습니다.
Docker가 필요한 Harbor·이미지 적재 작업과 EVA Managed Runtime 준비는 별개입니다.

앞 절의 Bundle 생성과 아래의 대상 캐시 배치·Harbor 인계를 모두 완료해야 합니다.
Base Release만 받은 상태나 Bundle 검증만 통과한 상태를 폐쇄망 설치 준비 완료로 판단하지 않습니다.

## 3. 설치 전 확인

| 항목 | 진행 조건 |
| --- | --- |
| 서버 | Ubuntu 24.04, `linux/amd64`, 설치 계정과 sudo 권한, 충분한 디스크·메모리 |
| 작업 승인 | Docker·k3s·GPU 실행 환경·영구 저장소 변경과 서비스 재시작이 가능한 시간 확보, 기존 데이터 백업 |
| Release | Tool·Infra·Solution·오프라인 자산의 버전과 checksum 일치, 설치할 모든 component의 자산 포함 |
| OS 패키지 | 대상 Ubuntu·Python 환경에 맞는 의존 패키지와 필요한 GPU driver·Container Toolkit 자산 포함 |
| Harbor | node와 Pod에서 모두 접근 가능한 DNS/IP:port, 승인된 project와 인증 정보, 필요한 이미지 적재 완료 |
| GPU | Agent/Vision 설치 시 지원 GPU·driver·`nvidia-smi`, 의도한 MIG 구성과 해당 모델·vLLM 이미지 준비 |
| 서비스 | App/IAM 공개 주소, 내부 DNS·라우팅·방화벽, 인증서, IAM·Redis 연결값과 발급된 라이선스 준비 |
| 데이터 | 영구 저장소 경로·용량·권한과 보존 정책 확인 |

- Harbor 주소는 `192.0.2.20:32080`처럼 **scheme 없는 `host:port`**로 지정합니다.
  `localhost`나 loopback 주소는 Pod에서 다른 대상을 가리키므로 사용하지 않습니다.
- Harbor에는 제품 이미지뿐 아니라 선택한 Infra와 Chart가 참조하는 보조 이미지도 있어야 합니다.
  원본 Docker Hub 경로를 사용하는 이미지의 mirror와 적재 경로도 준비 담당자가 확인합니다.
- Agent를 선택하면 Agent Init·vLLM·Qdrant 자산도 필요합니다. 모델 캐시와 이미지의 GPU profile은
  대상 서버의 실제 GPU/MIG 구성과 일치해야 합니다. IAM/App만 설치할 때는 이 자산이 필요하지 않습니다.
- Harbor snapshot 방식을 사용한다면 snapshot은 **일반 이미지와 별도로 게시한 OCI artifact**여야 합니다.
  준비 시 선택한 Qdrant Values의 artifact tag·파일·collection과 설치 설정을 맞춥니다.
  `local_pv` 방식과 혼합하지 않습니다.
- Local 설정에는 `credentials.aws`를 넣지 않습니다. 대상 서버에 AWS credential을 복사해
  외부 Registry나 S3에서 빠진 자산을 받는 방식으로 진행하지 않습니다.

### Vision 모델 공급

Vision weights는 컨테이너 이미지와 별도로 공급합니다. 인터넷 준비 서버의 `eva airgap prepare`가
S3에서 모델을 다운로드·검증해 Bundle에 자동 포함합니다. 준비 서버에는 모델 S3의 list/read와
Vision 이미지 ECR pull 권한이 필요하며, 대상 Harbor의 모델 게시 권한도 확인합니다.
Local 설치는 반입한 모델을 설정된 내부 Harbor에 자동 게시하고, Pod가 이를 영구 캐시에 받아
서비스를 준비합니다. 인증은 기존 EVA Infra의 Harbor 설정을 재사용하며 수동 Secret 생성이나
추가 설치 명령은 필요하지 않습니다.
**설치 시 모델 `latest` 갱신은 기존 배포에도 영향을 줄 수 있으므로 변경 승인을 확인합니다.**
첫 실행은 오래 걸릴 수 있으므로 다운로드·압축 해제와 영구 캐시를 위한 디스크 공간을 확보합니다.
**재시도·upgrade 시 모델 PVC를 삭제하지 않습니다.**
기본 모델을 사용하며, 승인된 custom 모델이 필요할 때만 준비와 설치의 모델 선택·설정을 일치시킵니다.

## 4. 대상 서버 반입·Tool 설치·캐시 배치

### 최초 반입: eva 없이 archive부터 검증

대상 서버에는 아직 `eva`가 없으므로 OS 명령으로 먼저 검증합니다. 준비 담당자가 **별도 경로로
전달한 기대 SHA-256**과 비교한 뒤, 새 영구 Release 디렉터리에 압축을 풉니다. 다음 변수의
`<...>`를 실제 값으로 바꾸고 Bash에서 실행합니다. 블록은 오류가 나면 중단합니다.

```bash
bundle="/data/eva/inbox/eva-airgap-bundle_<version>_linux_amd64.tar.gz"
expected_sha256="<별도-신뢰-경로로-받은-SHA256>"
release_root="/data/eva/releases/<version>"

(
  set -eu
  printf '%s  %s\n' "$expected_sha256" "$bundle" | sudo sha256sum --check --strict -
  sudo install -d -m 0755 "$(dirname "$release_root")"
  sudo mkdir -- "$release_root"
  sudo tar --extract --gzip --file "$bundle" --directory "$release_root" \
    --no-same-owner --keep-old-files
  cd "$release_root"
  sudo sha256sum --check --strict checksums.sha256
  sudo bash ./eva-tool-installer.sh
  eva version
  sudo eva verify --release "$release_root"
)
```

기존 디렉터리에는 덮어쓰지 않습니다. 중간 실패로 디렉터리가 남아도 내용을 확인하지 않고
명령을 반복하거나 기존 Release와 합치지 않습니다. Current Release가 참조하므로 설치 후에도
이 디렉터리를 보존하고, 임시 USB mount 경로만 등록한 채 매체를 제거하지 않습니다.
최초 Local 설치에서 Managed Runtime이 없으면 이 원본 형태의 Release에 **`eva-offline`이 필요**합니다.
installer는 Tool과 Release checksum을 검증하고 해당 Release를 Current Release로 등록합니다.

이미 Tool이 있는 서버에서는 `sudo eva verify --release "$bundle"`로 archive도 검증할 수 있습니다.
archive 검증이나 `release import-airgap`은 Harbor 적재·전체 설치 캐시 배치를 수행하지 않습니다.

### 실행 소스 준비 후 cache만 배치

먼저 반입 Release의 Infra·Solution 실행 소스를 준비합니다. 이 명령은 **offline cache를
추출하지 않습니다.**

```bash
sudo eva release prepare --release "$release_root"
```

`prepared_root`는 **위 명령이 출력한 경로**로 지정합니다. 기본값은 `/opt/eva/releases/<version>`이며
`v` 접두사를 포함해 출력과 정확히 맞춥니다. `offline_archive`도 검증한 Release의 metadata에
선언된 정확한 `eva-offline` 파일을 지정하며 wildcard나 다른 버전의 archive를 사용하지 않습니다.

새 cache 또는 비어 있는 cache인지 확인한 뒤 **`out/cache`만** 추출합니다. `runtime/`을 여기에
풀거나 source·metadata를 덮어쓰지 않습니다.

```bash
prepared_root="/opt/eva/releases/<version>"
offline_archive="$release_root/eva-offline_<version>_linux_amd64.tar.gz"

(
  set -eu
  sudo test -d "$prepared_root/src"
  sudo test -f "$offline_archive"
  sudo test ! -L "$prepared_root"
  sudo test ! -L "$prepared_root/out"
  sudo test ! -L "$prepared_root/out/cache"
  if sudo test -e "$prepared_root/out/cache"; then
    sudo test -d "$prepared_root/out/cache"
    cache_entry="$(sudo find "$prepared_root/out/cache" -mindepth 1 -maxdepth 1 -print -quit)"
    if [ -n "$cache_entry" ]; then
      printf '%s\n' 'cache가 비어 있지 않습니다. 기존 내용과 Release를 확인한 뒤 별도로 조치하세요.' >&2
      exit 1
    fi
  fi
  sudo install -d -m 0755 "$prepared_root/out/cache"
  sudo tar --extract --gzip --file "$offline_archive" --directory "$prepared_root" \
    --no-same-owner --keep-old-files out/cache
)
```

추출 도중 실패하면 부분 cache를 설치에 쓰지 않습니다. 준비 담당자가 원인과 기존 내용을
확인하고 승인된 새/빈 cache로 배치합니다. 캐시 경로를 임의 override하지 않습니다.

캐시 배치를 위해 prepare한 경로와 installer가 등록한 **원본 Current Release는 별개**입니다.
최초 설치에서는 Runtime을 읽을 수 있도록 원본 Release 선택을 유지합니다.
이후 설치 명령이 이미 준비한 실행 소스와 cache를 사용합니다.

### Harbor·이미지·snapshot 준비 — 별도 승인 작업

내부 Repository 담당자가 **Docker/Compose 준비 → Harbor 설치/접근 확인 → image load/push →
snapshot OCI artifact 게시**를 완료합니다. 이 작업은 `eva airgap prepare`나 `eva install`의
일부가 아닙니다. Harbor 설치·재구성은 서비스 중단이나 저장소 경로 변경을 일으킬 수 있으므로
기존 Harbor의 설정·데이터 보존 계획과 변경 승인을 먼저 확인합니다.
설정 helper가 새로 만드는 project는 **`public: false`**입니다.

검증한 Tool에 포함된 helper root는 기본 설치 시 `/opt/eva/tool/libexec/remote-root`입니다.
custom Tool root에서는 설치된 binary의 실제 경로로 찾습니다. 실행 소스에 `scripts/`가 있을
것으로 가정하거나 별도 Git checkout의 helper와 혼용하지 않습니다.

```bash
tool_binary="$(readlink -f "$(command -v eva)")"
tool_libexec="$(dirname "$(dirname "$tool_binary")")/libexec/remote-root"
```

| 설치된 helper (`$tool_libexec` 기준) | 사전 조건·입력 |
| --- | --- |
| `scripts/install/install_docker.sh --airgap` | Docker가 없을 때만 승인 후 사용; cache의 Docker DEB와 대상 OS 의존성 필요 |
| `scripts/install/setup_harbor.sh` | 동작하는 Docker/Compose, cache의 Harbor offline installer, Site ID, 영구 설치·데이터 경로, hostname·registry endpoint·project 및 승인된 인증 정보 |
| `scripts/publish/push_images_to_repository.sh` | Harbor project·권한·Docker 인증과 load된 이미지; `PULL_SOURCE_IMAGES=false`, `IMAGE_LIST`는 cache의 `images/airgap-images.txt` |
| `scripts/publish/push_qdrant_snapshots_to_harbor.sh` | 위 image push 후 실행; cache의 snapshot·ORAS·Harbor Values, 게시 권한 필요 |

helper에는 `EVA_CACHE_ROOT="$prepared_root/out/cache"`를 전달합니다. version catalog를 읽는 helper에는
`EVA_INFRA_VERSION_FILE="$prepared_root/src/infra/version.yaml"`,
`EVA_SOLUTION_VERSION_FILE="$prepared_root/src/solution/version.yaml"`도 전달해 같은 Release를 유지합니다.
이들은 **helper 입력**이며 `eva install`의 cache 우회 옵션이 아닙니다. Harbor 설정 helper에는
실제 `EVA_SITE_ID`, cache의 Harbor installer와 일치하는 `HARBOR_VERSION`, 영구 install/data 경로와
endpoint metadata 출력 경로를 명시합니다. 기본 HTTP `32080` helper를 쓴다면 승인된 내부망과
Docker client의 해당 Registry 접근 설정이 필요하며 daemon 설정을 자동 변경하는 절차로 취급하지 않습니다.

Docker가 준비된 Repository 서버에서 다음은 **load만** 수행합니다. Harbor가 별도 서버라면
검증된 cache/image archive를 그 서버에 인계하고 그 경로를 사용합니다.

```bash
sudo docker load --input "$prepared_root/out/cache/images/eva-images.tar"
```

push helper는 실제 `REPOSITORY_REGISTRY`(`host:port`)·`REPOSITORY_PROJECT`와 승인된 인증 경로를
사용합니다. 비밀번호를 명령행에 쓰거나 기본 비밀번호로 운영하지 않습니다. snapshot helper는
`EVA_AGENT_QDRANT_VALUES_FILE=values-k3s.harbor.yaml`을 사용하며, 해당 Values의 `SNAPSHOT_SPECS`
(tag·파일·collection)와 대상 설정을 일치시킵니다. 이미지 push만으로 snapshot이 게시되지는 않습니다.
helper 종료만 믿지 말고 필요한 태그·Docker Hub mirror 경로와 snapshot artifact의 실제 pull을 확인합니다.

설치 전 준비 담당자에게 다음을 확인받습니다.

- 사용할 Release와 실행 경로, 캐시 배치·무결성 검증 결과
- 선택한 component의 패키지·Chart·모델·helper 누락 여부
- Harbor image 적재 결과와, 해당 시 Qdrant snapshot artifact 게시 결과
- node와 Pod용 Harbor endpoint·인증·mirror 구성 및 필요한 방화벽 허용

`eva verify` 성공은 Release artifact 무결성 확인입니다. OS 의존성, 전체 cache 완전성,
Harbor 실제 pull이나 서비스 정상 동작은 별도로 확인합니다.

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
| `targets.server-a.ansible` | 대상 사용자, local 접속·sudo 정보, 필요한 Infra/Harbor 옵션 |
| `targets.server-a.values.iam` | 함께 설치할 IAM의 주소·관리자 설정·TLS·Redis |
| `targets.server-a.values.app` | App 공개 주소·TLS·라이선스, 외부 IAM 사용 시 SSO·Redis 연결값 |
| `values.agent` / `values.vision` | 승인된 Chart override가 있을 때만 추가 |

`<...>` 필수 입력은 모두 실제 값으로 바꿉니다. 대상의 인증서·데이터 경로는 절대 경로로 씁니다.
Values는 생성된 `values.<component>` 아래에 그대로 작성하며 `localhost`나 IP key로 다시
감싸지 않습니다. Release 기본 Values나 GPU profile 전체를 복사해 고정하지 않습니다.

Harbor 인증이나 snapshot 방식은 준비 담당자가 인계한 값에 맞춥니다. 다음 항목은 필요할 때
`targets.server-a.ansible`에 지정하는 **배포 옵션**이며 Chart Values가 아닙니다.

- `harbor_admin_user`, `harbor_admin_password`: 기존 Harbor 설정과 같은 인증으로 EVA Infra의
  node registry 인증을 구성합니다. 별도 `docker login`만으로 k3s·snapshot 인증이 구성되지는 않습니다.
- `k3s_dockerhub_mirror_enabled: true`: 원본 Docker Hub 경로의 이미지도 Harbor에서 공급할 때 사용;
  준비한 mirror 경로에 이미지가 있어야 합니다.
- Harbor snapshot 사용 시 `eva_agent_qdrant_snapshot_source: harbor`와
  `eva_agent_qdrant_values_file: values-k3s.harbor.yaml`을 함께 지정합니다.
- Agent 설치 시 선택되는 `eva_agent_vllm_profile`은 Bundle 생성 때의 `--vllm-profile`과
  실제 GPU/MIG 구성에 맞춥니다. 모든 profile의 Values가 있다는 이유로 다른 이미지를 선택하지 않습니다.

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

## 6. 입력과 사전 조건 확인

```bash
sudo eva workspace validate --config "$HOME/eva-install.yaml"
```

이 명령은 필수 입력·형식·설정 구조만 검증하며 Workspace를 생성하지 않습니다.
Harbor·외부 IAM·Redis 접근, credential 권한, GPU, 인증서의 실제 유효성은 별도로 확인합니다.

GPU를 사용할 서버에서는 실행합니다. driver가 없거나 GPU/MIG 구성이 의도와 다르면
승인된 오프라인 준비 절차를 완료한 뒤 계속합니다.

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
Release를 바꿨다면 **해당 버전의 cache와 Harbor 자산도 먼저 준비**해야 합니다.

선택한 component에 따라 precondition → Infra → Config → IAM → Agent → Vision → App 순으로
실행합니다. Agent/Vision/App을 선택하면 Config가 자동 포함되며, Infra-only는 Solution을
배포하지 않습니다. Workspace와 component별 Values는 내부에서 생성하므로 별도 작성하지 않습니다.
IAM/App 재설치 시 workload 재시작에 따른 일시 중단이 발생할 수 있습니다.

## 8. 결과 확인과 인계

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