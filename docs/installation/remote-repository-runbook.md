# EVA Remote Repository 설치 가이드

이 가이드는 인터넷에 연결된 Main 서버에서 EVA 설치 자산을 준비하고, 외부 인터넷이
차단된 Target에 Main Harbor와 게시된 Remote delivery만으로 EVA를 설치하는 절차입니다.
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
| [Target] GPU | 지원 NVIDIA GPU, driver 및 `nvidia-smi` |
| [Target] 네트워크 | Main Harbor 및 Main SSH 경로만 사용; 외부 인터넷은 사용하지 않음 |
| 권한 | Main과 Target에서 `sudo eva ...`를 실행할 수 있는 사용자 |

Main은 EVA Managed Runtime, Docker/Compose, Harbor와 `eva` project를 준비합니다.
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
명시합니다. 아래 Main preparation/verify/publish도 인자 없이 같은 Current Release를
사용합니다. 다른 Release가 필요한 경우에만 해당 Remote 명령에 `RELEASE_PATH`를 명시합니다.

## 3. [Main] AWS와 Harbor 입력 확인

정상 작업에 사용할 Main Harbor endpoint를 확인합니다. Target SSH 주소는 publish 명령에
직접 지정합니다.

```text
10.159.57.172:32080
```

Main Harbor endpoint에는 URL scheme을 넣지 않으며 `localhost` 또는 loopback 주소를
사용하지 않습니다. Target node와 Pod 모두 이 endpoint에 연결할 수 있어야 합니다.
기본 project는 `eva`입니다.

Managed Harbor의 초기 계정은 `admin`이며 초기 비밀번호 기본값은 `EVA123@`입니다.
`EVA_HARBOR_ADMIN_PASSWORD`가 비어 있지 않으면 그 값을 우선합니다. 비밀번호 CLI
option은 없고, receipt와 로그에 비밀번호를 기록하지 않습니다.

Remote preparation에는 Main 서버에서 사용할 AWS credential이 필요합니다.
`sudo eva remote prepare`는 EVA managed credential을 확인하며, credential이 없고
대화형 terminal에서 실행 중이면 AWS Access Key ID, AWS Secret Access Key 및 AWS Region을
입력받습니다. Region의 기본값은 `ap-northeast-2`입니다.

검증된 credential은 `/var/lib/eva/credentials/aws_key.ini`에 권한 `0600`으로 안전하게
저장됩니다. 다음 형식만 사용하며, credential을 명령행이나 Release에 넣지 않습니다.

```ini
aws_access_key_id = <AWS_ACCESS_KEY_ID>
aws_secret_access_key = <AWS_SECRET_ACCESS_KEY>
region = ap-northeast-2
```

credential은 preparation evidence, Target payload, Remote publish 결과에 포함되지 않으며
Target 서버에 AWS credential을 준비하지 않습니다.

## 4. [Main] Preparation Plane bootstrap

Main의 preparation plane을 준비하거나 기존 상태를 검증합니다.

```bash
sudo eva remote bootstrap \
  --registry 10.159.57.172:32080 \
  --yes
```

이 명령은 Managed Runtime, Docker Engine/Compose, Harbor 및 `eva` project를 준비하거나
검증하고, 같은 `sudo` 실행 identity의 Docker registry credential까지 확인한 뒤 Harbor
receipt를 기록합니다. 별도의 `docker login`은 정상 절차에 필요하지 않습니다. credential
오류가 발생하면 수동 login 대신 `sudo eva remote bootstrap --yes`를 다시 실행합니다.
Target의 k3s, GPU, NFS 또는 EVA component를
Main에 설치하지 않습니다. 이미 승인된 외부 Harbor를 쓸 때만 advanced
`--external-harbor` option을 사용합니다.

## 5. [Main] Remote 자산 준비

검증한 Base Release를 준비합니다. registry/project는 bootstrap receipt에서 자동으로
해석됩니다.

```bash
sudo eva remote prepare
```

성공하면 Remote Runtime artifact, Target package/host/chart/helper/model payload, Main
Harbor의 product/infra image 및 Qdrant snapshot OCI artifact와 preparation evidence가
생성됩니다. Container image와 Qdrant snapshot 파일은 Target payload에 중복 저장하지
않고 Main Harbor에서 공급합니다.

## 6. [Main] 준비 결과 검증

게시 전에 Main에 남은 preparation evidence를 read-only로 검증합니다.

```bash
sudo eva remote verify
```

이 명령은 preparation manifest, Runtime artifact, Target payload 및 local preparation
evidence를 확인합니다. 자산을 다시 다운로드하거나 게시하지 않으며, 성공해도 Target의
실제 Harbor pull까지 증명하는 것은 아닙니다.

## 7. [Main] Target으로 Release 게시

검증한 Base Release와 생성된 delivery artifact를 Target으로 게시합니다.

```bash
sudo eva remote publish \
  --target eva@10.159.56.196
```

Base Release, Remote Runtime 및 Target payload는 하나의 Target staging area로 전달되고,
Target에서 checksum과 identity를 재검증한 뒤 atomic publish됩니다. Workspace,
inventory와 AWS credential은 전송하지 않습니다. 동일 identity의 재게시는 허용하지만,
같은 version에 다른 identity를 덮어쓰지는 않습니다.

### 여러 Target에 순차 게시

`--target`은 반복할 수 있습니다. 각 Target은 독립 staging, 검증, atomic rename을
수행하며 한 Target이 실패해도 이후 Target은 계속 시도합니다. 전체 결과 중 하나라도
실패하면 명령은 non-zero로 끝납니다.

```bash
sudo eva remote publish \
  --target eva@10.159.56.196 \
  --target eva@10.159.56.197 \
  --target eva@10.159.56.198
```

bootstrap receipt와 같은 registry를 명시적으로 확인하려면 모든 Remote 명령에
`--registry 10.159.57.172:32080`를 붙일 수 있습니다. 다른 registry는 조용히
override되지 않습니다. 변경이 필요한 경우 새 endpoint가 정상인지 확인한 후 다음처럼
receipt를 교체합니다. 기존 Harbor data, preparation 결과와 Target Release는 삭제하지
않습니다.

```bash
sudo eva remote bootstrap \
  --registry 10.159.57.173:32080 \
  --replace-registry \
  --yes
```

## 8. [Target] EVA Tool 설치 및 게시된 Release 확인

Target에는 Tool이 없으므로 최초 한 번만 게시된 installer의 절대 경로를 실행합니다.

```bash
release_version=<version>
release_root="/var/lib/eva/inbox/releases/$release_version"

sudo bash "$release_root/eva-tool-installer.sh"
sudo eva verify
```

installer는 checksum과 Tool version을 검증한 뒤 이 게시 Release를 Current Release로
등록합니다. `sudo eva verify`는 Base Release 무결성을 확인합니다. Remote Runtime, payload
및 delivery marker의 전체 검증은 다음 `eva install`의 선행 검증에서 수행됩니다. 게시된
파일을 수정하거나 일반 사용자가 inbox Release directory로 이동할 필요가 없습니다. 검증에
실패하면 Main에서 다시 준비·게시합니다.

## 9. [Target] Workspace 입력 준비

Target의 site 입력은 Release 밖에 둡니다. Remote Target에는
`credentials/aws_key.ini`를 만들지 않습니다.

```bash
mkdir -p /home/eva/site-remote-196/{inventory,site-values}
```

```text
/home/eva/site-remote-196/
├── inventory/
│   └── inventory.ini
└── site-values/
    ├── site.yaml
    ├── iam.yaml
    └── app.yaml
```

`site-values/site.yaml`에는 Main Harbor를 지정합니다.

```yaml
site:
  id: site-remote-196

repository:
  mode: remote
  registry: 10.159.57.172:32080
  project: eva

components:
  infra: true
  iam: true
  agent: true
  vision: true
  app: true
  n8n: false
```

`inventory.ini`, `iam.yaml`, `app.yaml`에는 이 site에 승인된 inventory, TLS 및 제품
입력을 둡니다. Secret은 승인된 관리 절차를 사용하며 Release나 source control에 넣지
않습니다. Agent 또는 Vision chart override가 필요한 경우에만 해당 values 파일을
추가합니다. Workspace에 repository-derived image override나 AWS/S3/ECR 설정을 넣지
않습니다.

## 10. [Target] GPU 사전 조건 확인

설치 전에 NVIDIA driver와 감지된 GPU/MIG 상태를 확인합니다.

```bash
sudo eva preflight gpu
```

MIG를 사용할 site는 설치 전에 의도한 MIG 구성을 적용합니다. `infra`가 Docker, CDI,
k3s와 NVIDIA Device Plugin을 구성하며 이후 Config가 사용 가능한 GPU 또는 MIG resource를
선택합니다.

## 11. [Target] Argo CD 관리 연결 확인

기존 Argo CD가 같은 site의 EVA resource를 관리했을 수 있으면 설치 전에 handoff를
확인합니다.

```bash
sudo eva preflight argocd \
  --workspace /home/eva/site-remote-196
```

발견된 관리 연결을 해제할지 묻는 경우 현재 상태와 승인 내용을 확인합니다. 거절하거나
검증에 실패하면 설치를 시작하지 않습니다.

## 12. [Target] EVA 설치

Workspace와 게시된 Release를 검증한 뒤 설치를 실행합니다.

```bash
sudo eva workspace validate \
  --workspace /home/eva/site-remote-196

sudo eva install \
  --workspace /home/eva/site-remote-196 \
  --yes
```

다른 Release를 사용해야 할 때만 `--release`로 명시적으로 override합니다. 이 override는
Current Release를 변경하지 않습니다.

```bash
sudo eva verify --release /var/lib/eva/inbox/releases/<another-version>
sudo eva install --release /var/lib/eva/inbox/releases/<another-version> \
  --workspace /home/eva/site-remote-196 \
  --yes
```

Target에 유효한 Managed Runtime이 없으면 설치는 게시된 Remote Runtime artifact로만
bootstrap합니다. online Runtime 또는 original `eva-offline` fallback은 사용하지 않습니다.
Target payload는 managed cache로 materialize되고 image와 Qdrant snapshot은 Main Harbor를
사용합니다.

## 13. 설치 결과 확인

설치 상태와 작업 결과를 확인합니다.

```bash
sudo eva check --verbose
sudo eva status
```

선택한 Agent 또는 Vision component가 있으면 GPU/MIG allocatable resource, NVIDIA Device
Plugin readiness 및 workload의 할당 상태도 확인합니다. workload image가 Main Harbor prefix를
사용하는지, Target에 AWS credential이나 public source 설정이 남지 않았는지도 확인합니다.

## 14. 재실행 및 문제 해결

안전한 입력 수정 후에는 동일 Release와 Workspace로 설치를 다시 실행하거나 실패한 작업을
재시도합니다.

```bash
sudo eva install \
  --workspace /home/eva/site-remote-196 \
  --yes

sudo eva retry --yes
```

동일 Runtime/payload identity와 기존 영속 데이터는 재사용합니다. Release checksum,
delivery marker 또는 Runtime/payload 검증 오류는 Target에서 임의로 고치지 말고 Main의
준비·검증·게시 절차를 다시 수행합니다.
