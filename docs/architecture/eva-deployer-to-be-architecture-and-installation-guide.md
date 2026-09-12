# EVA Deployer TO-BE Architecture and Installation Guide

> 아키텍처 구조, 릴리스 빌드, S3 배포, 설치자 입력 및 Values 운영 계약

- 문서 버전: 1.1
- 기준일: 2026-09-12
- 상태: Phase 0 계약 반영, CLI orchestration 구현 진행
- 근거: EVA Deployer 전체 소스 번들, AS-IS 조사 문서, TO-BE 설계 논의

## 1. 문서 목적과 핵심 결론

이 문서는 EVA Deployer의 소스 구조를 단순화하면서도 Cloud, Remote Repository, 완전 Airgap 설치를 동일한 제품 Artifact 기준으로 운영하기 위한 통합 아키텍처를 정의한다.

개발자와 CI는 릴리스 Artifact를 빌드하고 검증하여 S3에 게시한다. 설치자는 GitHub 소스를 직접 빌드하지 않고, S3에 게시된 완성 Artifact와 고객 환경별 입력 파일만 준비한다.

> **핵심 원칙**
>
> `src/`와 `workspace/`는 입력, `tools/eva`는 공식 제어면, `out/`은 생성 결과다. `scripts/`는 bootstrap과 아직 CLI로 이관되지 않은 검증된 보조 자동화만 담당한다.

| 구분 | 책임 | 주 소유자 |
| --- | --- | --- |
| `src/` | Infra 및 Solution의 불변 배포 정의 | 제품 개발자 |
| `workspace/` | 고객 및 사이트별 가변 배포 입력 | 설치자/운영자 |
| `scripts/` | 다운로드, bootstrap, publish 보조 자동화 | 개발자/운영 도구 |
| `prompts/` | 반복 운영 작업을 위한 AI Agent 지침 | 개발자/운영 도구 |
| `tools/eva/` | 공식 EVA Tool CLI 제품 코드 | 도구 개발자 |
| `out/` | 캐시, 렌더링, 상태, 최종 배포물 | 도구/CI |

## 2. 권장 디렉터리 구조

```text
eva-deployer/
├── src/
│   ├── playbook-preflight.yaml
│   ├── playbook-vars.yaml
│   ├── infra/
│   │   ├── playbooks/
│   │   ├── roles/
│   │   │   └── gpu_mig/
│   │   │       └── vars/gpu_mig.yaml
│   │   └── version.yaml
│   └── solution/
│       ├── playbooks/
│       ├── roles/
│       ├── values/
│       └── version.yaml
├── workspace/
│   ├── inventory/
│   ├── site-values/
│   └── credentials/
├── scripts/
│   ├── download/
│   ├── install/
│   ├── publish/
│   └── sync/
├── tools/
│   └── eva/
│       ├── cmd/
│       ├── internal/
│       ├── go.mod
│       └── go.sum
├── out/
│   ├── cache/
│   │   ├── infra/
│   │   └── solution/
│   ├── work/
│   │   ├── config/
│   │   ├── rendered/
│   │   └── logs/
│   ├── state/
│   └── dist/
├── docs/
│   ├── architecture/
│   └── operations/
├── prompts/
│   └── codex/
│       └── infra-ansible.md
├── README.md
└── .gitignore
```

> 위 구조는 책임 경계를 나타낸다. 실제 책임 파일이 생기기 전에는 빈 하위 디렉터리를 미리 만들지 않는다.

### 2.1 `src/`: 불변 제품 배포 소스

`src/`에는 Git으로 관리되고 릴리스에 포함되는 제품 소스만 둔다.

`src/playbook-vars.yaml`과 `src/playbook-preflight.yaml`은 Infra와 Solution playbook이 함께 import하는 경로·version·입력 검증 계약이다. playbook은 현재 각 family의 `playbooks/` 경로에서 이 파일을 상대 import하므로, 해당 파일을 이동하거나 복제하지 않는다.

허용되는 내용:

- Ansible playbook과 role
- Jinja 템플릿
- 제품팀이 관리하는 Helm values 및 values 템플릿
- Infra 및 Solution version 정의

허용하지 않는 내용:

- 고객별 서버 주소와 실제 inventory
- 고객별 values 및 실제 Secret
- 다운로드된 Chart, Model, Image, Deb, Wheel
- 생성된 `eva.yaml`과 렌더링 결과
- 실행 로그와 설치 상태

#### `src/infra/`

```text
src/infra/
├── playbooks/
│   ├── site_infra.yaml
│   ├── site_gpu_mig.yaml
│   └── site_precondition.yaml
├── roles/
│   ├── awscli/
│   ├── base/
│   ├── docker/
│   ├── gpu/
│   ├── gpu_mig/
│   ├── helm/
│   ├── k3s/
│   ├── kubectl/
│   ├── nfs/
│   └── precondition/
└── version.yaml
```

Infra는 OS, Docker, GPU/MIG, Helm, kubectl, k3s, NFS 및 사전 점검을 담당한다. Infra는 EVA App, Agent, Vision, IAM의 Chart나 values를 참조하지 않는다.

#### `src/solution/`

```text
src/solution/
├── playbooks/
│   ├── site_eva.yaml
│   ├── site_eva_app.yaml
│   ├── site_eva_agent.yaml
│   ├── site_eva_vision.yaml
│   ├── site_eva_iam.yaml
│   ├── site_eva_config.yaml
│   └── site_n8n.yaml
├── roles/
│   ├── config/
│   ├── eva_app/
│   ├── eva_agent/
│   ├── eva_vision/
│   ├── eva_iam/
│   └── n8n/
├── values/
│   ├── app-k3s-override.yaml.j2
│   ├── agent-k3s-override.yaml.j2
│   ├── vision-k3s-override.yaml.j2
│   ├── eva-iam-k3s-override.yaml.j2
│   ├── qdrant-k3s-override.yaml.j2
│   └── vllm-k3s-override.yaml.j2
└── version.yaml
```

`src/solution/values/`에는 제품팀이 소유하는 불변 Helm values 또는 `.j2` 템플릿만 둔다. 실제 고객이 작성하는 `app.yaml`, `iam.yaml` 등의 파일은 이 경로에 두지 않는다.

### 2.2 `workspace/`: 설치자 입력

```text
workspace/
├── inventory/
│  ├── inventory.ini
├── site-values/
│  ├── site.yaml
│  ├── app.yaml
│  └── iam.yaml
└── credentials/
  ├── aws_key.ini
```

`workspace/`는 운영자가 작성하는 배포 입력 전용 공간이다.

- `inventory/inventory.ini`: 대상 호스트, 접속 사용자, SSH 방식 등 Ansible inventory 입력
- `site-values/site.yaml`: site ID, repository mode, Harbor endpoint, 설치 component 등 CLI 공통 입력
- `site-values/app.yaml`: EVA App Chart에 적용할 고객 변경분. App 커스텀이 있을 때만 생성
- `site-values/iam.yaml`: EVA IAM Chart에 적용할 고객 변경분. IAM 커스텀이 있을 때만 생성
- `credentials/aws_key.ini`: AWS ECR, S3, release asset 접근이 필요한 설치 단계에서 사용하는 AWS credential 입력

생성된 `eva.yaml`, 최종 values, 로그와 상태는 `workspace/`에 두지 않는다.

EVA CLI의 기본 workspace는 `/etc/eva/sites/<site-id>`다. `eva install --workspace /abs/path`으로 외부 workspace를 명시할 수 있으며, 같은 실행 안에서 workspace를 섞지 않는다. CLI는 선택한 workspace를 `eva_workspace_root`로 Ansible에 전달한다.

직접 Ansible 실행은 CLI 도입 전 호환 경로로 유지한다. 이 경로의 선택 우선순위는 `-e eva_workspace_root=/abs/path` → `EVA_WORKSPACE_ROOT=/abs/path` → `<repo>/workspace`다. repo-local `workspace/`는 sample과 개발용 입력 공간이며, system-wide CLI의 운영 기본 경로가 아니다.

현재 구현 정렬 상태:

- `src/infra/roles/awscli/tasks/main.yaml`은 기본값으로 `<selected-workspace>/credentials/aws_key.ini`를 읽는다.
- `aws_key_file` extra var로 control node의 절대 경로를 명시적 override할 수 있다.
- 기존 `<repo>/aws_key.ini` 직접 참조는 제거되었다.

```text
/etc/eva/sites/
└── customer-a/
    ├── inventory/
    ├── site-values/
    └── credentials/
```

`site-values/site.yaml`의 최소 schema는 다음과 같다. `mode=remote`와 `mode=local`은 `registry`를 요구하고, `project` 기본값은 `eva`다. Secret 원문은 이 파일에 두지 않는다.

```yaml
site:
  id: customer-a
repository:
  mode: cloud # cloud | remote | local
  project: eva
components:
  infra: true
  iam: true
  agent: true
  vision: true
  app: true
  n8n: false
```

현재 repository의 직접 Ansible playbook은 이 파일을 자동 로드하지 않는다. CLI orchestration이 이 schema를 검증하고 기존 Ansible extra vars로 변환하는 것이 다음 구현 단계다.

### 2.3 `scripts/`, `prompts/`, `tools/eva/`의 경계

#### `scripts/`

직접 실행 가능한 보조 Shell 자동화다.

```text
scripts/
├── download/    # 인터넷 환경의 자산 다운로드
├── install/     # 대상 서버 bootstrap
├── publish/     # Harbor image 및 OCI Artifact 게시
└── sync/        # 개발/운영 파일 동기화 보조 자동화
```

주요 대상:

- Offline asset, image, model, Qdrant snapshot 다운로드
- Python/Ansible wheel 및 Deb 준비
- Docker, Ansible, Harbor bootstrap
- Image 및 Qdrant snapshot의 Harbor publish

#### `prompts/`

반복적인 개발·운영 작업에서 AI Agent가 따라야 할 버전 관리된 지침이다.

```text
prompts/
└── codex/
    └── infra-ansible.md    # Infra Ansible 실행·검증·이력 기록 지침
```

Prompt는 실행 결과나 고객별 입력을 저장하지 않으며, Secret·credential·inventory 원문을 포함하지 않는다.

#### `tools/eva/`

버전이 부여되는 정식 EVA Tool CLI 제품 코드다.

```text
tools/eva/
├── cmd/
├── internal/
├── go.mod
└── go.sum
```

장기적으로 설치자에게 노출되는 공식 진입점은 EVA CLI 하나로 제한한다. 초기에는 CLI가 기존 검증된 Shell과 Ansible을 호출할 수 있다. CLI로 완전히 이관된 스크립트는 삭제하고, CLI 실행 이전에 필요한 bootstrap helper만 남긴다.

CLI와 Runtime은 user home에 의존하지 않는 system-wide 경로를 사용한다.

```text
/usr/local/bin/eva
/opt/eva/
├── tool/
├── runtime/
└── releases/
/var/lib/eva/
├── artifacts/
├── operations/
└── state/
/var/log/eva/
└── operations/
```

`eva-operators` 그룹은 `/etc/eva/sites/`의 site 입력을 읽고 `/var/lib/eva/operations/`, `/var/log/eva/operations/`의 일반 operation 기록을 생성할 수 있다. Secret 및 credential 파일은 site owner 또는 root만 읽을 수 있게 `0600`으로 관리한다.

### 2.4 `out/`: 생성 결과와 보존 정책

| 경로 | 내용 | 보존 정책 |
| --- | --- | --- |
| `out/cache/` | Chart, Model, Deb, Wheel, Image 등 외부 자산 | 삭제 후 재다운로드 가능 |
| `out/work/` | 동적 config, 렌더링 values, 빌드 및 실행 로그 | 원칙적으로 재생성 가능 |
| `out/state/` | Operation, Audit, Rollback, Resume 근거 | 운영 중 보존 필요 |
| `out/dist/` | S3 게시 및 USB 반입용 최종 Artifact | 게시본과 digest 일치 필요 |

`out/work/`의 생성 경로는 site namespace를 포함한다. `EVA_SITE_ID`는 운영 실행 전에 반드시 지정하며, 허용 값은 영문자 또는 숫자로 시작하는 영문자, 숫자, 점, 밑줄, 하이픈 조합이다.

| 경로 | 용도 |
| --- | --- |
| `out/work/config/<site>/<host>/` | precondition, `eva.yaml`, Secret, IAM handoff 등 host별 생성 config |
| `out/work/config/<site>/harbor-endpoint.yaml` | site별 portable Harbor endpoint metadata |
| `out/work/rendered/<site>/<host>/<component>/` | component별 최종 Helm values와 rendering 결과 |
| `out/work/logs/<site>/<operation-or-component>/` | 실행 및 검증 로그 |

`EVA_REPO_ROOT`는 playbook이 저장소 기본 위치가 아닌 경로에서 실행될 때 사용할 명시적 저장소 root override다. 지정하지 않으면 playbook 경로에서 repository root를 계산한다.

## 3. 전체 아키텍처와 책임 분리

```text
개발자 / CI
GitHub Tag → Build → out/dist → 검증 → S3 immutable release

설치자
S3 Artifact → checksum 검증 → workspace 작성 → inspect/plan → apply

Airgap
S3 Airgap Bundle → USB 반입 → 대상 서버 검증 → bootstrap → Harbor seed → install
```

Git tag는 소스 기준점이다. 실제 설치 기준점은 S3의 `release.yaml`과 Artifact digest다. 파일명이나 `v3.2.0` 문자열만으로 동일성을 판단하지 않는다.

### 3.1 Infra와 Solution 의존 규칙

- Solution은 지원하는 Infra Artifact version 또는 digest 범위를 선언할 수 있다.
- Infra는 Solution role, values, Chart 버전을 참조하지 않는다.
- Cloud와 Airgap은 동일한 Infra/Solution base Artifact digest를 사용한다.
- Airgap은 동일 base Artifact에 Offline 자산을 추가하여 전달하는 방식이다.
- 초기에는 `src/common/`을 만들지 않고, 실제로 안정된 공통 계약만 추후 추출한다.

## 4. 릴리스와 S3 Artifact 빌드

EVA App, Agent, Vision, IAM을 3.2.0 조합으로 검증하고 EVA Deployer `v3.2.0` 태그를 생성하면, CI가 소스와 고정된 version 정의를 이용해 완성 Artifact를 생성하고 S3에 게시한다.

사용자가 Git 소스를 받아 현장에서 build하는 흐름은 기본 운영 경로가 아니다.

```text
EVA App/Agent/Vision/IAM Release 확정
              ↓
src/infra/version.yaml + src/solution/version.yaml 갱신
              ↓
호환성, Cloud, Airgap 검증
              ↓
GitHub eva-deployer v3.2.0 Tag
              ↓
CI: EVA Tool build + 자산 수집 + Artifact 패키징 + 검증
              ↓
out/dist 생성
              ↓
S3 releases/v3.2.0/ immutable publish
```

### 4.1 `version.yaml`의 역할

상위 release version이 3.2.0이어도 App 실행 버전과 Chart 버전, Agent 종속 Chart, Qdrant, n8n 등은 각각 별도로 고정한다.

```yaml
version: 3.2.0
components:
  evaApp:
    deployVersion: 3.2.0
    chartVersion: 3.2.0
  evaAgent:
    deployVersion: 3.2.0
    chartVersion: 3.2.0
  evaVision:
    deployVersion: 3.2.0
    chartVersion: 3.2.0
  evaIam:
    deployVersion: 3.2.0
    chartVersion: 3.2.0
compatibility:
  infra: ">=3.2.0 <4.0.0"
```

재현성을 위해 build 시점에 `latest` 또는 `stable`을 다시 해석하지 않는다. 가능한 한 정확한 version, URL, checksum 및 image digest를 release 정의에 고정한다.

### 4.2 CI Build 단계

1. Tag와 `version.yaml` 정합성 검증
2. EVA Tool 정적 바이너리 빌드
3. Infra 및 Solution 외부 자산 다운로드
4. URL, checksum 및 image digest 검증
5. Infra/Solution base Artifact staging
6. Airgap Offline payload 또는 Bundle 조립
7. 재현 가능한 `.tar.gz` 생성
8. `release.yaml`과 `checksums.sha256` 생성
9. Artifact 추출 및 install dry-run 검증
10. S3 immutable 경로 게시

### 4.3 `out/dist/` 결과

```text
out/dist/
├── eva-tool_v3.2.0_linux_amd64.tar.gz   # 설치 실행용 EVA CLI
├── eva-infra_v3.2.0.tar.gz              # Infra 설치 정의
├── eva-solution_v3.2.0.tar.gz           # Solution 설치 정의
├── eva-offline_v3.2.0_ubuntu24.04_amd64.tar.gz  # Airgap Runtime, 패키지, 이미지, 모델
├── eva-airgap-bundle_v3.2.0_ubuntu24.04_amd64.tar.gz
├── release.yaml                                      # Artifact 조합과 digest
└── checksums.sha256                                  # 다운로드 및 반입 무결성 검증
```

Airgap 설치자에게는 `eva-airgap-bundle_v3.2.0_ubuntu24.04_amd64.tar.gz` 한 개와 외부 checksum 파일만 제공할 수 있다. Bundle 내부 `artifacts/`에는 이미 생성된 Tool, Infra, Solution, Offline archive를 byte-identical하게 넣고, 최상단에는 `release.yaml`, `checksums.sha256`, README만 둔다.

`eva release import-airgap --bundle <path>`는 Bundle 전체 SHA-256별 cache (`/var/lib/eva/artifacts/releases/<bundle-sha256>/`)에 nested archive를 추출한다. `release.yaml`의 각 artifact는 `artifacts/<filename>`을 가리켜야 하며, `checksums.sha256`의 같은 항목과 checksum이 일치해야 한다. Bundle 최상단에는 `release.yaml`, `checksums.sha256`, 선택 `README.md`, 그리고 metadata에 선언된 `artifacts/<filename>`만 허용한다. `eva install <bundle-path>`는 이 import를 자동 수행한 후 같은 local Release install 흐름으로 진행한다.

`release.yaml`은 build가 생성하는 최소 metadata이며, CLI가 local Release directory를 검증할 때 사용한다. 각 artifact의 `file`은 Release directory 기준 상대 경로이고 `sha256`은 해당 파일의 SHA-256이다.

```yaml
version: 3.2.0
platform:
  os: linux
  arch: amd64
artifacts:
  - name: eva-tool
    file: eva-tool_v3.2.0_linux_amd64.tar.gz
    sha256: <SHA256>
  - name: eva-infra
    file: eva-infra_v3.2.0.tar.gz
    sha256: <SHA256>
  - name: eva-solution
    file: eva-solution_v3.2.0.tar.gz
    sha256: <SHA256>
```

`eva-tool`, `eva-infra`, `eva-solution`은 모든 local Release에 필수다. CLI는 실행 host와 platform이 다르거나 metadata 밖으로 나가는 artifact 경로, 누락된 파일, checksum 불일치를 거부한다. S3 tag와 Airgap Bundle input resolver는 다음 단계에서 추가한다.

`eva release prepare --release <release-directory>`는 검증된 `eva-infra`와 `eva-solution` archive를 `/opt/eva/releases/<version>/`에 추출해 Apply 가능한 local Release source를 준비한다. archive는 top-level wrapper 없이 다음 경로를 직접 포함해야 한다.

```text
eva-infra:    ansible.cfg, src/playbook-preflight.yaml, src/playbook-vars.yaml, src/infra/
eva-solution: src/solution/
```

tar의 절대 경로, 상위 경로 탈출, symlink/hardlink 및 특수 파일은 거부한다. 준비된 Release는 marker와 `release.yaml`을 보존하며, 동일 version의 metadata가 다르면 교체하지 않는다. 준비가 끝난 경로는 `eva plan --release /opt/eva/releases/<version>`에 입력한다.

### 4.4 S3 권장 배치

```text
s3://<release-bucket>/eva-deployer/
├── releases/
│   └── v3.2.0/
│       ├── eva-tool_v3.2.0_linux_amd64.tar.gz
│       ├── eva-infra_v3.2.0.tar.gz
│       ├── eva-solution_v3.2.0.tar.gz
│       ├── eva-offline_v3.2.0_ubuntu24.04_amd64.tar.gz
│       ├── eva-airgap-bundle_v3.2.0_ubuntu24.04_amd64.tar.gz
│       ├── checksums.sha256
│       └── release.yaml
└── channels/
    ├── dev.yaml
    ├── candidate.yaml
    └── prod.yaml
```

운영 규칙:

- `releases/v3.2.0/` 경로는 immutable하게 운영한다.
- 동일 key에 다른 bytes를 재업로드하지 않는다.
- 동일 version과 동일 digest의 byte-identical 업로드만 허용할 수 있다.
- dev/candidate/prod 승격은 Artifact 재작성 대신 channel pointer 갱신으로 수행한다.
- `latest`는 조회 편의용이며 실제 설치 identity로 사용하지 않는다.

## 5. 설치자 준비물

### 5.1 Cloud 설치

S3에서 받는 파일:

```text
release.yaml
checksums.sha256
eva-tool_v3.2.0_linux_amd64.tar.gz
eva-infra_v3.2.0.tar.gz
eva-solution_v3.2.0.tar.gz
```

설치자가 작성하는 파일:

```text
workspace/inventory/inventory.ini
workspace/site-values/site.yaml
workspace/site-values/app.yaml  # 필요한 경우
workspace/site-values/iam.yaml  # 필요한 경우
workspace/credentials/aws_key.ini  # AWS 직접 접근이 필요한 경우
```

현재 구현 기준으로 선택된 workspace의 `credentials/aws_key.ini`가 Cloud 설치 입력에 포함된다. `site_eva_config.yaml`의 `awscli` role은 이 경로를 기본값으로 사용하고, 필요 시 `aws_key_file` override를 사용해 target user의 `~/.aws`를 구성한다.

### 5.2 Remote Repository 설치

Cloud 공통 파일에 Main Harbor를 채우기 위한 Offline payload가 추가된다.

```text
eva-offline_v3.2.0_ubuntu24.04_amd64.tar.gz
```

설치자는 Main Harbor endpoint, project 및 인증정보를 준비한다. AWS credential은 인터넷 가능한 Main 서버 또는 준비 서버에서 download/publish 스크립트를 실행할 때만 필요할 수 있으며, 대상 EVA 서버 입력으로 배포하는 것이 기본 계약은 아니다.

### 5.3 완전 Airgap 설치

가장 단순한 권장 제공 형태:

```text
eva-airgap-bundle_v3.2.0_ubuntu24.04_amd64.tar.gz
eva-airgap-bundle_v3.2.0_ubuntu24.04_amd64.tar.gz.sha256
```

완전 Airgap 전달물에는 실제 `credentials/aws_key.ini`나 AWS access key material이 포함되면 안 된다. AWS credential은 bundle 생성 전 인터넷 가능한 준비 서버에서만 사용하고, 반입 artifact에는 남기지 않는다.

Bundle 내부:

```text
eva-airgap-bundle_v3.2.0/
├── artifacts/
│   ├── eva-tool_v3.2.0_linux_amd64.tar.gz
│   ├── eva-infra_v3.2.0.tar.gz
│   ├── eva-solution_v3.2.0.tar.gz
│   └── eva-offline_v3.2.0_ubuntu24.04_amd64.tar.gz
├── release.yaml
├── checksums.sha256
└── README.md
```

### 5.4 설치자가 작성하는 파일

#### `workspace/inventory/inventory.ini`

```ini
[eva]
site-a-eva-node-01 ansible_host=<TARGET_IP_OR_DNS> ansible_user=<SSH_USER> ansible_ssh_private_key_file=<PATH_TO_SSH_KEY>
```

단일 서버 로컬 설치:

```ini
[local]
site-a-localhost ansible_connection=local
```

inventory hostname은 IP가 아니라 사이트를 식별하는 고유한 이름을 사용하고, 실제 접속 주소는 `ansible_host`에 넣는다. 기본 sample은 `workspace/inventory/inventory.ini.sample`이다.

#### `workspace/site-values/app.yaml`

```yaml
site-a-eva-node-01:
  app:
    browserTitleName: EVA Customer A
    license:
      activation_mode: offline
      product_code: eva-prod
      api_key: <LICENSE_API_KEY>
      shared_key: <LICENSE_SHARED_KEY>
```

#### `workspace/site-values/iam.yaml`

```yaml
ingress:
  ingressClassName: traefik
config:
  host: eva.customer.example
```

#### `workspace/credentials/aws_key.ini`

```ini
aws_access_key_id = <YOUR_ACCESS_KEY>
aws_secret_access_key = <YOUR_SECRET_KEY>
region = ap-northeast-2
```

`region`은 권장 입력값이며, 현재 구현 기준으로는 누락 시 `ap-northeast-2`가 기본값으로 사용된다. 문서와 sample에는 실제 Key 값을 넣지 않는다.

### 5.5 별도 보안 준비물

- SSH private key. Inventory에는 key 본문이 아니라 경로만 기록한다.
- TLS certificate와 private key
- App license API key와 shared key
- Harbor 또는 외부 Registry 인증정보
- IAM/SSO 관련 Secret
- AWS access key / secret access key

Secret은 YAML에 평문으로 직접 기록하기보다 Environment 또는 Secret reference를 사용한다.

### 5.6 AWS Credential 입력 계약

AWS credential은 설치 단계 전체에서 필요한 위치가 서로 다르므로, control node profile과 target server credential 설치를 같은 입력으로 취급하지 않는다.

AS-IS:

- 현재 `awscli` role은 `src/solution/playbooks/site_eva_config.yaml`에서만 호출된다.
- 현재 role은 `repository_mode=cloud_repository` 이고 `airgap_mode=false` 일 때만 실행된다.
- 현재 role은 control node의 `<selected-workspace>/credentials/aws_key.ini`를 기본값으로 읽는다.
- 현재 role은 `aws_key_file` extra var로 절대 경로 override를 받을 수 있다.
- 현재 role은 `ansible_user | default(ansible_ssh_user)` 사용자의 home 아래 `~/.aws`를 target server에 구성한다.
- 현재 role의 필수 필드는 `aws_access_key_id`, `aws_secret_access_key`이고 `region`은 없으면 `ap-northeast-2`를 사용한다.
- 현재 role의 credential 처리 block에는 `no_log: true`가 적용된다.
- 현재 README의 `aws configure set ... --profile default`는 준비 서버 로컬 profile 설정용이며, role 입력 파일을 대체하지 않는다.

TO-BE:

- 기본 경로는 `<selected-workspace>/credentials/aws_key.ini`로 단순화한다.
- 명시적 override로 `-e aws_key_file=/absolute/path/aws_key.ini`를 허용한다.
- 선택 우선순위는 `aws_key_file` → `<selected-workspace>/credentials/aws_key.ini`로 제한한다.
- 실제 Secret 파일은 Git source, release artifact, Airgap Bundle에 포함하지 않는다.
- AWS가 필요하지 않은 target에는 credential을 설치하지 않는다.

Repository mode별 목표 운영 원칙:

- `cloud_repository`: target이 AWS ECR, S3를 직접 사용해야 하는 경우에만 target credential 설치를 허용한다.
- `remote_repository`: 인터넷 가능한 Main 서버나 준비 서버의 profile이 필요할 수 있으나, Main Harbor만 사용하는 target에는 target credential을 배포하지 않는다.
- `local_repository`: bundle 생성 전 준비 서버에서만 AWS를 사용할 수 있으며, 완전 Airgap target에는 credential을 반입하지 않는다.

후속 구현 TODO:

- `remote_repository`/`local_repository` target에 불필요한 AWS credential이 배포되지 않도록 조건 정리
- 파일 존재 여부, 필수 필드, 권한 검증 추가
- targeted regression과 문서/구현 parity 검증 추가
- 기존 루트 `aws_key.ini` fallback 유지 여부 별도 결정

## 6. 설치 과정과 생성 YAML의 생명주기

설치자가 Deployer 생성 YAML을 먼저 보고 모든 입력을 작성하는 구조는 아니다. 설치자는 자신이 알고 있는 최소 입력을 먼저 작성하고, Tool이 대상 서버를 조사하여 생성 YAML과 설치 Plan을 만든 뒤 설치자가 검토한다.

```text
1. Artifact 검증
2. inventory 와 site-values 작성
3. Chart 기본 values 조회
4. 필요 시 app.yaml, iam.yaml 변경분 작성
5. eva inspect: 대상 서버 조사
6. precondition.yaml, eva.yaml 생성
7. eva plan: 입력 병합 및 최종 values/plan 생성
8. 설치자 검토
9. 문제가 있으면 workspace 입력 수정 후 plan 재생성
10. eva apply: 실제 설치
11. state/audit 기록
```

### 6.1 Tool이 생성하는 YAML

| 생성 파일 | 예상 위치 | 용도 및 수정 정책 |
| --- | --- | --- |
| `precondition.yaml` | `out/work/config/<site>/<host>/` | OS, 네트워크, GPU, 디스크 사전 조사. 검토만 수행 |
| `eva.yaml` | `out/work/config/<site>/<host>/` | GPU, MIG, NFS 등 파생 설정. 직접 수정 금지 |
| `secret.yaml`, `eva-iam.yaml` | `out/work/config/<site>/<host>/` | 민감한 cluster Secret 및 IAM-to-App handoff. 권한 `0600`으로 생성 |
| `harbor-endpoint.yaml` | `out/work/config/<site>/` | repository registry, project 등 site 공통 Harbor endpoint metadata |
| `chart-defaults.yaml` | `out/work/rendered/<site>/<host>/<component>/` | 선택한 Chart 기본값 전체. 참고용 |
| `resolved-values.yaml` | `out/work/rendered/<site>/<host>/<component>/` | Helm 최종 적용값. 검토만 수행 |
| `plan.yaml` | `out/work/plan/<operation-id>/` | Release, 대상, 설치 순서 및 변경 계획 |
| `operation.yaml`, `audit.yaml` | `out/state/<site>/<operation-id>/` | 감사, 재실행, rollback/resume 근거 |

> **수정 원칙**
>
> 생성 YAML에서 문제가 발견되면 생성 파일을 직접 수정하지 않는다. 선택된 workspace의 `site-values/` 원본 입력을 수정하고 `inspect` 또는 `plan`을 다시 실행한다.

### 6.2 자동 감지와 사용자 결정의 구분

설치자가 반드시 결정하는 값:

- Repository mode, registry, project
- 설치할 컴포넌트
- 서비스 domain과 TLS 경로
- License와 Secret reference
- IAM path, Qdrant snapshot 정책

Tool이 자동 감지하는 값:

- OS와 architecture
- GPU 수량과 종류
- MIG 상태
- 디스크 용량
- 외부 및 Harbor 연결성
- Docker, k3s, Ansible 현재 상태

자동 감지하지만 사용자가 override할 수 있는 값:

- vLLM profile
- NFS share path
- Harbor Pod 접근 endpoint
- timeout과 concurrency

## 7. EVA App Values 운영 계약

EVA App은 Chart 기본값을 일부 변경하거나 기본 `values.yaml`에 없는 지원 키를 추가하는 경우가 많다. 따라서 전체 기본값, 고객 변경분, 최종 병합 결과를 서로 다른 파일로 관리한다.

```text
Chart 기본값 전체
  + 제품 고정 values 템플릿
  + 자동 생성 runtime override
  + 고객 app.yaml 변경분
  + 설치 중 허용된 handoff
  = resolved-values.yaml
```

### 7.1 세 가지 핵심 파일

| 구분 | 위치 | 소유자 | 의미 |
| --- | --- | --- | --- |
| 기본값 전체 | `out/work/rendered/<site>/<host>/<component>/chart-defaults.yaml` | Tool | 선택한 Chart의 `helm show values` 결과 |
| 고객 변경분 | `workspace/site-values/app.yaml` | 설치자 | 기본값과 다른 값 또는 추가 키만 |
| 최종 전체값 | `out/work/rendered/<site>/<host>/<component>/resolved-values.yaml` | Tool | 모든 레이어의 병합 결과 |

### 7.2 설치자가 App 값을 작성하는 순서

1. Solution Artifact와 Chart identity 검증
2. EVA Tool로 App Chart 기본 values 추출
3. `chart-defaults.yaml` 확인
4. `workspace/site-values/app.yaml`에 변경분만 작성
5. `inspect`로 대상 서버 동적 정보 생성
6. `plan`으로 `resolved-values.yaml`과 diff 생성
7. 최종값 검토 후 필요하면 `app.yaml` 보완
8. Plan 재생성 후 `apply`

### 7.3 고객 `app.yaml` 예시

```yaml
app:
  browserTitleName: "EVA Customer A"
  license:
    activation_mode: offline
    product_code: eva-prod
    api_key: "${EVA_LICENSE_API_KEY}"
    shared_key: "${EVA_LICENSE_SHARED_KEY}"
  pipeline:
    streamer:
      dispatcherFaceAnonymizerEnabled: true
  backendSecurePort: 443
  sso:
    realm: eva-iam
    clientId: eva-app
```

변경하지 않는 기본값은 복사하지 않는다. 기본값 전체를 고객 파일로 복사하면 새로운 Chart 버전의 개선된 기본값을 가리고, 실제 변경 의도를 확인하기 어려워진다.

### 7.4 기본값에 없는 키 추가

Chart 기본 `values.yaml`에는 없지만 Chart template 또는 `values.schema.json`이 지원하는 키라면 고객 `app.yaml`에 추가할 수 있다.

```yaml
app:
  backendSecurePort: 443
```

검증 단계:

1. YAML 문법 검증
2. 변경 금지 경로 검증
3. Chart schema 검증
4. 제품 allowlist 또는 Chart 계약 검증
5. `helm template` 실행
6. 미인식 키 경고 또는 strict mode 오류

Chart template이 사용하지 않는 키는 Helm이 조용히 무시할 수 있으므로 단순 YAML 병합 성공만으로 정상 처리하지 않는다.

### 7.5 Values 우선순위

낮은 우선순위부터 높은 우선순위 순서:

1. Chart 기본 values
2. 제품 공통 k3s/repository values
3. 자동 생성 runtime values
4. 고객 `workspace/site-values/app.yaml`
5. Base digest에 결합된 Patch overlay
6. IAM 결과 등 설치 중 생성된 허용된 runtime handoff

일반 설치 흐름은 Release가 고정한 Chart와 image를 사용한다. 다만 현장 복구나 검증을 위해 CLI의 명시적 `--chart`, `--values`, `--set` override는 허용한다. Release 기본값과 다른 Chart 또는 image 관련 값은 차단하지 않고 `[WARN] field override detected`로 Plan과 operation 결과에 기록한다. Artifact 내부 plugin binary 경로와 release metadata 자체는 override 대상이 아니다.

### 7.6 IAM handoff

IAM 배포 후 생성되는 SSO Base URL이나 Admin Client Secret을 설치자가 다시 `app.yaml`에 복사하지 않는다.

```text
IAM 설치
  → 안전한 handoff output 생성
  → EVA Tool이 App runtime overlay 생성
  → 고객 app.yaml과 병합
  → App 설치
```

Secret 포함 handoff는 권한 `0600`, 로그 redaction, 보존 가능한 state 경로를 사용한다.

## 8. 환경별 설치 과정

### 8.1 Cloud

```text
S3에서 Tool + Infra + Solution + manifest/checksum 다운로드
→ Artifact 검증
→ workspace 작성
→ Chart defaults 확인 및 App override 작성
→ inspect / plan
→ Infra 설치
→ IAM 설치(사용 시)
→ Solution config 생성
→ Agent → Vision → App 배포
→ audit/state 기록
```

### 8.2 Remote Repository

```text
공식 Artifact + Offline payload 다운로드
→ Main Harbor에 image 및 snapshot seed
→ 대상 서버의 Main Harbor 접근 검증
→ workspace에 repository.registry 지정
→ inspect / plan / apply
→ audit/state 기록
```

### 8.3 완전 Airgap

```text
준비 측:
Airgap Bundle 다운로드
→ 외부 checksum 검증
→ USB 복사

대상 측:
압축 해제
→ 내부 checksum 검증
→ Docker/Python/Ansible/Harbor bootstrap
→ Image Harbor push
→ Qdrant snapshot OCI Artifact push
→ Infra 설치
→ IAM 설치
→ EVA config 생성
→ Agent → Vision → App 배포
→ 결과 검증 및 audit/state 기록
```

반드시 보존할 선행조건:

- Qdrant Harbor snapshot은 Harbor 설치와 일반 image seed 이후, Agent/Qdrant 배포 전에 별도로 seed한다.
- Agent와 Vision은 생성된 `eva.yaml`이 선행되어야 한다.
- App이 IAM을 사용하면 IAM handoff가 App 배포보다 먼저 완료되어야 한다.
- 현재 aggregate Solution 순서는 Agent → Vision → App이다. IAM은 별도 선행 단계로 취급한다.

## 9. 설치자용 CLI 계약

설치자에게 노출되는 공식 진입점은 EVA Tool 하나다. 대화형과 비대화형은 같은 use case를 사용하며, 일반 설치는 artifact metadata나 playbook 경로를 직접 지정하지 않는다.

```bash
eva install
eva install 3.2.0
eva install ./eva-airgap-bundle_v3.2.0_ubuntu24.04_amd64.tar.gz
eva install app --chart ./eva-app-fix.tgz --values ./app-fix.yaml --set replicaCount=2

eva plan
eva apply
eva status
eva verify
eva shell
eva exec ansible-playbook --version
```

| 명령 | 시스템 변경 | 기본 동작 |
| --- | --- | --- |
| `install [release-or-component]` | 있음 | release/workspace를 자동 탐지하고 plan 요약 후 apply |
| `plan [release-or-component]` | 없음 | 최신 입력으로 Plan과 effective values 생성 |
| `apply [operation-id]` | 있음 | 최신 Plan 또는 지정 Plan 적용 |
| `status [operation-id]` | 없음 | 최신 또는 지정 operation 상태 출력 |
| `verify [release]` | 없음 | `release.yaml`, artifact, checksum 검증 |
| `shell` | 없음 | 관리 Runtime PATH와 선택된 Release/Workspace 환경으로 shell 시작 |
| `exec <command> [args...]` | 명령에 따름 | 관리 Runtime command를 그대로 실행 |

Component alias는 `infra`, `iam`, `agent`, `vision`, `app`, `n8n`, `all`이다. CLI의 `cloud`, `remote`, `local`은 각각 Ansible의 `cloud_repository`, `remote_repository`, `local_repository`로 변환한다.

현재 구현된 CLI 기반은 `eva workspace validate|show|ansible-vars|env`, `eva release validate|show|prepare`, `eva install`, `eva plan`, `eva apply`, `eva status`, `eva runtime install|validate|show`, `eva exec`다. Workspace 명령은 `site-values/site.yaml`을 검증하고 system-wide 또는 `--workspace` 입력을 기존 Ansible extra vars와 Harbor/Shell 환경변수로 변환한다. Release 명령은 local Release directory의 `release.yaml`, platform, artifact checksum을 검증한다.

`eva plan --workspace <path> --release <path> [--output <path> | --save]`은 선택 component와 기존 playbook 순서, Ansible extra vars, 환경변수를 YAML로 생성한다. `agent` 또는 `vision`을 선택하면 `site_eva_config.yaml`을 자동 선행 단계로 넣는다. stdout 출력이 기본이며 `--output`을 지정한 plan 파일은 `0600` 권한으로 생성한다. `--save`는 `/var/lib/eva/operations/<operation-id>/`에 `planned` operation record와 Plan을 `0600` 권한으로 저장한다. 개발과 테스트에서는 `--state-root <path>`로 해당 기본 경로를 바꿀 수 있고, `eva status [operation-id]`는 최신 또는 지정 record를 읽는다.

`eva apply [operation-id]`는 최신 또는 지정 `planned` operation만 실행한다. TTY에서는 site와 operation ID를 표시하고 확인을 받고, 비대화형 실행은 `--yes`가 필수다. Runtime의 `ansible-playbook` 절대 경로만 사용하며, workspace inventory와 Plan에 기록된 allowlist playbook을 검증한 뒤 순차 실행한다. 첫 실패 뒤의 단계는 실행하지 않으며 `result.yaml`, `/var/log/eva/operations/<operation-id>/ansible.log`, Ansible internal log 및 terminal status를 갱신한다. Apply는 `eva release prepare`가 생성한 `ansible.cfg`와 `src/`를 가진 local Release source를 사용한다. S3 resolver, Helm effective values와 override 적용은 다음 단계다.

`eva install [release-path] --workspace <path>`는 local Release source를 검증하고, 필요하면 `release prepare`, Workspace 검증, Plan 요약, 확인, operation 생성, Apply를 차례로 수행하는 편의 명령이다. 이미 준비된 `/opt/eva/releases/<version>`을 입력하면 재추출하지 않는다. Airgap Bundle 경로는 nested artifact cache import를 먼저 수행한다. TTY가 아닌 자동화에서는 `--yes`가 필수이며, 사용자가 취소하거나 Runtime/Workspace 검증에 실패하면 Release prepare와 operation 생성은 실행하지 않는다. Release tag, Component shortcut과 Chart/Values override는 아직 지원하지 않는다.

EVA managed Runtime은 `/opt/eva/runtime/runtime.yaml` descriptor로 version과 도구 경로를 고정한다. `ansible-playbook`, `helm`, `kubectl`, `kustomize`, `oras`는 모두 Runtime root 내부의 실행 가능한 regular file이어야 하며, descriptor의 절대 경로, 상위 경로 탈출 및 Runtime 밖 symlink는 거부한다. `eva exec`는 이 allowlist의 절대 경로만 실행하므로 시스템 PATH의 동명 도구를 사용하지 않는다. `eva runtime install --source <validated-runtime-payload>`는 source를 먼저 검증하고 sibling staging directory에 완전 복사·재검증한 뒤 `/opt/eva/runtime`을 교체한다. 새 Runtime publish 실패 시 이전 Runtime은 복원하며, source payload 검증 실패는 기존 Runtime을 변경하지 않는다. Online/Offline payload의 artifact 추출과 `eva shell`은 다음 단계다.

TTY에서는 Plan 요약 뒤 실행 승인을 묻는다. 비대화형 실행은 `--yes`를 명시해야 하며, TTY가 아닌 환경에서는 질문하지 않고 `--yes` 누락을 오류로 처리한다. privilege escalation은 필요한 Runtime bootstrap 및 Ansible 단계에서만 CLI가 요청하며, CLI 전체를 항상 root로 실행하지 않는다.

Remote Runtime은 online bootstrap을 먼저 시도한다. online bootstrap이 실패했거나 Offline payload가 명시된 경우에만 검증된 Offline payload로 fallback한다. 현재 `eva exec`는 검증된 Runtime descriptor의 allowlist 도구만 직접 실행하며 Raw 실행의 operation log 기록은 아직 추가하지 않는다. `eva shell`과 함께 구현할 후속 단계에서는 실행 사용자, 명령 시작/종료 시각, 종료 코드를 operation log에 남기되 argument 값과 stdout/stderr는 1차에서 수집하지 않는다.

## 10. 기존 README 설치 순서와 마이그레이션

새 구조에서도 현재 README의 핵심 설치 순서는 유지할 수 있다. 그러나 파일만 이동하면 기존 명령은 깨진다.

주요 영향:

- 루트 playbook 경로가 `src/*/playbooks/`로 이동
- Infra와 Solution role 탐색 경로 분리
- `playbook_dir` 기준의 암묵적 `../../..` 계산을 공통 root 계약으로 치환
- 버전 카탈로그를 `src/infra/version.yaml`, `src/solution/version.yaml`으로 분리
- AWS credential 입력 계약을 workspace 선택 계약과 `credentials/aws_key.ini` 경로로 정렬
- 기존 `install/` 자산 경로를 `out/cache/` 또는 Artifact 내부 경로로 변경
- 생성 config의 producer와 consumer 경로 정렬
- Airgap 전달물을 `out/dist/`에서 완결된 Bundle로 조립

### 10.1 안전한 마이그레이션 순서

1. 전체 소스 기준 파일 분류와 기존 실행 parity를 고정한다.
2. 새 `src/`, `scripts/`, `out/` 경로에 복제하되 기존 루트 경로는 유지한다.
3. 영역별 Ansible `roles_path`를 명시한다.
4. `playbook_dir` 기반 version/config 참조를 명시적 root 변수로 교체한다.
5. 다운로드 결과를 `out/cache/`로 전환한다.
6. 고객 values를 `workspace/site-values/`로 전환한다.
7. AWS credential의 mode별 설치 조건, 존재 여부, 권한 검증을 보강한다.
8. 생성 config와 rendered values를 `out/work/`로 전환한다.
9. Airgap delivery bundle을 `out/dist/`에서 조립한다.
10. Cloud/Remote/Airgap parity 후 README 기본 명령을 EVA CLI로 전환한다.
11. 모든 회귀 검증 이후 기존 루트 경로와 `install/` 혼합 구조를 제거한다.

### 10.2 반드시 검증할 회귀 범위

- Infra-only 및 GPU/MIG 설치
- IAM-only와 App-only 설치
- Aggregate Agent → Vision → App 순서
- 생성 `eva.yaml`의 producer/consumer 경로
- App/IAM 고객 values 병합
- Cloud/Remote/Local repository mode
- AWS credential 경로 선택, target 설치 조건, Airgap 제외 정책
- Airgap Deb/Wheel/Harbor bootstrap
- Qdrant OCI snapshot seed 및 restore
- Chart 기본값, 고객 override, 최종 resolved values
- 동일 base Artifact digest의 Cloud/Airgap 사용

## 11. 확정 권고사항

| 항목 | 권고 |
| --- | --- |
| 최상위 구조 | `src`, `workspace`, `scripts`, `prompts`, `tools`, `out`, `docs` |
| `scripts`와 `tools` | 최상위에서 분리. `tools`에는 EVA CLI 제품 코드만 배치 |
| `deploy/` 폴더 | 현재는 만들지 않음. Deployer 자체 CI/Helm 책임이 실제로 생길 때 추가 |
| 고객 입력 | `workspace/` 또는 외부 site workspace |
| 제품 values | `src/solution/values/`에 불변 템플릿만 배치 |
| 생성 values | `out/work/rendered/<site>/<host>/<component>/`에 기록하고 직접 수정 금지 |
| 공식 Artifact | CI가 build하여 S3 immutable 경로에 게시 |
| 설치자 기본 경로 | S3 Artifact 소비. Git source build는 비기본 |
| Airgap | 단일 Bundle 제공 가능. 내부 base Artifact digest 보존 |
| 상태 | `work/`와 분리된 `state/`에 보존 |

### 11.1 후속 결정 항목

- operation/state의 YAML 또는 JSON 저장 형식
- Artifact 서명 기술과 Offline trust root 배포 방식
- App values 미인식 키를 경고로 볼지 strict mode 오류로 볼지
- `--set`의 문자열 강제 syntax와 type 처리 세부 규칙
- `release.yaml`이 없는 개발용 directory의 명시적 허용 option

## 12. 설치자 체크리스트

- [ ] 올바른 `release.yaml`과 환경별 Artifact를 확보했다.
- [ ] 외부 `checksums.sha256`으로 다운로드 파일을 검증했다.
- [ ] `/etc/eva/sites/<site-id>` 또는 `--workspace`로 사용할 workspace를 확정했다.
- [ ] `workspace/inventory/inventory.ini`를 작성했다.
- [ ] `workspace/site-values/site.yaml`에 site ID, mode, component를 작성했다.
- [ ] App Chart 기본 values를 확인했다.
- [ ] 필요한 경우 `workspace/site-values/app.yaml`에 변경분만 작성했다.
- [ ] 필요한 경우 `workspace/site-values/iam.yaml`에 변경분만 작성했다.
- [ ] AWS가 필요한 모드라면 준비 서버 profile 또는 `aws_key.ini` 입력 위치를 현재 계약에 맞게 준비했다.
- [ ] 실제 `aws_key.ini`가 Git, release artifact, Airgap Bundle에 포함되지 않음을 확인했다.
- [ ] SSH key, TLS 인증서 및 Secret을 준비했다.
- [ ] `precondition.yaml`과 `eva.yaml`을 검토했다.
- [ ] 대상, 릴리스, 설치 순서, resolved values와 diff를 검토했다.
- [ ] 승인된 Plan으로만 실제 설치를 실행했다.
- [ ] Pod, Service, Ingress, Qdrant restore 및 SSO를 확인했다.
- [ ] Operation state와 Audit 결과를 보관했다.

## 13. 문서 근거와 범위

본 문서는 다음 자료와 설계 논의를 종합했다.

- `eva-deployer-as-is.md`: 현재 구조, 파일 분류, SE-001~SE-034 및 ASIS-001~ASIS-012
- `eva-deployer-to-be.md`: Infra/Solution 분리, Artifact, Patch, compatibility, state, Go CLI, S3 promotion 제안
- `all_eva-deployer_contents_exact.txt`: README, Airgap runbook, playbook, role 및 install 스크립트의 전체 소스 번들

예시 CLI 명령과 신규 manifest/workspace schema는 아직 구현된 현재 기능이 아니라 TO-BE 계약이다. 실제 구현에서는 전체 소스의 exact path와 참조 anchor를 다시 확인하고, 단계별 parity 및 targeted regression을 통과한 뒤 기존 경로를 제거한다.
