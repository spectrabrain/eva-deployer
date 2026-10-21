# 기존 Helm 사용자 Values 자동 백업

기존 솔루션을 다시 설치하거나 업그레이드할 때 **변경 전 release에 저장된 사용자 Values**를
자동으로 백업한다. 새 installer 입력이나 업그레이드 후의 resolved Values와는 별도이다.
App DB 이전·로컬 사용자 CSV 보존은 [App DB 마이그레이션](app-database-migration.md)을 참고한다.

## 대상과 실행 시점

| 컴포넌트 | 기본 Helm release | 백업 파일 |
| --- | --- | --- |
| App | `eva-app` | `app-user-values.yaml` |
| IAM | `eva-iam` | `iam-user-values.yaml` |
| Agent | `eva-agent` | `agent-user-values.yaml` |
| Vision | `eva-vision` | `vision-user-values.yaml` |
| Agent init | `eva-agent-init` | `agent-init-user-values.yaml` |
| Qdrant | `eva-agent-qdrant` | `qdrant-user-values.yaml` |
| vLLM | `eva-agent-vllm` | `vllm-user-values.yaml` |

선택한 설치 role에서 해당 release의 reconciliation, scale-down, Helm upgrade보다 먼저 실행한다.
App/IAM/Vision의 사용자 지정 release 이름, 각 role의 namespace 및 kubeconfig를 따른다.
Online/Remote/Local 공통 동작이며, Helm이 아닌 manifest로 설치하는 n8n은 대상이 아니다.

Helm release가 실제로 없으면 백업 파일 없이 건너뛴다. failed/pending 상태 release도 현재 저장된
revision의 Values를 보존한다. API/RBAC/인증 오류, Helm 조회 실패, 파일 쓰기 오류는 신규 설치로
간주하지 않고 **설치를 중단**한다. Check mode에서는 백업 조회와 파일 생성을 하지 않는다.
Helm 이력이 없는 Argo CD 전용 설치는 `helm get values`로 기존 사용자 설정을 복원할 수 없다.

## 저장 내용과 경로

`helm get values <release> -n <namespace> --revision <기존 revision> -o yaml` 결과를 그대로 저장한다.
**`--all`을 쓰지 않으므로 Chart 기본값을 합친 computed Values가 아니다.** Helm에 기록된 사용자
override에는 과거 installer가 전달한 값도 포함될 수 있다. 사용자 override가 없으면 `null`도
정상적인 백업 내용이다. Kubernetes Secret의 실시간 변경이나 manifest 수동 수정은 포함하지 않는다.

기본 저장 구조는 다음과 같다. 파일은 **Ansible 대상 서버**에 남으며 별도 Control Node로 자동
복사하지 않는다. 일반 Remote/Local 설치에서는 Target에서 확인한다.

```text
/var/lib/eva/backups/helm-values/
  <site>/<namespace>/<기존 배포 버전>/<UTC 시각>-<random>/
    iam-user-values.yaml
    metadata.json
```

예: 기존 App이 `3.3.0-beta.1`이면 새 설치 버전과 관계없이
`<site>/<namespace>/3.3.0-beta.1/<UTC 시각>-<random>/`에 저장한다.
버전은 **변경 전 Helm release의 `app_version`**을 우선 사용한다. 없거나 경로에 안전하지 않으면
Chart 이름 끝의 버전(prerelease·`+build` 포함), 그것도 없으면 `unknown-revision-<revision>`을 사용한다.
Deployer 버전이나 새로 설치할 버전은 사용하지 않는다. 같은 namespace/버전의 서로 다른 release도
매번 고유한 실행 디렉터리로 보존하며, 원래 release 이름은 metadata에 기록한다.

role별 작업 디렉터리나 Release 압축 해제 경로와 독립적이므로 재설치 시 작업 디렉터리를 지워도
유지된다. 매 실행마다 새 디렉터리를 만들어 기존 백업을 덮어쓰지 않는다. 성공한 백업의 경로와
revision은 Ansible 출력에 표시하지만 **Values 내용은 출력하지 않는다**.

- 디렉터리 권한 `0700`, 파일 권한 `0600`; 기본 설치에서는 root 소유이다.
- 비밀번호·토큰·라이선스 키 등이 **원문 포함될 수 있다**. Git 등록, 일반 로그 첨부, 공유 폴더 복사를 하지 않는다.
- metadata에는 release, namespace, revision, Chart/App 버전, 상태, UTC 시각과 Values SHA-256을 기록한다.
  선택된 `version`과 `version_source`(`app_version`/`chart`/`revision`)도 기록한다.
- 조회 중 다른 작업이 release를 변경하면 중단한다. 남은 `.partial`은 완료된 백업이 아니다.
  `metadata.json`까지 생성된 백업을 사용한다. 동시 설치 자체를 허용하거나 잠그는 기능은 아니다.
- 백업 경로의 symlink는 거부한다. 백업 실패 원인에 Secret이 섞일 수 있어 Helm 상세 오류는 로그에 노출하지 않는다.

실패 메시지는 `release lookup` / `user Values export` 단계, 종료 코드와 고정된 `reason`만
표시한다. `unsupported-cli-option`은 CLI 호환성, `kubernetes-rbac-denied`는 RBAC,
`kubernetes-authentication-failed`는 인증, `kubernetes-tls-error`는 API 인증서,
`kubernetes-connection-failed` / `kubernetes-unreachable`은 API 연결,
`kubeconfig-error`는 kubeconfig를 확인한다. 원문 stderr나 Values는 노출하지 않는다.

백업 실패 시 해당 컴포넌트의 Helm upgrade는 진행하지 않는다. 앞선 Infra/Config 단계는 이미
실행되었을 수 있으므로 Operation 결과를 함께 확인한다. 기존 release를 삭제하거나 백업 실패를
무시해서 우회하지 않는다. 원인을 해결하고 설정을 변경했다면 새 설치 Operation으로 실행한다.

저장 root를 바꾸려면 설치 YAML의 해당 Target `ansible` 설정에
`eva_helm_values_backup_root: /보호된/절대경로`를 지정한다. Chart Values에 넣지 않는다.
기본 저장 root를 override할 때도 삭제되는 workdir/Release 디렉터리 아래는 피한다.

## EVA 명령으로 확인

**실제 백업이 저장된 Ansible 대상 서버에서**, 해당 서버에 설치된 `eva`로 실행한다.
기본 백업은 root 전용이므로 필요하면 아래처럼 `sudo`를 사용한다. 명령 자체는 권한 상승,
Runtime bootstrap, 클러스터 조회·변경, Control Node로의 파일 가져오기를 수행하지 않는다.

```bash
# 목록: 메타데이터와 디렉터리 경로만 표시 (eva backup도 동일)
sudo eva backup list --site <site> --component app

# 한 release의 가장 최근 백업 메타데이터와 파일 경로
sudo eva backup show --site <site> --namespace <namespace> --release <release> --latest

# 기존 버전으로 조회 범위 제한
sudo eva backup list --site <site> --version 3.3.0-beta.1

# 목록에서 고른 실행 디렉터리의 Values 원문을 명시적으로 출력
sudo eva backup values --path /var/lib/eva/backups/helm-values/<site>/<namespace>/3.3.0-beta.1/<UTC시각-random>
```

- 모든 조회에서 `--root PATH`, `--site ID`, `--component NAME`, `--namespace NS`,
  `--version VERSION`, `--release NAME`으로 필터링할 수 있다. `--root` 기본값은 위 저장 경로이며
  설치 root override 시 같은 값을 지정한다. 목록은 버전 크기가 아니라 **백업 시각 최신순**이다.
- `show`와 `values`는 같은 선택 옵션을 사용한다. 기본적으로 한 건만 일치해야 하며,
  `--path DIR`로 실행 디렉터리 하나를 고르거나 `--latest`를 사용한다. `--path`는 root 기준
  상대경로도 허용하지만 `..`, root 밖 경로, symlink는 거부한다.
- `--latest`도 서로 다른 site/namespace/release/component 사이에서는 자동 선택하지 않는다.
  필터를 더 지정하거나 `--path`를 사용한다. 같은 최신 시각이 여러 건이어도 `--path`로 구분한다.
- 기존 `<site>/<namespace>/<release>/<UTC시각-random>/` 구조도 `metadata.json`으로 식별한다.
  이전 파일을 옮길 필요가 없으며, 버전 필드는 기존 App/Chart metadata에서 계산한다.
- 완료 marker인 `metadata.json` 없는 실행은 건너뛴다. `list`/`show`는 Values 본문을 읽거나
  출력하지 않는다. `values`만 SHA-256 확인 후 원문을 stdout에 출력하고 stderr에 민감정보 경고를
  남긴다. 메타데이터는 64 KiB, 원문 출력은 64 MiB 이내로 제한한다. 원문을 일반 로그에 남기지 않는다.

## 복구 및 주의사항

백업은 자동으로 새 Values에 합치거나 다시 적용하지 않는다. 이전 버전 설정을 무조건 재사용하면
새 Chart의 기본값이나 schema 변경을 덮어쓸 수 있다. 설치자가 기존 파일과 새 Chart를 비교하여
필요한 설정만 현재 site Values 또는 CLI 입력에 반영한다.

이 기능은 DB, PV/hostPath, IAM 계정, 라이선스 파일의 백업이나 전체 Helm rollback을 대신하지 않는다.
특히 Helm rollback은 App DB schema를 되돌리지 않는다. 백업은 자동 만료·삭제하지 않으므로
운영자가 보관 정책과 디스크 용량을 관리해야 한다.
