# EVA Deployer

EVA Deployer는 Ansible 기반으로 EVA 설치 환경과 서비스를 구성하는 배포 도구입니다.
고객사 인프라와 환경 설정을 확인한 뒤, **동일한 EVA Release에 설치 옵션과 Repository 공급 경로를 적용**합니다.
상세 준비·설치·검증 절차는 아래에서 환경에 맞는 **Runbook 하나를 선택**해 따릅니다.

## 설치 위치와 인프라 준비

| 설치 위치 | 인프라 준비와 설치 방향 |
| --- | --- |
| **On-Premises** | 고객사 서버·네트워크를 준비하고, Deployer로 권장 구성의 k3s·GPU 실행 환경·영구 저장소 연결과 EVA 서비스를 설치합니다. |
| **Cloud** | Cloud 콘솔에서 네트워크·Kubernetes·저장소·DB·서비스 접속 환경을 먼저 구성한 뒤, Bastion에서 Deployer로 EVA를 설치하는 방향입니다. Repository는 Online을 사용합니다. |

현재 Runbook의 기본 설치 기준은 **Ubuntu 24.04, `linux/amd64`, k3s**입니다.
Cloud의 사전 구성 Kubernetes에 배포할 때는 사용하는 Release의 지원 범위를 먼저 확인합니다.

## Repository mode 선택

설치 위치와 별개로, **설치 대상의 네트워크 접근 조건**에 따라 선택합니다.

| 모드 | 선택 조건 | 설치 가이드 |
| --- | --- | --- |
| **Online** (`online`) | 대상이 인터넷을 통해 외부 Registry·Repository와 필요한 서비스에 직접 접근 | [Online Repository Runbook](docs/installation/online-repository-runbook.md) |
| **Remote** (`remote`) | 대상은 인터넷에 직접 접근하지 못하지만, 인터넷에 연결된 Main 서버의 Harbor와 설치 자산에 접근 가능 | [Remote Repository Runbook](docs/installation/remote-repository-runbook.md) |
| **Local** (`local`) | 인터넷과 Main Harbor에 접근할 수 없는 폐쇄망에 이미지·모델·패키지·설치 자산을 반입하고 내부 Harbor 사용 | [Local Repository Runbook](docs/installation/local-repository-runbook.md) |

Online은 인터넷 연결이 가능한 On-Premises에서도 사용합니다. **Bastion만 인터넷에 연결되고
설치 대상은 오프라인이면 Online이 아닙니다.** Remote·Local은 On-Premises 제한망·폐쇄망에서 사용합니다.
설치 명령과 설정값은 설치에 사용하는 **Tool·Release 버전의 Runbook**을 기준으로 합니다.

## 설치 전 준비

- **설치 대상:** Site, 서버 자원, 설치할 서비스와 GPU·저장소 요구 사항을 정합니다.
- **접속·네트워크:** 설치 계정과 sudo·SSH·클러스터 권한, 방화벽·라우팅, Repository 접근 조건을 확인합니다.
- **서비스 연결:** 서비스 주소·TLS 인증서, DB·저장소와 IAM 함께 설치·외부 IAM 연결값을 준비합니다.
- **고객사 설정:** 인증·세션, 데이터 보관·영상 처리 정책과 발급된 라이선스·활성화 방식을 확정합니다.

## 기본 설치 흐름

1. **Runbook 선택:** 설치 위치·Repository 유형을 정하고, 해당 Runbook에 따라 인프라·자산·Tool·Release를 준비하고 검증합니다.
2. **설정 작성:** 일반 계정으로 `eva install configure`를 실행해 설치 옵션을 고르고, 생성된 private YAML에 남은 필수 값과 고객사 설정을 입력합니다.
3. **입력 검증:** `sudo eva workspace validate --config <설정 파일>`로 필수 입력과 형식을 확인합니다. 서버·네트워크·GPU·인증서 조건은 Runbook의 사전 점검 절차를 따릅니다.
4. **설치 실행:** `sudo eva install --config <설정 파일> --yes`를 실행합니다. Deployer가 내부 Workspace와 서비스별 Helm Values를 생성·병합해 설치합니다.
5. **결과 확인:** 설치 상태·로그와 접속·로그인·카메라 분석·데이터 저장 동작을 확인하고 운영 담당자에게 인계합니다.

Secret이 포함된 원본 설정은 권한 `0600`으로 관리하고 Git이나 Release·Bundle에 넣지 않습니다.
설정 변경은 원본 YAML에 반영한 뒤 `install --config`로 새 작업을 시작합니다.
`retry`는 이전 작업의 snapshot을 사용하므로 변경한 원본 설정을 다시 읽지 않습니다.

## 추가 안내

- [단일 설치 설정](docs/installation/single-file-configuration.md): 입력 schema, 여러 target, Values와 snapshot 규칙
- [EVA Tool 명령어 가이드](docs/operations/eva-cli-command-guide.md): 설치 상태, 최종 Values, 생성 파일, 작업 이력과 로그 조회
- [Airgap Bundle 준비](docs/preparation/airgap-bundle-preparation.md): Local 설치용 자산 준비·반입