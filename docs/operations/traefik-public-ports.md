# IP 기반 EVA App·IAM 공개 포트와 Traefik

수정된 Deployer Release는 **IP 기반 App/IAM Ingress 설치 시** 실제 공개 URL에 맞춰 k3s
`kube-system/traefik` HelmChartConfig를 자동 정합화한다. Cloud/Remote/Local 공통이다.
기존 Release나 Helm 직접 설치에는 적용되지 않는다. DNS 기반 또는 Ingress 비활성 설치는
이 자동 변경을 수행하지 않는다.

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
   대조한다. 기존 양쪽이 hostless IP Ingress여도 확인된 공개 포트를 함께 보존한다.
3. 변경 전 HelmChart·HelmChartConfig·해당 Ingress 원본을 대상 서버의
   `/var/lib/eva/backups/traefik-ports/`에 private snapshot으로 저장한다.
4. App/IAM router를 서로 필요한 entrypoint에만 연결한다. `web`/`websecure`를 사용하거나
   추가 `eva-h-<port>`/`eva-s-<port>` entrypoint를 만들어 **양쪽 모두 HTTP인 경우에도 다른 포트로
   분리**한다. `websecure`라는 이름만 보고 HTTP IAM을 연결하지 않는다.
5. HelmChartConfig의 필요한 포트만 노출하고, 더 이상 필요 없는 기본 80/443 노출은 끈다.
   내부 container 포트와 외부 `exposedPort`는 같을 필요가 없다. Service를 직접 patch하지 않는다.
6. k3s Helm controller 반영 후 Traefik Service·Deployment·실제 Pod 인자와 readiness, ServiceLB
   DaemonSet/Pod의 새 hostPort와 이전 hostPort 해제를 확인한다. 기본 대기 한도는 300초다.

예를 들어 App HTTP 32010, IAM HTTP 32011이면 둘 다 HTTP listener로 공개하고 각 Ingress를
자기 listener에만 연결한다. 80/443을 사용하는 다른 Traefik 경로가 없고 해당 포트가 요청되지
않았다면 **ServiceLB의 80/443 바인딩을 해제**한다. 이후 별도 Docker 서비스가 이 포트를 쓸 수
있지만, 다른 프로세스·방화벽·Docker 바인딩 충돌까지 자동으로 해결하거나 보장하지는 않는다.

## 보호 조건과 운영 주의사항

- 기본 bundled k3s Traefik + LoadBalancer + ServiceLB 구성을 지원한다. 외부 LB/custom Chart,
  직접 hostNetwork/hostPort/nodePort override, HTTP3, 별도 listener redirect, 관리 대상
  `spec.set` 또는 `valuesSecrets` 충돌은 추측해서 덮어쓰지 않는다.
- 다른 서비스의 NodePort/LoadBalancer 포트, workload hostPort 또는 IP hostless 경로가 충돌하면
  중단한다. 무관한 Ingress/IngressRoute가 공유 listener를 사용하면 임의로 80/443을 옮기지 않는다.
- Helm 이력 없는 Argo CD 전용 설치, 소유권 불일치, 작업 중 ConfigMap/Ingress/HCC 변경도 중단한다.
  기존 설정의 다른 필드는 보존하며 UID/resourceVersion 검사를 사용한다.
- 전환 중 짧은 접속 중단이 있을 수 있다. 포트 관리와 router 변경이 일부 반영된 후 실패할 수도
  있다. 동시 운영자 변경을 덮어쓰지 않도록 **자동 rollback/무조건 재시도를 하지 않는다.** 오류에
  표시된 private snapshot과 현재 리소스를 비교한 뒤 재실행 여부를 결정한다.
- 같은 설치의 App/IAM을 순차 실행하거나 한쪽만 재설치해도 다른 쪽의 확인된 포트를 유지한다.
  이후 Helm upgrade에도 post-renderer로 최종 router annotation을 보존한다.
- 기존 HCC를 파일/GitOps로 별도 관리한다면 자동화끼리 덮어쓰지 않도록 관리 주체를 먼저 정한다.
  Secret이 포함될 수 있는 snapshot/HCC 전체를 일반 로그나 지원 티켓에 올리지 않는다.

설치 YAML의 해당 Target `ansible` 설정으로 조절한다. Chart Values 안에 넣는 변수가 아니다.

| 변수 | 기본값 | 의미 |
| --- | --- | --- |
| `eva_traefik_ports_manage` | `true` | IP 공개 listener/router 자동 관리. `false`면 운영자가 양쪽을 직접 책임짐 |
| `eva_traefik_ports_timeout` | `300` | controller·ServiceLB 반영 대기 초 |
| `eva_traefik_ports_backup_root` | `/var/lib/eva/backups/traefik-ports` | 대상 서버의 private snapshot 디렉터리 |

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

이 기능을 포함한 **새 Release로 설치**해야 적용된다. 저장소 수정만으로 현장의 Current Release나
기존 Tool binary가 바뀌지 않는다. 설정도 수정했다면 이전 snapshot을 쓰는 `eva retry` 대신
원본을 다시 읽는 새 `eva install --config ...` 작업을 시작한다.