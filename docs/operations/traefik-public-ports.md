# EVA App·IAM 공개 포트와 Traefik

App/IAM Ingress의 공개 포트는 k3s `kube-system/traefik`의 listener 설정과 함께 확인한다.
Online/Remote/Local 공통이며, 아래에서는 설정 조회와 Deployer의 포트 정합화 절차를 설명한다.

## `web`·`websecure` 설정 위치

`web`과 `websecure`는 Traefik의 entrypoint 이름이다. 설정은 다음 순서로 병합된다.

1. **Traefik Chart 기본 Values**: `HelmChart/traefik.spec.chart`가 가리키는 아카이브의
    `traefik/values.yaml` 안에 `ports.web`·`ports.websecure`가 있다. k3s 서버에서는
    `/var/lib/rancher/k3s/server/static/charts/` 아래의 해당 아카이브로 확인한다.
2. **HelmChart 재정의**: `kube-system/traefik`의 `spec.valuesContent`.
3. **HelmChartConfig 재정의**: 같은 namespace/name의 `spec.valuesContent`. 필요한 경우에만 존재한다.

`spec.set`은 위 Values보다 우선하며, `valuesSecrets`도 추가 입력이 될 수 있다. 자동 관리는
관리 대상 설정과 충돌하는 `spec.set`이나 `valuesSecrets`가 있으면 중단한다.

**HelmChartConfig가 없다는 것은 별도 재정의가 없다는 뜻이지, 포트 설정이 없다는 뜻이 아니다.**
HelmChart에도 포트 재정의가 없으면 Chart 기본값을 사용한다. 일반적인 기본값은 다음과 같다.

| entrypoint | `port` (Traefik 내부) | `exposedPort` (Service 공개) |
| --- | --- | --- |
| `web` | `8000` | `80` |
| `websecure` | `8443` | `443` |

실제 설치 Chart에 따라 달라질 수 있다. `expose.default`는 해당 entrypoint를 Service에 공개할지
결정한다. 구형 Chart의 `expose`는 boolean이다. `websecure`의 이름만으로 App/IAM의 프로토콜을
정하지 않으며, TLS와 공개 URL을 함께 확인한다.

대상 서버에서 **현재 Helm에 기록된 계산값의 포트 필드만** 조회한다(`jq` 필요).

```bash
sudo eva exec helm get values traefik -n kube-system --all -o json \
   | jq '.ports | {web: (.web | {port, exposedPort, expose}), websecure: (.websecure | {port, exposedPort, expose})}'

sudo eva exec kubectl -n kube-system get svc traefik \
   -o jsonpath='{range .spec.ports[*]}{.name}{" port="}{.port}{" targetPort="}{.targetPort}{"\n"}{end}'
```

Helm 계산값은 설치 이력이고 Service는 현재 리소스이므로, controller 반영 대기 중에는 다를 수 있다.
HelmChartConfig는 Deployer가 포트를 정합화할 때 없으면 생성한다. 별도 manifest 파일이 없어도
정상이며, k3s가 관리하는 Traefik 기본 manifest나 Service를 직접 수정할 필요가 없다.

## ClusterIP의 32010만으로 외부에서 접근할 수 없는 이유

App Service에 `32010/TCP`가 있어도 타입이 `ClusterIP`이면 노드의 32010을 공개한 것이 아니다.
외부 연결은 Traefik의 LoadBalancer Service와 k3s ServiceLB가 제공한다. Traefik이 여전히
80/443만 공개하면 `http://<IP>:32010/`으로 연결할 수 없다.

`kubectl get ingress`의 `PORTS` 80/443은 Ingress의 HTTP/TLS 표시이지 실제 노드의 공개 포트가
아니다. 변경 후에도 그 표시는 남을 수 있다. Traefik Service의 **`port`**, ServiceLB Pod의
**`hostPort`**, 실제 URL 응답을 함께 확인해야 한다. Service의 `nodePort`도 공개 URL 포트와 다르다.

## 설치 시 처리

1. Chart 기본값·Site Values·CLI `--set`을 적용한 최종 manifest에서 공개 주소를 읽는다.
   App HTTP는 `app.backendPublicPort`, HTTPS는 `app.backendSecurePort`, IAM은
   `config.publicPort`가 렌더링된 runtime URL을 따른다. App의 `service.port`에서 추측하지 않는다.
2. 기존 App/IAM이 있으면 Ingress·endpoint ConfigMap의 정확한 Helm 소유권과 Helm 저장 이력을
   대조한다. 기존 양쪽이 hostless Ingress여도 확인된 공개 포트를 함께 보존한다.
3. 변경 전 HelmChart·HelmChartConfig·해당 Ingress 원본을 대상 서버의
   `/var/lib/eva/backups/traefik-ports/`에 private snapshot으로 저장한다.
4. App/IAM router를 서로 필요한 entrypoint에만 연결한다. `web`/`websecure`를 사용하거나
   추가 `eva-h-<port>`/`eva-s-<port>` entrypoint를 만들어 **양쪽 모두 HTTP인 경우에도 다른 포트로
   분리**한다. `websecure`라는 이름만 보고 HTTP IAM을 연결하지 않는다.
5. HTTP로 사용하는 entrypoint의 리다이렉트를 해제한다. HelmChartConfig에 직접 있으면 제거하고,
   Chart/HelmChart에서 상속되면 재병합되지 않도록 비활성 override를 기록한다. HTTP 설치에서
   사용하지 않는 기본 `web`과 이전 EVA HTTP entrypoint도 정리하되, 다른 서비스가 사용하는
   기본 listener나 HTTPS·외부 관리 listener의 리다이렉트를 임의로 해제하지 않는다.
   **HTTPS 설치에서는 기존 리다이렉트를 보존하며, 설정이 있다는 이유로 설치를 중단하지 않는다.**
   유지하는 리다이렉트의 source·target listener가 이미 공개되어 있으면 그 공개도 유지한다.
6. HelmChartConfig의 필요한 포트만 노출하고, 더 이상 필요 없는 기본 80/443 노출은 끈다.
   내부 container 포트와 외부 `exposedPort`는 같을 필요가 없다. Service를 직접 patch하지 않는다.
7. k3s Helm controller 반영 후 Traefik Service·Deployment·실제 Pod 인자와 readiness, ServiceLB
   DaemonSet/Pod의 새 hostPort와 이전 hostPort 해제를 확인한다. HTTP 관리 listener에 리다이렉트
   인자가 남으면 완료로 처리하지 않는다. 기본 대기 한도는 300초다.

예를 들어 App HTTP 32010, IAM HTTP 32011이면 둘 다 HTTP listener로 공개하고 각 Ingress를
자기 listener에만 연결한다. 80/443을 사용하는 다른 Traefik 경로가 없고 해당 포트가 요청되지
않았다면 **ServiceLB의 80/443 바인딩을 해제**한다. 이후 별도 Docker 서비스가 이 포트를 쓸 수
있지만, 다른 프로세스·방화벽·Docker 바인딩 충돌까지 자동으로 해결하거나 보장하지는 않는다.

## 보호 조건과 운영 주의사항

- 기본 bundled k3s Traefik + LoadBalancer + ServiceLB 구성을 지원한다. 외부 LB/custom Chart,
   직접 hostNetwork/hostPort/nodePort override, HTTP3, 관리 대상
  `spec.set` 또는 `valuesSecrets` 충돌은 추측해서 덮어쓰지 않는다.
   Chart 기본값의 `ports.web.redirections.entryPoint: {}` 또는
   `ports.web.http.redirections.entryPoint: {}`는 **리다이렉트 비활성**이다.
   HTTP 관리 listener의 `redirectTo`, `redirections.entryPoint`, `http.redirections.entryPoint`와
   명시적인 entrypoint redirect 인자는 해제한다. HTTPS 설치의 기존 리다이렉트 설정·인자와
   다른 HTTP 옵션·TLS 설정은 보존한다. 혼합 설치에서는 HTTP 관리 listener의 해제를 우선한다.
   알 수 없는 redirect schema나 다른 entrypoint 인자 재정의는 자동 변경 전에 중단한다.
- 다른 서비스의 NodePort/LoadBalancer 포트, workload hostPort 또는 hostless 경로가 충돌하면
  중단한다. 무관한 Ingress/IngressRoute가 공유 listener를 사용하면 임의로 80/443을 옮기지 않는다.
- Helm 이력 없는 Argo CD 전용 설치, 소유권 불일치, 작업 중 ConfigMap/Ingress/HCC 변경도 중단한다.
  기존 설정의 다른 필드는 보존하며 UID/resourceVersion 검사를 사용한다.
- 전환 중 짧은 접속 중단이 있을 수 있다. 포트 관리와 router 변경이 일부 반영된 후 실패할 수도
  있다. 동시 운영자 변경을 덮어쓰지 않도록 **자동 rollback/무조건 재시도를 하지 않는다.** 오류에
  표시된 private snapshot과 현재 리소스를 비교한 뒤 재실행 여부를 결정한다.
- 같은 설치의 App/IAM을 순차 실행하거나 한쪽만 재설치해도 다른 쪽의 확인된 포트를 유지한다.
  이후 Helm upgrade에도 post-renderer로 최종 router annotation을 보존한다.
  현재 endpoint는 `eva.spectrabrain.com/traefik-public-endpoints`, 추가 listener의 소유권은
  `eva.spectrabrain.com/traefik-owned-entrypoints` annotation에 별도로 기록한다. 포트 변경이나
  HTTP/HTTPS 전환으로 비활성화된 추가 listener도 소유권을 유지하여 이후 재사용할 수 있다.
  소유권 annotation이 없는 기존 설치도 EVA 전용 listener 이름과 실제 포트 설정에서
  관리 대상을 복구한다. annotation 누락만으로 EVA 설치를 중단하지 않는다.
- 기존 HCC를 파일/GitOps로 별도 관리한다면 자동화끼리 덮어쓰지 않도록 관리 주체를 먼저 정한다.
  Secret이 포함될 수 있는 snapshot/HCC 전체를 일반 로그나 지원 티켓에 올리지 않는다.

설치 YAML의 해당 Target `ansible` 설정으로 조절한다. Chart Values 안에 넣는 변수가 아니다.

| 변수 | 기본값 | 의미 |
| --- | --- | --- |
| `eva_traefik_ports_manage` | `true` | 공개 listener/router 자동 관리. `false`면 운영자가 양쪽을 직접 책임짐 |
| `eva_traefik_ports_timeout` | `300` | controller·ServiceLB 반영 대기 초 |
| `eva_traefik_ports_backup_root` | `/var/lib/eva/backups/traefik-ports` | 대상 서버의 private snapshot 디렉터리 |

## 기존 EVA listener 재사용

Deployer는 설치 설정의 공개 URL에 맞춰 기존 EVA listener를 재사용한다.
Online/Offline 설치에 동일하게 적용된다.

`eva-h-<port>`와 `eva-s-<port>`는 Deployer의 EVA 전용 listener 이름이다. 해당 이름의 번호와
실제 container/public 포트가 같고 TCP 설정이 일치하면, 활성·비활성 여부와 관계없이 기존
관리 대상으로 복구한다. Chart/HelmChart/HCC의 병합된 설정을 확인하며, 소유권 annotation이
두 개 모두 없어도 처리한다. 기존 EVA Ingress도 설치된 공개 URL과 정확한 Helm 이력을
대조하여 해당 공개 포트와 프로토콜에 맞는 EVA entrypoint annotation을 이어받는다.

예를 들어 App/IAM이 HTTPS 443으로 기록된 상태에서 비활성 `eva-h-32010`이 남아 있어도,
IAM HTTP 32010 설치 시 해당 listener를 재사용하고 App의 HTTPS 443을 보존한다. 수동 HCC
편집이나 listener 삭제는 필요하지 않다. 설치 시 원본을 private snapshot으로 백업하고,
복구한 추가 listener의 소유권을 다시 기록한다. 사용하지 않는 EVA listener는 비활성화하며
HTTP 관리 listener의 리다이렉트도 기존 정책에 따라 정리한다.

무관한 Ingress/IngressRoute가 있는 공유 환경에서는 기존 외부 라우터 보호 조건을 적용한다.
이름의 번호와 다른 실제 포트, UDP/다른 routing target, 실제 포트 충돌 등 설정 불일치는
소유권 누락과 별도로 확인한다.

설치 설정을 변경했다면 원본 설정 파일로 새 설치 작업을 실행한다.

```bash
sudo eva install --config <설정 파일> --yes
```

## HTTP·HTTPS 리다이렉트 확인

HTTP로 설치한 App/IAM의 listener에는 HTTPS 강제 전환 리다이렉트가 없어야 한다.
Deployer는 위 관리 범위의 리다이렉트를 백업 후 해제한다. HTTPS 설치에서는 기존 리다이렉트를
그대로 보존한다. 리다이렉트에 사용 중인 80/443은 해제 대상이 아닐 수 있다.
공개 포트를 변경했다면 기존 redirect의 목적지 URL도 올바른지 확인한다. 명시적인 redirect
포트·목적지 정책을 다른 App/IAM으로 자동 재지정하지는 않는다.

현재 실행 중인 Traefik의 **리다이렉트 인자만** 확인하려면 대상 서버에서 다음을 실행한다.
전체 HCC/Values에는 민감정보가 포함될 수 있으므로 원문을 공유하지 않는다.

```bash
sudo eva exec kubectl -n kube-system get deployment traefik \
   -o jsonpath='{range .spec.template.spec.containers[?(@.name=="traefik")].args[*]}{.}{"\n"}{end}' \
   | grep -i 'entrypoints\..*\.http\.redirections\.'
```

조회 자체가 성공했고 일치하는 인자가 없다면 실행 중인 Deployment에는 해당 리다이렉트 인자가
없는 상태다(`grep` 종료 코드 1). 다만 HCC/HelmChart의 아직 반영되지 않은 설정까지 없다는 뜻은
아니다. HTTP 관리 listener에 인자가 계속 남으면 Helm controller 상태와 해당 entrypoint의
설정 출처를 확인한다. HTTP 설치의 관리 범위 밖 redirect나 알 수 없는 schema로 중단되면 운영 정책과
설정을 먼저 검토한다. `eva_traefik_ports_manage=false`는 단순 오류 우회가 아니며, 공개 listener·
80/443 hostPort 해제·App/IAM router 분리를 운영자가 모두 관리할 때만 사용한다.

설정 검증 단계에서 중단되면 해당 실행의 Ingress/HCC 변경은 수행하지 않는다. 반영 대기 중
실패했다면 일부 변경이 적용되었을 수 있으므로 private snapshot과 현재 상태를 비교한다.

## 설치 후 확인

대상 서버에서 다음은 **조회만** 수행한다. HCC에는 민감정보가 포함될 수 있어 전체 원문을 출력하지 않는다.

```bash
sudo eva exec kubectl -n kube-system get svc traefik
sudo eva exec kubectl -n kube-system get helmchart traefik
sudo eva exec kubectl -n kube-system get helmchartconfig traefik
sudo eva exec kubectl -n kube-system get pods -o wide
sudo eva exec kubectl -n eva-app get ingress eva-app -o jsonpath='{.metadata.annotations.traefik\.ingress\.kubernetes\.io/router\.entrypoints}'
curl --connect-timeout 5 -I http://<IP>:32010/
```

주소·프로토콜·포트는 실제 설치 입력으로 바꾼다. IAM도 자신의 URL에서 따로 확인한다.
Ingress/Service가 정상이어도 App readiness·IAM 기동·노드 방화벽·접근 경로 문제는 별도로 남을 수 있다.
`ss`에 80/443 listener가 없다는 사실만으로 ServiceLB의 iptables/hostPort 해제를 판정하지 않는다.

설치에는 대상 서버에 배포된 Deployer Release를 사용한다. 설정을 수정했다면 이전 snapshot을
쓰는 `eva retry` 대신 원본을 다시 읽는 새 `eva install --config ...` 작업을 시작한다.
