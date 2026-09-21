# EVA Remote Mode Implementation Design

## 1. 목적

Remote mode는 외부 인터넷 및 AWS에 직접 접근할 수 없는 EVA Target을, 인터넷에 접근 가능한 Main 서버가 준비한 Main Harbor와 설치 자산을 이용해 설치하는 모드다.

Remote mode는 Cloud mode와 다른 EVA 제품이나 Chart를 사용하지 않는다. 세 Repository mode는 동일한 EVA Release와 동일한 component version을 사용하며, 자산의 획득 위치와 Target 공급 경로만 다르다.

```text
Cloud:
  Target이 외부 저장소에서 직접 획득

Remote:
  Main 서버가 외부 저장소에서 한 번 획득
  Main Harbor 및 Main 준비 자산을 통해 Target에 공급

Airgap:
  인터넷 가능 준비 서버에서 미리 획득
  검증된 Bundle을 저장매체로 반입
  Local Harbor 및 Local 준비 자산을 통해 Target에 공급
```

## 2. 핵심 설계 원칙

### 2.1 동일 Release

모든 mode는 동일한 다음 항목을 사용한다.

```text
EVA Release version
Helm Chart version
Application image version
Infrastructure image version
Workspace schema
Component 설치 순서
Health check 계약
```

Mode별로 별도 Chart 또는 고객별 image를 만들지 않는다.

### 2.2 Acquire와 Deploy 분리

Remote 구현은 다음 두 단계를 명확히 분리한다.

```text
Acquire/Publish plane:
  외부 source에서 자산을 획득하고 내부 공급 지점에 게시

Deploy/Consume plane:
  Target이 내부 공급 지점의 자산만 사용해 EVA 설치
```

Remote E2E는 Main 준비 단계에서 외부 접근을 허용하되, Target 설치 단계에서는 외부 접근을 허용하지 않는다.

### 2.3 Target은 원래 공급 출처를 알 필요가 없음

Target은 image나 model이 원래 어디에서 왔는지 알 필요가 없다.

```text
금지:
  Target -> AWS ECR
  Target -> AWS S3
  Target -> Docker Hub
  Target -> Hugging Face
  Target -> 외부 Helm Repository

허용:
  Target -> Main Harbor
  Target <- Main 서버의 설치 자산 전송
  Target -> 내부 DNS/NTP/APT mirror 등 명시적으로 허용된 내부 서비스
```

### 2.4 Workspace에는 사이트 의도만 기록

다음 값은 Workspace에 반복하지 않는다.

```text
image.repository
image.tag
helper image repository
initContainer image repository
imagePullSecrets.enabled
Qdrant snapshot source implementation
Remote mode용 pull policy
Harbor project를 포함한 component image prefix
```

이 값은 아래 입력으로 Deployer가 계산한다.

```text
Repository mode
Repository registry
Repository project
Release metadata
Component version manifest
```

## 3. Mode별 자산 흐름

### 3.1 Cloud mode

```text
AWS / Docker Hub / S3 / Helm Repo
                 |
                 v
            EVA Target
```

Target이 직접 외부 서비스를 사용한다.

### 3.2 Remote mode

```text
AWS / Docker Hub / S3 / Helm Repo
                 |
                 | 외부 접근은 Main에서만 수행
                 v
Main Preparation Cache / Main Harbor
                 |
                 | 내부망
                 v
            EVA Target
```

Main 서버가 Release별로 한 번 다운로드하고, 여러 Target이 동일한 Main Harbor와 준비 자산을 재사용한다.

### 3.3 Airgap mode

```text
AWS / Docker Hub / S3 / Helm Repo
                 |
                 v
Internet Preparation / Verified Bundle
                 |
                 | 저장매체 반입
                 v
Airgap Preparation / Local Harbor
                 |
                 v
            EVA Target
```

Remote와 Airgap은 Target 관점에서는 유사하다. 차이는 내부 공급 자산을 만드는 과정이다.

## 4. 자산 유형별 공급 계약

### 4.1 Container image

Container image는 Remote와 Airgap 모두 Harbor를 통한다.

```text
Remote:
  외부 Registry -> Main Docker cache -> Main Harbor -> Target

Airgap:
  외부 Registry -> repository-images.tar
  -> 저장매체 -> Airgap Docker cache -> Local Harbor -> Target
```

Remote Target의 workload에서 허용되는 기본 image prefix:

```text
<repository.registry>/<repository.project>/
```

예:

```text
10.159.56.124:32080/eva/
```

검증 대상은 다음을 모두 포함한다.

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

### 4.2 Helm Chart 및 설치 asset

Helm Chart, manifest, script, plugin, post-renderer는 Release 또는 준비 cache에 포함한다.

Target 설치 시 외부 Helm Repository에서 다운로드하지 않는다.

```text
Remote:
  Main 서버가 외부에서 다운로드
  검증된 Release/preparation cache에서 Target 설치에 사용

Airgap:
  준비 서버가 다운로드
  검증된 Bundle에 포함
  반입된 Release/cache에서 설치에 사용
```

### 4.3 OS package와 Runtime

Remote Target이 인터넷에 접근하지 않는다는 mode 정의를 유지하려면 Runtime과 일반 OS package도 외부 다운로드에 의존하면 안 된다.

최종 목표:

```text
Remote:
  Main 서버가 Runtime과 package bundle 준비
  Target에는 Main 서버가 전달
  Target 설치 중 public apt repository 접근 없음

Airgap:
  Bundle에 Runtime과 package bundle 포함
  Target 설치 중 network download 없음
```

현재 구현에서 Remote가 `apt-get update` 또는 online Runtime bootstrap을 시도한다면 구현 갭으로 취급한다.

다음 계약으로 구현을 정렬한다.

```text
cloud:
  online Runtime 허용
  online APT 허용

remote:
  prepared Runtime 사용
  prepared package bundle 사용
  Main 서버가 Target에 전달

local:
  imported Airgap Bundle의 Runtime 사용
  imported package bundle 사용
```

Remote와 Local이 같은 package 설치 role을 재사용하되 asset root만 달라지는 구조를 지향한다.

### 4.4 vLLM model

기존 Remote values의 다음 구성은 Remote Target의 AWS 의존을 만든다.

```yaml
initContainer:
  name: s3-sync
  image: amazon/aws-cli:2.33.8

envFromSecret:
  name: aws-credentials
```

Remote mode에서는 이 구조를 사용하지 않는다.

#### Remote model 흐름

```text
Main 서버:
  AWS/S3 또는 지원한 외부 source에서 model 다운로드
  Release별 model cache 생성
  checksum/manifest 검증

Target:
  Main 준비 cache에서 model을 전달받아 NFS cache에 materialize
  vLLM은 local NFS model path를 사용
  S3 sync initContainer를 실행하지 않음
  aws-credentials Secret을 사용하지 않음
```

#### Airgap model 흐름

```text
인터넷 준비 서버:
  model 다운로드
  manifest와 함께 Bundle에 포함

Airgap 환경:
  Bundle import
  Local model cache에서 NFS cache로 materialize
  vLLM은 Remote와 동일한 local model path 사용
```

따라서 vLLM의 Deploy 단계는 Remote와 Airgap에서 동일해야 한다.

```text
Prepared model cache
-> NFS/PVC materialization
-> offline vLLM startup
```

차이는 Prepared model cache를 얻는 방법뿐이다.

```text
Remote:
  Main 서버가 Cloud source에서 준비

Airgap:
  Bundle에서 import
```

#### vLLM Remote 계약

```text
HF_HUB_OFFLINE=1
TRANSFORMERS_OFFLINE=1
HF_HUB_DISABLE_TELEMETRY=1

S3 sync initContainer 없음
aws-credentials Secret 참조 없음
외부 model repository 접근 없음
model path는 Target local/NFS cache
```

Model image 또는 model OCI artifact 방식은 향후 확장할 수 있지만, 첫 Remote E2E에서는 기존 model download/cache 자산과 NFS materialization 흐름을 우선 재사용한다. Harbor OCI model 기능을 새 필수 요구사항으로 동시에 도입하지 않는다.

### 4.5 Qdrant snapshot

Qdrant snapshot은 Harbor OCI artifact 경로를 Remote 표준으로 사용한다.

#### Remote 흐름

```text
Main 서버:
  AWS S3에서 snapshot 다운로드
  checksum/manifest 확인
  Main Harbor에 OCI artifact로 push

Target:
  qdrant-snapshot-sync가 Main Harbor에서 ORAS pull
  snapshot PVC에 저장
  Qdrant restore API 실행
```

#### Airgap 흐름

```text
인터넷 준비 서버:
  AWS S3에서 snapshot 다운로드
  Bundle에 snapshot 포함

Airgap 환경:
  Bundle의 snapshot을 Local Harbor OCI artifact로 push

Target:
  Remote와 같은 ORAS pull 및 restore 흐름 사용
```

즉, Qdrant Deploy 계약은 Remote와 Airgap에서 동일하다.

```text
Harbor OCI artifact
-> snapshot PVC
-> Qdrant restore
```

#### Qdrant Remote 금지 항목

```text
amazon/aws-cli sidecar
aws-credentials Secret
S3_BUCKET
S3_PREFIX
aws s3 sync
```

#### Qdrant Remote 필수 항목

```text
values-k3s.harbor.yaml
qdrant-snapshot-sync image in Main Harbor
qdrant-snapshot-harbor Secret
Pod에서 접근 가능한 Harbor endpoint
Harbor OCI artifact manifest
```

## 5. Agent component 설계

Agent 계열은 하나의 mode-aware component로 관리한다.

```text
eva-agent
eva-agent-vllm
eva-agent-qdrant
```

### 5.1 공통 결정 함수

Deployer는 Workspace mode를 다음 내부 mode로 변환한다.

```text
cloud  -> cloud_repository
remote -> remote_repository
local  -> local_repository
```

Agent 설치는 내부 mode별로 source profile을 선택한다.

```text
cloud_repository:
  model_source=s3
  qdrant_snapshot_source=s3
  image_source=cloud

remote_repository:
  model_source=prepared_cache
  qdrant_snapshot_source=harbor
  image_source=harbor

local_repository:
  model_source=prepared_cache
  qdrant_snapshot_source=harbor
  image_source=harbor
```

### 5.2 EVA Agent 본체

다음은 Repository mode에서 자동 계산한다.

```text
agent image repository/tag
helper/init image repository/tag
image pull Secret
pull policy
```

다음만 Workspace override 대상으로 남긴다.

```text
CPU/memory
replica
site-specific node selector
site-specific NFS path
기능 설정
```

기본값과 같다면 `agent.yaml`을 만들지 않는다.

### 5.3 vLLM

Deployer 책임:

```text
vLLM image repository/tag
model materialization mode
prepared model path
offline environment
AWS initContainer 포함 여부
AWS Secret 참조 여부
runtimeClassName
기본 node selector
Release의 모델 이름과 version
```

Workspace 책임:

```text
특정 GPU/MIG resource
replicaCount
requestCPU/requestMemory
tensor parallel size
maxModelLen
site-specific scheduling
성능 tuning
```

GPU/MIG와 model profile을 Config 단계에서 자동 결정할 수 있다면 Workspace override도 생략한다.

### 5.4 Qdrant

Deployer 책임:

```text
Qdrant image repository/tag
snapshot-sync image repository/tag
Harbor snapshot profile 선택
Harbor endpoint/project
Harbor credential Secret
chart test image
snapshot manifest
snapshot restore command
```

Workspace 책임:

```text
StorageClass
volume size
NodePort 필요 여부
site-specific node selector
보존 정책
```

기본 storage 정책과 같다면 별도 Qdrant Workspace values를 만들지 않는다.

## 6. IAM, App, Vision repository 자동화

Remote mode에서 아래 image는 모두 자동 rewrite 대상이다.

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
vLLM
Qdrant
Qdrant snapshot-sync
AWS CLI 또는 대체 helper
chart test image
기타 init/helper image
```

Repository rewrite는 Workspace values보다 낮은 우선순위의 chart default에만 기대하지 않고, Deployer가 생성하는 mode-specific resolved values에 명시적으로 남긴다.

## 7. Workspace 목표 구조

Remote Target의 이상적인 Workspace:

```text
site-pt-a/
|-- inventory/
|   `-- inventory.ini
`-- site-values/
    |-- site.yaml
    |-- iam.yaml
    |-- app.yaml
    |-- vision.yaml      # GPU/MIG/MPS 차이가 있을 때만
    `-- agent.yaml       # resource/storage 차이가 있을 때만
```

Remote Target에는 다음 파일을 두지 않는다.

```text
credentials/aws_key.ini
site-values/agent-vllm.yaml
site-values/agent-qdrant.yaml
```

단, vLLM 및 Qdrant의 사이트별 tuning을 현재 Workspace schema가 `agent.yaml` 안에서 표현하지 못한다면, 구조 변경 전까지 기존 component override 파일을 임시로 유지할 수 있다. 이 경우에도 repository와 AWS 관련 값은 포함하지 않는다.

## 8. Main preparation workflow

Remote mode는 Main 서버에서 다음 순서로 준비한다.

### 8.1 Release 검증

```text
Release checksum 검증
Release version 확인
지원 OS/architecture 확인
versions/manifest 확인
```

### 8.2 외부 credential 사용

AWS credential은 Main 준비 단계에서만 사용한다.

```text
Main 서버:
  ECR login
  S3 asset/model/snapshot download

Remote Target:
  AWS credential 없음
```

AWS credential을 다음에 포함하지 않는다.

```text
Release archive
Remote Workspace
operation plan
Ansible log
Harbor artifact annotation
Target ~/.aws
Kubernetes Secret
```

### 8.3 Image 준비 및 publish

```text
EVA images 다운로드
Infra images 다운로드
필요 시 n8n image 다운로드
필수 helper/init/test image 다운로드
Main Harbor push
Harbor repository/tag 검증
```

### 8.4 Model 준비

```text
S3 또는 지원 source에서 모델 다운로드
checksum와 manifest 검증
Main preparation cache에 게시
Target별 NFS model cache로 전달 가능하게 준비
```

### 8.5 Qdrant snapshot 준비

```text
S3 snapshot 다운로드
manifest 검증
Main Harbor OCI artifact로 push
OCI digest 기록
```

### 8.6 Runtime/package 준비

```text
Managed Runtime payload 준비
Ubuntu package bundle 준비
manifest 및 dependency 검증
Remote Target에 전달 가능하게 게시
```

## 9. Remote 설치 workflow

설치자 정상 경로:

```text
1. Main preparation 완료 확인
2. Target에서 Main Harbor 연결 확인
3. Workspace validate
4. Release verify
5. Managed Runtime 설치
6. GPU preflight
7. Argo CD handoff
8. eva install
9. eva check
10. repository 및 external dependency 검증
11. 동일 입력 재실행
```

표준 설치 명령은 `eva` CLI로 제한한다.

```bash
sudo eva workspace validate \
  --workspace /home/eva/site-pt-a

sudo eva workspace show \
  --workspace /home/eva/site-pt-a

sudo eva preflight gpu

sudo eva preflight argocd \
  --workspace /home/eva/site-pt-a

sudo eva install . \
  --workspace /home/eva/site-pt-a \
  --yes

sudo eva check --verbose
sudo eva status
```

직접 `ansible-playbook` 또는 `helm`을 실행하는 절차는 정상 Runbook에서 제외한다.

## 10. Fail-closed 조건

Remote install은 다음 조건에서 설치 전에 실패해야 한다.

```text
repository.registry 미지정
repository.registry에 URL scheme 포함
registry가 localhost
Target에서 Harbor 접근 불가
Harbor project 없음
필수 image/tag 누락
Qdrant snapshot OCI artifact 누락
vLLM model manifest 또는 model cache 누락
Runtime payload 누락
package bundle 누락
Remote Target에 AWS source가 선택됨
외부 image reference가 resolved values에 남음
```

다음 fallback은 허용하지 않는다.

```text
Harbor image 누락 시 Docker Hub 사용
model cache 누락 시 Hugging Face 사용
snapshot artifact 누락 시 S3 사용
offline package 누락 시 public apt 사용
```

Remote mode에서는 fallback보다 명확한 사전 실패를 우선한다.

## 11. E2E 완료 기준

### 11.1 Main 준비 검증

```text
[OK] Release 검증
[OK] 필수 image 목록 완성
[OK] 모든 image Main Harbor push
[OK] vLLM model cache 및 manifest 준비
[OK] Qdrant snapshot OCI artifact push
[OK] Runtime payload 준비
[OK] package bundle 준비
```

### 11.2 Target 설치 검증

```text
[OK] Target public outbound 차단
[OK] Main Harbor 접근
[OK] Workspace mode=remote
[OK] internal mode=remote_repository
[OK] Remote Runtime 설치
[OK] GPU preflight
[OK] 전체 component 설치
[OK] eva check --verbose
[OK] eva status
```

### 11.3 공급 경로 검증

```text
[OK] 모든 product container image가 Main Harbor 사용
[OK] 모든 initContainer image가 Main Harbor 사용
[OK] 모든 helper/test image가 Main Harbor 사용
[OK] vLLM model이 prepared local/NFS cache 사용
[OK] Qdrant snapshot이 Harbor OCI artifact 사용
[OK] Target에서 S3 요청 없음
[OK] Target에서 ECR 요청 없음
[OK] Target에서 Docker Hub 요청 없음
[OK] Target에 AWS credential 없음
```

### 11.4 재실행 검증

```text
[OK] 같은 Release/Workspace 재실행 성공
[OK] model 재다운로드 없음
[OK] snapshot 불필요 재복구 없음
[OK] Harbor credential Secret 불필요 변경 없음
[OK] persistent data 초기화 없음
```

## 12. 구현 단계

### Phase R1. 설계 및 현재 계약 고정

```text
remote-mode-implementation-design.md 작성
README에서 Remote runbook 연결
현재 mode mapping contract 확인
현재 Remote 관련 테스트 목록 확인
```

### Phase R2. Repository-derived values 자동화

```text
IAM image rewrite
App image rewrite
Vision image rewrite
Agent image rewrite
모든 helper/init/test image rewrite
Workspace의 image override 제거 가능 상태 확보
```

### Phase R3. Agent Remote source 정렬

```text
vLLM Target S3 의존 제거
prepared model cache materialization
Qdrant Harbor snapshot profile 자동 선택
AWS Secret 참조 제거
Remote workload의 AWS CLI image 제거
```

### Phase R4. Runtime/package 정렬

```text
Remote Runtime offline 공급
Remote package bundle 사용
Target public apt 의존 제거
Remote와 Local의 installation asset consumer 통합
```

### Phase R5. Main preparation 자동화

```text
필수 image 목록 생성
Harbor publish
model 준비
snapshot OCI publish
Runtime/package 준비
release-level preparation manifest 생성
```

### Phase R6. Remote E2E

```text
외부 차단 환경에서 전체 설치
공급 경로 검증
health 검증
재실행 검증
발견된 갭 수정
```

### Phase R7. Runbook 확정

```text
docs/remote-runbook.md 확정
README의 오래된 직접 Ansible 절차 축소
E2E에서 사용한 최소 명령만 문서화
```

### Phase A1. Airgap 전환

Remote E2E 완료 후 준비 자산의 획득 방식만 Bundle import로 바꾼다.

```text
Remote:
  Cloud source -> Main preparation cache

Airgap:
  Verified Bundle -> Local preparation cache
```

Target의 resource materialization과 component 설치 계약은 가능한 한 동일하게 유지한다.

## 13. 고정 결정 사항

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
```

## 14. 다음 분석 및 구현 순서

다음 focused context에 포함할 범위:

```text
Workspace mode mapping
Remote/Airgap mode derivation
Runtime bootstrap
APT/package bundle
IAM/App/Vision image override
Agent/vLLM/Qdrant values generation
Model download/materialization
Qdrant snapshot profile
Harbor publish scripts
관련 contract tests
```

구현은 아래 순서로 나눈다.

```text
1. 설계 문서와 documentation contract
2. repository-derived image rewrite 완전성
3. Qdrant Remote Harbor profile
4. vLLM Remote model materialization
5. Remote Runtime/package 공급
6. Main preparation 명령
7. 전체 E2E
```
