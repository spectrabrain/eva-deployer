# EVA Deployer

## 개요

EVA Deployer는 Ansible로 EVA 설치 환경과 서비스를 구성하는 배포 도구입니다. 권장 OS는 **Ubuntu 24.04**입니다.

EVA는 고객마다 Network와 Security 환경이 다르므로, 하나의 설치 Package를 모든 환경에 동일한 배포 모드로 전달하기 어렵습니다. EVA Deployer는 고객별 Helm Chart나 제품 이미지를 따로 만들지 않고, **동일한 EVA Release에서 이미지 Repository의 공급 경로만 변경하여** 다양한 환경에 EVA를 배포합니다.

### Repository 기반 배포 모드

설치 시 이미지 출처는 `repository_mode`로 결정하며, 기본값은 `cloud_repository`입니다.

| Repository 모드 | 적용 환경 | 설치 Image 공급 경로 |
| --- | --- | --- |
| `cloud_repository` | 대상 서버가 인터넷과 외부 Repository에 접근 가능 | AWS ECR, Docker Hub, S3 등 외부 Repository |
| `remote_repository` | 대상 서버는 인터넷에 접근할 수 없지만 Main 서버에 접근 가능 | Main 서버의 Harbor |
| `local_repository` | 외부 인터넷과 외부 Network가 모두 차단된 완전 폐쇄망 | 저장매체로 반입한 고객 내부 Harbor |

`remote_repository`는 대상 EVA 서버와 통신할 수 있고 외부 인터넷에 접근 가능한 Main 서버의 Harbor를 여러 EVA 서버가 함께 사용할 수 있어, 다수 사업장에 같은 Release를 배포할 때 사용합니다. `local_repository`는 완전 폐쇄망에서 Local Harbor에 사전 반입한 이미지를 등록한 뒤 설치하는 모드입니다.

### 동일 Release와 Version 관리

세 배포 모드는 서로 다른 제품 또는 별도 Helm Chart가 아닙니다. 고객 Network 환경에 따라 Image 공급 경로만 달라지며, 실제 EVA Application 구성과 Version은 동일하게 유지합니다. EVA App·Agent·Vision·IAM·n8n 버전은 `src/solution/version.yaml`, Infra 관련 버전은 `src/infra/version.yaml`에서 관리하고, `repository_registry`와 `repository_project`로 배포 대상 Repository를 지정합니다. EVA Deployer도 EVA 버전과 동일한 기준으로 버저닝하며, 특정 EVA 버전을 배포할 때 해당 버전을 지원하는 Deployer 버전을 함께 사용합니다.

이 구조는 하나의 EVA Release를 인터넷 연결 환경, 제한된 내부망, 완전 폐쇄망에 반복 배포하면서도 고객별 Version 분기를 만들지 않도록 합니다.

### Offline Delivery

`local_repository` 환경에서는 필요한 항목을 표준 Offline Release Package로 사전에 준비합니다.

- EVA 및 Infrastructure Container Image
- AI Model, Helm Chart, 설치 Asset
- Python·Ansible 패키지와 일반 apt 의존성 bundle
- Harbor Offline Installer
- Version Manifest와 설치·검증 Script

폐쇄망에서는 누락된 Package 하나로 설치가 중단될 수 있으므로, 설치 전에 Package 구성·Version·manifest를 검증해야 합니다. 모드별 준비 절차와 Repository 주소 규칙은 [1. 사전 준비 및 Repository 준비](#1-사전-준비-및-repository-준비)에서 설명합니다.

### Workspace 선택 계약

Ansible playbook은 저장소 내부의 제품 소스(`src/`)와 설치자 입력 workspace를 분리해서 사용합니다.

- 기본 workspace: `<repo>/workspace`
- 외부 workspace 지정: `-e eva_workspace_root=/abs/path` 또는 `EVA_WORKSPACE_ROOT=/abs/path`
- site values 경로: 항상 `<선택된 workspace>/site-values`
- AWS credential 기본 경로: 항상 `<선택된 workspace>/credentials/aws_key.ini`
- site 식별자: 모든 Harbor 및 Ansible 실행 전에 `EVA_SITE_ID` 환경변수로 반드시 지정
- 생성 config 경로: `out/work/config/<site>/<host>/...`
- 렌더링 values 경로: `out/work/rendered/<site>/<host>/<component>/...`
- 실행 로그 경로: `out/work/logs/<operation-or-component>/...`

하나의 실행에서는 하나의 workspace와 site 식별자만 사용합니다. repo-local `workspace/`와 외부 workspace를 fallback으로 섞지 않습니다. `EVA_SITE_ID`가 비어 있으면 playbook과 Harbor 설치가 실패합니다. 테스트를 포함해 `default`를 사용하려면 `export EVA_SITE_ID=default`로 명시해야 합니다. 이전 구조에서 생성된 `out/work/config/<host>/...`와 `out/work/rendered/<host>/...`는 더 이상 읽지 않으므로, 전환 후 `site_eva_config.yaml`과 Harbor metadata 생성을 다시 실행해야 합니다.

```bash
# 모든 Harbor 및 Ansible 명령을 실행할 같은 shell에서 한 번만 설정
export EVA_SITE_ID=customer-a
```

### EVA CLI 설치

Tag Base Release를 풀어 나온 디렉터리에서 installer를 실행하면 sibling `eva-tool` archive와 `checksums.sha256`의 matching digest를 자동으로 검증한 뒤 `/opt/eva/tool`에 binary를 설치하고 `/usr/local/bin/eva` 링크를 생성합니다.

```bash
unzip -q eva-base-release-v3.2.0.zip -d eva-base-release-v3.2.0
cd eva-base-release-v3.2.0
sudo bash ./eva-tool-installer.sh
command -v eva
eva version
```

설치 사용자는 `eva-operators` 그룹에 추가됩니다. 새 그룹 권한은 다음 login session부터 적용됩니다.

---

## 1. 사전 준비 및 Repository 준비

이 섹션에서는 설치 전에 필요한 패키지·asset·이미지·Harbor를 준비합니다. 먼저 아래에서 하나의 Repository 모드를 선택하고, 해당 모드의 절차만 진행하세요. 실제 Ansible 설치 명령은 이후 `인프라 설치`, `EVA IAM 설치`, `EVA 배포`, `n8n 설치` 섹션에서 같은 모드로 실행합니다.

> **설치 사용자 계정은 `eva` 사용을 권장합니다.**
> 
> 일부 기본 설정(예: `/home/eva/.aws`, `/home/eva/certs`)이 `eva` 계정을 기준으로 구성되어 있습니다.
> 다른 계정을 사용하는 경우 관련 경로를 환경에 맞게 수정해야 할 수 있습니다.

### 1-1. Repository 모드 선택

| 모드 | 사용 환경 | 준비/Ansible 실행 위치 | 대상 서버의 이미지 출처 | 파일 이동 |
| --- | --- | --- | --- | --- |
| `cloud_repository` | 대상 서버가 인터넷과 AWS에 직접 접근 가능 | 대상 서버 또는 별도 control node | 외부 Registry, ECR, S3 | 불필요 |
| `remote_repository` | 대상 서버는 인터넷이 없지만 Main Harbor에는 연결 가능 | 인터넷 가능 Main 서버 | Main Harbor | 불필요 (Main 서버가 Ansible로 전달) |
| `local_repository` | 대상 서버가 완전 Airgap | 인터넷 가능 준비 서버 → Airgap 서버 | Airgap 서버의 Local Harbor | USB 등 저장 매체 필요 |

`remote_repository`와 `local_repository`는 실행 시 아래 변수를 반드시 함께 지정합니다.

```text
repository_mode=<remote_repository|local_repository>
repository_registry=<Harbor host:port>
repository_project=eva
```

`repository_registry`에는 `https://`를 제외한 **k3s 노드와 Pod에서 실제 접근 가능한 주소**를 지정합니다. Local Harbor도 `localhost:32080`을 사용하지 않고, Harbor가 실행되는 노드의 DNS 또는 IP와 `32080` 포트를 사용합니다. `setup_harbor.sh`가 생성한 `out/work/config/$EVA_SITE_ID/harbor-endpoint.yaml`을 Ansible extra vars로 전달하면 이 주소와 project를 함께 적용할 수 있습니다.

### 1-2. 모든 모드 공통: 버전 단일 관리

EVA App/Agent/Vision/IAM/n8n 및 Helm Chart 버전은 `src/solution/version.yaml`, Infra 버전은 `src/infra/version.yaml`에서 관리합니다. EVA Deployer도 EVA 버전과 동일한 기준으로 버저닝합니다. 따라서 특정 EVA 버전을 배포할 때는 해당 버전을 지원하는 Deployer 버전을 함께 사용해야 합니다.
버전 업데이트 시 우선 이 파일만 수정하면:

- Ansible 배포(`src/solution/playbooks/site_eva.yaml`)에 자동 반영
- 다운로드 스크립트(`scripts/download/download_offline_assets.sh`, `scripts/download/download_eva_images.sh`, `scripts/download/download_n8n_images.sh`)에 자동 반영

이미지/asset 다운로드 전에 먼저 version catalog를 수정하세요.

예시:

```yaml
# src/solution/version.yaml
eva_app_deploy_version: 3.1.3-rc.3
eva_app_chart_version: 3.1.8
eva_vision_deploy_version: 3.1.0
eva_vision_chart_version: 3.1.0
eva_agent_deploy_version: 3.1.0
eva_agent_chart_version: 3.1.0
n8n_image: docker.n8n.io/n8nio/n8n:2.32.7
```

Harbor를 사용하는 모드에서는 이미지가 아래 형태로 저장됩니다.

```text
<repository_registry>/<repository_project>/eva-agent:<version>
<repository_registry>/<repository_project>/eva-app:<version>
<repository_registry>/<repository_project>/eva-vision:<version>
<repository_registry>/<repository_project>/n8n:<version>
```

### 1-3. remote_repository/local_repository 공통: Airgap 설치 자산과 패키지 bundle

Airgap 서버에 Docker가 설치되어 있지 않을 수 있으므로, 인터넷이 가능한 준비 서버에서 `scripts/download/download_offline_assets.sh`를 실행하면 Docker Engine, containerd, buildx, compose plugin 및 apt 의존성 `.deb` 파일도 `out/cache/docker/debs/`에 함께 준비됩니다. 오프라인 자산은 `out/cache/` 아래에 정리되며, 대상 서버에는 필요한 산출물과 저장소 전체를 함께 전달합니다.

이 스크립트는 Docker `.deb` 패키지와 실제 설치에 필요한 의존성 전체를 받기 위해 `apt-get update`를 실행하므로, 준비 서버에서 `root` 또는 비밀번호 없이 사용할 수 있는 `sudo` 권한이 필요합니다. 일반 사용자로 실행하는 경우 먼저 터미널에서 `sudo -v`를 실행해 인증한 뒤 다운로드 명령을 실행하세요. 준비 서버와 Airgap 서버는 같은 Ubuntu 릴리스 및 아키텍처를 사용해야 하며, 실행할 때마다 기존 `out/cache/docker/debs/*.deb`는 최신 의존성 묶음으로 교체됩니다. 다운로드 후 `out/cache/docker/debs/manifest.txt`가 생성되었는지도 확인하세요. 기존에 생성한 Docker `.deb` 묶음은 의존성이 부족할 수 있으므로 수정된 스크립트로 반드시 다시 생성해야 합니다.

`out/cache/apt/debs/` bundle은 Ansible base/NFS role에 필요한 `unzip`, `curl`, `gnupg`, `nfs-kernel-server`, `nfs-common`, `keyutils`와 그 일반 의존성을 준비합니다. `systemd`, `udev`, `libsystemd0`, `libudev1`, `dpkg`, `libc6` 같은 **OS 핵심 패키지는 bundle 및 Ansible base/NFS 설치 대상에서 제외**합니다. 준비 서버와 Airgap 서버의 Ubuntu patch level이 달라도 배포 중 OS 핵심 패키지가 섞여 설치되어 의존성이 깨지지 않게 하기 위함입니다.

`local_repository`/`remote_repository`용 offline asset을 준비한 뒤에는 `out/cache/apt/debs/` 전체와 `manifest.txt`를 함께 전달하세요. Airgap Ansible은 설치 전에 bundle의 package dependency를 검사하며, 설치 시에는 `apt-get --no-download`로 bundle 안의 일반 패키지만 설치합니다.

Airgap 서버에서 offline `.deb` 설치가 중간에 실패했을 때, `systemd`/`udev`와 무관한 bundle 문제는 아래 명령으로 검사하고 복구합니다. `systemd`, `udev`, `libsystemd0`, `libudev1` 오류가 보이면 이 명령으로 bundle 전체를 재설치하지 말고 다음 `systemd recovery` 절차를 사용하세요.

```bash
cd /home/eva/eva-deployer
./scripts/install/validate_offline_debs.sh ./out/cache/apt/debs
sudo ./scripts/install/repair_offline_debs.sh ./out/cache/apt/debs
sudo dpkg --audit
```

#### systemd recovery: 이미 OS 핵심 패키지 version mismatch가 발생한 경우

현재 저장소에는 전용 recovery helper가 포함되어 있지 않습니다. 이 상태에서 `systemd`, `udev`, `libsystemd0`, `libudev1`가 서로 다른 patch level로 어긋났다면, 기존 offline bundle을 계속 재적용하지 말고 준비 서버와 동일한 Ubuntu patch level의 패키지를 별도로 확보한 뒤 수동 복구 절차를 먼저 수행해야 합니다.

최소 확인 항목은 아래 두 가지입니다.

```bash
dpkg-query -W -f='${Package}\t${Version}\n' \
  systemd systemd-sysv udev libsystemd0 libudev1
sudo dpkg --audit
```

위 다섯 패키지의 version이 서로 다르면, Ansible 재실행 전에 OS 패키지 상태부터 맞춰야 합니다.

### 1-4. Cloud/Remote/Local 준비 서버: AWS 인증

현재 저장소에는 AWS 인증 입력이 두 흐름으로 나뉘어 있습니다. 이 둘은 목적이 다르며, 서로를 자동으로 대체하지 않습니다.

#### A. AS-IS: Ansible `awscli` role 입력 계약

현재 구현 기준으로 `awscli` role은 아래처럼 동작합니다.

- 호출 playbook: `src/solution/playbooks/site_eva_config.yaml`
- 실행 조건: `repository_mode=cloud_repository` 이고 `airgap_mode=false` 인 경우만 실행
- 읽는 위치: control node의 `<selected-workspace>/credentials/aws_key.ini`
- 명시적 override: `-e aws_key_file=/absolute/path/aws_key.ini`
- 읽는 주체: Ansible control node의 `lookup('ansible.builtin.file', ...)`
- 설정 대상: target server의 `ansible_user | default(ansible_ssh_user)` 사용자 홈 아래 `~/.aws`
- 필수 필드: `aws_access_key_id`, `aws_secret_access_key`
- region 처리: 파일에 없으면 `ap-northeast-2` 기본값 사용
- 비노출 처리: credential parsing과 `aws configure set` block 전체에 `no_log: true` 적용

현재 role이 기대하는 파일 형식은 아래와 같습니다.

```ini
aws_access_key_id = <YOUR_ACCESS_KEY>
aws_secret_access_key = <YOUR_SECRET_KEY>
region = ap-northeast-2
```

현재 구현을 그대로 사용할 때는 실제 Secret 파일을 선택된 workspace의 `credentials/aws_key.ini`에 두어야 합니다. 필요하면 `aws_key_file` extra var로 절대 경로를 명시적으로 지정할 수 있습니다.

#### B. 현재 README의 `aws configure` 안내가 담당하는 역할

아래 명령은 Ansible `awscli` role 입력 파일을 만드는 절차가 아닙니다. 인터넷이 가능한 준비 서버 또는 control node에서 다운로드 스크립트와 `AWS_PROFILE=default` 기반 명령을 실행하기 위한 로컬 AWS CLI profile 설정입니다.

```bash
aws configure set aws_access_key_id <AK> --profile default
aws configure set aws_secret_access_key <SK> --profile default
aws configure set region ap-northeast-2 --profile default
```

따라서 현재 README의 `aws configure` 안내는 `scripts/download/*`, 일부 publish flow, S3/ECR 접근이 필요한 준비 서버 작업에는 유효합니다. 다만 이것만으로 target server에 대한 Ansible `awscli` role 입력이 충족되지는 않습니다.

#### C. 현재 기본 계약

현재 구현과 문서 계약은 아래 경로로 맞춰져 있습니다.

```text
<selected-workspace>/
├── inventory/
│   └── inventory.ini
├── site-values/
│   ├── app.yaml
│   └── iam.yaml
└── credentials/
    └── aws_key.ini
```

- 기본 경로: `<selected-workspace>/credentials/aws_key.ini`
- 명시적 override: `-e aws_key_file=/absolute/path/aws_key.ini`
- 선택 우선순위:
  1. `aws_key_file`
  2. `<selected-workspace>/credentials/aws_key.ini`

현재 구현은 위 계약을 따릅니다. 기존 `<repo>/aws_key.ini` 직접 참조는 제거되었습니다.

Migration note:

- 이전: `site_eva_config.yaml`의 `awscli` role이 control node의 `<repo>/aws_key.ini`를 읽음
- 현재: 선택된 workspace의 `credentials/aws_key.ini`를 기본값으로 사용하고, 필요 시 `aws_key_file`로 명시적 override
- 후속 구현 필요: mode별 credential 설치 조건 세분화, 권한/존재 검증 보강, regression 확대

#### D. Repository mode별 AWS credential 사용 위치

`cloud_repository`

- 대상 서버가 AWS ECR, S3 등에 직접 접근해야 할 수 있습니다.
- 현재 구현에서는 `site_eva_config.yaml` 실행 시 control node의 `<selected-workspace>/credentials/aws_key.ini`를 기본값으로 읽어 target user의 `~/.aws`를 구성합니다.
- AWS CLI binary 설치와 credential 설정은 같은 role 안에 있지만, 책임상으로는 별도 항목입니다.

`remote_repository`

- 인터넷 가능한 Main 서버 또는 준비 서버가 AWS에서 asset, image, model을 다운로드할 때는 준비 서버의 AWS CLI profile이 필요할 수 있습니다.
- 대상 EVA 서버가 Main Harbor만 사용한다면 target server에 AWS credential을 배포할 필요는 없습니다.
- 현재 구현도 `remote_repository`를 `airgap_mode=true`로 간주하므로 `awscli` role은 실행되지 않습니다.

`local_repository`

- 인터넷 가능한 준비 서버가 Airgap Bundle과 image/model cache를 준비할 때 AWS credential이 필요할 수 있습니다.
- 완전 Airgap 대상 서버에는 AWS credential을 전달하지 않는 것을 원칙으로 합니다.
- 실제 `aws_key.ini`는 Airgap Bundle, USB 반입 artifact, Harbor seed tar에 포함하면 안 됩니다.

#### E. Secret 파일 운영 정책

- 실제 Secret 파일은 Git에 커밋하지 않습니다.
- 현재 `.gitignore`의 `*.ini` 규칙으로 repo 내부의 `<repo>/aws_key.ini`와 `<repo>/workspace/credentials/aws_key.ini`는 제외됩니다.
- 외부 workspace는 repository `.gitignore`로 보호할 수 없으므로, 운영자가 별도 저장소 정책과 파일 권한을 관리해야 합니다.
- 실제 Secret 파일 권한은 `0600`을 권장합니다.
- Access Key와 Secret Key를 문서, shell history, CI log, Airgap manifest에 출력하지 않습니다.
- 현재 Ansible `awscli` role은 credential block에 `no_log: true`를 사용합니다.
- 형식 참고용 sample은 `workspace/credentials/aws_key.ini.sample`만 사용하고, sample에는 실제 Secret을 넣지 않습니다.

### 1-5. cloud_repository: 대상 서버가 외부 Registry를 직접 사용

대상 서버가 인터넷/ECR/Docker Hub/S3에 직접 접근 가능한 모드입니다. 별도 Harbor 준비가 필요 없습니다.

1. 대상 서버(또는 대상 서버에 접속 가능한 control node)에 Python/Ansible을 준비합니다.
2. 대상 서버가 AWS ECR, Docker Hub/외부 Registry, S3에 연결되는지 확인합니다.
3. 이후 설치 단계에서 `-e repository_mode=cloud_repository`를 사용합니다.

#### 대상 서버 또는 control node: Ansible 준비

대상 서버에서 Python/Ansible을 준비합니다.

```bash
python3 -m venv .venv
source .venv/bin/activate
pip install ansible
```

대상 서버가 아래에 직접 접근할 수 있으면 됩니다.

- AWS ECR
- Docker Hub 또는 외부 container registry
- S3 모델/릴리즈 버킷

### 1-6. remote_repository: Main Harbor를 대상 서버가 사용

인터넷 가능한 Main 서버에 Harbor를 구성하고, 설치 대상 Airgap 서버는 Main Harbor에 접근 가능한 구조입니다.

1. **Main 서버**에 Ansible, Docker, Harbor를 준비합니다.
2. **Main 서버**에서 asset·모델·이미지를 내려받아 Main Harbor의 `eva` project로 push합니다.
3. **대상 서버**에서 Main Harbor의 `hostname:32080`에 접근할 수 있어야 합니다.
4. 이후 Main 서버에서 Ansible을 실행하며 `repository_registry=<Main Harbor host:32080>`를 지정합니다.

Main 서버에서 Python/Ansible을 준비합니다. Main 서버에서 Ansible을 실행해 Airgap 서버를 설치하는 흐름입니다.

#### Main 서버: Ansible과 Harbor 준비

```bash
python3 -m venv .venv
source .venv/bin/activate
pip install ansible
```

Main 서버에 Harbor를 설치합니다. Harbor는 Docker Engine과 Docker Compose plugin이 먼저 설치되어 실행 중이어야 합니다. `repository_registry`는 Harbor의 `hostname:32080`과 맞춰야 합니다.

```bash
# Docker가 없는 경우 먼저 Docker 설치 script를 실행합니다.
./scripts/install/install_docker.sh

# Docker 설치 및 실행을 확인한 뒤 Harbor를 시작합니다.
./scripts/install/setup_harbor.sh --hostname harbor.main.local
```

비밀번호나 데이터 저장 경로를 바꾸고 싶으면 인자로 넘깁니다.

```bash
./scripts/install/setup_harbor.sh \
  --hostname harbor.main.local \
  --admin-password 'your-admin-password' \
  --data-volume /data001/harbor
```

Harbor 기본값:

- `HARBOR_VERSION`: `v2.15.2`
- `http.port`: `32080` 고정
- `hostname`: 지정하지 않으면 Harbor 서버의 default-route IPv4 (k3s 노드와 Pod에서 도달 가능한 DNS/IP를 명시 권장)
- `harbor_admin_password`: `EVA123@`
- `data_volume`: Harbor template 기본값 유지
- `project`: `eva`

#### Main 서버: asset·모델·이미지 준비 및 Harbor push

Main 서버에서 asset과 이미지를 준비합니다.

```bash
# Harbor 로그인
docker login harbor.main.local:32080 -u admin

# chart/values/script/kustomize/manifest 등 설치 asset 다운로드
# Docker Engine/Compose와 Ansible base/NFS role용 일반 apt 의존성 .deb bundle도 함께 준비됩니다.
# Docker apt 패키지 다운로드를 위해 sudo 인증 후 실행
sudo -v && AWS_PROFILE=default ./scripts/download/download_offline_assets.sh

# EVA 모델 캐시 다운로드 -> out/cache/models
AWS_PROFILE=default ./scripts/download/download_eva_models.sh

# EVA/infra/n8n 이미지 pull 및 이미지 목록 생성
AWS_PROFILE=default ./scripts/download/download_eva_images.sh
./scripts/download/download_infra_images.sh
./scripts/download/download_n8n_images.sh
```

이미지를 Main Harbor로 push합니다.

```bash
# EVA 이미지 push
REPOSITORY_REGISTRY=harbor.main.local:32080 \
REPOSITORY_PROJECT=eva \
AWS_PROFILE=default \
./scripts/publish/push_images_to_repository.sh

# Infra 이미지 push: nvidia-device-plugin, CUDA sample, MIG 검증용 CUDA
IMAGE_LIST=./out/cache/images/infra-images-pulled.txt \
REPOSITORY_REGISTRY=harbor.main.local:32080 \
REPOSITORY_PROJECT=eva \
./scripts/publish/push_images_to_repository.sh

# n8n 이미지 push
IMAGE_LIST=./out/cache/images/n8n-images.txt \
REPOSITORY_REGISTRY=harbor.main.local:32080 \
REPOSITORY_PROJECT=eva \
./scripts/publish/push_images_to_repository.sh
```

준비 결과 Main Harbor에는 아래 형태의 이미지가 있어야 합니다.

```text
harbor.main.local:32080/eva/eva-agent:3.0.4
harbor.main.local:32080/eva/eva-app:3.0.5
harbor.main.local:32080/eva/eva-vision:2.0.5
harbor.main.local:32080/eva/vllm-openai:v0.21.0
harbor.main.local:32080/eva/qdrant:v1.16.3
harbor.main.local:32080/eva/mysql:8.0.42-bookworm
harbor.main.local:32080/eva/k8s-device-plugin:v0.18.0
harbor.main.local:32080/eva/cuda-sample:vectoradd-cuda12.5.0
harbor.main.local:32080/eva/cuda:12.5.0-base-ubuntu22.04
harbor.main.local:32080/eva/n8n:2.32.7
```

`harbor.main.local:32080`을 Docker 또는 k3s에서 HTTP registry로 사용할 경우, 해당 노드의 Docker/containerd에 insecure registry 또는 인증서 신뢰 설정이 필요할 수 있습니다.

### 1-7. local_repository: USB로 전달한 이미지를 대상 서버 Local Harbor에 저장

인터넷 가능 환경에서 필요한 파일을 모두 준비한 뒤, `eva-deployer` 폴더를 USB로 Airgap 서버에 복사합니다. Airgap 서버 내부에는 Local Harbor를 띄우고, k3s는 해당 Harbor에서 이미지를 pull합니다.

1. **인터넷 가능 준비 서버**에서 Ansible wheel, Docker/apt bundle, asset, 모델, 이미지, Harbor installer를 모두 받습니다.
2. `repository-images.tar`와 `eva-deployer` 전체를 USB 등으로 **Airgap 서버**에 복사합니다.
3. **Airgap 서버**에서 Python/Ansible, Docker, Local Harbor를 설치하고 archive 이미지를 Harbor에 push합니다.
4. `setup_harbor.sh`가 만든 `out/work/config/$EVA_SITE_ID/harbor-endpoint.yaml`을 모든 Airgap Ansible 실행에 extra vars로 전달합니다. 이 파일의 registry 주소는 Local Harbor 노드의 실제 DNS/IP와 `32080` 포트입니다.

#### A. 인터넷 가능 준비 서버: 설치 파일과 이미지 준비

인터넷 가능 환경에서 Airgap 서버용 Python/Ansible 패키지, asset, 이미지, Harbor offline installer를 준비합니다.

```bash
# Airgap 서버에서 Python venv/Ansible 설치에 필요한 패키지 다운로드
./scripts/download/download_python_venv_debs.sh
./scripts/download/download_ansible_wheels.sh

# chart/values/script/kustomize/manifest 등 설치 asset 다운로드
# Docker Engine/Compose와 Ansible base/NFS role용 일반 apt 의존성 .deb bundle도 함께 준비됩니다.
# Docker apt 패키지 다운로드를 위해 sudo 인증 후 실행
sudo -v && AWS_PROFILE=default ./scripts/download/download_offline_assets.sh

# EVA 모델 캐시 다운로드 -> out/cache/models
AWS_PROFILE=default ./scripts/download/download_eva_models.sh

# EVA/infra/n8n 이미지 pull 및 이미지 목록 생성
AWS_PROFILE=default ./scripts/download/download_eva_images.sh
./scripts/download/download_infra_images.sh
./scripts/download/download_n8n_images.sh

# Airgap 서버에서 Local Harbor 설치에 필요한 Harbor offline installer 다운로드
./scripts/install/setup_harbor.sh --download-only
```

USB로 이미지를 옮기기 위해 Docker image archive를 생성합니다. 이 archive는 k3s에 직접 import하는 용도가 아니라, Airgap 서버의 Local Harbor에 이미지를 seed하기 위한 용도입니다.

```bash
cat ./out/cache/images/images-pulled.txt \
    ./out/cache/images/infra-images-pulled.txt \
    ./out/cache/images/n8n-images.txt \
  | sort -u > ./out/cache/images/repository-images.txt

docker save \
  -o ./out/cache/images/repository-images.tar \
  $(cat ./out/cache/images/repository-images.txt)
```

저장 공간이 부족하면 `docker save` 후 인터넷 가능 환경의 Docker image를 삭제해도 됩니다. `repository-images.tar`와 `eva-deployer` 폴더를 Airgap 서버로 옮긴 뒤에는 Airgap 서버에서 다시 `docker load`합니다.

```bash
docker image rm $(cat ./out/cache/images/repository-images.txt)
```

`eva-deployer` 폴더를 저장 매체로 복사하여 Airgap 서버로 이동합니다.

#### B. Airgap 서버: 실행 환경과 Local Harbor 설치

다음 순서로 진행합니다. Python/Ansible은 Ansible 실행을 위해 먼저 설치하고, Local Harbor는 Docker Engine과 Compose plugin 설치가 완료된 뒤 시작합니다.

##### 1. Python/Ansible 설치

```bash
./scripts/install/install_python_venv_airgap.sh
./scripts/install/install_ansible_airgap.sh
source .venv/bin/activate
ansible --version
```

##### 2. Docker Engine과 Compose plugin 설치

준비된 Docker `.deb` bundle로 Docker Engine과 Compose plugin을 설치합니다.

```bash
# out/cache/ 가 준비된 airgap 서버에서 Docker 설치
sudo ./scripts/install/install_docker.sh --airgap
```

`docker` 그룹 권한을 적용하려면 SSH 세션을 종료한 뒤 다시 접속하세요.

##### 3. Local Harbor 설치 또는 이전

Local Harbor의 실행 설정은 `eva-deployer` 밖의 `~/.local/share/eva-harbor`에 저장합니다. 이후에는 Harbor를 중지하지 않고 `eva-deployer` 전체를 다시 동기화할 수 있습니다.

새로 설치하는 경우 아래 명령을 실행합니다. `HARBOR_HOST`는 Local Harbor가 실행되는 노드의 DNS 또는 IP이며, `localhost`나 `127.0.0.1`을 사용하면 안 됩니다. 최초 설치의 관리자 계정은 `admin`, 기본 비밀번호는 `EVA123@`입니다. 운영 환경에서는 `--admin-password`로 변경하세요.

```bash
export HARBOR_HOST=<NODE_IP_OR_DNS>
export HARBOR_REGISTRY="${HARBOR_HOST}:32080"

./scripts/install/setup_harbor.sh \
  --hostname "$HARBOR_HOST" \
  --registry-endpoint "$HARBOR_REGISTRY" \
  --install-root ~/.local/share/eva-harbor
```

설치가 끝나면 `out/work/config/$EVA_SITE_ID/harbor-endpoint.yaml`이 생성됩니다. 이 파일에는 비밀번호 없이
Harbor의 k3s/Pod 접근 주소, `repository_registry`, project가 들어 있습니다. 이후 이 문서의 Ansible 실행에는
`-e @out/work/config/$EVA_SITE_ID/harbor-endpoint.yaml`을 추가합니다. Harbor와 k3s 배포 서버가 다르면 이 파일도
USB bundle과 함께 배포 controller로 복사하세요.

Harbor 서버에서 실행하는 image/snapshot seed 스크립트는 `harbor.yml`의 hostname과 일치하는
registry endpoint에 한해 관리자 비밀번호를 자동으로 읽습니다. Harbor와 배포 controller가 분리되어
Qdrant snapshot을 배포하는 경우에는 `-e harbor_admin_password='<Harbor 비밀번호>'`를 추가하세요.

기존에 `eva-deployer/install/harbor/harbor`에 Harbor를 설치했다면, 위의 새 설치 대신 기존 Harbor를 중지한 뒤 외부 runtime 경로로 한 번 이전합니다.

```bash
cd ~/.local/share/eva-harbor/harbor
sudo docker compose down

cd /home/eva/eva-deployer
export HARBOR_HOST=<NODE_IP_OR_DNS>
export HARBOR_REGISTRY="${HARBOR_HOST}:32080"
./scripts/install/setup_harbor.sh \
  --hostname "$HARBOR_HOST" \
  --registry-endpoint "$HARBOR_REGISTRY" \
  --install-root ~/.local/share/eva-harbor
```

기존 Harbor의 volume 경로만 바꾸려면 `--data-volume`을 추가합니다. 기존 `harbor.yml`의 관리자 비밀번호는 별도로 지정하지 않으면 유지됩니다. 기존 Harbor 데이터는 자동 복사하지 않으므로, 새 경로는 빈 registry 저장소로 시작합니다.

```bash
./scripts/install/setup_harbor.sh \
  --hostname "$HARBOR_HOST" \
  --registry-endpoint "$HARBOR_REGISTRY" \
  --install-root ~/.local/share/eva-harbor \
  --data-volume /data001/harbor
```

`repository-images.tar`는 Harbor에 직접 push할 수 없습니다. Docker daemon에 load한 뒤 Local Harbor로 push해야 합니다. push 중에는 archive, Docker image cache, Harbor registry가 일시적으로 모두 저장 공간을 사용합니다.

#### C. Airgap 서버: Local Harbor에 이미지 seed

Airgap 서버에서 이미지를 load한 뒤 `HARBOR_REGISTRY`로 Local Harbor에 push합니다.
`push_images_to_repository.sh`는 표준 Local Harbor의 `harbor.yml`에서 관리자 비밀번호를 읽어 자동으로 로그인합니다.

```bash
docker load -i ./out/cache/images/repository-images.tar

# EVA 이미지 push
PULL_SOURCE_IMAGES=false \
REPOSITORY_REGISTRY="$HARBOR_REGISTRY" \
REPOSITORY_PROJECT=eva \
./scripts/publish/push_images_to_repository.sh

# Infra 이미지 push
PULL_SOURCE_IMAGES=false \
IMAGE_LIST=./out/cache/images/infra-images-pulled.txt \
REPOSITORY_REGISTRY="$HARBOR_REGISTRY" \
REPOSITORY_PROJECT=eva \
./scripts/publish/push_images_to_repository.sh

# n8n 이미지 push
PULL_SOURCE_IMAGES=false \
IMAGE_LIST=./out/cache/images/n8n-images.txt \
REPOSITORY_REGISTRY="$HARBOR_REGISTRY" \
REPOSITORY_PROJECT=eva \
./scripts/publish/push_images_to_repository.sh
```

Harbor에 저장된 repository를 확인합니다.

```bash
curl -fsS -u "admin:${HARBOR_ADMIN_PASSWORD:-EVA123@}" \
  "http://${HARBOR_REGISTRY}/api/v2.0/repositories?project_name=eva&page_size=100" \
  | python3 -c 'import json, sys; print("\n".join(item["name"] for item in json.load(sys.stdin)))'
```

Harbor 확인 후 Docker cache와 archive가 더 이상 필요 없으면 삭제해 공간을 확보할 수 있습니다.

```bash
docker image rm $(cat ./out/cache/images/repository-images.txt)
docker image rm $(docker images --format '{{.Repository}}:{{.Tag}}' | awk -v prefix="${HARBOR_REGISTRY}/eva/" 'index($0, prefix) == 1 { print }')
rm -f ./out/cache/images/repository-images.tar
```

#### D. Qdrant 전용 Airgap/Harbor snapshot 검증

Qdrant snapshot 변경만 확인할 때는 `values-k3s.harbor.yaml` profile을 사용합니다. 이 profile은 `qdrant-snapshot-sync`가 S3에 접속하지 않고 Local Harbor의 OCI artifact를 `oras pull`하여 snapshot PVC에 받은 뒤 Qdrant restore API를 호출합니다.

준비 서버에서 필요한 파일은 아래입니다. `values-k3s.harbor.yaml`은 EVA Agent release `3.1.0`의 `eva-agent-qdrant/`에 포함되어 있어야 합니다.

- `out/cache/qdrant/qdrant-<version>.tgz`
- `out/cache/eva-agent/release/<release>/eva-agent-qdrant/values-k3s.harbor.yaml`
- `out/cache/eva-agent/release/<release>/plugins/eva-agent-qdrant/{post-renderer.sh,plugin.yaml}`
- `out/cache/tools/oras` — Local Harbor에 snapshot OCI artifact를 push하는 CLI
- `out/cache/images/images-pulled.txt` 및 `out/cache/images/repository-images.tar` — `qdrant`와 `eva-agent-qdrant-snapshot-sync:0.1.0` 포함
- `out/cache/qdrant-snapshots/*.snapshot` — `SNAPSHOT_SPECS`에 지정된 snapshot 파일
- `scripts/publish/push_qdrant_snapshots_to_harbor.sh`

`src/solution/version.yaml`을 대상 release/chart 버전으로 맞춘 뒤 준비 서버에서 실행합니다. `COMPONENTS`로 이미지 준비만 Qdrant로 제한할 수 있습니다.

```bash
EVA_AGENT_QDRANT_SNAPSHOT_SOURCE=harbor \
  AWS_PROFILE=default ./scripts/download/download_offline_assets.sh

EVA_AGENT_QDRANT_SNAPSHOT_SOURCE=harbor \
  COMPONENTS=eva-agent-qdrant \
  AWS_PROFILE=default ./scripts/download/download_eva_images.sh

EVA_AGENT_QDRANT_SNAPSHOT_SOURCE=harbor \
  AWS_PROFILE=default ./scripts/download/download_qdrant_snapshots.sh
```

`repository-images.tar`, `eva-deployer/`, 그리고 `out/cache/qdrant-snapshots/`를 폐쇄망 서버로 옮깁니다. Local Harbor 설치 및 Docker image seed 후 snapshot도 OCI artifact로 push합니다. artifact 이름은 `<registry>/eva/qdrant-snapshots:<SNAPSHOT_SPECS 첫 번째 필드>`입니다.

```bash
docker load -i ./out/cache/images/repository-images.tar

PULL_SOURCE_IMAGES=false \
IMAGE_LIST=./out/cache/images/images-pulled.txt \
REPOSITORY_REGISTRY="$HARBOR_REGISTRY" \
REPOSITORY_PROJECT=eva \
./scripts/publish/push_images_to_repository.sh

EVA_AGENT_QDRANT_VALUES_FILE=values-k3s.harbor.yaml \
REPOSITORY_REGISTRY="$HARBOR_REGISTRY" \
REPOSITORY_PROJECT=eva \
./scripts/publish/push_qdrant_snapshots_to_harbor.sh
```

Qdrant만 배포하려면 (vLLM/EVA Agent 본체는 설치하지 않음) k3s와 Local Harbor가 준비된 뒤 아래 Helm 명령을 사용합니다. `qdrant-snapshot-harbor` Secret은 Harbor가 private project인 경우 ORAS 인증에 필요합니다.

기존 `site_eva_agent.yaml` 전체 배포에 이 profile을 적용할 때는 아래 두 변수를 함께 지정합니다. 기본값은 기존 `values-k3s.yaml`/PVC 사전복사 방식이므로, 기존 Airgap 배포에는 영향이 없습니다.

```bash
ansible-playbook -i 'localhost,' -c local src/solution/playbooks/site_eva_agent.yaml -K \
  -e repository_mode=local_repository \
  -e @out/work/config/$EVA_SITE_ID/harbor-endpoint.yaml \
  -e eva_agent_vllm_profile=PRO6000-MIGx4 \
  -e eva_agent_qdrant_values_file=values-k3s.harbor.yaml \
  -e eva_agent_qdrant_snapshot_source=harbor
```

`repository_registry`와 `repository_project`로 Qdrant 본체, snapshot-sync sidecar,
snapshot OCI artifact repository와 Qdrant chart test 이미지 경로가 정해집니다. 위 명령은 metadata의
`repository_registry`를 사용해 Qdrant 본체를 `<NODE_IP_OR_DNS>:32080/eva/qdrant:v<chart-version>`으로, sidecar를
`<NODE_IP_OR_DNS>:32080/eva/eva-agent-qdrant-snapshot-sync:0.1.0`으로 배포합니다. 이 role은
`qdrant-snapshot-harbor` Secret도 자동으로 만듭니다. 이 Secret은 Harbor 설치 단계에서
생기는 것이 아니라, Harbor snapshot 모드의 Qdrant role이 Helm 설치 직전에 만드는 Qdrant
전용 Kubernetes Secret입니다. Secret 이름은 고정이며, 사용자/비밀번호는
`harbor_admin_user`와 `harbor_admin_password`를 사용합니다. 표준 Local Harbor
(`~/.local/share/eva-harbor/harbor/harbor.yml`)를 사용하고 비밀번호를 별도로 넘기지 않으면,
role이 그 파일에서 실제 `harbor_admin_password`를 읽어 Secret에 넣습니다. `-e
harbor_admin_password=...`를 지정하면 그 값이 우선합니다.
Harbor를 기본 경로가 아닌 곳에 설치했다면 `-e eva_agent_harbor_config_path=<harbor.yml 경로>`를
지정합니다.

기본 project는 세 단계 모두 `eva`이므로 별도 값이 필요 없습니다. project를 바꾸는 경우에만
Harbor 설치의 `HARBOR_PROJECT`, image/snapshot push의 `REPOSITORY_PROJECT`, 배포의
`repository_project`를 **같은 값**으로 지정해야 합니다. 세 값은 서로 자동 전달되지 않습니다.

Local Harbor도 k3s 노드와 Pod에서 도달 가능한 `<NODE_IP_OR_DNS>:32080`을 사용합니다.
`scripts/install/setup_harbor.sh`가 생성한 `out/work/config/$EVA_SITE_ID/harbor-endpoint.yaml`은 이 주소를
`repository_registry`와 Qdrant snapshot sidecar endpoint로 함께 기록합니다. 모든 Ansible 실행에 이 파일을
extra vars로 전달해 image pull과 ORAS artifact pull이 같은 주소를 사용하게 하세요.

```bash
./scripts/install/setup_harbor.sh \
  --hostname harbor.internal.example \
  --registry-endpoint harbor.internal.example:32080 \
  --project eva
```

별도 Harbor 서버의 metadata를 배포 controller에 복사한 뒤, k3s 설치와 agent 설치에 같은 파일을
extra vars로 적용합니다. 그러면 containerd image pull과 Qdrant ORAS pull이 모두 실제 Harbor URL을
사용합니다.

```bash
ansible-playbook -i workspace/inventory/inventory.ini src/infra/playbooks/site_infra.yaml \
  -e repository_mode=remote_repository \
  -e @out/work/config/$EVA_SITE_ID/harbor-endpoint.yaml

ansible-playbook -i workspace/inventory/inventory.ini src/solution/playbooks/site_eva_agent.yaml \
  -e repository_mode=remote_repository \
  -e @out/work/config/$EVA_SITE_ID/harbor-endpoint.yaml \
  -e harbor_admin_password='<Harbor admin password>' \
  -e eva_agent_qdrant_values_file=values-k3s.harbor.yaml \
  -e eva_agent_qdrant_snapshot_source=harbor
```

단일/다중 k3s 노드와 별도 Harbor 모두 **Pod에서 도달 가능한** Harbor hostname/IP와 port를
`repository_registry`에 사용해야 합니다. `localhost`는 Harbor canonical address나 Pod용 registry endpoint로 사용할 수 없습니다.

배포 후에는 Qdrant Pod의 두 컨테이너 이미지가 모두 선택한 Harbor를 보는지와 snapshot 복구 상태를
확인합니다.

```bash
kubectl get pods -n eva-agent -l app.kubernetes.io/instance=eva-agent-qdrant \
  -o jsonpath='{range .items[*]}{range .spec.containers[*]}{.image}{"\n"}{end}{end}' | sort -u
kubectl logs -n eva-agent statefulset/eva-agent-qdrant -c qdrant-snapshot-sync --tail=100
kubectl rollout status statefulset/eva-agent-qdrant -n eva-agent --timeout=900s
```

`<harbor_base_url>`가 보이거나 `ErrImagePull`이 나면 배포에 사용한
`values-k3s.harbor.yaml`이 오래된 bundle일 수 있습니다. role은 이 placeholder를
`repository_registry`로 치환하지만, 준비 단계에서는 최신 `download_eva_images.sh`로 image
archive를 다시 만든 뒤 `push_images_to_repository.sh`와
`push_qdrant_snapshots_to_harbor.sh`를 다시 실행하세요.

```bash
export REPOSITORY_REGISTRY="$HARBOR_REGISTRY"
export REPOSITORY_PROJECT=eva
export SNAPSHOT_HARBOR_REGISTRY="$HARBOR_REGISTRY"

kubectl create namespace eva-agent --dry-run=client -o yaml | kubectl apply -f -
kubectl create serviceaccount sa-eva-agent -n eva-agent --dry-run=client -o yaml | kubectl apply -f -
kubectl create secret generic qdrant-snapshot-harbor -n eva-agent \
  --from-literal=username=admin \
  --from-literal=password="${HARBOR_ADMIN_PASSWORD}" \
  --dry-run=client -o yaml | kubectl apply -f -

helm upgrade --install eva-agent-qdrant ./out/cache/qdrant/qdrant-<version>.tgz \
  --namespace eva-agent --create-namespace \
  --values ./out/cache/eva-agent/release/<release>/eva-agent-qdrant/values-k3s.harbor.yaml \
  --set-string image.repository="${REPOSITORY_REGISTRY}/${REPOSITORY_PROJECT}/qdrant" \
  --set-string image.tag=v<version> \
  --set-string sidecarContainers[0].image="${REPOSITORY_REGISTRY}/${REPOSITORY_PROJECT}/eva-agent-qdrant-snapshot-sync:0.1.0" \
  --set-string sidecarContainers[0].env[0].value="${SNAPSHOT_HARBOR_REGISTRY}" \
  --set-string sidecarContainers[0].env[1].value="${REPOSITORY_PROJECT}" \
  --set-string chartTests.dbInteraction.image="${REPOSITORY_REGISTRY}/${REPOSITORY_PROJECT}/bci-base:latest" \
  --post-renderer ./out/cache/eva-agent/release/<release>/plugins/eva-agent-qdrant/post-renderer.sh

kubectl rollout status statefulset/eva-agent-qdrant -n eva-agent --timeout=900s
kubectl logs -n eva-agent statefulset/eva-agent-qdrant -c qdrant-snapshot-sync
```

`docker network create --internal`은 Harbor/ORAS artifact 경로만 별도로 차단 검증할 때 사용하세요. 실제 host k3s 설치의 Pod network는 Docker의 사용자 정의 network에 자동으로 들어가지 않으므로, 이 명령 하나만으로 k3s Pod의 외부 통신이 차단되지는 않습니다. 실제 배포 검증은 서버의 외부 라우팅/DNS를 차단한 상태에서 위 Helm 배포를 실행해야 하며, Docker 기반 k3d 테스트라면 Harbor container와 모든 k3d node를 같은 `--internal` network에 연결하고 values의 `HARBOR_REGISTRY`를 그 network에서 해석되는 Harbor hostname:port로 바꿔야 합니다.

---

## 2. Inventory

cloud_repository 모드 설치:

inventory.ini
```ini
[all]
localhost ansible_connection=local ansible_become_password=<sudo 비밀번호>

[all:vars]
repository_mode=cloud_repository
```

aws_key.ini
```ini
aws_access_key_id = <YOUR_ACCESS_KEY>
aws_secret_access_key = <YOUR_SECRET_KEY>
region = ap-northeast-2
```


원격 서버 설치:

```ini
site-a-eva-node-01 ansible_host=<TARGET_IP_OR_DNS> ansible_user=<SSH_USER> ansible_ssh_private_key_file=<PATH_TO_SSH_KEY>
```

Airgap 서버에 SSH로 접속한 뒤, 해당 서버에서 직접 실행:

```ini
site-a-localhost ansible_connection=local
```

`ansible_connection=local`을 지정하면 Ansible은 localhost에 SSH로 다시 접속하지 않고 현재 로그인한 계정으로 실행합니다.

권장 inventory 경로는 `workspace/inventory/inventory.ini`입니다. 예시는 `workspace/inventory/inventory.ini.sample`을 복사한 뒤 필요한 블록만 주석 해제해서 사용합니다. inventory hostname은 IP가 아니라 사이트를 식별하는 고유한 이름을 사용하고, 실제 접속 주소는 `ansible_host`에 넣습니다.

SSH 키:

```bash
ssh-keygen -t rsa -b 4096
ssh-copy-id {{계정}}@{{IP}}
```

---

## 3. 실행 공통

`check`는 변경 없이 시뮬레이션하는 `--check` 실행입니다. `run`은 실제 서버에 변경을 적용합니다.

실행에 사용할 site 식별자를 먼저 고정하고, 로그 폴더는 site와 playbook별로 생성합니다. `EVA_SITE_ID`는 필수이며 영문자 또는 숫자로 시작하고 영문자, 숫자, 점, 밑줄, 하이픈만 사용할 수 있습니다.

```bash
export EVA_SITE_ID=customer-a
mkdir -p out/work/logs/$EVA_SITE_ID/precondition out/work/logs/$EVA_SITE_ID/infra out/work/logs/$EVA_SITE_ID/gpu-mig out/work/logs/$EVA_SITE_ID/eva out/work/logs/$EVA_SITE_ID/iam out/work/logs/$EVA_SITE_ID/n8n
```

---

## 4. 사전 점검

대상 서버가 EVA 설치를 진행할 수 있는 상태인지 먼저 확인합니다. 이 단계는 서버 설정을 변경하지 않고, 점검 결과를 control node의 `out/work/config/<site>/<target-ip>/precondition.yaml`에 저장합니다.

확인 항목:

- Public outbound 확인: `https://ifconfig.me`로 public IP 조회 가능 여부
- 외부 서비스 DNS/TCP/HTTPS 접근 확인: GitHub, Docker Hub, AWS ECR, S3
- AWS CLI installer 다운로드 가능 여부: `https://awscli.amazonaws.com/awscli-exe-linux-x86_64.zip` partial download
- DNS 설정 확인: `/etc/resolv.conf`
- Routing table 확인: `ip route`
- Kernel 정보 확인: `uname -a`
- Disk 사용량 확인: `df -h`
- MIG 활성화 사전 조건 확인: kernel lockdown, Secure Boot, NVIDIA Display Mode

MIG 관련해서는 아래 명령 결과를 함께 저장합니다.

```bash
cat /sys/kernel/security/lockdown
mokutil --sb-state
nvidia-smi -q | grep -A5 "Display Mode"
```

`precondition.yaml`에서 특히 아래 값을 확인합니다.

- `public_outbound.success`: public IP 조회 가능 여부
- `external_services`: 외부 서비스별 DNS/TCP/HTTPS 접근 결과
- `aws_cli_download.downloadable`: AWS CLI installer 다운로드 가능 여부
- `mig_activation.possible`: MIG 활성화 가능 여부
- `mig_activation.failed_reasons`: MIG 활성화가 어려운 경우 사유

```bash
ANSIBLE_LOG_PATH=out/work/logs/$EVA_SITE_ID/precondition/ansible-internal.log \
.venv/bin/ansible-playbook -i workspace/inventory/inventory.ini src/infra/playbooks/site_precondition.yaml --check 2>&1 | tee out/work/logs/$EVA_SITE_ID/precondition/ansible-check.log

ANSIBLE_LOG_PATH=out/work/logs/$EVA_SITE_ID/precondition/ansible-internal.log \
.venv/bin/ansible-playbook -i workspace/inventory/inventory.ini src/infra/playbooks/site_precondition.yaml -vvv 2>&1 | tee out/work/logs/$EVA_SITE_ID/precondition/ansible-run.log
```

결과 확인:

```bash
ls -l ./out/work/config/<site>/<target-ip>/precondition.yaml
```

---

## 5. 인프라 설치

인프라는 k3s, kubectl, helm, NFS CSI, NVIDIA runtime/device-plugin 등을 구성합니다.

### [cloud_repository]

대상 서버가 인터넷/ECR/Docker Hub에 접근 가능한 경우입니다.

```bash
mkdir -p out/work/logs/$EVA_SITE_ID/infra

ANSIBLE_LOG_PATH=out/work/logs/$EVA_SITE_ID/infra/ansible-internal.log \
.venv/bin/ansible-playbook -i workspace/inventory/inventory.ini src/infra/playbooks/site_infra.yaml --check \
  -e repository_mode=cloud_repository \
  2>&1 | tee out/work/logs/$EVA_SITE_ID/infra/ansible-check.log

ANSIBLE_LOG_PATH=out/work/logs/$EVA_SITE_ID/infra/ansible-internal.log \
.venv/bin/ansible-playbook -i workspace/inventory/inventory.ini src/infra/playbooks/site_infra.yaml \
  -e repository_mode=cloud_repository \
  -vvv 2>&1 | tee out/work/logs/$EVA_SITE_ID/infra/ansible-run.log
```

### [remote_repository]

Main Harbor에서 infra 이미지를 pull하는 경우입니다.

```bash
mkdir -p out/work/logs/$EVA_SITE_ID/infra

ANSIBLE_LOG_PATH=out/work/logs/$EVA_SITE_ID/infra/ansible-internal.log \
.venv/bin/ansible-playbook -i workspace/inventory/inventory.ini src/infra/playbooks/site_infra.yaml --check \
  -e repository_mode=remote_repository \
  -e repository_registry=harbor.main.local:32080 \
  -e repository_project=eva \
  2>&1 | tee out/work/logs/$EVA_SITE_ID/infra/ansible-check.log

ANSIBLE_LOG_PATH=out/work/logs/$EVA_SITE_ID/infra/ansible-internal.log \
.venv/bin/ansible-playbook -i workspace/inventory/inventory.ini src/infra/playbooks/site_infra.yaml \
  -e repository_mode=remote_repository \
  -e repository_registry=harbor.main.local:32080 \
  -e repository_project=eva \
  -vvv 2>&1 | tee out/work/logs/$EVA_SITE_ID/infra/ansible-run.log
```

### [local_repository]

Airgap 서버 내부 Local Harbor에서 infra 이미지를 pull하는 경우입니다.

```bash
mkdir -p out/work/logs/$EVA_SITE_ID/infra

ANSIBLE_LOG_PATH=out/work/logs/$EVA_SITE_ID/infra/ansible-internal.log \
.venv/bin/ansible-playbook -i workspace/inventory/inventory.ini src/infra/playbooks/site_infra.yaml --check \
  -e repository_mode=local_repository \
  -e @out/work/config/$EVA_SITE_ID/harbor-endpoint.yaml \
  2>&1 | tee out/work/logs/$EVA_SITE_ID/infra/ansible-check.log

ANSIBLE_LOG_PATH=out/work/logs/$EVA_SITE_ID/infra/ansible-internal.log \
.venv/bin/ansible-playbook -i workspace/inventory/inventory.ini src/infra/playbooks/site_infra.yaml \
  -e repository_mode=local_repository \
  -e @out/work/config/$EVA_SITE_ID/harbor-endpoint.yaml \
  -vvv 2>&1 | tee out/work/logs/$EVA_SITE_ID/infra/ansible-run.log
```

드라이버 패키지를 지정하려면 추가 변수로 넘깁니다.

```bash
.venv/bin/ansible-playbook -i workspace/inventory/inventory.ini src/infra/playbooks/site_infra.yaml \
  -e repository_mode=local_repository \
  -e @out/work/config/$EVA_SITE_ID/harbor-endpoint.yaml \
  -e gpu_driver_package=nvidia-driver-580 \
  -vvv
```

결과 확인:

```bash
kubectl get nodes
kubectl get ds -n kube-system | grep nvidia-device-plugin
kubectl logs -n kube-system -l name=nvidia-device-plugin --tail=50
```

---

## 6. GPU MIG 설정

MIG 설정은 EVA 환경 설정 전에 수행합니다.

MIG에서 `display_mode_selector`가 필요하면 인터넷 가능 환경에서 미리 받습니다.

```bash
AWS_PROFILE=default AWS_REGION=ap-northeast-2 ./scripts/download/download_display_mode_selector.sh
```

### [cloud_repository]

```bash
mkdir -p out/work/logs/$EVA_SITE_ID/gpu-mig

ANSIBLE_LOG_PATH=out/work/logs/$EVA_SITE_ID/gpu-mig/ansible-internal.log \
.venv/bin/ansible-playbook -i workspace/inventory/inventory.ini src/infra/playbooks/site_gpu_mig.yaml \
  -e repository_mode=cloud_repository \
  -vvv 2>&1 | tee out/work/logs/$EVA_SITE_ID/gpu-mig/ansible-run.log
```

### [remote_repository]

```bash
mkdir -p out/work/logs/$EVA_SITE_ID/gpu-mig

ANSIBLE_LOG_PATH=out/work/logs/$EVA_SITE_ID/gpu-mig/ansible-internal.log \
.venv/bin/ansible-playbook -i workspace/inventory/inventory.ini src/infra/playbooks/site_gpu_mig.yaml \
  -e repository_mode=remote_repository \
  -e repository_registry=harbor.main.local:32080 \
  -e repository_project=eva \
  -vvv 2>&1 | tee out/work/logs/$EVA_SITE_ID/gpu-mig/ansible-run.log
```

### [local_repository]

```bash
mkdir -p out/work/logs/$EVA_SITE_ID/gpu-mig

ANSIBLE_LOG_PATH=out/work/logs/$EVA_SITE_ID/gpu-mig/ansible-internal.log \
.venv/bin/ansible-playbook -i workspace/inventory/inventory.ini src/infra/playbooks/site_gpu_mig.yaml \
  -e repository_mode=local_repository \
  -e @out/work/config/$EVA_SITE_ID/harbor-endpoint.yaml \
  -vvv 2>&1 | tee out/work/logs/$EVA_SITE_ID/gpu-mig/ansible-run.log
```

결과 확인:

```bash
nvidia-smi
```

---

## 7. EVA 환경 설정

`src/solution/playbooks/site_eva_config.yaml`은 EVA 배포에 필요한 설정 파일을 생성합니다. 생성된 값은 `out/work/config/<site>/<target>/eva.yaml`에 저장됩니다.

### [cloud_repository]

```bash
mkdir -p out/work/logs/$EVA_SITE_ID/eva

ANSIBLE_LOG_PATH=out/work/logs/$EVA_SITE_ID/eva/ansible-internal.log \
.venv/bin/ansible-playbook -i workspace/inventory/inventory.ini src/solution/playbooks/site_eva_config.yaml \
  -e repository_mode=cloud_repository \
  -vvv 2>&1 | tee out/work/logs/$EVA_SITE_ID/eva/ansible-config.log
```

### [remote_repository]

```bash
mkdir -p out/work/logs/$EVA_SITE_ID/eva

ANSIBLE_LOG_PATH=out/work/logs/$EVA_SITE_ID/eva/ansible-internal.log \
.venv/bin/ansible-playbook -i workspace/inventory/inventory.ini src/solution/playbooks/site_eva_config.yaml \
  -e repository_mode=remote_repository \
  -e repository_registry=harbor.main.local:32080 \
  -e repository_project=eva \
  -vvv 2>&1 | tee out/work/logs/$EVA_SITE_ID/eva/ansible-config.log
```

### [local_repository]

```bash
mkdir -p out/work/logs/$EVA_SITE_ID/eva

ANSIBLE_LOG_PATH=out/work/logs/$EVA_SITE_ID/eva/ansible-internal.log \
.venv/bin/ansible-playbook -i workspace/inventory/inventory.ini src/solution/playbooks/site_eva_config.yaml \
  -e repository_mode=local_repository \
  -e @out/work/config/$EVA_SITE_ID/harbor-endpoint.yaml \
  -vvv 2>&1 | tee out/work/logs/$EVA_SITE_ID/eva/ansible-config.log
```

결과 확인:

```bash
ls -l ./out/work/config/<site>/<target-ip>/eva.yaml
```

---

## 8. EVA IAM 설치 (SSO, 선택)

EVA IAM을 사용하는 경우 인프라 설치 후, EVA App을 포함한 Solution 배포 전에 실행합니다. IAM을 사용하지 않는 설치에서는 이 절을 건너뜁니다.

IAM Chart의 고객 변경분은 선택된 workspace의 `site-values/iam.yaml`에서 관리합니다. sample을 복사한 뒤 실제 Secret은 Git에 커밋하지 마세요.

```bash
cp workspace/site-values/iam.yaml.sample workspace/site-values/iam.yaml
```

`iam.yaml`은 chart values를 그대로 사용합니다. 한 대상에 공통 적용할 값은 최상위에 쓰고, 대상별 값을 분리할 때는 `ansible_host`가 있으면 그 값, 없으면 inventory hostname을 최상위 키로 사용합니다. 운영 환경에서는 `keycloak.realmPatch.realmAdmin.password`와 database/Redis password를 반드시 실제 Secret으로 바꾸세요.

```yaml
# workspace/site-values/iam.yaml
10.0.0.10:
  config:
    host: iam.customer.example
  keycloak:
    realmPatch:
      realmAdmin:
        password: "<STRONG_REALM_ADMIN_PASSWORD>"
  ingress:
    path: /iam
    tls:
      hostPath: /home/eva/certs
```

실행 전에 대상 서버의 `eva_iam_tls_host_path`(기본값 `/home/eva/certs`)에 `tls.crt`와 `tls.key`가 있어야 합니다. `eva_iam_host` 또는 `config.host`에는 포트를 붙이지 마세요. 기본 HTTPS 포트가 아닌 경우에는 현재 catalog의 IAM Chart가 `config.publicPort`/`config.publicUrl`을 지원하는지 먼저 확인해야 합니다.

App과 같은 host를 사용할 때는 IAM ingress path를 `/iam`처럼 App의 path와 겹치지 않게 지정합니다. EVA App이 IAM Redis를 사용하도록 구성하는 경우에는 Redis external/TLS/NodePort도 함께 켜야 합니다.

### [cloud_repository]

```bash
mkdir -p out/work/logs/$EVA_SITE_ID/iam

ANSIBLE_LOG_PATH=out/work/logs/$EVA_SITE_ID/iam/ansible-internal.log \
.venv/bin/ansible-playbook -i workspace/inventory/inventory.ini src/solution/playbooks/site_eva_iam.yaml \
  -e repository_mode=cloud_repository \
  -e eva_iam_host=iam.customer.example \
  -e eva_iam_ingress_path=/iam \
  -e eva_iam_redis_external_enabled=true \
  -e eva_iam_redis_tls_enabled=true \
  -e eva_iam_redis_nodeport=32070 \
  -e '{"eva_iam_app_redirect_uris": ["https://app.customer.example/*"]}' \
  -vvv 2>&1 | tee out/work/logs/$EVA_SITE_ID/iam/ansible-run.log
```

### [remote_repository]

```bash
mkdir -p out/work/logs/$EVA_SITE_ID/iam

ANSIBLE_LOG_PATH=out/work/logs/$EVA_SITE_ID/iam/ansible-internal.log \
.venv/bin/ansible-playbook -i workspace/inventory/inventory.ini src/solution/playbooks/site_eva_iam.yaml \
  -e repository_mode=remote_repository \
  -e repository_registry=harbor.main.local:32080 \
  -e repository_project=eva \
  -e eva_iam_host=iam.customer.example \
  -e eva_iam_ingress_path=/iam \
  -e eva_iam_redis_external_enabled=true \
  -e eva_iam_redis_tls_enabled=true \
  -e eva_iam_redis_nodeport=32070 \
  -e '{"eva_iam_app_redirect_uris": ["https://app.customer.example/*"]}' \
  -vvv 2>&1 | tee out/work/logs/$EVA_SITE_ID/iam/ansible-run.log
```

### [local_repository]

`out/cache/eva-iam/` Chart와 IAM 이미지는 1-7의 offline asset/image 준비에 포함되어 있어야 합니다.

```bash
mkdir -p out/work/logs/$EVA_SITE_ID/iam

ANSIBLE_LOG_PATH=out/work/logs/$EVA_SITE_ID/iam/ansible-internal.log \
.venv/bin/ansible-playbook -i workspace/inventory/inventory.ini src/solution/playbooks/site_eva_iam.yaml \
  -e repository_mode=local_repository \
  -e @out/work/config/$EVA_SITE_ID/harbor-endpoint.yaml \
  -e eva_iam_host=iam.customer.example \
  -e eva_iam_ingress_path=/iam \
  -e eva_iam_redis_external_enabled=true \
  -e eva_iam_redis_tls_enabled=true \
  -e eva_iam_redis_nodeport=32070 \
  -e '{"eva_iam_app_redirect_uris": ["https://app.customer.example/*"]}' \
  -vvv 2>&1 | tee out/work/logs/$EVA_SITE_ID/iam/ansible-run.log
```

배포 후 Keycloak rollout과 IAM endpoint를 확인합니다.

```bash
kubectl rollout status deployment/eva-iam-keycloak -n eva-iam --timeout=600s
curl -fsS https://iam.customer.example/iam/realms/eva-iam/.well-known/openid-configuration
```

IAM과 App이 같은 EVA CLI Operation에 포함되면, IAM role은 control node의 `out/work/config/<site>/<target>/eva-iam.yaml`에 App SSO handoff를 권한 `0600`으로 기록하고 App role이 이를 자동으로 읽어 `app.sso.baseUrl`, `app.sso.adminClientSecret`에 병합합니다. Workspace의 `site-values/app.yaml`은 설치자 입력으로 유지되며 실행 중 수정하지 않습니다. IAM handoff가 없거나 SSO 값이 비어 있으면 App 단계는 secret을 출력하지 않고 명확한 오류로 중단합니다.

App-only Operation은 `workspace/site-values/app.yaml`의 `app.sso`를 공식 입력으로 사용한다. 이 값이 없을 때만 현재 Workspace의 동일 site/target metadata를 통과한 handoff를 재사용하며, 다른 site 또는 target의 handoff는 자동으로 사용하지 않는다. 중앙 IAM을 별도 서버 또는 Workspace에 구성하는 경우에는 각 App Workspace의 `app.sso.baseUrl`, `app.sso.adminClientSecret`에 중앙 IAM 값을 명시한다.

---

## 9. EVA 배포

`src/solution/playbooks/site_eva.yaml`은 EVA Agent, EVA Vision, EVA App을 배포합니다.

배포 시 Helm values는 아래 순서로 적용합니다.

- chart 기본 values
- release에 포함된 k3s values 또는 secret values
- 이 repository의 `src/solution/values/*-k3s-override.yaml.j2`
- `7. EVA 환경 설정`에서 생성된 `out/work/config/<site>/<target>/eva.yaml` 기반 override

`src/solution/values/` 폴더의 파일은 전체 values 사본이 아니라, EVA deployer가 책임지는 k3s/repository override만 담습니다. `repository_mode`, `repository_registry`, `repository_project`에 따른 image repository 변경과 k3s 실행에 필요한 값은 여기서 관리하고, 환경별 App/Agent 설정은 `site_eva_config.yaml`이 생성한 `out/work/config/<site>/<target>/eva.yaml` 값을 배포 단계에서 추가 override로 반영합니다.

EVA App의 호스트별 설정은 선택된 workspace의 `site-values/app.yaml` 하나에서 관리합니다. 이 파일에는 license credential이 포함될 수 있으므로 Git에 커밋하지 않습니다. 저장소에는 `workspace/site-values/app.yaml.sample`만 포함합니다.

처음 설치할 때 sample을 복사합니다.

```bash
cp workspace/site-values/app.yaml.sample workspace/site-values/app.yaml
```

현재 구현에서 `workspace/site-values/app.yaml`의 최상위 키는 inventory에 `ansible_host`를 지정했다면 그 값, 없으면 inventory hostname과 동일하게 맞춥니다. license credential은 배포 환경별로 다르므로, `workspace/site-values/app.yaml.sample`에서 대상 환경의 블록을 선택해 주석을 해제하고 해당 환경에 발급된 키를 사용합니다. prod와 dev의 credential을 섞어 사용하면 안 됩니다.

```yaml
site-a-localhost:
  app:
    browserTitleName: "EVA SHEE (서초)"
    license:
      activation_mode: "offline"
      product_code: "eva-prod"
      api_key: "<PROD_API_KEY>"
      shared_key: "<PROD_SHARED_KEY>"
    pipeline:
      streamer:
        dispatcherFaceAnonymizerEnabled: true
```

prod는 `activation_mode: "offline"`, `product_code: "eva-prod"`와 prod용 API/shared key를 사용합니다. dev는 `activation_mode: "online"`, `product_code: "eva-dev"`와 dev용 API/shared key를 사용합니다.

예를 들어 `ansible_host=10.0.0.10`인 dev inventory에는 `10.0.0.10:` 아래에 dev 설정을 작성합니다. `ansible_host`를 쓰지 않는 inventory라면 `site-a-eva-node-01:`처럼 inventory hostname을 사용합니다. 해당 호스트 키가 없으면 기존 공통 설정만 적용됩니다. `workspace/site-values/app.yaml`이 없으면 호스트별 override 없이 배포합니다.

배포 중 렌더링된 최종 override values는 control node의 `out/work/rendered/<site>/<target>/` 아래에 component별로 남습니다.

```text
out/work/rendered/<site>/<target>/
  app/app-k3s-override.yaml
  vision/vision-k3s-override.yaml
  agent/agent-k3s-override.yaml
  qdrant/qdrant-k3s-override.yaml
  vllm/vllm-k3s-override.yaml
  vllm/values-override-from-config.yaml
```

`vllm/values-override-from-config.yaml`은 `out/work/config/<site>/<target>/eva.yaml`에 vLLM override 값이 있을 때만 생성됩니다.

### [cloud_repository]

## 1. TLSStore 생성 (IP 사용시에만)

```bash
kubectl create secret tls eva-tls-for-traefik \
  -n kube-system \
  --cert=/home/eva/certs/tls.crt \
  --key=/home/eva/certs/tls.key
```

```bash
cat <<EOF | kubectl apply -f -
apiVersion: traefik.io/v1alpha1
kind: TLSStore
metadata:
  name: default
  namespace: kube-system
spec:
  defaultCertificate:
    secretName: eva-tls-for-traefik
EOF
```

## 2. EVA IAM 설치

사전 확인

```bash
ls -ld /home/home/.aws
ls -ld /home/eva/certs
```

설치 명령어 실행

eva_iam_host: 실제 사용할 host url (IP or DNS)
eva_iam_ingress_path: 주소 뒤 구분 path
eva_iam_app_redirect_uris: eva-app에서 사용할 https url + /*

**설치 시 BECOME password는 sudo 비밀번호입니다.** 

```bash
ansible-playbook -i 'localhost,' -c local site_eva_iam.yaml -K \
  -e repository_mode=cloud_repository \
  -e eva_iam_host=10.158.2.185 \
  -e eva_iam_node_user=eva \
  -e eva_iam_ingress_path=/iam \
  -e eva_iam_redis_external_enabled=true \
  -e eva_iam_redis_tls_enabled=true \
  -e eva_iam_redis_nodeport=32070 \
  -e '{"eva_iam_app_redirect_uris": ["https://10.158.2.185/*"]}'

```
이 레거시 Ansible 예시는 EVA CLI Operation의 자동 handoff 경로를 사용하지 않는다. 현재 설치 절차에서는 `eva install`을 사용하며, 중앙 IAM을 별도 서버 또는 Workspace에 구성할 때만 App Workspace의 `site-values/app.yaml`에 `app.sso`를 명시한다. generated handoff의 Secret을 사람이 다른 App 입력으로 복사하지 않는다.

## 4. EVA APP 설치

```bash
ansible-playbook -i 'localhost,' -c local site_eva_app.yaml -K \
  -e repository_mode=cloud_repository \
  -e repository_registry=localhost:32080 \
  -e eva_app_backend_host=10.158.2.185 \
  -e eva_app_backend_secure=true
```

## 5. EVA Vision Secret 생성

네임스페이스가 없는 경우 아래 명령어 실행
```bash
kubectl create namespace eva-visionkubectl create namespace eva-vision
```

```bash
ECR_PASSWORD=$(aws ecr get-login-password --region ap-northeast-2)

kubectl create secret docker-registry eva-vision-regcred \
  -n eva-vision \
  --docker-server=339713051385.dkr.ecr.ap-northeast-2.amazonaws.com \
  --docker-username=AWS \
  --docker-password="$ECR_PASSWORD"
```

## 6. EVA Agent Secret 생성

네임스페이스가 없는 경우 아래 명령어 실행
```bash
kubectl create namespace eva-visionkubectl create namespace eva-agent
```
```bash
ECR_PASSWORD=$(aws ecr get-login-password --region ap-northeast-2)

kubectl create secret docker-registry eva-agent-dockerconfig \
  -n eva-agent \
  --docker-server=339713051385.dkr.ecr.ap-northeast-2.amazonaws.com \
  --docker-username=AWS \
  --docker-password="$ECR_PASSWORD"
```

## 8. EVA Agent AWS Secret 생성

```bash
AWS_ACCESS_KEY_ID=$(aws configure get aws_access_key_id)
AWS_SECRET_ACCESS_KEY=$(aws configure get aws_secret_access_key)
AWS_REGION=$(aws configure get region)

kubectl create secret generic aws-credentials \
  -n eva-agent \
  --from-literal=AWS_ACCESS_KEY_ID="$AWS_ACCESS_KEY_ID" \
  --from-literal=AWS_SECRET_ACCESS_KEY="$AWS_SECRET_ACCESS_KEY" \
  --from-literal=AWS_REGION="${AWS_REGION:-ap-northeast-2}"
```

## 9. EVA-Vision, EVA-Agent 설치

```bash
mkdir -p out/work/logs/$EVA_SITE_ID/eva

ANSIBLE_LOG_PATH=out/work/logs/$EVA_SITE_ID/eva/ansible-internal.log \
.venv/bin/ansible-playbook -i workspace/inventory/inventory.ini src/solution/playbooks/site_eva.yaml \
  -e repository_mode=cloud_repository \
  -vvv 2>&1 | tee out/work/logs/$EVA_SITE_ID/eva/ansible-run-eva.log
```
kustomize 패키지 설치 시 에러 발생하는 경우, 수동 설치 후 명령어 실행 sudo ln -s /snap/bin/kustomize /usr/local/bin/kustomize 다음 재설치


### [remote_repository]

```bash
mkdir -p out/work/logs/$EVA_SITE_ID/eva

ANSIBLE_LOG_PATH=out/work/logs/$EVA_SITE_ID/eva/ansible-internal.log \
.venv/bin/ansible-playbook -i workspace/inventory/inventory.ini src/solution/playbooks/site_eva.yaml \
  -e repository_mode=remote_repository \
  -e repository_registry=harbor.main.local:32080 \
  -e repository_project=eva \
  -vvv 2>&1 | tee out/work/logs/$EVA_SITE_ID/eva/ansible-run-eva.log
```

### [local_repository]

```bash
mkdir -p out/work/logs/$EVA_SITE_ID/eva

ANSIBLE_LOG_PATH=out/work/logs/$EVA_SITE_ID/eva/ansible-internal.log \
.venv/bin/ansible-playbook -i workspace/inventory/inventory.ini src/solution/playbooks/site_eva.yaml \
  -e repository_mode=local_repository \
  -e @out/work/config/$EVA_SITE_ID/harbor-endpoint.yaml \
  -vvv 2>&1 | tee out/work/logs/$EVA_SITE_ID/eva/ansible-run-eva.log
```

vLLM GPU 프로파일을 지정하려면 추가 변수로 넘깁니다.

```bash
.venv/bin/ansible-playbook -i workspace/inventory/inventory.ini src/solution/playbooks/site_eva.yaml \
  -e repository_mode=local_repository \
  -e @out/work/config/$EVA_SITE_ID/harbor-endpoint.yaml \
  -e eva_agent_vllm_profile=A6000x1 \
  -vvv
```

지원 프로파일:

- `A6000x1`
- `L40sx1`
- `PRO5000x3`
- `PRO6000-MIGx4`

결과 확인:

```bash
kubectl get pods -A
kubectl get svc -A
```

기본 포트는 `http://<target-ip>:32010/`입니다.

---

## 10. n8n 설치 (Optional)

n8n은 EVA 설치와 분리해서 별도 playbook으로 실행합니다.

### [cloud_repository]

```bash
mkdir -p out/work/logs/$EVA_SITE_ID/n8n

ANSIBLE_LOG_PATH=out/work/logs/$EVA_SITE_ID/n8n/ansible-internal.log \
.venv/bin/ansible-playbook -i workspace/inventory/inventory.ini src/solution/playbooks/site_n8n.yaml \
  -e repository_mode=cloud_repository \
  -vvv 2>&1 | tee out/work/logs/$EVA_SITE_ID/n8n/ansible-run.log
```

### [remote_repository]

Main Harbor에 아래 이미지가 준비되어 있어야 합니다.

```text
harbor.main.local:32080/eva/n8n:2.32.7
```

```bash
mkdir -p out/work/logs/$EVA_SITE_ID/n8n

ANSIBLE_LOG_PATH=out/work/logs/$EVA_SITE_ID/n8n/ansible-internal.log \
.venv/bin/ansible-playbook -i workspace/inventory/inventory.ini src/solution/playbooks/site_n8n.yaml \
  -e repository_mode=remote_repository \
  -e repository_registry=harbor.main.local:32080 \
  -e repository_project=eva \
  -vvv 2>&1 | tee out/work/logs/$EVA_SITE_ID/n8n/ansible-run.log
```

### [local_repository]

Local Harbor에 아래 이미지가 준비되어 있어야 합니다.

```text
<NODE_IP_OR_DNS>:32080/eva/n8n:2.32.7
```

```bash
mkdir -p out/work/logs/$EVA_SITE_ID/n8n

ANSIBLE_LOG_PATH=out/work/logs/$EVA_SITE_ID/n8n/ansible-internal.log \
.venv/bin/ansible-playbook -i workspace/inventory/inventory.ini src/solution/playbooks/site_n8n.yaml \
  -e repository_mode=local_repository \
  -e @out/work/config/$EVA_SITE_ID/harbor-endpoint.yaml \
  -vvv 2>&1 | tee out/work/logs/$EVA_SITE_ID/n8n/ansible-run.log
```

결과 확인:

```bash
kubectl get all -n n8n
```

브라우저에서 `http://<target-ip>:30678` 접속 후 n8n UI를 확인합니다.
