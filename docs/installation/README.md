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

자산 준비 작업의 상세 절차는 다음 문서를 참고합니다.

- [Remote Asset Preparation](../preparation/remote-asset-preparation.md)
- [Airgap Bundle Preparation](../preparation/airgap-bundle-preparation.md)
