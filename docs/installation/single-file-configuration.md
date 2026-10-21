# 단일 설치 설정

설치자는 원하는 위치의 YAML 파일 하나만 관리한다. Deployer는 이를 검증하고 기존 Workspace
형식으로 변환한다. Ansible playbook, Chart Values 병합, Release·Runtime 관리는 기존 계약을
유지한다. 이 기능은 해당 변경이 포함된 Tool부터 사용할 수 있다.

Repository mode의 표준 입력은 `online` / `remote` / `local`이며 Ansible 모드는 각각
`online_repository` / `remote_repository` / `local_repository`이다. 기본값은 `online`이다.
레거시 `cloud` / `cloud_repository`도 호환 입력으로 허용하며 `online` / `online_repository`로 정규화한다.
모드 선택 기준은 [설치 Runbook](README.md#repository-mode-선택)을 따른다.

## 시작

일반 설치자는 **sudo 없이** 다음 명령 하나로 시작한다.

```bash
eva install configure
```

명령은 사용자 홈의 `eva-install.yaml`(일반 계정이면 `/home/<사용자>/eva-install.yaml`)을
`0600`으로 만든다. 설치, SSH 연결 시험, 자산 다운로드는 실행하지 않는다. 기존 파일이나
symlink는 덮어쓰지 않으며, 다른 위치가 필요하면 `--output FILE`을 사용한다.
`sudo`로 실행해 root 소유 파일을 만드는 대신 일반 계정으로 구성하고 검증·설치할 때만 sudo를 사용한다.

질문 순서는 다음과 같다. 번호를 선택하고 Enter를 누르며 `[번호]`가 기본값이다.

1. **설치환경**: `online` / `remote` / `local`.
2. **Online 실행 방식**: 현재 서버 설치 / 현재 서버를 Bastion(controller)으로 사용.
   Remote/Local은 기존 방식대로 해당 Target에서 구성한다.
3. **Bastion일 때 타겟 한 대 등록**: 닉네임 → IP → SSH 계정 → 숨김 비밀번호.
   private key는 이 흐름에서 받지 않으며 SSH와 sudo에 같은 비밀번호를 쓴다.
4. **타겟 설치 범위**: Infra만 / Infra + Solution.
5. **Solution을 선택했을 때**: HTTP/HTTPS → 동일 도메인/별도 도메인/IP 직접 접속 →
   IAM + APP 함께 설치/APP만 설치(외부 IAM) → 추가 Agent/Vision 구성.
   함께 설치 시 Agent+Vision이 기본이고, APP만 선택하면 추가 설치 없음이 기본이다.
6. **Bastion일 때 타겟 추가/완료**. 다음 타겟은 이전 타겟의 설치 옵션을 재사용하거나
   전부 별도로 선택할 수 있다. 재사용해도 IP·비밀번호·서비스 주소·Secret을 복사하지 않는다.
7. 생성 위치와 **남은 필수 값을 파일에서 채우라는 안내**, 검증·설치 명령을 표시한다.

멀티 타겟을 CSV나 한 줄 목록으로 받지 않는 이유는 각 타겟의 비밀번호를 숨겨 입력하고
Infra-only/외부 IAM/HTTP/HTTPS 조합을 타겟마다 확인하기 위해서다. SSH 계정은 직전 계정을
다음 기본값으로 제시하지만 비밀번호는 매번 따로 입력한다. 잘못된 번호, 중복 닉네임/IP,
예약 닉네임, loopback IP 등은 재입력을 요청한다. 입력 종료·취소 시 출력 파일을 만들지 않는다.

IP 직접 접속은 SSH 대상 IP가 있으면 서비스 host와 함께 설치 IAM의 redirect URI에 미리 채운다.
현재 서버 설치라면 `<TARGET_IP>`를 채운다. HTTPS IP 접속은 그 IP의 SAN이 있는 인증서가 필요하다.
외부 IAM을 선택한 경우 주소/프로토콜 선택은 App에만 적용되고 IAM의 주소는 별도 입력이다.

### Online Bastion의 의미

- 현재 서버에 Tool·Managed Runtime·검증된 Release를 준비하고 `eva install --config`를 실행한다.
  현재 서버는 inventory에 넣지 않으며 실제 Infra/Solution은 등록한 타겟에만 설치한다.
- Bastion에서 각 타겟으로 **직접 SSH**한다. 별도 ProxyJump 게이트웨이나 SSH 터널을 구성하는 기능은 아니다.
- Bastion의 인터넷 연결만으로 충분하지 않다. 각 타겟도 upstream/인터넷 Repository·ECR·S3 및 필요한 외부 서비스에
  접근할 수 있어야 한다. 타겟은 Ubuntu·GPU 등 기존 사전 조건을 충족해야 한다.
- 대상 계정에 SSH password 인증과 같은 비밀번호를 사용하는 sudo를 허용해야 한다.
  설치/preflight가 설정된 계정·비밀번호로 연결하며 최초 SSH host key를 자동 등록한다.
  이미 등록된 키가 변경되면 연결을 중단한다.
- 공통 Online AWS credential은 기존 배포 단계에서 각 해당 타겟에 사용된다.
  타겟별로 다른 AWS credential을 받는 기능은 아니며 Remote Main의 `remote target` 등록과도 별개이다.
- 각 타겟은 독립 설치 대상이다. 여러 타겟을 입력한다고 하나의 multi-node k3s cluster를 만드는 것은 아니다.
- 기존 `eva preflight gpu`와 `eva check`는 실행한 현재 서버 기준이다. Bastion에서 실행해
  원격 GPU/서비스 검증이 완료됐다고 판단하지 않는다. 타겟에서 별도로 확인하고 실제 설치 precondition과
  타겟별 생성 Config·로그를 함께 확인한다.

### 기존 샘플 명령과 자동화

기존 `eva config sample`도 계속 지원한다. 단일 서버용 틀을 옵션으로 직접 고르거나
비대화형 자동화에서 사용한다. 이 명령은 환경 선택과 멀티 타겟 등록을 하는 wizard는 아니다.

승인된 위치를 선택한다. 기존 설정이 있으면 덮어쓰지 말고 다른 파일 이름을 사용한다.

```bash
eva config sample --mode online --output "$HOME/eva-install.yaml"
```

Remote/Local 설치는 `--mode remote`/`--mode local`을 사용한다. 샘플은 binary에 내장되어
source checkout이 필요 없다. 터미널에서는 다음 목록을 차례로 선택한다.

```text
IAM 구성
  1) IAM 함께 설치 (동일 target SSO handoff)
  2) 외부 IAM 연동만 사용 (App SSO 직접 입력)
선택 [1]:

주소 구성
  1) EVA APP과 EVA IAM 동일 도메인/IP, path 분리 (App /, IAM /iam)
  2) EVA APP과 EVA IAM 다른 도메인 (각각 /)
선택 [1]:

외부 접속 환경 (v3.3 대상)
  1) HTTP (인증서 불필요, 격리된 검증 환경용)
  2) HTTPS (인증서 필요, 권장)
선택 [2]:
```

주소 구성 질문은 IAM 함께 설치를 선택했을 때만 표시된다. 외부 IAM을 선택하면 주소 질문을
생략하고 App의 HTTP/HTTPS를 선택한다. IAM baseUrl과 Redis 연결값은 App 프로토콜과 독립적이다.

내부에는 Online/Remote/Local 기본 틀, 네 가지 함께 설치 Values 틀과 외부 IAM용 App HTTP/HTTPS 틀이 있다.
선택한 조합으로 합쳐 주석을 보존한 단일 YAML을 만든다. mode는 이미지 공급 방식이며
서비스의 HTTP/HTTPS와는 별개이다. 기본값은 터미널에서 **IAM 함께 설치 + 동일 주소 + HTTPS**이다.
도메인 분리 모드는 App과 IAM의 서로 다른 도메인을 사용한다. IP 직접 접속은 동일 주소 모드를
사용하고 HTTPS 인증서에 해당 IP SAN을 포함한다.

`--output`은 존재하지 않는 파일을 `0600`으로 생성하며 기존 파일·symlink를 덮어쓰지 않는다.
부모 디렉터리는 이미 있어야 한다. 이 명령은 설치나 credential 입력을 실행하지 않는다.

자동화에서는 선택값을 명시한다. 기존 `--address`와 `--protocol`을 모두 지정하는 명령은
`--iam`을 생략하면 함께 설치로 처리하여 추가 질문 없이 유지한다.

```bash
eva config sample --mode remote --address shared --protocol https --output "$HOME/eva-install.yaml"
eva config sample --mode local --address separate --protocol https --output "$HOME/eva-install.yaml"
eva config sample --mode online --iam external --protocol https --output "$HOME/eva-external-iam.yaml"
```

`--iam install|external`로 IAM 구성을 명시한다. 외부 IAM에서는 `--address`를 생략한다
(`separate` 명시는 허용하며 `shared`는 거부한다). `--protocol`은 외부 IAM 모드에서 App에만 적용된다.
`--non-interactive` 또는 비TTY 입력에서는 질문하지 않고 생략한 값은 `install`/`separate`/`https`를
사용한다(기존 비대화 기본값 유지). `--output`을 생략하면 YAML은 stdout, 메뉴와 안내는 stderr로
분리하므로 파일 리다이렉션도 가능하다. 리다이렉션으로 파일을 만들면 `umask 077`과 `chmod 600`을
직접 적용한다. TTY 입력이 유지되면 stdout을 파일로 보내도 선택 메뉴가 표시된다.

### 필수 입력

`<SITE_ID>`, `<TARGET_USER>`, `<SUDO_PASSWORD>`, host·credential·license placeholder는
**설치 전 필수 교체**이다. 같은 placeholder가 반복되면 모든 위치에 같은 값을 입력한다.
passwordless sudo라면 `ansible_become_password` 항목을 제거한다. HTTPS는 공통 또는
각 서비스의 인증서 경로를 지정하고 해당 target에 `tls.crt`와 `tls.key`를 준비한다.

필수 항목과 고정 틀/선택 항목은 YAML 주석에 표시한다. 검증은 placeholder가 하나라도 남으면
미입력 필드 경로를 안내하고 Workspace 생성 전에 중단한다. 이미 입력한 Secret이나
placeholder의 실제 문자열은 오류에 출력하지 않는다.

모든 placeholder 값 옆에는 `ex)` 주석으로 예시가 있다. 예를 들어
`host: "<IAM_HOST>" # 필수: IAM 도메인. ex) iam.eva.com`처럼 입력 형식을 안내한다.
예시 IP는 문서용 주소이며, 예시 비밀번호·AWS key·license key는 실제 사용하지 않는다.

### App 라이선스

`targets.<alias>.values.app.app.license.activation_mode`의 생성 기본값은 Online이 `online`,
Remote/Local이 `offline`이다. 라이선스는 설치 전에 결정하며 추가 질문이나 연결 검사를 하지 않는다.
Repository mode는 이미지 공급 방식이므로 라이선스 활성화 방식을 강제하지 않는다.
사전 발급된 라이선스가 온라인 방식이면 Remote에서도 `online`으로 변경할 수 있다.
`online`을 사용할 때는 **App에서 라이선스 서버로 연결**할 수 있어야 한다.

`product_code`는 발급된 라이선스에 맞는 `eva-dev` 또는 `eva-prod`를 입력하고,
그 제품에 발급된 `api_key`와 `shared_key`를 함께 사용한다. 제품 코드만으로
`online`/`offline`을 결정하지 않으며 두 설정 모두 사전 결정한 라이선스와 일치시킨다.

### v3.3 대상 주소·프로토콜 샘플

샘플은 **게시 App `3.3.0-beta.3` / IAM `3.3.0-beta.1`의 Values·template 계약**을 기준으로 한다.
현재 소스의 Release catalog도 이 버전을 사용한다. 이미 설치된 Tool이나 Current Release는 자동
갱신되지 않으므로 변경이 포함된 새 Tool과 Release를 함께 사용한다. 실제 설치 E2E는 별도 검증 대상이다.

| 주소 구성 | 프로토콜 | App 예시 | IAM 예시 | 인증서 입력 |
| --- | --- | --- | --- | --- |
| `shared` | HTTP | `http://192.0.2.10/` | `http://192.0.2.10/iam` | 없음 |
| `shared` | HTTPS | `https://eva.example.com/` | `https://eva.example.com/iam` | 공통 디렉터리 |
| `separate` | HTTP | `http://app.eva.com/` | `http://iam.eva.com/` | 없음 |
| `separate` | HTTPS | `https://app.eva.com/` | `https://iam.eva.com/` | 서비스별 디렉터리 |

위 네 틀은 IAM 함께 설치용이며 각각 Online/Remote/Local과 결합한다. HTTP는 암호화되지 않으므로 격리된 검증 환경용이며
운영 환경에는 HTTPS를 권장한다. 설정 계약은 다음과 같다.

- App/IAM 모두 `ingress.tls.enabled`를 명시한다. `tls.create`는 인증서 Secret 생성 여부이며
  **`create: false`만으로 HTTP로 바뀌지 않는다.** HTTP에서는 둘 다 `false`이다.
- App은 HTTP에 `backendSecure: false`, `backendPublicPort: 80`, HTTPS에
  `backendSecure: true`, `backendSecurePort: 443`을 사용한다.
- IAM `config.publicPort: ""`를 기본 제공한다. 비워두면 HTTP는 **80**, HTTPS는 **443**을 사용하며
  URL에는 포트를 붙이지 않는다. `"30080"`/`"30443"`처럼 명시하면 IAM 외부 URL·issuer·App SSO
  handoff에 해당 포트를 반영한다. **실제 Ingress/Service/LB의 노출 포트를 변경하는 설정은 아니다.**
  `config.publicUrl`을 별도로 지정하면 이 전체 URL이 우선한다.
- IAM `config.publicUrl`, `keycloak.config.realmSslRequired`처럼
  Chart가 기본값·자동 계산으로 처리하는 항목은 샘플에서 생략한다. 외부 URL과 issuer는
  TLS 설정·host·path에서, realm SSL 모드는 HTTP에서 `none`, HTTPS에서 `external`로 결정된다.
  게시 Chart는 HTTP에서 hostname HTTPS 강제를 끄고 realm patch로 기존 HSTS도 정리한다.
- IAM client `redirectUris`는 IAM이 아니라 **App의 외부 주소**를 가리킨다. 비표준 포트이면
  IAM과 App 각각의 실제 외부 포트를 반영한다. IAM은 `config.publicPort`를 채우고,
  App은 해당 외부 포트와 redirect URI를 함께 맞춘다. `shared`에서는 포트도 같다.
- IAM `redis.tls.enabled`는 HTTP 샘플에서 `false`, HTTPS 샘플에서 `true`로 맞춘다.
  HTTPS Redis는 기본적으로 IAM Ingress 인증서 Secret을 재사용하므로 Redis 클라이언트 접속 주소도
  인증서 SAN에 포함되어야 한다. 내부 dispatcher용 평문 리스너는 Chart 기본값을 유지한다.
- IAM 함께 설치 시 `redis.external.enabled`는 모든 샘플에서 `true`로 고정한다. 게시 Chart 기본값 기준으로
  NodePort `32079`가 HTTP 구성에서는 평문 `6379`, HTTPS 구성에서는 TLS `6380` 리스너로 연결된다.
  대상 방화벽에서 Redis 접속을 허용할 클라이언트를 제한한다.
- App SSO URL·Secret은 기존 IAM handoff를 사용한다. App Redis는 `port: 32079`, `db: 0`,
  `password: eva-redis-pass`, 선택 override `url: ""`을 제공한다. URL이 비어 있으면 App이 SSO 변수로
  조합한다. 기본 password는 IAM Redis와 동일하게 맞추고 운영 정책에 맞게 변경한다.
  App Redis TLS도 별도 값을 반복하지 않고
  전달된 SSO URL의 HTTP/HTTPS에서 자동 결정하게 둔다. Redis TLS 자체는 독립 설정이지만 이 샘플은
  요청한 외부 HTTP/HTTPS 구성에 맞춰 사용한다.

HTTP에서는 host 인증서 SAN 검사·TLS Secret Job·Traefik 기본 TLSStore 생성을 실행하지 않는다.
HTTPS라도 Ingress가 꺼졌거나 `tls.create: false` 또는 `tls.authType: secret`이면 hostPath
인증서를 요구하지 않는다. `create: false`의 기존 TLS Secret과 IP 접속용 기본 인증서는 운영자가
미리 관리해야 한다. 기존 TLS 리소스를 삭제하거나 Traefik 외부 포트를 바꾸지는 않는다.

### 외부 IAM 서버 연동

IAM이 다른 서버에서 운영 중이면 `--iam external`을 선택한다. 생성 결과는
`components.iam: false`이고 `targets.<alias>.values.iam`은 포함하지 않는다. 외부 IAM의
realm·client·Redis나 인증서를 배포·변경하지 않으며, 이 서버에는 App 연결값만 설정한다.
다른 서버의 handoff를 자동 검색하거나 복사하지 않는다.

App의 `values.app.app` 아래에 다음 SSO 틀이 포함된다.

```yaml
sso:
  baseUrl: "<EXTERNAL_IAM_BASE_URL>" # 필수: scheme/host/선택 port/path 포함. ex) https://iam.eva.com/iam
  realm: "<SSO_REALM>" # 필수: 외부 IAM realm. ex) eva-iam
  clientId: "<SSO_CLIENT_ID>" # 필수: App OIDC client. ex) eva-app
  adminClientId: "<SSO_ADMIN_CLIENT_ID>" # 필수: 관리 API용 confidential client. ex) eva-admin-api
  adminClientSecret: "<SSO_ADMIN_CLIENT_SECRET>" # 필수: 해당 client의 실제 Secret. ex) demo-admin-client-secret (예시 전용)
  brokerMode: false # 외부 SSO(IdP)를 IAM broker로 연결할 때 true
  brokerIdpHint: "" # 외부 SSO 연결 시 IAM의 Identity Provider alias. ex) company-sso
  redis:
    port: 32079
    db: 0
    password: eva-redis-pass # IAM Redis와 동일하게 맞추고 운영 비밀번호로 변경
    url: "" # 선택 override. 비우면 App이 SSO 변수에서 조합
```

- `baseUrl`은 Keycloak의 공개 base URL이다. `/iam` 같은 노출 path는 포함하지만
  `/realms/<realm>`은 붙이지 않는다. App이 HTTP여도 외부 IAM은 HTTPS일 수 있으며 반대도 독립 설정이다.
  운영 환경에는 신뢰할 수 있는 인증서를 사용하는 HTTPS를 권장한다.
- `realm`, `clientId`, `adminClientId`는 기존 서버의 실제 값으로 입력한다. App Chart 기본값이
  해당 IAM과 같다고 가정하지 않는다. Secret은 IAM Admin Console의 해당 client Credentials에서 확인한다.
- 외부 IAM 관리자는 App의 실제 주소에 맞는 client redirect URI와 web origin을 사전에 허용해야 한다.
  예를 들어 App 주소가 `https://app.eva.com`이면 redirect URI는 `https://app.eva.com/*`,
  web origin은 `https://app.eva.com`이다. 관리 API client의 권한도 해당 설치 정책과 일치해야 한다.
- `brokerMode`와 `brokerIdpHint`는 함께 설치·외부 IAM 모두 제공한다. 기존 IAM 서버를 사용하는 것만으로
  broker mode를 켜는 것은 아니다. **외부 SSO(IdP)를 IAM의 Identity Broker로 연동하는 경우에만
  `brokerMode: true`와 해당 `brokerIdpHint`(IAM Identity Provider alias)를 설정**한다.
- Redis는 IAM이 사용하는 동일 Redis에 연결한다. 기본 `url: ""`이면 **App 런타임**이 SSO `baseUrl`의
  host, HTTPS 여부(TLS 자동 선택), `port/db/password`를 조합한다. Helm에서 완성 URL을 만드는 것은 아니다.
  App Chart 기본 port `32070` 대신 샘플은 IAM 외부 NodePort 기본 `32079`를 명시한다.
- 별도 Redis host, SSO URL과 다른 TLS, 비밀번호 URL 인코딩 등이 필요할 때만 `url`에 전체
  `redis://:password@host:port/db` 또는 `rediss://...`를 넣는다. 명시적 URL이 자동 조합보다 우선한다.
  비밀번호의 URL 특수문자는 인코딩하며, `rediss://` 접속 host는 인증서 SAN과 일치해야 한다.
  `redis.tls` override도 Chart에서 지원하지만 자동 결정과 같을 때는 샘플에 반복하지 않는다.
- 명시적 `baseUrl`과 `adminClientSecret`이 모두 있으면 App role은 같은 target의 이전 handoff보다
  이 설정을 우선한다. 새 샘플은 모든 외부 연결 placeholder를 채워야 입력 검증을 통과한다.
  검증 통과가 외부 서버 접속·Secret 권한·Redis 연결의 성공까지 보장하지는 않는다.
- Online/Remote/Local 모두 지원하는 **샘플 구성**이다. Remote/Local에서는 외부 IAM/Redis까지의
  내부망 도달 경로와 방화벽을 별도로 준비하고 AWS credential을 Target에 추가하지 않는다.

**샘플 입력 검증과 실제 HTTP 설치 준비는 별개이다.** 필수 값을 채운 HTTP 샘플도
`workspace validate`와 내부 Workspace 변환을 통과한다. 이것이 기존 배포 Chart의 HTTP 지원이나
실제 설치 성공을 보장하지는 않는다. HTTP 실설치 전에는 v3.3 Chart/Release 전환과 Deployer의
TLS SAN precondition·IP TLSStore 처리에 대한 프로토콜 연동 및 E2E 검증이 추가로 필요하다.
이번 작업에서는 해당 실행 경로를 변경하거나 인증서 검증을 우회하지 않는다.

아래는 사용 중인 Release와 일치하는 **HTTPS 설정**을 검증·설치하는 예시이다.

```bash
sudo eva workspace validate --config "$HOME/eva-install.yaml"
sudo eva install --config "$HOME/eva-install.yaml" --yes
```

`validate`는 문법·구조·site·repository·target만 검사하고 파일을 생성하지 않는다.
TLS SAN, GPU, network 및 credential의 실제 유효성은 기존 설치 precondition/실행 단계가 검증한다.

## 입력 schema

Online 전체 예시는 `eva config sample --mode online`로 출력한다.

| 항목 | 역할 |
| --- | --- |
| `schema_version` | 현재 `v1`만 지원. Release version과 별개 |
| `site`, `repository`, `components` | 기존 Workspace의 site 설정과 동일 |
| `credentials.aws` | Online용 공통 AWS credential. 생략하면 파일을 만들지 않음 |
| `targets.<alias>.ansible` | target별 Ansible host variables |
| `targets.<alias>.components` | 선택 사항. 해당 target에 설치할 component 부분집합; 생략하면 전역 선택 상속 |
| `targets.<alias>.values.iam` | target의 IAM Chart Values |
| `targets.<alias>.values.app` | target의 App Chart Values. Chart의 `app` key는 그대로 유지 |
| `targets.<alias>.values.agent`, `.vision` | 필요한 경우만 지정하는 Chart override |

AWS 항목은 `aws_access_key_id`, `aws_secret_access_key`, `region`이다. 앞의 두 항목은 함께
지정하며 region 기본값은 `ap-northeast-2`이다. Online AWS CLI 구성 단계가 필요한 설치에서는
이 credential을 제공한다. Remote/Local 설정의 `credentials.aws`는 거부한다.
Main 서버의 credential 준비 절차는 이 Target 입력과 별개이다.

IAM 함께 설치 시 모든 Repository mode의 기본 틀에는
`values.iam.keycloak.realmConfig.substitutions.EVA_IAM_ADMIN_EMAIL: "admin@eva.com"`을 포함한다.
관리자 이메일은 사이트 정책에 맞게 변경한다.

Online 기본 틀은 `imagePullSecrets.hostPath`를 넣지 않고 대상 사용자 기준의 기본 경로를 사용한다.
IAM role은 `eva_iam_node_home`(기본 `/home/{ansible_user}`) 아래 `.aws`를 사용하며,
별도 사용자 설정이 없으면 `ansible_ssh_user`, 최종 `eva` 순으로 선택한다.
AWS CLI 구성 role은 계정 DB의 **실제 홈** 아래 `.aws`에 credential을 기록하므로 root(`/root`)나
사용자 지정 홈은 `values.iam.imagePullSecrets.hostPath`와 `values.app.imagePullSecrets.hostPath`를
필요할 때만 추가하거나 각 role의 Ansible 경로 override를 사용한다. 이 값은 대상 노드의
AWS credential 디렉터리이며(예: `/root/.aws`), controller의 Workspace 경로가 아니다.
기존 설치에서 AWS CLI 구성을 실행하지 않으면
해당 디렉터리의 credential을 별도로 준비해야 한다.
Remote/Local은 ECR pull Secret의 `enabled`/`create`를 끄므로 이 경로를 샘플에 넣지 않는다.

`targets`는 alias별 mapping이다. alias는 영문·숫자로 시작하고 영문·숫자·점·밑줄·하이픈을
사용한다. `all`, `ungrouped`, `eva`는 inventory group과 충돌해 사용할 수 없다.
`ansible_host`는 literal IP 또는 hostname이며 생략하면 alias를 사용한다. 여러 target이
같은 effective host identity를 사용할 수 없다.

### 타겟별 설치 범위

wizard는 전역 `components`에 모든 타겟 선택의 합집합을, 각 `targets.<alias>.components`에
실제 설치 범위를 기록한다. 타겟 map에서 생략한 component는 `false`이며 전역 활성 항목의
부분집합이어야 한다. 전역 활성 항목은 적어도 한 타겟에 선택되어야 한다. 타겟별 map 전체를
생략한 기존 입력은 전역 범위를 상속한다. 비활성 component의 Values는 scoped 타겟에서 거부한다.

예를 들어 Infra-only 타겟과 IAM+App 타겟, App+외부 IAM 타겟을 한 파일에 넣어도 IAM 단계는
IAM을 선택한 타겟에서만 실행된다. Config도 Agent/Vision/App 대상에만 실행한다. Plan에는
단계별 대상 alias만 저장하고 실행 시 Ansible `--limit`로 제한한다. `--component`는 전역과
타겟 선택의 교집합으로 적용하며 retry도 원래 대상 제한을 유지한다.
생성 Workspace의 `target_components`와 `eva_target_enabled_components`는 내부 bridge이므로
직접 작성하지 않는다. 원본의 `targets.<alias>.components`를 수정한다.

멀티 타겟 Values에는 `<TARGET_1_...>`, `<TARGET_2_...>`처럼 타겟별 placeholder를 생성한다.
같은 타겟에서 반복된 placeholder만 같은 값으로 채우며 타겟 간 domain/license/Secret은
각 환경에 맞게 따로 입력한다. SSH 비밀번호는 처음 입력한 문자열 그대로 보관한다.

### 접속 방식과 경로

대상 서버에서 실행할 때는 `ansible_connection: local`을 사용한다. SSH 예시:

```yaml
targets:
  server-a:
    ansible:
      ansible_connection: ssh
      ansible_host: 192.0.2.10
      ansible_user: eva
      ansible_ssh_private_key_file: ./keys/id_ed25519
      ansible_become: true
      ansible_become_password: "<SUDO_PASSWORD>" # 필수: 대상 sudo 비밀번호. ex) demo-sudo-only (예시 전용)
```

SSH password 방식은 key 경로 대신 `ansible_password`를 쓴다. password와 숫자로 보이는 문자열은
quote하고 boolean은 `true`/`false`로 쓴다. 상대 SSH key 경로는 설정 파일의 디렉터리 기준으로
절대 경로로 변환한다. `~` 표현은 지원하지 않는다. 대상 서버의 인증서·데이터 hostPath는 해당
서버의 절대 경로이며 controller 경로로 변환하지 않는다. 인증서·SSH private key·대용량 자산
자체는 inline하지 않고 파일 경로로 참조한다.

### Chart Values

```yaml
targets:
  server-a:
    ansible:
      ansible_connection: local
    values:
      app:
        database:
          name: eva
        app:
          browserTitleName: "EVA"
```

위 DB 이름은 기존 설치의 이름을 의도적으로 유지할 때의 예시이다. 새 설치는 생략하여 Chart
기본값을 따른다. Chart YAML을 `values.<component>` 아래에 그대로 쓰고 기존 파일처럼 host
key로 한 번 더 감싸지 않는다. Deployer가 `ansible_host` 또는 alias 기준의 component별 파일로
변환하며 다른 target에는 빈 block을 기록해 Values가 섞이지 않게 한다.

이 Values의 우선순위는 기존 Workspace override와 같고 CLI `--values`/`--set`이 더 우선한다.
GPU/MIG/vLLM profile과 Release 기준 Values는 복사하지 않는다. Infra/n8n Ansible 변수는
target의 `ansible`에 지정한다. `values.infra`/`values.n8n`은 지원하지 않는다.

## 내부 생성과 재시도

`install`, `plan`, `preflight argocd`, `workspace validate|show|env|ansible-vars`에
`--config`를 지원한다. `--workspace`와 동시에 지정할 수 없다. `--site`를 함께 쓰면
`site.id`와 일치해야 한다. `--config` 없는 기존 `--site`는 계속 `/etc/eva/sites/<id>`를
선택하며 관리 snapshot을 암묵적으로 선택하지 않는다.

```bash
sudo eva preflight argocd --config "$HOME/eva-install.yaml"
sudo eva plan --release /path/to/verified-release --config "$HOME/eva-install.yaml"
sudo eva workspace show --config "$HOME/eva-install.yaml"
```

`validate --config`만 입력 검사 후 Workspace를 생성하지 않는다. 그 외 명령은 설치를 실행하지
않아도 관리 Workspace를 생성할 수 있다. 특히 `plan --config`는 read-only 명령이 아니다.

```text
/var/lib/eva/workspaces/<site-id>/<content-sha256>/
├── credentials/aws_key.ini             # Online credential이 있는 경우만
├── inventory/
│   ├── inventory.ini
│   └── host_vars/<alias>.yaml
└── site-values/
    ├── site.yaml
    ├── iam.yaml                        # 지정된 component만
    └── app.yaml
```

디렉터리는 `0700`, 파일은 `0600`이다. 완성된 임시 디렉터리를 atomic rename으로 게시한다.
동일 내용은 동일 snapshot을 검증·재사용하고 변경된 설정은 새 snapshot을 만든다.
기존 snapshot의 내용·권한이 변경됐거나 symlink가 있으면 덮어쓰지 않고 실패한다.
생성 snapshot은 직접 수정하지 않는다.

Operation Plan에는 생성 Workspace 경로만 저장하고 원본 credential/Chart Values는 넣지 않는다.
입력 수정 후에는 `install --config`로 **새 작업**을 시작한다. `retry`는 원래 작업의 snapshot을
사용하고 원본 YAML을 다시 읽지 않으므로 원본을 이동해도 재시도 입력은 유지된다.
이는 생성 Helm Values, 외부 certificate·key·data까지 과거 상태로 복원하는 기능은 아니다.

## Secret 관리

- 원본 설정은 일반 파일이며 symlink가 아니어야 하고 group/other 접근이 없어야 한다(`0600`).
- 알 수 없는 필드, 중복 key, 다중 YAML document를 거부한다.
- `<UPPER_CASE>`는 필수 입력 placeholder로 예약되어 있으며 실제 credential에 이 형태를 넣지 않는다.
- 원본 설정·내부 Workspace를 Git, 채팅, Release/Bundle에 포함하지 않는다.
- YAML decoder 오류에 원본 Secret 값이 노출되지 않게 한다.
- 기존 `inspect` 동작은 유지하므로 Values/Config 조회 시 Secret은 마스킹하지 않는다.
- snapshot은 retry에 필요해 자동 삭제하지 않는다. 작업 보관·폐기 정책과 함께 관리하고
  참조 중인 Workspace를 임의로 삭제하지 않는다.
