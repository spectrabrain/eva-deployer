# EVA Installation Runbooks

설치 환경의 Repository mode에 해당하는 Runbook 하나를 선택해 사용합니다.

## Repository mode 선택

- `online`: Target이 upstream/인터넷 Registry·Repository와 외부 서비스에 직접 접근합니다.
  - [Online Repository Runbook](online-repository-runbook.md)
- `remote`: Target은 Main Harbor와 Main 서버가 준비한 설치 자산만 사용합니다.
  - [Remote Repository Runbook](remote-repository-runbook.md)
- `local`: Airgap Bundle과 Local Harbor를 사용합니다.
  - [Local Repository Runbook](local-repository-runbook.md)

`online`은 온프레미스/클라우드 제공자와 무관합니다. Bastion만 인터넷에 연결되고 Target은
오프라인이면 `online`이 아닙니다. Main Harbor를 쓰면 `remote`, 저장매체로 반입한 Bundle과
Local Harbor를 쓰는 완전 폐쇄망이면 `local`을 선택합니다.

각 Repository Runbook은 설치자가 해당 문서 하나만 보고 전체 설치와 검증을 수행할 수 있도록 작성합니다.

## 설치 입력: YAML 파일 하나

새 설치는 **sudo 없이 `eva install configure`**로 시작합니다. Online/Remote/Local을 고르고
Online은 현재 서버/Bastion SSH 설치를 선택합니다. Bastion은 타겟을 한 대씩 등록하고 타겟별
Infra-only/Solution, HTTP/HTTPS, 동일/별도 도메인/IP, IAM 함께 설치/외부 IAM을 선택합니다.
다음 타겟은 설치 옵션을 재사용할 수 있지만 주소와 비밀번호는 독립적입니다.
사용자 홈의 `eva-install.yaml`에 남은 필수 값을 채운 뒤 `eva install --config <파일> --yes`로 실행합니다.
기존 `eva config sample --mode online|remote|local`은 단일 서버용 옵션/자동화 명령으로 유지합니다.
샘플은 게시 App `3.3.0-beta.3` / IAM `3.3.0-beta.1` 기준입니다. 이 변경을 포함한 Tool과 Release를
사용합니다. HTTP는 격리 테스트용이며 인증서 검사·TLS 생성 단계를 생략하고 HTTPS 검증은 유지합니다.
실제 설치 E2E는 별도 검증 대상입니다.
두 생성 명령의 `--output <파일>`은 덮어쓰기 없이 새 `0600` 파일을 만듭니다. `<...>` 필수 입력이 남으면 검증이 거부됩니다.
`sudo eva workspace validate --config <파일>`은 입력만 검사하고 파일을 생성하지 않습니다.
Deployer가 inventory·credential·component Values를 내부 Workspace로 변환하므로 설치자가
폴더와 여러 파일을 직접 만들 필요가 없습니다. 샘플은 Tool에 내장되어 source checkout이 필요 없습니다.

입력 schema, 여러 target, 경로 규칙과 snapshot 수명은
[단일 설치 설정](single-file-configuration.md)을 참고합니다.

원본 설정은 `0600`으로 관리합니다. 생성 Workspace는
`/var/lib/eva/workspaces/<site-id>/<content-sha256>/`에 보존하며 디렉터리 `0700`, 파일 `0600`입니다.
설정 수정 후에는 `install --config`로 새 작업을 시작합니다. `retry`는 이전 작업의 snapshot을
사용하고 변경한 원본 YAML을 다시 읽지 않습니다. 기존 `--workspace` 방식도 계속 지원합니다.

설치 후 최종 Values, 생성 파일, 작업 이력과 로그를 조회하거나 Tool 명령 목록을 확인할 때는
[EVA Tool 명령어 가이드](../operations/eva-cli-command-guide.md)를 사용합니다.

Local Airgap Bundle 준비가 필요한 경우에만 다음 문서를 함께 사용합니다.

- [Airgap Bundle Preparation](../preparation/airgap-bundle-preparation.md)
