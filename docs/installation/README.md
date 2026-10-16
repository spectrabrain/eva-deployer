# EVA Installation Runbooks

설치 환경의 Repository mode에 해당하는 Runbook 하나를 선택해 사용합니다.

## Repository mode 선택

- `cloud`: Target이 Cloud Repository와 외부 서비스를 직접 사용합니다.
  - [Cloud Repository Runbook](cloud-repository-runbook.md)
- `remote`: Target은 Main Harbor와 Main 서버가 준비한 설치 자산만 사용합니다.
  - [Remote Repository Runbook](remote-repository-runbook.md)
- `local`: Airgap Bundle과 Local Harbor를 사용합니다.
  - [Local Repository Runbook](local-repository-runbook.md)

각 Repository Runbook은 설치자가 해당 문서 하나만 보고 전체 설치와 검증을 수행할 수 있도록 작성합니다.

설치 후 최종 Values, 생성 파일, 작업 이력과 로그를 조회하거나 Tool 명령 목록을 확인할 때는
[EVA Tool 명령어 가이드](../operations/eva-cli-command-guide.md)를 사용합니다.

Local Airgap Bundle 준비가 필요한 경우에만 다음 문서를 함께 사용합니다.

- [Airgap Bundle Preparation](../preparation/airgap-bundle-preparation.md)
