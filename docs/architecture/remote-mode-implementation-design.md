# EVA Remote Mode Implementation Design and VS Code Agent Guide

## 0. 문서 목적

이 문서는 EVA Remote Repository mode의 설계 기준과 구현 지침을 함께 정의한다.

특히 다음 작업을 VS Code Agent가 안전하게 수행할 수 있도록 구현 범위, CLI 계약, Release packaging, 검증 순서, 금지 사항을 명시한다.

- Main 서버의 Remote 설치 자산 준비 자동화
- Main Harbor 게시 및 준비 결과 검증
- 검증된 Release의 Remote Target 전달
- Remote Target의 외부 의존 제거
- 설치자 인터페이스를 `eva` CLI로 통일
- Remote와 Airgap의 Target consumer 흐름 정렬

이 문서는 구현 방향의 기준 문서다. 기존 shell script는 재사용 가능한 backend일 수 있지만 설치자에게 노출되는 정상 인터페이스는 아니다.

---

## 1. 목적과 mode 정의

Remote mode는 외부 인터넷과 AWS에 직접 접근할 수 없는 EVA Target을, 인터넷에 접근 가능한 Main 서버가 준비한 Main Harbor와 설치 자산으로 설치하는 모드다.

Remote mode는 Cloud mode와 다른 제품, Chart, component version을 사용하지 않는다. 세 Repository mode는 동일한 EVA Release를 사용하고 자산 획득 위치와 Target 공급 경로만 다르다.

```text
Cloud:
  Target이 외부 저장소에서 직접 획득

Remote:
  Main 서버가 외부 저장소에서 한 번 획득
  Main Harbor와 Main preparation cache를 통해 Target에 공급

Local/Airgap:
  인터넷 가능 준비 서버에서 자산 획득
  검증된 Bundle을 폐쇄망으로 반입
  Local Harbor와 Local preparation cache를 통해 Target에 공급
```

---

## 2. 핵심 설계 원칙

### 2.1 동일 Release

모든 mode는 다음 계약을 공유한다.

```text
EVA Release version
Helm Chart version
Application image version
Infrastructure image version
Workspace schema
Component 설치 순서
Health check 계약
```

Mode별 별도 Chart나 고객별 image를 만들지 않는다.

### 2.2 Acquire/Publish와 Deploy/Consume 분리

```text
Acquire/Publish plane:
  Main 또는 Airgap 준비 서버가 외부 source에서 자산을 획득
  내부 공급 지점에 검증된 형태로 게시

Deploy/Consume plane:
  Target이 내부 공급 지점의 자산만 사용해 EVA 설치
```

Remote E2E에서 외부 접근은 Main 준비 단계에만 허용한다. Target 설치 단계에서는 외부 접근을 허용하지 않는다.

### 2.3 Target은 원래 공급 출처를 알 필요가 없음

```text
금지:
  Target -> AWS ECR
  Target -> AWS S3
  Target -> Docker Hub
  Target -> Hugging Face
  Target -> 외부 Helm Repository
  Target -> public APT repository

허용:
  Target -> Main Harbor
  Target <- Main 서버가 전달한 검증된 Release와 preparation payload
  Target -> 명시적으로 허용된 내부 DNS, NTP, package mirror
```

### 2.4 Workspace에는 사이트 의도만 기록

다음 값은 Workspace에 반복하지 않는다.

```text
image.repository
image.tag
helper/init/test image repository
imagePullSecrets.enabled
Qdrant snapshot source 구현 방식
Remote mode용 pull policy
Harbor project를 포함한 image prefix
AWS credential 또는 AWS source 설정
```

Deployer는 다음 입력으로 repository-derived 값을 계산한다.

```text
Repository mode
Repository registry
Repository project
Release metadata
Component version catalog
Preparation manifest
```

### 2.5 정상 인터페이스는 `eva` CLI

설치자와 자산 준비 담당자가 직접 실행하는 정상 경로는 `eva` CLI만 사용한다.

직접 `ansible-playbook`, `helm`, 개별 download/publish script를 실행하는 절차는 정상 Runbook에 포함하지 않는다.

내부 shell script는 다음 조건에서만 허용한다.

- `eva` CLI가 호출하는 versioned backend
- Release에 포함되고 checksum으로 검증됨
- 설치된 EVA Tool 경로에서 실행 가능함
- Git checkout 또는 현재 Repository layout에 의존하지 않음

---

## 3. 사용자 CLI 계약

CLI는 정상 경로에 필요한 parameter만 노출한다. 기본값이 명확한 값과 내부 구현 상세는 생략한다.

### 3.1 Main 자산 준비

```bash
sudo eva remote prepare . \
  --registry 10.159.56.124:32080
```

`RELEASE_PATH`는 생략 가능하며 기본값은 현재 디렉터리 `.`이다.

### 3.2 준비 결과 검증

```bash
sudo eva remote verify . \
  --registry 10.159.56.124:32080
```

검증은 자산을 다시 다운로드하거나 게시하지 않는다.

### 3.3 Target으로 Release 게시

```bash
sudo eva remote publish . \
  --target eva@10.159.56.196
```

### 3.4 Target 설치

Target에서 게시된 원본 Release 디렉터리로 이동한 뒤 실행한다.

```bash
sudo eva workspace validate \
  --workspace /home/eva/site-remote-196

sudo eva preflight gpu

sudo eva preflight argocd \
  --workspace /home/eva/site-remote-196

sudo eva install . \
  --workspace /home/eva/site-remote-196 \
  --yes

sudo eva check --verbose
sudo eva status
```

### 3.5 정상 경로에서 생략하는 기본값

```text
Release path:
  현재 디렉터리 .

Harbor project:
  eva

AWS authentication:
  Main 서버의 기본 AWS credential chain

AWS region:
  Release 또는 자산 source의 관리 기본값

Component preparation scope:
  전체 EVA stack

Qdrant snapshot source in Remote:
  harbor

Qdrant values profile in Remote:
  values-k3s.harbor.yaml

Target Release root:
  /var/lib/eva/inbox/releases

Cache root:
  CLI가 Release version 기준으로 관리

vLLM profile:
  준비 단계에서 필요한 지원 자산을 준비
  Target Config 단계에서 GPU/MIG 기준으로 선택
```

다음 값은 필요하면 advanced option으로 지원할 수 있으나 대표 help와 Runbook의 정상 명령에는 나열하지 않는다.

```text
--project
--target-root
--ssh-option
명시적 AWS profile/region override
일부 component만 준비하는 override
cache root override
```

---

## 4. `eva remote` 명령 책임

### 4.1 `eva remote prepare`

Main 서버에서 다음 작업을 orchestration한다.

```text
1. 원본 Release와 checksum 검증
2. Remote 준비에 필요한 version catalog 확인
3. Runtime 및 OS package payload 준비
4. Helm Chart, plugin, post-renderer 준비
5. EVA product image 다운로드
6. Infrastructure image 다운로드
7. Agent 및 vLLM model cache 준비
8. Qdrant snapshot 다운로드
9. 모든 필수 image를 Main Harbor에 게시
10. Qdrant snapshot을 Main Harbor OCI artifact로 게시
11. release-level preparation manifest 생성
12. 최종 fail-closed 검증
```

Remote 기본값은 CLI가 일관되게 backend에 주입한다.

```text
REPOSITORY_PROJECT=eva
COMPONENTS=all
EVA_AGENT_QDRANT_SNAPSHOT_SOURCE=harbor
EVA_AGENT_QDRANT_VALUES_FILE=values-k3s.harbor.yaml
PULL_PLATFORM=linux/amd64
```

민감한 credential 값은 manifest, stdout summary, Release artifact에 기록하지 않는다.

### 4.2 `eva remote verify`

다음 항목을 read-only로 검증한다.

```text
Release checksum과 platform
필수 eva-offline artifact
Runtime payload와 runtime descriptor
OS package bundle과 manifest
필수 Chart, plugin, post-renderer
필수 image list 완전성
Main Harbor의 모든 필수 image/tag 존재 여부
Agent/vLLM model cache와 manifest
Qdrant snapshot input과 OCI artifact 존재 여부
preparation manifest schema, Release version, digest 정합성
외부 source reference가 Target용 resolved values에 남지 않았는지 여부
```

누락 항목이 하나라도 있으면 성공으로 처리하지 않는다.

### 4.3 `eva remote publish`

검증된 원본 Release와 Release identity에 결합된 non-Harbor payload를 Target으로 함께 전달한다.

필수 계약:

```text
Source Release checksum 검증
payload manifest, checksum, Release identity와 archive entry 재검증
정확히 하나의 eva-offline artifact 요구
symlink와 unsafe path 거부
Target staging directory로 전송
Target에서 checksum 재검증
Target에서 Release version 재검증
atomic rename으로 최종 게시
동일 Release 재게시 허용
동일 version의 다른 Release 덮어쓰기 차단
실패 시 final path를 삭제하거나 교체하지 않음
Workspace, inventory, AWS credential은 전송하지 않음
```

---

## 5. EVA Tool packaging과 backend 실행 구조

### 5.1 문제 정의

`eva` binary만 설치하고 backend script를 Repository 경로에서 찾도록 구현하면 개발 checkout에서는 동작하지만 실제 Tag Base Release 설치 환경에서는 실패한다.

따라서 `eva remote`가 사용하는 backend는 EVA Tool archive에 포함되어야 한다.

### 5.2 목표 archive layout

```text
eva-tool_<version>_linux_amd64.tar.gz
├── bin/
│   └── eva
└── libexec/
    └── remote-root/
        ├── scripts/
        │   ├── download/
        │   ├── lib/
        │   ├── publish/
        │   └── remote/
        └── src/
            ├── infra/version.yaml
            └── solution/version.yaml
```

설치 결과:

```text
/opt/eva/tool/bin/eva
/opt/eva/tool/libexec/remote-root/
/usr/local/bin/eva -> /opt/eva/tool/bin/eva
```

### 5.3 backend root 해석

정상 설치에서는 현재 실행 중인 `eva` binary의 real path를 기준으로 backend root를 계산한다.

```text
<eva-binary-dir>/../libexec/remote-root
```

테스트 목적의 override는 허용할 수 있다.

```text
EVA_REMOTE_LIBEXEC_ROOT
```

이 환경변수는 정상 Runbook에 노출하지 않는다.

### 5.4 archive 및 installer 안전 계약

```text
필수 entry 존재 확인
absolute path 거부
.. path traversal 거부
symlink 거부
block/character device, FIFO, socket 거부
regular file과 directory만 허용
bin/eva executable 검증
backend shell script executable mode 적용
root:root ownership 적용
staging 설치 후 atomic publish
기존 tool backup 후 실패 시 복원
```

### 5.5 구현 단순성 원칙

- `main.go`에는 argument parsing과 command dispatch만 둔다.
- Remote orchestration은 `tools/eva/internal/remote` package로 분리한다.
- shell backend의 경로와 환경 구성은 한 곳에서 관리한다.
- 기존 script의 모든 저수준 option을 CLI에 그대로 노출하지 않는다.
- 새 legacy fallback 또는 Repository checkout fallback을 추가하지 않는다.
- backend가 없으면 명확하게 실패한다.

---

## 6. 자산 유형별 공급 계약

### 6.1 Container image

```text
Remote:
  외부 Registry -> Main Docker cache -> Main Harbor -> Target

Airgap:
  외부 Registry -> verified Bundle -> Airgap cache
  -> Local Harbor -> Target
```

Remote Target workload의 product/helper image는 기본적으로 다음 prefix를 사용한다.

```text
<repository.registry>/<repository.project>/
```

검증 범위:

```text
containers
initContainers
ephemeralContainers
Helm hook/test Pod
Job/CronJob
Deployment
StatefulSet
DaemonSet
```

주소가 없는 Docker Hub image도 mirror 계약에 포함한다.

### 6.2 Helm Chart와 설치 asset

Chart, manifest, script, plugin, post-renderer는 Release 또는 preparation payload에서 공급한다. Target 설치 중 외부 Helm Repository를 사용하지 않는다.

### 6.3 Runtime과 OS package

```text
cloud:
  online Runtime 허용
  online APT 허용

remote:
  prepared Runtime 사용
  prepared package bundle 사용
  Target public APT 접근 금지

local:
  imported Airgap Bundle의 Runtime 사용
  imported package bundle 사용
```

Remote와 Local은 가능하면 동일한 consumer를 사용하고 asset root만 다르게 한다.

### 6.4 vLLM model

Remote Target에서는 다음을 사용하지 않는다.

```text
S3 sync initContainer
amazon/aws-cli model helper
aws-credentials Secret
Hugging Face online fallback
외부 model repository 접근
```

Remote 흐름:

```text
Main:
  외부 source에서 model 다운로드
  Release별 prepared model cache와 manifest 생성

Target:
  Main preparation payload를 local/NFS cache에 materialize
  vLLM은 local model path 사용
```

필수 offline 환경:

```text
HF_HUB_OFFLINE=1
TRANSFORMERS_OFFLINE=1
HF_HUB_DISABLE_TELEMETRY=1
```

첫 Remote E2E에서는 기존 model cache와 NFS materialization을 우선 재사용한다. Harbor model OCI artifact를 동시에 새 필수 요구사항으로 도입하지 않는다.

### 6.5 Qdrant snapshot

Remote 표준은 Harbor OCI artifact다.

```text
Main:
  S3에서 snapshot 다운로드
  manifest 검증
  Main Harbor에 OCI artifact push

Target:
  qdrant-snapshot-sync가 ORAS pull
  snapshot PVC에 저장
  Qdrant restore API 실행
```

금지:

```text
amazon/aws-cli sidecar
aws-credentials Secret
S3_BUCKET
S3_PREFIX
aws s3 sync
```

필수:

```text
values-k3s.harbor.yaml
qdrant-snapshot-sync image in Main Harbor
qdrant-snapshot-harbor Secret
Pod에서 접근 가능한 Harbor endpoint
Harbor OCI artifact manifest
```

---

## 7. Component별 repository 자동화

Remote mode에서는 product image뿐 아니라 helper/init/test image까지 자동 rewrite한다.

### IAM

```text
Keycloak
Dispatcher
Realm config CLI
BusyBox initContainer
PostgreSQL
Redis
TLS Job kubectl
```

### App

```text
EVA App
MySQL
TLS Job kubectl
기타 Chart helper image
```

### Vision

```text
EVA Vision
Vision init/helper image
```

### Agent

```text
EVA Agent
Agent init
vLLM
Qdrant
Qdrant snapshot-sync
chart test image
기타 init/helper image
```

Repository rewrite는 chart default에 암묵적으로 의존하지 않는다. Deployer가 생성하는 mode-specific effective/resolved values에 명시적으로 남긴다.

---

## 8. Workspace 목표 구조

```text
site-remote-196/
├── inventory/
│   └── inventory.ini
└── site-values/
    ├── site.yaml
    ├── iam.yaml
    ├── app.yaml
    ├── vision.yaml    # 사이트별 차이가 있을 때만
    └── agent.yaml     # 사이트별 차이가 있을 때만
```

Remote Target에는 다음 파일을 두지 않는다.

```text
credentials/aws_key.ini
site-values/agent-vllm.yaml
site-values/agent-qdrant.yaml
repository/image override 전용 파일
```

현재 schema 제약으로 임시 component override가 필요하더라도 repository와 AWS 관련 값은 포함하지 않는다.

---

## 9. Preparation manifest

`eva remote prepare`는 Release 단위의 preparation manifest를 원자적으로 생성해야 한다.

권장 위치:

```text
/var/lib/eva/preparation/remote/<release-version>/manifest.yaml
```

최소 schema:

```yaml
schema_version: v1
release:
  version: 3.2.0
  release_yaml_sha256: <sha256>
  checksums_sha256: <sha256>
repository:
  registry: 10.159.56.124:32080
  project: eva
assets:
  runtime: prepared
  packages: prepared
  images: published
  models: prepared
  qdrant_snapshot: published
artifacts:
  image_mapping: <path>
  model_manifest: <path>
  snapshot_manifest: <path>
  harbor_artifact_manifest: <path>
```

주의 사항:

- credential, token, password를 기록하지 않는다.
- timestamp만으로 완료 여부를 판단하지 않는다.
- Release digest와 registry/project가 현재 요청과 일치해야 재사용한다.
- 일부 단계 성공 후 실패하면 성공한 단계의 증거를 보존하되 전체 상태를 성공으로 기록하지 않는다.
- manifest publish는 temporary file 작성 후 rename으로 처리한다.

---

## 10. Fail-closed 조건

다음 조건에서 설치 또는 준비를 시작하기 전에 실패한다.

```text
repository.registry 미지정
registry에 URL scheme 포함
registry가 localhost
Harbor 접근 불가
Harbor project 준비 실패
필수 image/tag 누락
image download missing list가 비어 있지 않음
Qdrant snapshot input 또는 OCI artifact 누락
vLLM model manifest 또는 model cache 누락
Runtime payload 누락
package bundle 누락
원본 Release에 eva-offline이 없음
Remote Target에 AWS source가 선택됨
Target용 resolved values에 외부 image reference가 남음
preparation manifest와 Release digest 불일치
```

Fallback 금지:

```text
Harbor image 누락 -> Docker Hub
model cache 누락 -> Hugging Face/S3
snapshot artifact 누락 -> S3
package 누락 -> public APT
backend 누락 -> Git checkout의 script 탐색
```

---

## 11. E2E 완료 기준

### 11.1 Main 준비

```text
[OK] Release 검증
[OK] 필수 image 목록 완성
[OK] 모든 product/helper/init/test image Main Harbor 게시
[OK] vLLM model cache와 manifest 준비
[OK] Qdrant snapshot OCI artifact 게시
[OK] Runtime payload 준비
[OK] package bundle 준비
[OK] preparation manifest 생성 및 verify 통과
```

### 11.2 Target 설치

```text
[OK] Target public outbound 차단
[OK] Main Harbor 접근
[OK] Workspace mode=remote
[OK] internal mode=remote_repository
[OK] Offline Runtime 설치
[OK] package bundle만 사용
[OK] 전체 component 설치
[OK] eva check --verbose
[OK] eva status
```

### 11.3 공급 경로

```text
[OK] 모든 product image가 Main Harbor 사용
[OK] 모든 initContainer image가 Main Harbor 사용
[OK] 모든 helper/test image가 Main Harbor 사용
[OK] vLLM model이 prepared local/NFS cache 사용
[OK] Qdrant snapshot이 Harbor OCI artifact 사용
[OK] Target에서 S3 요청 없음
[OK] Target에서 ECR 요청 없음
[OK] Target에서 Docker Hub 요청 없음
[OK] Target에 AWS credential 없음
```

### 11.4 재실행

```text
[OK] 같은 Release와 입력으로 prepare 재실행 성공
[OK] 검증된 image 불필요 재게시 없음 또는 안전한 idempotent 처리
[OK] model 불필요 재다운로드 없음
[OK] snapshot 불필요 재복구 없음
[OK] 동일 Release 재게시 성공
[OK] persistent data 초기화 없음
```

---

## 12. VS Code Agent 구현 지침

### 12.1 작업 방식

Agent는 구현 전에 현재 worktree의 exact context를 다시 확인한다.

필수 원칙:

```text
추정 anchor 사용 금지
전역 치환 금지
한 번에 거대한 patch 금지
수정 전 exact match count 확인
단계별 targeted test 수행
기존 fallback/legacy 로직을 무조건 보존하지 않음
새 기능의 경계를 단순하고 선명하게 유지
오류 발생 시 파일을 부분 적용 상태로 남기지 않음
```

프로젝트 Python 명령이 필요하면 시스템 `python` 대신 `.venv/bin/python`을 사용한다. Ruff 단계는 추가하지 않는다.

### 12.2 우선 확인할 파일

```text
tools/eva/cmd/eva/main.go
tools/eva/cmd/eva/main_test.go
tools/eva/internal/release/release.go
tools/eva/internal/release/release_test.go
scripts/install/install_eva_tool.sh
scripts/release/build_release.sh
scripts/remote/publish_release_to_target.sh
scripts/test/test_remote_release_transport.sh
scripts/download/download_offline_assets.sh
scripts/download/download_eva_images.sh
scripts/download/download_infra_images.sh
scripts/download/download_eva_models.sh
scripts/download/download_qdrant_snapshots.sh
scripts/publish/push_images_to_repository.sh
scripts/publish/push_qdrant_snapshots_to_harbor.sh
scripts/lib/load_versions.sh
src/infra/version.yaml
src/solution/version.yaml
.github/workflows/pr-ci.yaml
.github/workflows/tag-release.yaml
```

### 12.3 Phase R8-1: Remote backend packaging

목표:

```text
eva-tool archive에 Remote backend 포함
installer가 /opt/eva/tool/libexec/remote-root에 설치
설치된 eva binary가 backend root를 안정적으로 해석
```

구현 항목:

1. `build_release.sh`가 필요한 backend 파일과 version catalog를 tool staging에 복사한다.
2. Tool archive가 `bin/eva`와 `libexec/remote-root`를 포함한다.
3. 포함 파일은 explicit allowlist 또는 관리되는 directory set으로 제한한다.
4. installer의 기존 `exactly bin/eva` 계약을 새 archive layout에 맞게 수정한다.
5. archive와 extracted tree에서 unsafe path, symlink, special file을 거부한다.
6. shell 실행 파일 mode, directory mode, root ownership을 설정한다.
7. installer smoke test가 backend 파일과 `eva remote --help`를 검증한다.

주의:

- `download_offline_assets.sh`가 호출하는 추가 helper가 package에 빠지지 않았는지 dependency를 확인한다.
- 단순히 현재 Bundle에 포함된 4개 script directory만 복사하고 끝내지 말고 실제 command reference를 검사한다.
- 필요 script 누락 시 runtime failure가 아니라 build/test 단계에서 실패하도록 한다.

### 12.4 Phase R8-2: `eva remote publish`

목표 CLI:

```bash
sudo eva remote publish . --target eva@10.159.56.196
```

구현 구조:

```text
tools/eva/internal/remote/
  backend.go
  publish.go
  publish_test.go
```

`main.go` 책임:

```text
remote command dispatch
최소 argument parsing
Release path 기본값 .
--target 필수 확인
internal/remote 호출
```

internal package 책임:

```text
backend root 해석
publish backend regular/executable 확인
validated Release 전달
backend process 실행
stdin/stdout/stderr 연결
error wrapping
```

검증:

```text
help shape
release positional argument가 flag 앞 또는 뒤에 있어도 동작
--target 누락 실패
prepared Release 거부
backend 누락 실패
backend non-executable 실패
argument 전달 정확성
transport contract test 유지
```

### 12.5 Phase R8-3: `eva remote prepare`

목표 CLI:

```bash
sudo eva remote prepare . --registry 10.159.56.124:32080
```

권장 구조:

```text
tools/eva/internal/remote/
  prepare.go
  prepare_test.go
  runner.go
  manifest.go
  manifest_test.go
```

단계는 명시적인 ordered step으로 구성한다.

```text
validate-release
prepare-offline-assets
download-product-images
download-infra-images
download-models
download-qdrant-snapshots
publish-product-images
publish-infra-images
publish-qdrant-snapshots
write-manifest
verify
```

각 step은 다음 정보를 갖는다.

```text
name
backend command
필수 환경변수
입력 증거
출력 증거
재실행 skip 조건
```

필수 동작:

- Remote Qdrant profile을 자동 주입한다.
- product image와 infra image list를 각각 정확히 게시한다.
- download 단계 이후 missing list가 비어 있는지 확인한다.
- prepare 성공 전에 `verify`를 호출한다.
- 단순 command exit code만으로 완료를 판단하지 않는다.
- AWS credential을 command argument나 manifest에 직렬화하지 않는다.

### 12.6 Phase R8-4: `eva remote verify`

목표 CLI:

```bash
sudo eva remote verify . --registry 10.159.56.124:32080
```

verify는 read-only여야 한다.

검증 구현은 가능하면 Go에서 수행한다. 단, Harbor/ORAS 확인에 관리 backend가 필요한 경우 credential이 process list나 persistent config에 노출되지 않도록 한다.

최소 테스트:

```text
complete preparation 성공
missing image list 실패
empty snapshot 실패
missing model manifest 실패
Release digest mismatch 실패
registry mismatch 실패
credential이 manifest에 포함되면 실패
verify 실행 중 download/push command가 호출되지 않음
```

### 12.7 Phase R8-5: CI와 Runbook

CI:

```text
Go format
Go test
Go vet
shell syntax
ShellCheck
Remote transport contract
Tool archive layout contract
installer smoke test
remote CLI argument contract
preparation manifest tests
```

Runbook 정상 경로에는 다음 세 명령만 노출한다.

```bash
sudo eva remote prepare . --registry <main-harbor>
sudo eva remote verify . --registry <main-harbor>
sudo eva remote publish . --target <user@target>
```

내부 environment variable, script path, Qdrant values filename, ORAS 세부 명령은 architecture 또는 developer troubleshooting 문서에만 둔다.

---

## 13. 구현 중 반드시 재검토할 현재 갭

### 13.1 Tool backend dependency 완전성

현재 download script가 다른 helper script를 동적으로 호출할 수 있다. 예를 들어 NVIDIA driver repository builder처럼 직접 Bundle 목록에 없던 dependency가 있을 수 있다.

Agent는 다음을 수행해야 한다.

```text
backend 대상 script의 source/exec reference 수집
참조되는 local script가 archive에 모두 포함되는지 확인
참조 누락 contract test 추가
```

### 13.2 `eva-offline` 생성 주체

결정: original Release의 `eva-offline`은 Runtime bootstrap용으로 유지하고,
`eva remote prepare`는 versioned Remote Target payload를 별도로 생성한다. payload는
Release manifest를 변경하지 않으며 publish staging에서 original Release와 함께
검증·게시된다. Target은 설치 전에 payload를 기존 cache consumer가 읽는 managed
cache로 materialize한다. 따라서 `remote publish`의 original Release에 정확히 하나의
`eva-offline`이 필요하다는 계약도 유지된다.

### 13.3 Runtime tool dependency

`download_eva_images.sh`는 `helm`과 `docker`를 요구한다. Main 준비 서버가 system tool을 쓸지 EVA Managed Runtime을 쓸지 명확히 한다.

권장 방향:

```text
helm/oras 등 관리 가능한 도구는 EVA Runtime 또는 versioned payload 사용
docker, apt 등 host-level 도구는 preflight로 명확히 검증
```

### 13.4 Registry credential UX

정상 CLI parameter에는 password를 넣지 않는다.

지원 우선순위 예시:

```text
기존 Docker credential
승인된 environment/secret injection
일치하는 local Harbor configuration
interactive secure prompt
```

실제 정책을 정한 뒤 prepare와 verify가 같은 credential resolver를 사용하도록 한다.

### 13.5 Root 실행과 사용자 credential

`sudo eva remote prepare`가 root의 HOME과 credential store를 사용하면 일반 사용자의 AWS/Docker credential을 찾지 못할 수 있다.

Agent는 이를 암묵적으로 무시하지 말고 다음 중 하나로 명확히 설계한다.

```text
Main 준비 전용 service credential
sudo preserve 정책을 문서화한 approved environment
CLI가 credential source를 안전하게 명시적으로 resolve
root 실행이 불필요한 단계와 필요한 단계를 분리
```

credential 값을 CLI argument, log, manifest에 노출하지 않는다.

### 13.6 R8-6 E2E readiness audit 결과

R8-6의 static contract는 코드와 문서의 교차 계약만 점검한다. 실제
Release, Main host, Harbor, Target readiness는 개발 검증 문서
[`remote-live-e2e-readiness.md`](../development/remote-live-e2e-readiness.md)에
별도로 기록한다.

#### [DECISION] R8-8 Main preparation preflight

`eva remote prepare`는 첫 download backend 전에 `main-preflight` manifest step을
실행한다. 이 단계는 host tool executable/version probe, Docker daemon의 amd64 및 data
root, 현재 실행 주체의 AWS STS credential, registry protocol·matching Docker credential,
preparation/Docker storage, 그리고 현재 backend source의 bounded TCP probe를 확인한다.
probe는 pull, login, push, project 생성 또는 credential 복사를 수행하지 않는다.

성공 결과는 credential-free `reports/main-preflight.yaml`에만 기록한다. 실패는
sanitized error로 manifest를 failed 처리하고 모든 download/push backend 실행 전에
중단한다. succeeded preparation 재사용은 이미 검증된 local evidence만 재검증하며,
새 live preflight를 수행하지 않는다. 실제 live readiness 자체는 여전히 E2E evidence가
필요하다.

#### [DECISION] R8-7 Target payload supply contract

Remote prepare는 managed preparation root에서 별도 versioned payload archive를
생성한다. original Release의 checksum 또는 `eva-offline`은 사후 변경하지 않는다.
이 선택은 Cloud Release와 registry별 Remote preparation 결과를 분리하면서 기존
`eva_cache_root` consumer를 그대로 재사용한다.

payload identity는 schema version, Release version, `release.yaml` 및
`checksums.sha256` SHA-256, registry/project, platform을 결합한 digest다. archive와
content digest, category는 payload manifest와 checksum manifest로 검증한다. 같은
Release version이라도 다른 registry/project identity는 같은 Target final Release를
덮어쓰지 않는다.

포함 범위는 offline package/host asset, chart·plugin·helper, Agent/vLLM model cache와
그 consumer metadata다. container image archive와 Qdrant snapshot file은 포함하지
않으며 Main Harbor OCI 경로를 유지한다. credential, workspace, inventory, site values,
log, temporary work 및 secret-like path는 payload 생성과 검증에서 거부한다.

`remote prepare`가 archive를 만들고 `remote verify`가 read-only로 재검증한다.
`remote publish`는 original Release와 payload를 하나의 Target staging area에서 모두
검증한 뒤 atomic rename한다. Target `eva install`은 Remote mode에서만 archive를
staging extract·재검증 후 versioned managed cache로 atomic materialize하고, 그 cache를
기존 `eva_cache_root`와 Agent cache 변수로 전달한다. 동일 identity는 재사용하며,
손상 또는 다른 identity의 기존 cache/final Release는 삭제하거나 덮어쓰지 않는다.

Airgap은 향후 동일한 cache consumer를 사용하되, bundle import가 producer가 된다.
Remote payload의 source는 Main preparation이며 Target은 외부 APT, S3, Hugging Face,
Docker Hub 또는 외부 Chart repository로 fallback하지 않는다. 실제 Main/Harbor/Target
live E2E는 여전히 수행 전이다.

---

## 14. 구현 순서와 완료 marker

```text
R8-1  Tool libexec packaging과 installer
R8-2  eva remote publish
R8-3  preparation manifest와 step runner
R8-4  eva remote prepare
R8-5  eva remote verify
R8-6  Remote preparation/runbook 정리
R8-7  Remote Target payload supply contract
A1    동일 consumer 기반 Airgap 전환
```

각 phase 종료 시 다음 형식으로 결과를 남긴다.

```text
[OK] 변경 파일
[OK] 핵심 계약
[OK] targeted tests
[OK] regression tests
[INFO] 아직 구현하지 않은 범위
[BLOCKED] 결정 또는 외부 환경이 필요한 항목
```

---

## 15. 고정 결정 사항

```text
[DECISION] Remote Main 서버의 Cloud 접근은 허용
[DECISION] Remote Target의 Cloud 접근은 금지
[DECISION] AWS credential은 Main preparation에서만 사용
[DECISION] 모든 container image는 Main Harbor를 통해 공급
[DECISION] vLLM model은 Main prepared cache에서 Target NFS로 공급
[DECISION] Qdrant snapshot은 Main Harbor OCI artifact로 공급
[DECISION] Remote와 Airgap은 동일한 Target consumer 흐름을 사용
[DECISION] 차이는 preparation cache를 만드는 방법으로 제한
[DECISION] repository-derived values는 Workspace에 반복하지 않음
[DECISION] Vision/Agent YAML은 사이트별 차이가 있을 때만 작성
[DECISION] Remote 누락 자산은 Cloud fallback 없이 fail-closed
[DECISION] 설치자 및 준비 담당자의 정상 인터페이스는 eva CLI
[DECISION] 내부 shell script는 versioned libexec backend로만 사용
[DECISION] Release path 기본값은 현재 디렉터리
[DECISION] Harbor project 기본값은 eva
[DECISION] 전체 component가 기본 preparation 범위
[DECISION] Remote Qdrant Harbor profile은 자동 선택
[DECISION] Target Release root 기본값은 /var/lib/eva/inbox/releases
[DECISION] 대표 help와 Runbook에는 advanced option을 나열하지 않음
```

---

## 16. Agent 시작 프롬프트

다음 내용을 VS Code Agent에 전달해 첫 구현을 시작한다.

```text
이 문서를 EVA Remote mode 구현의 기준으로 사용해라.

먼저 Phase R8-1만 구현한다. 현재 worktree를 읽고 exact context를 확인한 뒤,
EVA Tool archive가 bin/eva와 versioned Remote backend를 함께 포함하도록 만들고
installer가 /opt/eva/tool/libexec/remote-root에 안전하게 설치하도록 수정한다.

중요 조건:
- Repository checkout fallback을 추가하지 않는다.
- archive와 extracted tree의 symlink, unsafe path, special file을 fail-closed한다.
- backend local-script dependency를 분석해 누락 없이 package한다.
- existing installer atomic replacement와 rollback 계약을 유지한다.
- main.go에 orchestration을 넣지 않는다.
- 변경을 작은 단계로 나누고 단계마다 targeted test를 실행한다.
- Ruff는 실행하지 않는다.
- Python이 필요하면 .venv/bin/python을 사용한다.
- 완료 후 변경 파일, 계약, 테스트 결과, 남은 갭을 [OK]/[INFO]/[BLOCKED]로 요약한다.

R8-1 검증이 통과한 뒤에만 R8-2 eva remote publish를 구현한다.
사용자 명령은 다음 형태여야 한다.

  sudo eva remote publish . --target eva@10.159.56.196

Release path 기본값은 현재 디렉터리이며 --target만 정상 경로의 필수 option이다.
기존 publish_release_to_target.sh의 checksum 재검증, staging, atomic rename,
idempotent republish, conflicting same-version 거부 계약을 보존한다.
```
