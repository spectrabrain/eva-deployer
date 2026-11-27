# EVA-Dock 설치와 운영

EVA-Dock은 여러 EVA 서버의 탐지 알림·이력·Live 영상을 한 화면에서 보는 관제 화면이다.
Deployer는 Solution 구성요소 `dock`으로 [eva-dock chart](https://github.com/spectrabrain/eva-dock-chart)를
설치한다. Online/Remote/Local Repository와 k3s·NKS(기존 Kubernetes)를 모두 지원한다.

| 항목 | 값 |
| --- | --- |
| component | `dock` (`components.dock`, `targets.<alias>.values.dock`) |
| Helm release / namespace | `eva-dock` / `eva-dock` |
| Chart·이미지 버전 | `src/solution/version.yaml`의 `eva_dock_chart_version`, `eva_dock_image_tag` |
| 설치 순서 | App 다음, n8n 이전 (`src/solution/playbooks/site_eva_dock.yaml`) |
| 저장 데이터 | SQLite(`/data/eva_live.db`)와 상태 파일. k3s 기본 hostPath `/eva-dock`, NKS는 PVC |

Dock은 EVA App의 기존 API와 event-recipient 웹훅을 사용하므로 App 설정을 바꾸지 않는다.
설치 후 Dock 화면의 `#/servers`에서 연동할 EVA를 등록하고, 각 EVA App의 event-recipient
대상에 Dock의 공개 주소(`config.dockPublicUrl`)를 지정한다.

## 설치 설정 만들기

`eva install configure`에서 Solution을 선택하면 Agent/Vision 다음에 Dock 설치 여부를 묻는다.
기본값은 **설치**이며, 설치하면 Dock 접속 프로토콜(HTTP/HTTPS)을 따로 고른다. 기본값은 App/IAM에
선택한 프로토콜이다. NKS는 Cloud LB 주소를 직접 입력하므로 프로토콜을 묻지 않는다.

생성된 `values.dock.config`에는 Dock 화면 메뉴 기본값이 chart와 같게 표시된다. configure는 이 값을 묻지
않으므로 필요하면 `eva-install.yaml`에서 직접 바꾼다. 설치 후 변경도 같은 값을 고친 뒤
`eva install --component dock`으로 적용한다.

| 키 | 기본값 | 화면 |
| --- | --- | --- |
| `dashboardEnabled` | `true` | DASHBOARD |
| `liveEnabled` | `false` | LIVE(탐지 알림 이력) |
| `streamingEnabled` | `false` | STREAMING(카메라 실시간 MJPEG 영상) |

STREAMING은 Dock이 EVA 서버의 카메라 영상을 중계하므로 EVA 서버당 동시 영상 수를
`config.livePerEva`(기본 16)로 제한한다.

Dock은 `/`와 `/api` 경로를 사용하므로 App(`/`)과 경로로 나눌 수 없다. 주소 구성에 따라 다음과 같이
분리한다.

| 주소 구성 | Dock 공개 주소 | 생성되는 입력 |
| --- | --- | --- |
| IP 직접 접속 | App과 같은 IP의 전용 포트 `32020` | `ingress.host`(IP), `config.dockPublicUrl` |
| 동일/별도 도메인 | Dock 전용 도메인의 80/443 | `<DOCK_HOST>`, HTTPS면 `<DOCK_TLS_CERT_DIRECTORY>` |
| NKS | Cloud LB 주소 | `<DOCK_PUBLIC_URL>`, `<DOCK_STORAGE_CLASS>` |

생성 예시(IP + HTTPS):

```yaml
components:
  dock: true
targets:
  server-a:
    values:
      dock:
        ingress:
          host: "192.0.2.10"
          tls:
            enabled: true
            create: true
            hostPath: "/etc/eva/certs" # tls.crt + tls.key, IP SAN 포함. App HTTPS와 같은 디렉터리 재사용 가능
        config:
          dockPublicUrl: "https://192.0.2.10:32020"
          dashboardEnabled: true
          liveEnabled: false      # LIVE 메뉴. 사용하려면 true
          streamingEnabled: false # STREAMING 메뉴. 사용하려면 true
        secrets:
          dockAccessPassword: "<DOCK_ACCESS_PASSWORD>" # 필수
```

- `ingress.host`에는 scheme·port·path 없이 도메인 또는 IP만 넣는다. IP는 hostless Ingress로 렌더링된다.
- 포트는 `config.dockPublicUrl`에서 정한다. 비워 두면 Ingress host와 TLS 설정으로 만들며, IP일 때는
  `:32020`을 붙인다. scheme은 `ingress.tls.enabled`와 같아야 하고 path는 넣지 않는다.
- 브라우저와 EVA 웹훅은 `config.dockPublicUrl`로 접속하고 Traefik은 `ingress.host`로 라우팅한다.
  Kubernetes Ingress에는 포트가 없으므로 공개 포트는 `dockPublicUrl`에만 쓰며, Deployer가 이 포트로
  Traefik listener를 만든다. 두 값의 host가 다르면 설치 전에 중단한다(대소문자 무시). NAT·외부 LB 뒤처럼
  의도적으로 다르면 target `ansible`에 `eva_dock_allow_public_host_mismatch: true`를 지정한다.
- Dock은 하위 경로(예: `/dock`)를 지원하지 않는다. 앱이 `/api`·`/assets`를 루트 기준 절대 경로로 호출하므로
  IP 접속은 전용 포트, 도메인 접속은 전용 도메인으로 분리한다.
- `secrets.dockAccessPassword`(또는 미리 만든 `secrets.existingSecret`)가 없으면 설치 전에 중단한다.
  Helm upgrade는 이전 Values를 재사용하지 않으므로 **업데이트할 때도 기존 비밀번호를 그대로 입력**한다.
- 그 밖의 키(`config.livePerEva`, `config.dashboardEnabled`, `persistence.hostPath.path`, `resources` 등)는
  chart Values 그대로 `values.dock`에 쓴다. chart에 없는 최상위 키는 설치 전에 거부한다.

## Deployer가 적용하는 기본값

`src/solution/values/dock-k3s-override.yaml.j2`가 chart 기본값 위에 적용되고, 설치 설정의
`values.dock`이 그 위에 적용된다(`values.yaml → dock-k3s-override.yaml → host override` 순서).

| 항목 | Online | Remote/Local |
| --- | --- | --- |
| 이미지 | ECR `spectrabrain/eva-dock:<eva_dock_image_tag>` | Harbor `<registry>/<project>/eva-dock` |
| 이미지 인증 | chart의 ECR pull Secret + 갱신 CronJob (`/home/<user>/.aws`) | 사용 안 함 |
| TLS Secret Job kubectl | `bitnami/kubectl` | Harbor `<registry>/<project>/kubectl` |
| Chart | Helm repo `eva-dock/eva-dock --version` | `out/cache/eva-dock/eva-dock-<version>.tgz` |

공통으로 Traefik Ingress(`className: traefik`), TLS Secret 이름 `eva-dock-tls`, 인증서 파일
`tls.crt`/`tls.key`, hostPath 저장소 `/eva-dock`를 사용한다. Remote/Local 준비 단계는 Dock chart와
이미지를 다른 Solution 자산과 함께 받고 Harbor에 게시한다.

## 공개 포트와 Traefik

k3s의 IP 접속에서는 Dock도 App/IAM과 같은 [Traefik 공개 포트 정합화](traefik-public-ports.md) 대상이다.
최종 manifest의 `DOCK_PUBLIC_URL`에서 scheme·IP·포트를 읽어 `web`/`websecure` 또는 전용
`eva-h-<port>`/`eva-s-<port>` listener에 연결하고, Helm post-renderer로 Dock router를 그 listener에만
고정한다. App/IAM을 다시 설치해도 확인된 Dock listener는 유지된다.

- App과 같은 공개 포트에 Dock `/`를 두면 경로가 충돌하므로 설치 전에 중단한다.
- 도메인 주소는 host 기반 라우팅이므로 listener를 변경하지 않는다.
- IP + HTTPS에서 TLS Secret Job을 hostPath로 사용하고 인증서 파일명이 `tls.crt`/`tls.key`이면 Traefik
  기본 TLS 인증서도 구성한다. 브라우저는 IP 접속에 SNI를 보내지 않으므로 기본 인증서가 Dock IP SAN을
  포함해야 한다. 다른 파일명(예: `tls_dock.crt`)을 쓰면 기본 인증서는 변경하지 않고 안내만 출력한다.
- `eva_traefik_ports_manage: false`로 끄면 운영자가 listener와 router 분리를 직접 관리한다.

## NKS(기존 Kubernetes)

NKS 설정 틀은 chart Ingress를 끄고(`ingress.enabled: false`) Service를 ClusterIP로 두며, Cloud LB나
기존 Ingress가 Service 80번 포트로 연결하는 구성을 기본으로 한다. 이미지 인증은 Deployer가 관리하는
`aws-credentials` Secret(`imagePullSecret.authType: secret`)을 사용하고, 데이터는 PVC에 저장한다.
설치 전 ECR 자격 증명·이미지 접근을 검증하며 기존 PVC 변경은 cluster guard가 차단한다.
SSE·MJPEG 스트림이 끊기지 않도록 LB의 idle timeout을 충분히 길게 설정한다.

## 확인

`eva install`이 성공하면 App·IAM·Harbor와 함께 **Dock** 접속 주소(`dockPublicUrl`)를 Access URLs에
표시한다. `eva check`는 설치된 Dock ConfigMap의 `DOCK_PUBLIC_URL`을 읽어 같은 주소를 표시한다.

```bash
sudo eva check                  # eva-dock workload 상태와 Access URLs의 Dock 주소(dockPublicUrl)
sudo eva inspect values --component dock
```

`eva install --component dock`으로 Dock만 다시 설치할 수 있고, `--values dock=FILE`,
`--set dock:KEY=VALUE`, `--chart dock=FILE`로 일회성 override를 지정할 수 있다.

## 기존 수동 설치에서 전환

release `eva-dock`(namespace `eva-dock`)로 직접 설치한 Dock은 같은 release를 그대로 업데이트한다.
chart는 리소스 이름을 `<release>-eva-dock`(예: `eva-dock-eva-dock`)으로 만들며 Deployer도 이를 유지한다.
기존 데이터 경로·인증서 파일명·비밀번호·공개 URL이 기본값과 다르면 `values.dock`에 명시한다.

```yaml
dock:
  persistence:
    type: hostPath
    hostPath:
      path: /data001/share/eva-dock   # 기존 데이터 경로
  ingress:
    host: "222.108.131.192"
    tls:
      enabled: true
      create: true
      hostPath: /root/certs
      certFile: tls_dock.crt          # 기존 인증서 파일명
      keyFile: tls_dock.key
  config:
    dockPublicUrl: "https://222.108.131.192"   # 기존 443 유지
  secrets:
    dockAccessPassword: "<EXISTING_DOCK_ACCESS_PASSWORD>"
```

수동 설치에서 쓰던 `imagePullSecrets`(직접 만든 ECR Secret)는 Online에서 chart의 관리형 ECR Secret으로
대체되므로 남겨 두지 않아도 된다. 업데이트 전 기존 Helm 사용자 Values는 자동으로 백업된다
([Helm 사용자 Values 백업](helm-user-values-backup.md)).
