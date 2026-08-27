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

세 배포 모드는 서로 다른 제품 또는 별도 Helm Chart가 아닙니다. 고객 Network 환경에 따라 Image 공급 경로만 달라지며, 실제 EVA Application 구성과 Version은 동일하게 유지합니다. EVA App·Agent·Vision과 Chart Version은 `versions.json`에서 단일하게 관리하고, `repository_registry`와 `repository_project`로 배포 대상 Repository를 지정합니다. EVA Deployer도 EVA 버전과 동일한 기준으로 버저닝하며, 특정 EVA 버전을 배포할 때 해당 버전을 지원하는 Deployer 버전을 함께 사용합니다.

이 구조는 하나의 EVA Release를 인터넷 연결 환경, 제한된 내부망, 완전 폐쇄망에 반복 배포하면서도 고객별 Version 분기를 만들지 않도록 합니다.

### Offline Delivery

`local_repository` 환경에서는 필요한 항목을 표준 Offline Release Package로 사전에 준비합니다.

- EVA 및 Infrastructure Container Image
- AI Model, Helm Chart, 설치 Asset
- Python·Ansible 패키지와 일반 apt 의존성 bundle
- Harbor Offline Installer
- Version Manifest와 설치·검증 Script

폐쇄망에서는 누락된 Package 하나로 설치가 중단될 수 있으므로, 설치 전에 Package 구성·Version·manifest를 검증해야 합니다. 모드별 준비 절차와 Repository 주소 규칙은 [1. 사전 준비 및 Repository 준비](#1-사전-준비-및-repository-준비)에서 설명합니다.

---

## 1. 사전 준비 및 Repository 준비

이 섹션에서는 설치 전에 필요한 패키지·asset·이미지·Harbor를 준비합니다. 먼저 아래에서 하나의 Repository 모드를 선택하고, 해당 모드의 절차만 진행하세요. 실제 Ansible 설치 명령은 이후 `인프라 설치`, `EVA 배포`, `n8n 설치` 섹션에서 같은 모드로 실행합니다.

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

`repository_registry`에는 `https://`를 제외한 **k3s 노드에서 실제 접근 가능한 주소**를 지정합니다. 단일 서버 Local Harbor의 기본값은 `localhost:32080`이지만, 여러 노드에서는 각 노드가 공통으로 접근 가능한 Harbor hostname 또는 IP를 사용해야 합니다.

### 1-2. 모든 모드 공통: 버전 단일 관리

EVA App/Agent/Vision 및 Helm Chart 버전은 `versions.json` 한 곳에서 관리합니다. EVA Deployer도 EVA 버전과 동일한 기준으로 버저닝합니다. 따라서 특정 EVA 버전을 배포할 때는 해당 버전을 지원하는 Deployer 버전을 함께 사용해야 합니다.
버전 업데이트 시 우선 이 파일만 수정하면:

- Ansible 배포(`site_eva.yaml`)에 자동 반영
- 다운로드 스크립트(`install/download_offline_assets.sh`, `install/download_eva_images.sh`, `install/download_n8n_images.sh`)에 자동 반영

이미지/asset 다운로드 전에 먼저 `versions.json`을 수정하세요.

예시:

```json
{
  "eva_app_deploy_version": "3.0.5",
  "eva_app_chart_version": "2.1.3",
  "eva_vision_deploy_version": "2.0.5",
  "eva_vision_chart_version": "2.0.5",
  "n8n_image": "docker.n8n.io/n8nio/n8n:2.32.7"
}
```

Harbor를 사용하는 모드에서는 이미지가 아래 형태로 저장됩니다.

```text
<repository_registry>/<repository_project>/eva-agent:<version>
<repository_registry>/<repository_project>/eva-app:<version>
<repository_registry>/<repository_project>/eva-vision:<version>
<repository_registry>/<repository_project>/n8n:<version>
```

### 1-3. remote_repository/local_repository 공통: Airgap 설치 자산과 패키지 bundle

Airgap 서버에 Docker가 설치되어 있지 않을 수 있으므로, 인터넷이 가능한 준비 서버에서 `install/download_offline_assets.sh`를 실행하면 Docker Engine, containerd, buildx, compose plugin 및 apt 의존성 `.deb` 파일도 `install/docker/debs/`에 함께 준비됩니다. `install/` 전체를 Airgap 서버로 복사한 뒤 설치를 실행하세요.

이 스크립트는 Docker `.deb` 패키지와 실제 설치에 필요한 의존성 전체를 받기 위해 `apt-get update`를 실행하므로, 준비 서버에서 `root` 또는 비밀번호 없이 사용할 수 있는 `sudo` 권한이 필요합니다. 일반 사용자로 실행하는 경우 먼저 터미널에서 `sudo -v`를 실행해 인증한 뒤 다운로드 명령을 실행하세요. 준비 서버와 Airgap 서버는 같은 Ubuntu 릴리스 및 아키텍처를 사용해야 하며, 실행할 때마다 기존 `install/docker/debs/*.deb`는 최신 의존성 묶음으로 교체됩니다. 다운로드 후 `install/docker/debs/manifest.txt`가 생성되었는지도 확인하세요. 기존에 생성한 Docker `.deb` 묶음은 의존성이 부족할 수 있으므로 수정된 스크립트로 반드시 다시 생성해야 합니다.

`install/apt/debs/` bundle은 Ansible base/NFS role에 필요한 `unzip`, `curl`, `gnupg`, `nfs-kernel-server`, `nfs-common`, `keyutils`와 그 일반 의존성을 준비합니다. `systemd`, `udev`, `libsystemd0`, `libudev1`, `dpkg`, `libc6` 같은 **OS 핵심 패키지는 bundle 및 Ansible base/NFS 설치 대상에서 제외**합니다. 준비 서버와 Airgap 서버의 Ubuntu patch level이 달라도 배포 중 OS 핵심 패키지가 섞여 설치되어 의존성이 깨지지 않게 하기 위함입니다.

`local_repository`/`remote_repository`용 offline asset을 준비한 뒤에는 `install/apt/debs/` 전체와 `manifest.txt`를 함께 전달하세요. Airgap Ansible은 설치 전에 bundle의 package dependency를 검사하며, 설치 시에는 `apt-get --no-download`로 bundle 안의 일반 패키지만 설치합니다.

Airgap 서버에서 offline `.deb` 설치가 중간에 실패했을 때, `systemd`/`udev`와 무관한 bundle 문제는 아래 명령으로 검사하고 복구합니다. `systemd`, `udev`, `libsystemd0`, `libudev1` 오류가 보이면 이 명령으로 bundle 전체를 재설치하지 말고 다음 `systemd recovery` 절차를 사용하세요.

```bash
cd /home/eva/eva-deployer
./install/validate_offline_debs.sh ./install/apt/debs
sudo ./install/repair_offline_debs.sh ./install/apt/debs
sudo dpkg --audit
```

#### systemd recovery: 이미 OS 핵심 패키지 version mismatch가 발생한 경우

기존 bundle 설치가 `libsystemd0` 또는 `libudev1`만 다른 patch version으로 바꿔 `systemd`/`udev` 의존성 오류가 발생한 경우, 전체 `install/` bundle을 다시 만들 필요가 없습니다. 인터넷 가능 준비 서버에서 현재 준비 서버와 같은 systemd 세트만 `install/recovery/`에 준비합니다.

```bash
# 준비 서버: 현재 systemd 세트의 버전을 확인
dpkg-query -W -f='${Version}\n' systemd

# 예: 255.4-1ubuntu8.16 systemd lockstep package 12개 준비
./install/recovery/prepare.sh 255.4-1ubuntu8.16
```

생성된 `install/recovery/` 전체만 Airgap 서버로 전달합니다. Airgap 서버에서는 Ansible을 다시 실행하기 전에 아래 명령으로 recovery package 12개를 같은 version으로 설치합니다.

```bash
cd /home/eva/eva-deployer/install/recovery
./recover.sh

dpkg-query -W -f='${Package}\t${Version}\n' \
  systemd systemd-sysv udev libsystemd0 libudev1
sudo dpkg --audit
```

`recover.sh`가 성공하면 위 5개 package는 모두 `systemd-version.txt`에 기록된 동일 version이어야 하며, `sudo dpkg --audit` 출력은 없어야 합니다. 자세한 동작은 `install/recovery/README.md`를 참고하세요.

### 1-4. Cloud/Remote/Local 준비 서버: AWS 인증

ECR 이미지, S3 모델, release asset을 인터넷 가능 환경에서 받을 때 필요합니다. `cloud_repository`는 대상 서버(또는 Ansible 실행 서버)에, `remote_repository`와 `local_repository`는 인터넷 가능 준비 서버에 설정하세요.

```bash
aws configure set aws_access_key_id <AK> --profile default
aws configure set aws_secret_access_key <SK> --profile default
aws configure set region ap-northeast-2 --profile default
```

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
./install/install_docker.sh

# Docker 설치 및 실행을 확인한 뒤 Harbor를 시작합니다.
./install/setup_harbor.sh --hostname harbor.main.local
```

비밀번호나 데이터 저장 경로를 바꾸고 싶으면 인자로 넘깁니다.

```bash
./install/setup_harbor.sh \
  --hostname harbor.main.local \
  --admin-password 'your-admin-password' \
  --data-volume /data001/harbor
```

Harbor 기본값:

- `HARBOR_VERSION`: `v2.15.2`
- `http.port`: `32080` 고정
- `hostname`: `localhost`
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
sudo -v && AWS_PROFILE=default ./install/download_offline_assets.sh

# EVA 모델 캐시 다운로드 -> install/models
AWS_PROFILE=default ./install/download_eva_models.sh

# EVA/infra/n8n 이미지 pull 및 이미지 목록 생성
AWS_PROFILE=default ./install/download_eva_images.sh
./install/download_infra_images.sh
./install/download_n8n_images.sh
```

이미지를 Main Harbor로 push합니다.

```bash
# EVA 이미지 push
REPOSITORY_REGISTRY=harbor.main.local:32080 \
REPOSITORY_PROJECT=eva \
AWS_PROFILE=default \
./install/push_images_to_repository.sh

# Infra 이미지 push: nvidia-device-plugin, CUDA sample, MIG 검증용 CUDA
IMAGE_LIST=./install/images/infra-images-pulled.txt \
REPOSITORY_REGISTRY=harbor.main.local:32080 \
REPOSITORY_PROJECT=eva \
./install/push_images_to_repository.sh

# n8n 이미지 push
IMAGE_LIST=./install/images/n8n-images.txt \
REPOSITORY_REGISTRY=harbor.main.local:32080 \
REPOSITORY_PROJECT=eva \
./install/push_images_to_repository.sh
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
harbor.main.local:32080/eva/n8n:1.103.2
```

`harbor.main.local:32080`을 Docker 또는 k3s에서 HTTP registry로 사용할 경우, 해당 노드의 Docker/containerd에 insecure registry 또는 인증서 신뢰 설정이 필요할 수 있습니다.

### 1-7. local_repository: USB로 전달한 이미지를 대상 서버 Local Harbor에 저장

인터넷 가능 환경에서 필요한 파일을 모두 준비한 뒤, `eva-deployer` 폴더를 USB로 Airgap 서버에 복사합니다. Airgap 서버 내부에는 Local Harbor를 띄우고, k3s는 해당 Harbor에서 이미지를 pull합니다.

1. **인터넷 가능 준비 서버**에서 Ansible wheel, Docker/apt bundle, asset, 모델, 이미지, Harbor installer를 모두 받습니다.
2. `repository-images.tar`와 `eva-deployer` 전체를 USB 등으로 **Airgap 서버**에 복사합니다.
3. **Airgap 서버**에서 Python/Ansible, Docker, Local Harbor를 설치하고 archive 이미지를 Harbor에 push합니다.
4. 이후 Airgap 서버에서 Ansible을 실행하며 `repository_registry=localhost:32080`을 지정합니다.

#### A. 인터넷 가능 준비 서버: 설치 파일과 이미지 준비

인터넷 가능 환경에서 Airgap 서버용 Python/Ansible 패키지, asset, 이미지, Harbor offline installer를 준비합니다.

```bash
# Airgap 서버에서 Python venv/Ansible 설치에 필요한 패키지 다운로드
./install/download_python_venv_debs.sh
./install/download_ansible_wheels.sh

# chart/values/script/kustomize/manifest 등 설치 asset 다운로드
# Docker Engine/Compose와 Ansible base/NFS role용 일반 apt 의존성 .deb bundle도 함께 준비됩니다.
# Docker apt 패키지 다운로드를 위해 sudo 인증 후 실행
sudo -v && AWS_PROFILE=default ./install/download_offline_assets.sh

# EVA 모델 캐시 다운로드 -> install/models
AWS_PROFILE=default ./install/download_eva_models.sh

# EVA/infra/n8n 이미지 pull 및 이미지 목록 생성
AWS_PROFILE=default ./install/download_eva_images.sh
./install/download_infra_images.sh
./install/download_n8n_images.sh

# Airgap 서버에서 Local Harbor 설치에 필요한 Harbor offline installer 다운로드
./install/setup_harbor.sh --download-only
```

USB로 이미지를 옮기기 위해 Docker image archive를 생성합니다. 이 archive는 k3s에 직접 import하는 용도가 아니라, Airgap 서버의 Local Harbor에 이미지를 seed하기 위한 용도입니다.

```bash
cat ./install/images/images-pulled.txt \
    ./install/images/infra-images-pulled.txt \
    ./install/images/n8n-images.txt \
  | sort -u > ./install/images/repository-images.txt

docker save \
  -o ./install/images/repository-images.tar \
  $(cat ./install/images/repository-images.txt)
```

저장 공간이 부족하면 `docker save` 후 인터넷 가능 환경의 Docker image를 삭제해도 됩니다. `repository-images.tar`와 `eva-deployer` 폴더를 Airgap 서버로 옮긴 뒤에는 Airgap 서버에서 다시 `docker load`합니다.

```bash
docker image rm $(cat ./install/images/repository-images.txt)
```

`eva-deployer` 폴더를 저장 매체로 복사하여 Airgap 서버로 이동합니다.

#### B. Airgap 서버: 실행 환경과 Local Harbor 설치

다음 순서로 진행합니다. Python/Ansible은 Ansible 실행을 위해 먼저 설치하고, Local Harbor는 Docker Engine과 Compose plugin 설치가 완료된 뒤 시작합니다.

##### 1. Python/Ansible 설치

```bash
./install/install_python_venv_airgap.sh
./install/install_ansible_airgap.sh
source .venv/bin/activate
ansible --version
```

##### 2. Docker Engine과 Compose plugin 설치

준비된 Docker `.deb` bundle로 Docker Engine과 Compose plugin을 설치합니다.

```bash
# install/ 전체가 복사된 airgap 서버에서 Docker 설치
sudo ./install/install_docker.sh --airgap
```

`docker` 그룹 권한을 적용하려면 SSH 세션을 종료한 뒤 다시 접속하세요.

##### 3. Local Harbor 설치 또는 이전

Local Harbor의 실행 설정은 `eva-deployer` 밖의 `~/.local/share/eva-harbor`에 저장합니다. 이후에는 Harbor를 중지하지 않고 `eva-deployer` 전체를 다시 동기화할 수 있습니다.

새로 설치하는 경우 아래 명령을 실행합니다. Harbor는 `localhost:32080`으로 실행됩니다. 최초 설치의 관리자 계정은 `admin`, 기본 비밀번호는 `EVA123@`입니다. 운영 환경에서는 `--admin-password`로 변경하세요.

```bash
./install/setup_harbor.sh \
  --hostname localhost \
  --install-root ~/.local/share/eva-harbor
```

기존에 `eva-deployer/install/harbor/harbor`에 Harbor를 설치했다면, 위의 새 설치 대신 기존 Harbor를 중지한 뒤 외부 runtime 경로로 한 번 이전합니다.

```bash
cd /home/eva/eva-deployer/install/harbor/harbor
sudo docker compose down

cd /home/eva/eva-deployer
./install/setup_harbor.sh \
  --hostname localhost \
  --install-root ~/.local/share/eva-harbor
```

기존 Harbor의 volume 경로만 바꾸려면 `--data-volume`을 추가합니다. 기존 `harbor.yml`의 관리자 비밀번호는 별도로 지정하지 않으면 유지됩니다. 기존 Harbor 데이터는 자동 복사하지 않으므로, 새 경로는 빈 registry 저장소로 시작합니다.

```bash
./install/setup_harbor.sh \
  --hostname localhost \
  --install-root ~/.local/share/eva-harbor \
  --data-volume /data001/harbor
```

`repository-images.tar`는 Harbor에 직접 push할 수 없습니다. Docker daemon에 load한 뒤 Local Harbor로 push해야 합니다. push 중에는 archive, Docker image cache, Harbor registry가 일시적으로 모두 저장 공간을 사용합니다.

#### C. Airgap 서버: Local Harbor에 이미지 seed

Airgap 서버에서 이미지를 load한 뒤 Local Harbor로 push합니다.
`push_images_to_repository.sh`는 `localhost:32080`의 `harbor.yml`에서 관리자 비밀번호를 읽어 자동으로 로그인합니다.

```bash
docker load -i ./install/images/repository-images.tar

# EVA 이미지 push
PULL_SOURCE_IMAGES=false \
REPOSITORY_REGISTRY=localhost:32080 \
REPOSITORY_PROJECT=eva \
./install/push_images_to_repository.sh

# Infra 이미지 push
PULL_SOURCE_IMAGES=false \
IMAGE_LIST=./install/images/infra-images-pulled.txt \
REPOSITORY_REGISTRY=localhost:32080 \
REPOSITORY_PROJECT=eva \
./install/push_images_to_repository.sh

# n8n 이미지 push
PULL_SOURCE_IMAGES=false \
IMAGE_LIST=./install/images/n8n-images.txt \
REPOSITORY_REGISTRY=localhost:32080 \
REPOSITORY_PROJECT=eva \
./install/push_images_to_repository.sh
```

Harbor에 저장된 repository를 확인합니다.

```bash
curl -fsS -u "admin:${HARBOR_ADMIN_PASSWORD:-EVA123@}" \
  'http://localhost:32080/api/v2.0/repositories?project_name=eva&page_size=100' \
  | python3 -c 'import json, sys; print("\n".join(item["name"] for item in json.load(sys.stdin)))'
```

Harbor 확인 후 Docker cache와 archive가 더 이상 필요 없으면 삭제해 공간을 확보할 수 있습니다.

```bash
docker image rm $(cat ./install/images/repository-images.txt)
docker image rm $(docker images --format '{{.Repository}}:{{.Tag}}' | awk '$0 ~ /^localhost:32080\/eva\// { print }')
rm -f ./install/images/repository-images.tar
```

#### D. Qdrant 전용 Airgap/Harbor snapshot 검증

Qdrant snapshot 변경만 확인할 때는 `values-k3s.harbor.yaml` profile을 사용합니다. 이 profile은 `qdrant-snapshot-sync`가 S3에 접속하지 않고 Local Harbor의 OCI artifact를 `oras pull`하여 snapshot PVC에 받은 뒤 Qdrant restore API를 호출합니다.

준비 서버에서 필요한 파일은 아래입니다. `values-k3s.harbor.yaml`은 EVA Agent release `3.1.0`의 `eva-agent-qdrant/`에 포함되어 있어야 합니다.

- `install/qdrant/qdrant-<version>.tgz`
- `install/eva-agent/release/<release>/eva-agent-qdrant/values-k3s.harbor.yaml`
- `install/eva-agent/release/<release>/plugins/eva-agent-qdrant/{post-renderer.sh,plugin.yaml}`
- `install/tools/oras` — Local Harbor에 snapshot OCI artifact를 push하는 CLI
- `install/images/images-pulled.txt` 및 `repository-images.tar` — `qdrant`와 `eva-agent-qdrant-snapshot-sync:0.1.0` 포함
- `install/qdrant-snapshots/*.snapshot` — `SNAPSHOT_SPECS`에 지정된 snapshot 파일
- `install/push_qdrant_snapshots_to_harbor.sh`

`versions.json`을 대상 release/chart 버전으로 맞춘 뒤 준비 서버에서 실행합니다. `COMPONENTS`로 이미지 준비만 Qdrant로 제한할 수 있습니다.

```bash
EVA_AGENT_QDRANT_VALUES_FILE=values-k3s.harbor.yaml \
  AWS_PROFILE=default ./install/download_offline_assets.sh

EVA_AGENT_QDRANT_VALUES_FILE=values-k3s.harbor.yaml \
  COMPONENTS=eva-agent-qdrant \
  AWS_PROFILE=default ./install/download_eva_images.sh

EVA_AGENT_QDRANT_VALUES_FILE=values-k3s.harbor.yaml \
  AWS_PROFILE=default ./install/download_qdrant_snapshots.sh
```

`repository-images.tar`, `eva-deployer/`, 그리고 `install/qdrant-snapshots/`를 폐쇄망 서버로 옮깁니다. Local Harbor 설치 및 Docker image seed 후 snapshot도 OCI artifact로 push합니다. artifact 이름은 `<registry>/eva/qdrant-snapshots:<SNAPSHOT_SPECS 첫 번째 필드>`입니다.

```bash
docker load -i ./install/images/repository-images.tar

PULL_SOURCE_IMAGES=false \
IMAGE_LIST=./install/images/images-pulled.txt \
REPOSITORY_REGISTRY=localhost:32080 \
REPOSITORY_PROJECT=eva \
./install/push_images_to_repository.sh

EVA_AGENT_QDRANT_VALUES_FILE=values-k3s.harbor.yaml \
REPOSITORY_REGISTRY=localhost:32080 \
REPOSITORY_PROJECT=eva \
./install/push_qdrant_snapshots_to_harbor.sh
```

Qdrant만 배포하려면 (vLLM/EVA Agent 본체는 설치하지 않음) k3s와 Local Harbor가 준비된 뒤 아래 Helm 명령을 사용합니다. `qdrant-snapshot-harbor` Secret은 Harbor가 private project인 경우 ORAS 인증에 필요합니다.

기존 `site_eva_agent.yaml` 전체 배포에 이 profile을 적용할 때는 아래 두 변수를 함께 지정합니다. 기본값은 기존 `values-k3s.yaml`/PVC 사전복사 방식이므로, 기존 Airgap 배포에는 영향이 없습니다.

```text
-e eva_agent_qdrant_values_file=values-k3s.harbor.yaml
-e eva_agent_qdrant_snapshot_source=harbor
```

```bash
kubectl create namespace eva-agent --dry-run=client -o yaml | kubectl apply -f -
kubectl create serviceaccount sa-eva-agent -n eva-agent --dry-run=client -o yaml | kubectl apply -f -
kubectl create secret generic qdrant-snapshot-harbor -n eva-agent \
  --from-literal=username=admin \
  --from-literal=password="${HARBOR_ADMIN_PASSWORD}" \
  --dry-run=client -o yaml | kubectl apply -f -

helm upgrade --install eva-agent-qdrant ./install/qdrant/qdrant-<version>.tgz \
  --namespace eva-agent --create-namespace \
  --values ./install/eva-agent/release/<release>/eva-agent-qdrant/values-k3s.harbor.yaml \
  --set-string image.repository=localhost:32080/eva/qdrant \
  --set-string image.tag=v<version> \
  --post-renderer ./install/eva-agent/release/<release>/plugins/eva-agent-qdrant/post-renderer.sh

kubectl rollout status statefulset/eva-agent-qdrant -n eva-agent --timeout=900s
kubectl logs -n eva-agent statefulset/eva-agent-qdrant -c qdrant-snapshot-sync
```

`docker network create --internal`은 Harbor/ORAS artifact 경로만 별도로 차단 검증할 때 사용하세요. 실제 host k3s 설치의 Pod network는 Docker의 사용자 정의 network에 자동으로 들어가지 않으므로, 이 명령 하나만으로 k3s Pod의 외부 통신이 차단되지는 않습니다. 실제 배포 검증은 서버의 외부 라우팅/DNS를 차단한 상태에서 위 Helm 배포를 실행해야 하며, Docker 기반 k3d 테스트라면 Harbor container와 모든 k3d node를 같은 `--internal` network에 연결하고 values의 `HARBOR_REGISTRY`를 그 network에서 해석되는 Harbor hostname:port로 바꿔야 합니다.

---

## 2. Inventory

원격 서버 설치:

```ini
{{IP}} ansible_user={{계정}} ansible_ssh_private_key_file=~/.ssh/id_rsa ansible_become_password={{비밀번호}}
```

Airgap 서버에 SSH로 접속한 뒤, 해당 서버에서 직접 실행:

```ini
localhost ansible_connection=local ansible_become_password={{현재 로그인 계정의 sudo 비밀번호}}
```

`ansible_connection=local`을 지정하면 Ansible은 localhost에 SSH로 다시 접속하지 않고 현재 로그인한 계정으로 실행합니다.

SSH 키:

```bash
ssh-keygen -t rsa -b 4096
ssh-copy-id {{계정}}@{{IP}}
```

---

## 3. 실행 공통

`check`는 변경 없이 시뮬레이션하는 `--check` 실행입니다. `run`은 실제 서버에 변경을 적용합니다.

로그 폴더는 playbook별로 먼저 생성합니다.

```bash
mkdir -p logs_precondition logs_infra logs_gpu_mig logs_eva logs_n8n
```

---

## 4. 사전 점검

대상 서버가 EVA 설치를 진행할 수 있는 상태인지 먼저 확인합니다. 이 단계는 서버 설정을 변경하지 않고, 점검 결과를 control node의 `config/<target-ip>/precondition.yaml`에 저장합니다.

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
ANSIBLE_LOG_PATH=logs_precondition/ansible-internal.log \
.venv/bin/ansible-playbook -i inventory.ini site_precondition.yaml --check 2>&1 | tee logs_precondition/ansible-check.log

ANSIBLE_LOG_PATH=logs_precondition/ansible-internal.log \
.venv/bin/ansible-playbook -i inventory.ini site_precondition.yaml -vvv 2>&1 | tee logs_precondition/ansible-run.log
```

결과 확인:

```bash
ls -l ./config/<target-ip>/precondition.yaml
```

---

## 5. 인프라 설치

인프라는 k3s, kubectl, helm, NFS CSI, NVIDIA runtime/device-plugin 등을 구성합니다.

### [cloud_repository]

대상 서버가 인터넷/ECR/Docker Hub에 접근 가능한 경우입니다.

```bash
mkdir -p logs_infra

ANSIBLE_LOG_PATH=logs_infra/ansible-internal.log \
.venv/bin/ansible-playbook -i inventory.ini site_infra.yaml --check \
  -e repository_mode=cloud_repository \
  2>&1 | tee logs_infra/ansible-check.log

ANSIBLE_LOG_PATH=logs_infra/ansible-internal.log \
.venv/bin/ansible-playbook -i inventory.ini site_infra.yaml \
  -e repository_mode=cloud_repository \
  -vvv 2>&1 | tee logs_infra/ansible-run.log
```

### [remote_repository]

Main Harbor에서 infra 이미지를 pull하는 경우입니다.

```bash
mkdir -p logs_infra

ANSIBLE_LOG_PATH=logs_infra/ansible-internal.log \
.venv/bin/ansible-playbook -i inventory.ini site_infra.yaml --check \
  -e repository_mode=remote_repository \
  -e repository_registry=harbor.main.local:32080 \
  -e repository_project=eva \
  2>&1 | tee logs_infra/ansible-check.log

ANSIBLE_LOG_PATH=logs_infra/ansible-internal.log \
.venv/bin/ansible-playbook -i inventory.ini site_infra.yaml \
  -e repository_mode=remote_repository \
  -e repository_registry=harbor.main.local:32080 \
  -e repository_project=eva \
  -vvv 2>&1 | tee logs_infra/ansible-run.log
```

### [local_repository]

Airgap 서버 내부 Local Harbor에서 infra 이미지를 pull하는 경우입니다.

```bash
mkdir -p logs_infra

ANSIBLE_LOG_PATH=logs_infra/ansible-internal.log \
.venv/bin/ansible-playbook -i inventory.ini site_infra.yaml --check \
  -e repository_mode=local_repository \
  -e repository_registry=localhost:32080 \
  -e repository_project=eva \
  2>&1 | tee logs_infra/ansible-check.log

ANSIBLE_LOG_PATH=logs_infra/ansible-internal.log \
.venv/bin/ansible-playbook -i inventory.ini site_infra.yaml \
  -e repository_mode=local_repository \
  -e repository_registry=localhost:32080 \
  -e repository_project=eva \
  -vvv 2>&1 | tee logs_infra/ansible-run.log
```

드라이버 패키지를 지정하려면 추가 변수로 넘깁니다.

```bash
.venv/bin/ansible-playbook -i inventory.ini site_infra.yaml \
  -e repository_mode=local_repository \
  -e repository_registry=localhost:32080 \
  -e repository_project=eva \
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
AWS_PROFILE=default AWS_REGION=ap-northeast-2 ./install/download_display_mode_selector.sh
```

### [cloud_repository]

```bash
mkdir -p logs_gpu_mig

ANSIBLE_LOG_PATH=logs_gpu_mig/ansible-internal.log \
.venv/bin/ansible-playbook -i inventory.ini site_gpu_mig.yaml \
  -e repository_mode=cloud_repository \
  -vvv 2>&1 | tee logs_gpu_mig/ansible-run.log
```

### [remote_repository]

```bash
mkdir -p logs_gpu_mig

ANSIBLE_LOG_PATH=logs_gpu_mig/ansible-internal.log \
.venv/bin/ansible-playbook -i inventory.ini site_gpu_mig.yaml \
  -e repository_mode=remote_repository \
  -e repository_registry=harbor.main.local:32080 \
  -e repository_project=eva \
  -vvv 2>&1 | tee logs_gpu_mig/ansible-run.log
```

### [local_repository]

```bash
mkdir -p logs_gpu_mig

ANSIBLE_LOG_PATH=logs_gpu_mig/ansible-internal.log \
.venv/bin/ansible-playbook -i inventory.ini site_gpu_mig.yaml \
  -e repository_mode=local_repository \
  -e repository_registry=localhost:32080 \
  -e repository_project=eva \
  -vvv 2>&1 | tee logs_gpu_mig/ansible-run.log
```

결과 확인:

```bash
nvidia-smi
```

---

## 7. EVA 환경 설정

`site_eva_config.yaml`은 EVA 배포에 필요한 설정 파일을 생성합니다. 생성된 값은 `config/<target>/eva.yaml`에 저장됩니다.

### [cloud_repository]

```bash
mkdir -p logs_eva

ANSIBLE_LOG_PATH=logs_eva/ansible-internal.log \
.venv/bin/ansible-playbook -i inventory.ini site_eva_config.yaml \
  -e repository_mode=cloud_repository \
  -vvv 2>&1 | tee logs_eva/ansible-config.log
```

### [remote_repository]

```bash
mkdir -p logs_eva

ANSIBLE_LOG_PATH=logs_eva/ansible-internal.log \
.venv/bin/ansible-playbook -i inventory.ini site_eva_config.yaml \
  -e repository_mode=remote_repository \
  -e repository_registry=harbor.main.local:32080 \
  -e repository_project=eva \
  -vvv 2>&1 | tee logs_eva/ansible-config.log
```

### [local_repository]

```bash
mkdir -p logs_eva

ANSIBLE_LOG_PATH=logs_eva/ansible-internal.log \
.venv/bin/ansible-playbook -i inventory.ini site_eva_config.yaml \
  -e repository_mode=local_repository \
  -e repository_registry=localhost:32080 \
  -e repository_project=eva \
  -vvv 2>&1 | tee logs_eva/ansible-config.log
```

결과 확인:

```bash
ls -l ./config/<target-ip>/eva.yaml
```

---

## 8. EVA 배포

`site_eva.yaml`은 EVA Agent, EVA Vision, EVA App을 배포합니다.

배포 시 Helm values는 아래 순서로 적용합니다.

- chart 기본 values
- release에 포함된 k3s values 또는 secret values
- 이 repository의 `values/*-k3s-override.yaml.j2`
- `7. EVA 환경 설정`에서 생성된 `config/<target>/eva.yaml` 기반 override

`values/` 폴더의 파일은 전체 values 사본이 아니라, EVA deployer가 책임지는 k3s/repository override만 담습니다. `repository_mode`, `repository_registry`, `repository_project`에 따른 image repository 변경과 k3s 실행에 필요한 값은 여기서 관리하고, 환경별 App/Agent 설정은 `site_eva_config.yaml`이 생성한 `config/<target>/eva.yaml` 값을 배포 단계에서 추가 override로 반영합니다.

EVA App의 호스트별 설정은 로컬 전용 파일 `values/app.yaml` 하나에서 관리합니다. 이 파일에는 license credential이 포함될 수 있으므로 Git에 커밋하지 않습니다. 저장소에는 `values/app.yaml.sample`만 포함합니다.

처음 설치할 때 sample을 복사합니다.

```bash
cp values/app.yaml.sample values/app.yaml
```

복사한 `values/app.yaml`의 최상위 키를 배포 대상의 `ansible_host` 또는 inventory hostname으로 지정합니다. license credential은 배포 환경별로 다르므로, `values/app.yaml.sample`에서 대상 환경의 블록을 선택해 주석을 해제하고 해당 환경에 발급된 키를 사용합니다. prod와 dev의 credential을 섞어 사용하면 안 됩니다.

```yaml
localhost:
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

예를 들어 dev inventory에 `10.186.0.75`가 있으면 `10.186.0.75:` 아래에 dev 설정을 작성합니다. 해당 호스트 키가 없으면 기존 공통 설정만 적용됩니다. `values/app.yaml`이 없으면 호스트별 override 없이 배포합니다.

배포 중 렌더링된 최종 override values는 control node의 `deploy/<target>/` 아래에 component별로 남습니다.

```text
deploy/<target>/
  app/app-k3s-override.yaml
  vision/vision-k3s-override.yaml
  agent/agent-k3s-override.yaml
  qdrant/qdrant-k3s-override.yaml
  vllm/vllm-k3s-override.yaml
  vllm/values-override-from-config.yaml
```

`vllm/values-override-from-config.yaml`은 `config/<target>/eva.yaml`에 vLLM override 값이 있을 때만 생성됩니다.

### [cloud_repository]

```bash
mkdir -p logs_eva

ANSIBLE_LOG_PATH=logs_eva/ansible-internal.log \
.venv/bin/ansible-playbook -i inventory.ini site_eva.yaml \
  -e repository_mode=cloud_repository \
  -vvv 2>&1 | tee logs_eva/ansible-run-eva.log
```

### [remote_repository]

```bash
mkdir -p logs_eva

ANSIBLE_LOG_PATH=logs_eva/ansible-internal.log \
.venv/bin/ansible-playbook -i inventory.ini site_eva.yaml \
  -e repository_mode=remote_repository \
  -e repository_registry=harbor.main.local:32080 \
  -e repository_project=eva \
  -vvv 2>&1 | tee logs_eva/ansible-run-eva.log
```

### [local_repository]

```bash
mkdir -p logs_eva

ANSIBLE_LOG_PATH=logs_eva/ansible-internal.log \
.venv/bin/ansible-playbook -i inventory.ini site_eva.yaml \
  -e repository_mode=local_repository \
  -e repository_registry=localhost:32080 \
  -e repository_project=eva \
  -vvv 2>&1 | tee logs_eva/ansible-run-eva.log
```

vLLM GPU 프로파일을 지정하려면 추가 변수로 넘깁니다.

```bash
.venv/bin/ansible-playbook -i inventory.ini site_eva.yaml \
  -e repository_mode=local_repository \
  -e repository_registry=localhost:32080 \
  -e repository_project=eva \
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

## 9. n8n 설치 (Optional)

n8n은 EVA 설치와 분리해서 별도 playbook으로 실행합니다.

### [cloud_repository]

```bash
mkdir -p logs_n8n

ANSIBLE_LOG_PATH=logs_n8n/ansible-internal.log \
.venv/bin/ansible-playbook -i inventory.ini site_n8n.yaml \
  -e repository_mode=cloud_repository \
  -vvv 2>&1 | tee logs_n8n/ansible-run.log
```

### [remote_repository]

Main Harbor에 아래 이미지가 준비되어 있어야 합니다.

```text
harbor.main.local:32080/eva/n8n:1.103.2
```

```bash
mkdir -p logs_n8n

ANSIBLE_LOG_PATH=logs_n8n/ansible-internal.log \
.venv/bin/ansible-playbook -i inventory.ini site_n8n.yaml \
  -e repository_mode=remote_repository \
  -e repository_registry=harbor.main.local:32080 \
  -e repository_project=eva \
  -vvv 2>&1 | tee logs_n8n/ansible-run.log
```

### [local_repository]

Local Harbor에 아래 이미지가 준비되어 있어야 합니다.

```text
localhost:32080/eva/n8n:1.103.2
```

```bash
mkdir -p logs_n8n

ANSIBLE_LOG_PATH=logs_n8n/ansible-internal.log \
.venv/bin/ansible-playbook -i inventory.ini site_n8n.yaml \
  -e repository_mode=local_repository \
  -e repository_registry=localhost:32080 \
  -e repository_project=eva \
  -vvv 2>&1 | tee logs_n8n/ansible-run.log
```

결과 확인:

```bash
kubectl get all -n n8n
```

브라우저에서 `http://<target-ip>:30678` 접속 후 n8n UI를 확인합니다.
