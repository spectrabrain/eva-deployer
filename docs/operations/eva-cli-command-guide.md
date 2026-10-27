# EVA Tool 명령어 가이드

설치자가 사용하는 공개 `eva` 명령과 설치 결과 조회 방법을 정리한다.
실제 설치 순서는 [Online](../installation/online-repository-runbook.md),
[Remote](../installation/remote-repository-runbook.md),
[Local](../installation/local-repository-runbook.md) Runbook을 따른다.
이 문서는 현재 소스에 구현된 명령을 기준으로 하며, 기존 설치 binary에는 새 명령이 없을 수 있다.
`eva version`과 `eva help`로 설치된 Tool의 버전과 지원 명령을 먼저 확인한다.
`airgap prepare`, `backup`, `inspect`, `install configure`, `config sample`, `--config`를 현장에서 사용하려면 해당 명령이 포함된 Tool·Release를 배포해야 한다.

## 1. 가장 자주 쓰는 조회

설치를 시작한 서버에서 `status`로 작업 결과를 확인한다. Remote Main 설치에서는 Main의
`check`가 타겟별 서비스를 점검한다. 상세 App Values·Config·Helm 백업은 **Target에서** 조회한다.
별도 Control Node에서 Ansible을 실행한 환경은 그 Control Node에서 생성 파일을 조회한다.

```bash
eva version
eva help
sudo eva status
sudo eva check
sudo eva inspect
sudo eva inspect operations
sudo eva inspect files
sudo eva inspect values --component app
sudo eva inspect values --component app --source effective
sudo eva inspect config
sudo eva inspect result
sudo eva inspect logs
```

- `inspect`는 저장된 파일을 **읽기만** 한다. 설치, 다운로드, Runtime bootstrap, 권한 변경을 하지 않는다.
- Values와 Config의 Secret은 **가리지 않고 원문 출력**한다. 추가 승인이나 unmask option은 없다.
- private 파일을 읽기 위해 `sudo eva ...`를 사용한다. Tool이 내부에서 sudo를 실행하거나 파일 권한을 완화하지 않는다.
- 조회 내용은 새로운 Operation이나 Tool 로그에 저장하지 않는다. 터미널 녹화·리다이렉션에는 원문이 남을 수 있다.
- 파일 내용은 stdout, 선택한 operation과 파일 경로는 stderr로 출력한다.
- 인터넷이나 Kubernetes API 연결 없이 조회 가능하다. 실제 cluster 상태는 `eva check` 또는 `eva exec kubectl ...`로 별도 확인한다.

## 2. 조회 대상 선택

### 자동 선택

`inspect`는 기본적으로 **가장 최근에 생성된 Operation**의 Plan에서 Site, Workspace,
Release root를 찾는다. `--site`를 지정하면 해당 Site의 Operation만 대상으로 한다.
실행 결과가 없는 planned Operation도 포함한다. 이전 설치를 보려면 `--operation`을 지정한다.
기존 `status`, `apply`, `retry`의 자동 선택은 최근 **갱신된** Operation 기준이라는 차이가 있다.

```bash
sudo eva inspect operations --site customer-a
sudo eva inspect paths --site customer-a
sudo eva inspect files --operation <operation-id>
sudo eva inspect values --site customer-a --component app
```

Target 폴더가 하나이면 자동으로 선택한다. 둘 이상이면 임의 선택하지 않고 후보를 안내한다.
`--target`에는 Inventory의 `ansible_host` 값, 없으면 `inventory_hostname`을 지정한다.
설정 파일의 타겟 닉네임이나 Site ID와 반드시 같은 것은 아니다.

```bash
sudo eva inspect files --site customer-a
sudo eva inspect values --site customer-a --target 10.0.0.21 --component app
```

**중요:** Values 파일은 Operation별 immutable snapshot이 아니라
Release/Site/Target/Component별 작업 산출물이다. 같은 경로로 재설치하면 덮어쓸 수 있다.
`--operation`은 해당 Plan의 경로를 선택하는 것이지 과거 시점의 Values를 복구하는 기능이 아니다.
`inspect plan`은 저장된 Plan 원문을 조회하고 `inspect result`는 해당 Operation 실행 결과를 조회한다.
`resolved`는 자동으로 `effective`에 fallback하지 않으며, 파일이 없으면 이유와 조회 방법을 안내한다.

기존 Helm release의 사용자 Values는 변경 전 별도 private 영구 백업으로 보존한다.
이 파일은 `inspect values` 조회 대상과 별개다. 실제 백업이 저장된 Ansible 대상 서버에서
`sudo eva backup list --site customer-a --component app`으로 목록을,
`sudo eva backup show --site customer-a --component app --latest`로 최신 메타데이터/경로를 조회한다.
새 백업은 `<site>/<namespace>/<기존 App 또는 Chart 버전>/<시각-고유값>/`에 저장한다.
`--version <DEPLOYED_VERSION>`으로 기존 배포 버전을 필터링하고, `backup values --path DIR`로 선택한
Values 원문을 명시적으로 출력할 수 있다. `list`/`show`는 원문을 출력하지 않는다. 경로·대상·제약은
[Helm 사용자 Values 백업](helm-user-values-backup.md)을 참고한다.

### Operation 없이 직접 Ansible로 생성한 파일

```bash
sudo eva inspect paths --release-root /path/to/eva-deployer --site customer-a
sudo eva inspect files --release-root /path/to/eva-deployer --site customer-a
sudo eva inspect values --release-root /path/to/eva-deployer --site customer-a --component app
```

`--release-root`는 이미 존재하는 실행 소스 root이다. 압축 artifact나 inbox의 원본 Release를
자동으로 풀거나 검증하지 않는다. `--site`가 필수이며 `--operation`, `operations`, `plan`,
`result`, `logs`와 함께 쓸 수 없다.
기본 생성 위치는 `<release-root>/out/work/rendered/<site>`와
`<release-root>/out/work/config/<site>`이다.

직접 Ansible 실행에서 산출물 경로를 변경했다면 **Site 단위 경로**를 명시한다.
임의의 Inventory 변수나 Jinja 표현을 조회 과정에서 다시 실행하지 않는다.

```bash
sudo eva inspect values --site customer-a --component app --rendered-root /custom/rendered/customer-a
sudo eva inspect config --site customer-a --config-root /custom/config/customer-a
```

## 3. inspect 명령 상세

| 명령 | 출력 | 주요 option |
| --- | --- | --- |
| `eva inspect` / `eva inspect paths` | Site, Release, Workspace, mode, 산출물·Plan·결과·로그 경로 | `--site`, `--operation` |
| `eva inspect operations` | ID, Site, Release, status, 생성 시각 목록 | `--site` |
| `eva inspect files` | 생성 YAML의 Target, component, 파일명, 크기, 권한, 전체 경로 | `--target`, `--component` |
| `eva inspect values` | 저장된 Values 원문 | `--component` 필수, `--target`, `--source`, `--file` |
| `eva inspect config` | 자동 생성 Config 원문 | `--target`, `--file` |
| `eva inspect plan` | 선택한 Operation Plan 원문 | `--operation` |
| `eva inspect result` | 단계별 종료 코드 등 실행 결과 원문 | `--operation` |
| `eva inspect logs` | Ansible 출력 로그의 끝부분 | `--operation`, `--tail` |
| `eva inspect --help` | 조회 명령 도움말 | 없음 |

모든 명령은 `--state-root PATH`로 다른 Operation store를 지정할 수 있다.
기본값은 `/var/lib/eva/operations`이다. option은 subcommand 뒤에 넣는다.
`--file`에는 `.yaml` basename만 허용하며 경로 이탈과 외부 파일로 향하는 symlink는 거부한다.

### Values 출처

| `--source` | 의미 | 파일 |
| --- | --- | --- |
| `resolved` (기본) | 설치 후 `helm get values --all`로 수집한 계산값 | `resolved-values.yaml` |
| `effective` | Deployer가 병합한 Helm 입력. CLI `--set`은 아직 포함하지 않음 | `effective-input-values.yaml` |
| `defaults` | Chart에서 추출한 기본 Values | `chart-defaults.yaml` |
| `override` | Deployer가 렌더링한 k3s override | `<component>-k3s-override.yaml`; IAM은 `eva-iam-k3s-override.yaml` |
| `sources` | Config, Workspace, CLI 등의 사용 여부와 산출물 경로 | `values-sources.yaml` |

```bash
sudo eva inspect values --component app --source resolved
sudo eva inspect values --component app --source effective
sudo eva inspect values --component app --source defaults
sudo eva inspect values --component app --source override
sudo eva inspect values --component app --source sources
sudo eva inspect values --component vision
sudo eva inspect values --component agent
sudo eva inspect values --component vllm
sudo eva inspect values --component qdrant
sudo eva inspect values --component iam --source resolved
sudo eva inspect values --component iam --source effective
sudo eva inspect values --component iam --source override
```

App, IAM, Vision, Agent, vLLM, Qdrant는 설치 단계에서 effective/resolved Values를 기록한다.
IAM은 defaults/override/effective/sources/resolved를 모두 control node에 저장하며,
Values에 비밀번호가 포함될 수 있으므로 파일 권한은 `0600`이다. `effective`는
role override < Workspace < CLI Values 순으로 병합한 입력이며, Chart 기본값과 CLI `--set`까지
포함한 최종 값은 `resolved`에서 확인한다.

**산출물이 없는 경우:** 선택한 Operation에 기록되지 않은 파일은 조회 시 자동 생성하지 않는다.
`inspect files`로 저장 여부를 확인한다. 기존 설치를 변경하지 않고 Values를 확인하려면 대상 클러스터에서
`sudo eva exec helm get values eva-iam -n eva-iam --all -o yaml`로 직접 조회할 수 있다.
release/namespace는 실제 설치 이름에 맞추고, 출력에는 secret이 마스킹 없이 포함되므로 공유하지 않는다.

n8n은 현재 Helm Values 방식이 아니라 Kubernetes manifest 방식으로 설치되므로
`inspect values --component n8n`에 대응하는 파일이 없을 수 있다. `inspect files`로 실제 파일을 확인한다.
`resolved`는 Helm 기록이지 이후 수동 kubectl 수정 사항까지 반영한 cluster snapshot이 아니다.

### App SSO 기본값과 생성 Config

App SSO의 기본 식별자는 `app.sso.realm: eva-iam`, `app.sso.clientId: eva-app`이다.
Deployer의 role override에서 이 기본값을 적용한다.
유효한 IAM handoff는 realm/client/admin-client 식별자를 함께 넘기며,
Workspace, CLI Values, CLI `--set`에 명시한 값은 기존 우선순위대로 보존한다.
완전한 Workspace SSO(URL과 admin secret)를 지정하면 handoff를 사용하지 않는다.

Config가 생성한 `app.vision`, `app.agent`, `app.pipeline`은 Chart의 `app` 아래로 병합한다.
생성 Config의 `backendHost`는 요약 정보로만 사용하며, 실제 공개 호스트는 App role의 설정
(기본: 대상 서버의 기본 IPv4), Workspace `app.backendHost`, CLI 순으로 결정한다.
`resolved` 파일은 설치 당시 Helm 기록이며 조회 단계에서 필드를 숨기거나 재작성하지 않는다.

### App MySQL DB 이름

App의 `database.name`은 별도 입력이 없으면 **선택한 Chart 기본값**을 따른다.
공개 App Chart 3.2.0의 기본값은 `eva_v3_0`이다. Deployer가 이를 `eva`로 덮어쓰지 않는다.
기본 Values 추출에도 설치에 지정한 Chart 버전을 사용하며, `--chart app=...`를 주면 해당
Chart 자체의 기본값을 사용한다.

`eva_app_db_name`을 명시하면 role override에 포함되며, 그 뒤 Workspace의
`database.name`, CLI `--values`의 `database.name`, CLI `--set app:database.name=...` 순으로
덮어쓴다. 이름을 지정하지 않았다면 `effective`/`override`에 `database.name`이 없는 것이
정상이다. 기본값은 `defaults`, 설치 후 최종 계산값은 `resolved`에서 확인한다.

**기존 설치 주의:** 기존 내부 MySQL의 DB 이름이 바뀌면
새 App 기동 전에 원본을 dump/restore하고 `eva_id`를 검증한다. 원본 DB는 삭제하지 않으며,
정상 대상 DB가 이미 있으면 그 데이터를 유지하고 로컬 license cache와 다를 때만 백업 후
`config.eva_id` 한 행을 정합화한다. 다른 DB를 덮어쓰거나 병합하지 않는다. v3.2 이전 로컬 사용자 schema의 전체 CSV도
DB 이름 변경 여부와 무관하게 먼저 백업한다. 위치·옵션·제약은
[App DB 이전과 사용자 백업](app-database-migration.md)을 따른다.
MySQL 초기화 환경 변수 변경은 데이터 이관이 아니다. PVC·MySQL·license 디렉터리를 삭제하지 않는다.

### IP 공개 포트와 Traefik

IP 기반 App/IAM은 ClusterIP의 `service.port`만으로 외부 포트가 열리지 않는다. Deployer는
최종 공개 URL에 맞춰 k3s HelmChartConfig listener와 App/IAM router를 정합화하고, 필요 없는
기본 80/443의 ServiceLB hostPort를 해제한다. 양쪽이 HTTP여도 서로 다른 공개 포트를 사용할 수 있다.
`kubectl get ingress`의 `PORTS`가 80/443으로 표시되는 것만으로 실패를 판정하지 않는다.
실제 공개 포트는 Traefik Service와 ServiceLB를 확인한다. 범위·옵션·중단 조건은
[Traefik 공개 포트 운영 안내](traefik-public-ports.md)를 참고한다.

### Config, 결과, 로그

```bash
sudo eva inspect config
sudo eva inspect config --file precondition.yaml
sudo eva inspect config --file n8n.yaml
sudo eva inspect plan --operation <operation-id>
sudo eva inspect result --operation <operation-id>
sudo eva inspect logs --operation <operation-id> --tail 200
sudo eva inspect logs --operation <operation-id> --tail 0
```

Config 기본 파일은 `eva.yaml`이다. 다른 Config는 `inspect files`에 실제 표시된 이름으로 선택한다.
로그 기본값은 100줄이며 `--tail 0`은 전체 원문을 출력한다. 실시간 follow 기능은 제공하지 않는다.
시작되지 않은 작업에는 로그가 없고, 실패 위치에 따라 result 또는 Values 파일이 없을 수 있다.

## 4. 공개 명령 전체 목록

### 설치 계획·실행·상태

| 명령 | 역할 | 변경 여부 |
| --- | --- | --- |
| `eva version` | Tool 버전·commit·build date | 조회 |
| `eva help` | 공개 명령 목록 | 조회 |
| `eva install configure [--output FILE]` | 환경·현재 서버/SSH 설치·타겟별 옵션 질문 후 사용자 홈에 private YAML 생성 | 로컬 파일 생성 |
| `eva config sample --mode online` | mode별 단일 설치 설정 샘플 출력 | 조회 |
| `eva config sample --mode remote --output FILE` | 주소/프로토콜 선택 후 private 샘플 저장 | 로컬 파일 생성 |
| `eva install --config FILE` | private YAML을 내부 Workspace로 변환해 설치 | 변경 |
| `eva install [RELEASE_PATH] --site ID` | Workspace와 Release로 설치 | 변경 |
| `eva install --workspace PATH` | 명시적 Workspace로 설치 | 변경 |
| `eva plan --release PATH --workspace PATH` | Plan YAML 출력, 설치하지 않음 | 조회 |
| `eva plan ... --output PATH` | Plan을 지정 파일로 저장 | 로컬 파일 생성 |
| `eva plan ... --save` | Plan을 Operation으로 저장 | 상태 생성 |
| `eva apply [OPERATION_ID]` | 저장된 planned Operation 실행 | 변경 |
| `eva retry [OPERATION_ID]` | 실패 작업의 입력을 복제한 새 Operation으로 재시도 | 변경 |
| `eva status [OPERATION_ID]` | Operation 상태와 경로 조회 | 조회 |
| `eva check [--verbose]` | 관리 Runtime, 설치 상태와 실제 서비스 점검 | 조회 |
| `eva inspect ...` | 저장된 설정·결과·로그 조회 | 조회 |
| `eva backup list ...` | 기존 Helm 사용자 Values 백업 목록; 버전·component 필터 | 조회 |
| `eva backup show ... --latest` | 한 release의 최신 백업 메타데이터·경로; Values 원문 제외 | 조회 |
| `eva backup values --path DIR` | SHA-256 확인 후 선택한 백업 Values 원문 출력 | 민감정보 조회 |

일반 설치자는 sudo 없이 `eva install configure`로 시작한다. Online/Remote/Local → Online/Remote 현재 서버/SSH 설치 →
타겟별 Infra-only/Solution·HTTP/HTTPS·도메인/IP·IAM 함께 설치/외부 IAM을 선택한다.
Bastion 타겟은 한 대씩 닉네임/IP/SSH 계정/숨김 비밀번호를 입력하며 sudo도 같은 비밀번호를 쓴다.
다음 타겟은 이전 설치 옵션을 재사용하거나 별도로 고른다. 서비스 주소/Secret은 복사하지 않는다.
기본 결과는 사용자 홈의 `eva-install.yaml`(`0600`)이며, 필수 입력을 채우라는 안내만 하고 설치하지 않는다.
기존 파일은 덮어쓰지 않으며 `--output FILE`로 다른 위치를 고른다. 타겟별 컴포넌트 선택은 Plan의
설치 범위에 보존된다. Bastion·Main은 설치 대상이 아니다. Online 타겟은 외부 저장소에 직접 접근하며
Remote 타겟은 준비된 Main Harbor를 사용한다. Remote에서는 별도 `remote target` 등록 없이
Main의 `eva install --config FILE`이 전달과 타겟 설치를 실행한다. 설치 후 Main의 `eva check`는 Remote 타겟을
점검한다. Online의 `eva check`와 `preflight gpu`는 실행 서버 기준이다.

Bastion의 설치/preflight는 설정된 SSH 계정·비밀번호로 연결하고 최초 host key를 자동 등록한다.
별도 SSH 접속 준비는 필요 없으며 이미 등록된 키가 변경되면 중단한다.

설치 입력을 파일 하나로 관리하려면 `--config FILE`을 사용한다. 옵션/자동화용 Online/Remote/Local 샘플은
`eva config sample --mode online|remote|local`로 얻으며 원본 설정은 `0600`으로 관리한다.
터미널에서는 IAM 함께 설치/외부 IAM 연동을 먼저 선택하고 함께 설치인 경우 주소 구성도 고른다.
`--iam external --protocol https`는 IAM 설치를 끄고 App에 외부 IAM·client·Redis 연결값을 입력하는 틀을 만든다.
외부 모드에서 `--protocol`은 App에만 적용되고 IAM/Redis 연결은 독립적이다. `--address shared`는 사용하지 않는다.
App SSO의 `brokerMode: false`, `brokerIdpHint: ""`은 외부 IdP broker 연동 시 true와 alias로 변경한다.
Redis는 `port: 32079`, `db: 0`, `password: eva-redis-pass`를 실제 IAM과 맞추며 `url`은 선택 override이다.
빈 URL은 App 런타임이 SSO baseUrl의 host/TLS와 Redis 필드로 조합한다.
`--address shared|separate --protocol https`로 질문을 생략할 수 있고 `--non-interactive` 또는
비TTY 입력의 기본값은 `install`/`separate`/`https`이다. `--output FILE`은 기존 파일을 덮어쓰지 않는다.
HTTP는 격리된 검증 환경에서 사용하며 운영 환경에서는 HTTPS와 유효한 인증서를 준비한다.
`<...>` 필수 입력 옆에는 예시 주석이 있으며 모두 실제 값으로 교체해야 한다.
미입력은 Secret 값 대신 필드 경로로 안내한다.
`--config`는 install/plan, `preflight argocd`, Workspace 명령에 적용되고 `--workspace`와
동시에 사용하지 않는다. `workspace validate --config`는 입력만 검사한다. 그 외 명령은
private 관리 Workspace를 생성할 수 있으며 `plan --config`도 예외가 아니다.
설정 수정 후에는 새 install을 시작하고 retry는 이전 snapshot을 사용한다.
Remote Main 설치의 Chart Values는 `targets.<name>.values`에 작성한다. 이 실행 방식에서는
CLI `--chart`·`--values`·`--set`을 사용하지 않는다.
전체 schema와 수명 규칙은 [단일 설치 설정](../installation/single-file-configuration.md)을 참고한다.

`--workspace` 생략 시 `--site ID`는 `/etc/eva/sites/<ID>`를 선택한다.
Site 입력의 component는 `infra`, `iam`, `agent`, `vision`, `app`, `n8n`이다.
`--component NAME`은 install/plan에 반복 지정할 수 있고 `all`은 활성화된 항목 전체를 선택한다.
Agent/Vision/App을 선택하면 Config 단계가 자동 포함된다.
`--yes`는 install/apply/retry의 실행 확인을 생략하지만 TLS, Argo CD handoff, APT 정책을 우회하지 않는다.
Retry는 중단 task부터 resume하거나 전체 rollback하는 기능이 아니다.

```bash
sudo eva install --workspace /etc/eva/sites/customer-a --yes
sudo eva install --workspace /etc/eva/sites/customer-a --component app
sudo eva install --workspace /etc/eva/sites/customer-a --component app --chart app=/path/app-fix.tgz
sudo eva install --workspace /etc/eva/sites/customer-a --component app --values app=/path/app-fix.yaml
sudo eva install --workspace /etc/eva/sites/customer-a --component app --set app:replicaCount=2
```

`--chart COMPONENT=PATH`, `--values COMPONENT=PATH`, `--set COMPONENT:KEY=VALUE`는
IAM/App/Agent/Vision에 적용한다. 한 component의 Chart/Values 파일은 각각 하나이고
`--set`은 반복 지정할 수 있다. Secret이 필요한 override는 shell history에 남는
`--set`보다 승인된 private Values 파일을 사용한다. 조회 시에는 Secret을 가리지 않는다.

install/plan의 `--release`와 positional Release는 동시에 사용하지 않는다.
`install`, `verify`, `airgap prepare`, Remote preparation 명령은 Current Release를 사용할 수 있다.
현재 `plan`과 `release` 하위 명령은 기본적으로 현재 디렉터리 또는 명시적 경로를 해석하므로
원하는 Release를 `--release PATH`로 명시하는 편이 안전하다.
저장 Plan을 apply할 때는 실행 가능한 준비된 Release 소스를 사용한다.

### Release와 Workspace

| 명령 | 역할 |
| --- | --- |
| `eva verify [--release PATH \| PATH]` | 선택 Release 또는 Airgap Bundle 검증; 등록 Current Release 지원 |
| `eva release validate [--release PATH \| PATH]` | 로컬 Release 검증 |
| `eva release show [--release PATH \| PATH]` | 버전, 플랫폼, artifact 정보 |
| `eva release env [--release PATH \| PATH]` | Release 환경변수 표시 |
| `eva release prepare --release PATH [--install-root PATH]` | 검증 Release를 실행 소스로 추출 |
| `eva release import-airgap --bundle PATH [--artifact-root PATH]` | Airgap Bundle 검증·반입 |
| `eva workspace validate --config FILE` | 단일 설정 검증; Workspace 생성 없음 |
| `eva workspace validate --site ID \| --workspace PATH` | Site 입력 검증 |
| `eva workspace show --site ID \| --workspace PATH` | Workspace 경로·모드·component 표시 |
| `eva workspace ansible-vars --site ID \| --workspace PATH` | Ansible 전달 변수 표시 |
| `eva workspace env --site ID \| --workspace PATH` | Workspace 환경변수 표시 |

현재 Git tag나 S3 위치로 Release를 자동 다운로드하는 일반 명령은 제공하지 않는다.
`--release-root`는 inspect 전용 진단 입력이고 install의 `--release`와 의미가 다르다.
`release prepare`는 Infra·Solution 실행 소스만 추출하며 `eva-offline`의 `out/cache`는 배치하지 않는다.
Local의 검증·반입·새/빈 cache 배치는 [Local Runbook](../installation/local-repository-runbook.md)을 따른다.

### Airgap 준비 — 인터넷 준비 서버에서 실행

| 명령 | 역할·변경 범위 |
| --- | --- |
| `sudo eva airgap prepare [--release ORIGINAL_DIR] --output FILE.tar.gz --vllm-profile PROFILE [--driver-package PACKAGE] [--display-mode-selector FILE.zip] --yes` | 원본 Release에 맞는 오프라인 자산·Runtime을 받아 검증된 새 Local Bundle 생성; Docker pull/export 및 누락 Runtime 온라인 bootstrap |
| `eva airgap prepare --help` | 지원 옵션·실행 범위 조회 |

- `--release` 생략 시 Current Release를 사용한다. installer artifact를 포함한 **검증된 완전한 원본
	Release 디렉터리**만 허용하며 prepared/imported Release와 Bundle archive는 입력으로 쓰지 않는다.
- `--output FILE.tar.gz`, `--vllm-profile`, `--yes`는 필수다. profile은 `A6000x1`, `L40sx1`,
	`PRO5000x3`, `PRO6000-MIGx4` 중 하나이며 선택한 vLLM 이미지만 받는다. 모든 profile Values가
	포함되어도 실제 대상 GPU/MIG·설치 profile과 다르면 사용할 수 없다.
- driver 기본값은 `nvidia-driver-580`; `nvidia-driver-NNN` 또는 `nvidia-driver-NNN-open`을 지원한다.
	전체 의존성 repo를 준비하고 누락을 허용하지 않는다. Display Mode Selector 옵션은 특정 GPU
	전환에 필요한 승인된 독점 배포 ZIP 입력이며 GPU/MIG 구성을 실행하지 않는다.
- **온라인 Ubuntu 24.04 amd64에서 root(`sudo`)**로 실행한다. 동작하는 amd64 Docker daemon,
	AWS CLI, sudo, Ubuntu·Docker·NVIDIA APT 인덱스와 package candidate가 선행 조건이다.
	필수 OS 도구 전체 목록과 대상 kernel·저장공간 조건은 Local Runbook을 따른다.
- AWS credential은 `/var/lib/eva/credentials/aws_key.ini`를 재사용하고 STS로 검증한다. 없으면
	대화형 숨김 입력을 사용하며 CLI Secret 옵션은 없다. 비대화형은 승인된 credential 파일이 필요하다.
- Infra/App/IAM/Agent/Vision 패키지·Chart·helper, n8n 이미지, 모델, Harbor Qdrant snapshot,
	Harbor offline installer도 준비한다. 기존 유효 Runtime은 재사용하며 없으면 **온라인 bootstrap의
	APT/pip 호스트 변경**을 `--yes`로 승인한다. 손상된 Runtime은 먼저 복구한다.
- Docker/AWS 설치, Harbor 배포·이미지/snapshot 게시, GPU driver 설치·MIG 전환, Docker daemon
	설정 변경은 하지 않는다. 대상의 cache 배치와 Repository 준비는 별도 작업이다.
- 출력 옆의 격리 임시 작업은 성공·실패 시 정리하고 resume하지 않는다. 기존 출력은 거부하며
	검증된 Bundle만 최종 경로에 게시한다. 다운로드 backend 출력은 숨기고 단계명·최종 결과를 표시한다.
	누락 Runtime bootstrap 시 기존 설치기의 APT/pip 진행 출력은 추가될 수 있다.
	다운로드·staging 복사본·검증 압축 해제까지 고려한 상당한 디스크 공간이 필요하다.
- 결과는 installer와 Tool archive가 root에 나란히 있는 flat Release Bundle이다. 새 metadata와
	checksum을 만들고 원본은 변경하지 않는다. `eva-offline_<version>_linux_amd64.tar.gz`에는
	`runtime/`과 `out/cache/`가 있다. `eva verify --release BUNDLE.tar.gz`로 검증할 수 있으며,
	credential은 반입하지 않는다. 최초 대상 검증은 Tool 설치 전에 OS checksum 검증부터 시작한다.

실행 예시·안전한 최초 반입·cache 배치와 별도 Harbor 준비는
[Local Repository 준비·설치](../installation/local-repository-runbook.md)를 따른다.

### Remote 준비·설치 — Main에서 실행

| 명령 | 역할 |
| --- | --- |
| `eva remote bootstrap --registry HOST:PORT --yes` | Runtime, Docker, Harbor, registry 인증 준비·검증 |
| `eva remote bootstrap --registry HOST:PORT --replace-registry --yes` | 승인된 registry 교체 |
| `eva remote prepare [RELEASE_PATH] [--registry HOST:PORT]` | Runtime/payload/image 등 준비 |
| `eva remote verify [RELEASE_PATH] [--registry HOST:PORT]` | 준비 결과 계약 검증 |
| `eva install configure` | Remote와 SSH 설치를 선택해 타겟별 설정 작성 |
| `eva install --config FILE --yes` | 자산 전달·Tool 설치·타겟별 EVA 설치 |
| `eva check [--verbose]` | 최근 Remote 설치 작업의 타겟별 서비스 점검 |

첫 bootstrap 이후 registry는 receipt에서 해석한다. 준비·검증이 끝나면 Main에서
설정 파일을 작성하고 `install`을 실행한다. 별도 Target 등록이나 수동 전달 명령은 필요하지 않다.
Harbor 설치 방식과 HTTP/HTTPS는 bootstrap이 자동 확인한다. 기존 Harbor는 재사용하고,
저장된 인증이 유효하지 않으면 같은 명령 안에서 게시 계정을 입력받는다. 별도 `docker login`은 필요하지 않다.
`remote verify`는 준비 자산을 검증하며 실제 타겟 서비스는 설치 후 `eva check`로 확인한다.
`status`·`retry`·`apply`는 Main의 설치 작업을 대상으로 한다. 타겟별 Values·Config·Helm 백업은 타겟에서 조회한다.

개별 Target 관리와 전달만 필요할 때 다음 명령을 사용할 수 있다.

| 명령 | 역할 |
| --- | --- |
| `eva remote target add NAME` | SSH/sudo 입력을 받아 관리 Target 등록 |
| `eva remote target list` | 등록 Target 목록, credential은 표시하지 않음 |
| `eva remote target verify NAME` | SSH, sudo, 플랫폼, storage, inbox 사전 검증 |
| `eva remote target remove NAME [--yes]` | 지정 Target 등록·credential 삭제 |
| `eva remote publish [RELEASE_PATH] --target NAME [--registry HOST:PORT]` | 검증된 Release와 delivery를 Target으로 게시 |

`remote publish`는 파일 전달만 수행한다. 이 명령으로 별도 전달한 경우에는 타겟에서 Tool을 설치하고
현재 서버 설치 설정으로 `eva install`을 실행한다. `--target USER@HOST`는 SSH key/passwordless 경로를 전제로 한다.

### Runtime·사전 점검·고급 실행

| 명령 | 역할 |
| --- | --- |
| `eva runtime show [--runtime-root PATH]` | 관리 도구 버전·실행파일 경로 |
| `eva runtime validate [--runtime-root PATH]` | Runtime 유효성 검사 |
| `eva runtime bootstrap [--offline PATH] [--runtime-root PATH]` | pin된 online Runtime 또는 offline archive로 구성 |
| `eva runtime install --source PATH [--runtime-root PATH]` | 추출된 Runtime payload 설치 |
| `eva preflight gpu` | NVIDIA driver·GPU·MIG 상태 확인 |
| `eva preflight argocd --workspace PATH` | 기존 관리 연결 검증 및 승인 기반 handoff |
| `eva troubleshoot apt` | 지원 APT 문제 진단 |
| `eva troubleshoot apt --fix-known --yes` | 명시적으로 지원되는 APT 복구 |
| `eva shell [--site ID \| --workspace PATH] [--release PATH]` | 관리 Runtime 환경의 shell |
| `eva shell --command CMD` | 관리 Runtime shell에서 명령 실행 |
| `eva exec <ansible-playbook\|helm\|kubectl\|kustomize\|oras> [args...]` | 관리 도구 직접 실행 |

`shell`/`exec`는 Operation에 기록되지 않는 고급 실행이다. 명령에 따라 cluster를 변경할 수 있다.
`exec --runtime-root PATH`로 다른 관리 Runtime을 지정할 수 있다.
Runtime bootstrap과 Online Infra 설치는 지원하는 APT 저장소 문제를 자동 복구하고 재검증한다.
Bastion 설치는 각 타겟에서 처리한다. `troubleshoot apt`는 개별 진단이 필요할 때 사용하는 명령이며
일반 설치 절차에서 별도 실행하지 않는다.
내부 installer용 `eva internal ...`은 공개 설치자 명령이 아니며 직접 사용하지 않는다.

## 5. 조회 문제 해결

| 증상 | 확인할 명령·조치 |
| --- | --- |
| `unknown command "inspect"` | 기존 Tool이다. 변경이 포함된 Release/Tool을 배포하고 `eva help` 확인 |
| Operation store 읽기 권한 오류 | `sudo eva inspect ...` 사용; chmod로 private 상태를 공개하지 않음 |
| 설치가 아닌 Main prepare/publish만 있음 | Target에서 `eva install` 후 Target에서 조회 |
| matching Operation이 없음 | `inspect operations` 또는 직접 Ansible의 `--release-root ... --site ...` 사용 |
| 최신 작업이 다른 Site 또는 계획만 저장된 작업 | `inspect operations --site ID` 후 `--operation ID` 지정 |
| Target이 여러 개 | `inspect files`에서 실제 Target key를 보고 `--target` 지정 |
| resolved 파일 없음 | App 단계 완료 여부·`inspect result`·`inspect logs` 확인. 입력만 보려면 `--source effective` |
| 과거 Operation인데 Values가 달라 보임 | 재설치가 공유 산출물을 덮어썼는지 확인. Operation별 snapshot은 아님 |
| 기본 경로에 파일이 없음 | `inspect paths`; custom Ansible 경로이면 `--rendered-root`/`--config-root` 지정 |

정확한 지원 option은 `eva help`, `eva inspect --help` 및 각 하위 명령의 `--help` 출력을 기준으로 한다.
