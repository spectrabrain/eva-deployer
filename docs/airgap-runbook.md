# EVA Airgap Runbook

인터넷이 닫힌 k3s 노드에 EVA 스택(iam · app · agent · vision)을 올리는 절차입니다.
**iam · app 까지는 실제 서버에서 검증**했고, agent · vision 단계는 role 의 선행 조건을 정리한 것입니다.

호스트가 두 개라는 점이 이 작업의 핵심입니다.

| 표기 | 의미 |
|---|---|
| **[준비]** | 인터넷 · docker · helm · ECR 자격증명이 되는 서버. bundle 을 만듭니다 |
| **[대상]** | airgap 서버. Harbor 와 k3s 가 여기 있습니다 |
| **⚑** | 검증 지점 — 통과하지 못하면 다음으로 가면 안 됩니다 |

검증 환경 (2026-08-26)

| | |
|---|---|
| 대상 노드 | evashee / 10.158.200.113 |
| 서비스 host | magok.eva.lge.com |
| Registry | localhost:32080 / project `eva` |
| eva-app | chart 3.1.4 · app 3.1.2 · 경로 `/` |
| eva-iam | chart 3.1.0 · 경로 `/iam` |
| Ingress | traefik |

---

## 시작점에 따라 순서가 달라집니다

| 시작 상태 | 순서 |
|---|---|
| **A. k3s · Harbor 가 이미 있음** | 아래 번호 그대로 (07 → 08 → 09 → 10 → 11 …) |
| **B. bare Ubuntu 24.04 (k3s · Harbor · docker 없음)** | **10번을 09번보다 먼저** — `site_infra.yaml` 이 ansible 로 실행되기 때문입니다 |

B 의 실제 순서

```
07 load + 무결성 확인
08 docker 설치 → Harbor 설치 → 이미지 push
10 ansible 설치                 ← 먼저
09 site_infra.yaml              ← k3s + registries.yaml 을 여기서 함께 설치
11 인증서 …
```

B 에서 추가로 필요한 것

1. **wheelhouse 에 full `ansible`** — `site_infra.yaml` 의 nfs role 이 `ansible.posix.mount` 를 씁니다.
   `ANSIBLE_AIRGAP_REQUIREMENTS="ansible-core==..."` 단축로는 쓸 수 없습니다
2. **NVIDIA 드라이버 offline 리포** — gpu role 이 `lspci` 로 GPU 를 감지하면 드라이버부터 설치합니다 (03번 참고).
   GPU 가 없거나 건너뛰려면 `-e gpu_skip_driver_update=true`
3. **Ubuntu 릴리스 일치** — `base` role 이 `install/apt/debs` 의 `.deb` 를 dpkg 로 설치합니다.
   준비 서버와 대상 서버의 릴리스·패치 레벨이 다르면 `libc6` 같은 버전 의존성에서 깨집니다

```bash
# 양쪽에서 비교
lsb_release -a

# bundle 이 이 서버와 맞는지 진단
./install/validate_offline_debs.sh ./install/apt/debs
sudo ./install/repair_offline_debs.sh ./install/apt/debs
```

B 에서는 09번의 수동 `registries.yaml` 작성이 필요 없습니다 — `site_infra.yaml` 이 써줍니다.

---

## Phase 0 — 준비 서버 점검

이걸 건너뛰면 옮긴 tar 가 깨져 있습니다.

### 01. docker 이미지 스토어 확인 · **[준비]** ⚑

containerd 이미지 스토어를 쓰는 docker 는 `docker save` 에서 레이어를 빼먹은 tar 를 만듭니다.
대상 서버에서 `docker load` 는 성공한 듯 보이지만 push 가 `failed to read config content` 로 실패합니다.

```bash
docker info --format '{{.DriverStatus}}'
```

**판단 기준은 하나입니다 — 이 머신에서 `docker save` 로 tar 를 만들어 옮기는가?**

- 그렇다 → 아래처럼 끕니다
- 아니다 (cloud 설치 대상, 이미지 빌드 전용 머신 등) → 기본값 그대로 두세요

```bash
sudo cat /etc/docker/daemon.json 2>/dev/null   # 기존 insecure-registries 등 보존

sudo tee /etc/docker/daemon.json >/dev/null <<'EOF'
{ "features": { "containerd-snapshotter": false } }
EOF
sudo systemctl restart docker
```

주의할 점

- 스토어가 갈리므로 전환 후 기존 이미지가 `docker images` 에 안 보입니다. 다시 pull 해야 하고, 되돌리면 다시 보입니다
- `docker buildx` 로 멀티아키 빌드를 하는 머신이면 끄면 안 됩니다
- 그 머신에서 docker 로 도는 서비스(Harbor 등)가 재시작됩니다
- daemon 설정을 건드리지 않는 대안: `skopeo copy --override-arch amd64 docker://<image> docker-archive:/tmp/x.tar:<image>` 또는 `crane pull --platform linux/amd64`

### 02. versions.json 짝 맞추기 · **[준비]**

다운로드 스크립트가 이 파일을 읽어 **무엇을 받을지** 정합니다. 나중에 고치면 bundle 에는 옛 버전이 들어갑니다.

```bash
cd <repo 루트>
grep -E "eva_app_(chart|deploy)_version|eva_iam_chart_version" versions.json
#   eva_app_chart_version  : 3.1.4
#   eva_app_deploy_version : 3.1.2   ← 차트 3.1.4 의 appVersion
#   eva_iam_chart_version  : 3.1.0
```

> eva-app 은 role 이 이미지 태그를 `eva_app_deploy_version` 으로 **강제**합니다.
> 차트만 올리고 이 값을 두면 Harbor 에 없는 태그를 찾습니다. eva-iam 은 태그를 건드리지 않아 이 문제가 없습니다.

---

## Phase 1 — bundle 만들기

### 03. 차트 · 패키지 · wheel · **[준비]**

```bash
sudo -v && ./install/download_offline_assets.sh
./install/download_infra_images.sh
./install/setup_harbor.sh --download-only

# wheelhouse — 반드시 별도 실행
./install/download_python_venv_debs.sh
TARGET_PYTHON=3.12 ./install/download_ansible_wheels.sh
ls install/wheels/*.whl | wc -l
```

대상 서버가 **bare Ubuntu 이고 GPU 를 쓴다면** 드라이버 offline 리포도 만들어야 합니다.
`download_offline_assets.sh` 가 받는 container-toolkit 과는 별개입니다.

```bash
./install/build_nvidia_driver_repo.sh nvidia-driver-580
ls install/nvidia/
#   nvidia-driver-repo/      ← 이 스크립트가 만듦
#   container-toolkit-debs/  ← download_offline_assets.sh 가 받음
```

> 마지막 두 줄을 `&&` 로 묶지 마세요. 앞 명령이 sudo 로 실패하면 wheel 다운로드가 조용히 건너뛰어지고,
> 그 사실은 대상 서버에서 pip 에러로만 드러납니다. `TARGET_PYTHON` 은 **대상 서버**의 Python 버전입니다.

### 04. 모델 캐시 · **[준비]**

eva-agent 와 vllm 이 쓰는 HuggingFace 모델입니다. bundle 에서 가장 큰 덩어리이고,
eva_agent role 이 대상 서버에서 두 경로의 존재를 assert 합니다.

```bash
AWS_PROFILE=default ./install/download_eva_models.sh
du -sh install/models/*
#   install/models/agent/hf
#   install/models/vllm/hf
```

iam · app 만 설치할 계획이면 생략합니다.

### 05. 이미지 · **[준비]** ⚑

```bash
rm -rf install/images

# 전체 스택
AWS_PROFILE=default ./install/download_eva_images.sh

# 일부만 — iam · app 만 볼 때
# COMPONENTS="eva-app eva-iam" AWS_PROFILE=default ./install/download_eva_images.sh
```

**GPU 프로파일 — vllm 이미지가 여기서 갈립니다.**
`download_offline_assets.sh` 는 vllm values 4종(`A6000x1`, `L40sx1`, `PRO5000x3`, `PRO6000-MIGx4`)을 모두 받지만,
이미지 목록은 그중 하나로 렌더해서 만듭니다. 기본값은 `PRO6000-MIGx4` 입니다.

```bash
ssh <대상 서버> 'nvidia-smi --query-gpu=name --format=csv,noheader'

EVA_AGENT_VLLM_VALUES_FILE=values-k3s.L40sx1.yaml \
  AWS_PROFILE=default ./install/download_eva_images.sh
```

끝에 무결성 검사가 돕니다. **모든 줄이 `amd64/linux` 여야 합니다.**

```
[verify] 이미지 무결성 확인
  ...eva-app:3.1.2         amd64/linux
  mysql:8.0.42-bookworm    amd64/linux
[verify] 이상 없음
```

`/` 처럼 앞이 빈 줄이 있으면 01번으로 돌아가세요. 그래도 남으면 그 이미지만 digest 로 받습니다.

```bash
REPO=<레지스트리>/<경로>/<이미지>
docker manifest inspect "$REPO:<태그>" | python3 -c '
import json,sys
d=json.load(sys.stdin)
for m in d.get("manifests",[]):
    p=m.get("platform",{}); print(p.get("os"),p.get("architecture"),m["digest"])'

docker rmi -f "$REPO:<태그>"          # 먼저 지워야 "Image is up to date" 로 건너뛰지 않습니다
docker pull "$REPO@sha256:<linux amd64 digest>"
docker tag  "$REPO@sha256:<...>" "$REPO:<태그>"
docker image inspect "$REPO:<태그>" --format '{{.Architecture}}/{{.Os}}'
```

### 06. tar 로 묶어 전송 · **[준비]**

```bash
docker save -o install/images/repository-images.tar \
  $(cat install/images/images-pulled.txt)
ls -lh install/images/repository-images.tar     # 수 GB — 10K 면 01번 문제

rsync -a --info=progress2 --partial \
  --exclude '.venv' --exclude '.git' --exclude 'install/images/rendered' \
  ./ eva@10.158.200.113:/home/eva/eva-deployer-jj/

du -sh install/*
ssh eva@10.158.200.113 'du -sh ~/eva-deployer-jj/install/*'
```

---

## Phase 2 — 대상 서버 준비

### 07. load 후 무결성 재확인 · **[대상]** ⚑

```bash
cd ~/eva-deployer-jj
docker load -i install/images/repository-images.tar

while read -r i; do
  printf '%-72s %s\n' "$i" \
    "$(docker image inspect "$i" --format '{{.Architecture}}/{{.Os}}' 2>/dev/null)"
done < install/images/images-pulled.txt
```

전부 `amd64/linux` 여야 합니다. 한 줄이라도 `/` 면 그 이미지는 push · 배포가 실패합니다.
이 서버에 예전 기록이 남아 있으면 새 tar 를 load 해도 건너뛰므로 `docker rmi -f <이미지>` 를 먼저 하세요.

### 08. Harbor 로 push · **[대상]**

docker · Harbor 가 없을 때만 앞 두 줄을 실행합니다.

```bash
sudo ./install/install_docker.sh --airgap        # 후 SSH 재접속
./install/setup_harbor.sh --hostname localhost \
  --install-root ~/.local/share/eva-harbor

PULL_SOURCE_IMAGES=false IMAGE_LIST=install/images/images-pulled.txt \
  REPOSITORY_REGISTRY=localhost:32080 REPOSITORY_PROJECT=eva \
  ./install/push_images_to_repository.sh

PULL_SOURCE_IMAGES=false IMAGE_LIST=install/images/infra-images-pulled.txt \
  REPOSITORY_REGISTRY=localhost:32080 REPOSITORY_PROJECT=eva \
./install/push_images_to_repository.sh
```

Harbor 설치가 `install/harbor-endpoint.yaml`을 생성합니다. 단일 서버에서는 agent playbook이 이
파일의 Pod 접근 endpoint를 자동으로 Qdrant snapshot sidecar에 적용합니다. Harbor와 k3s 서버가
다르면 이 파일도 배포 controller로 옮기고, `site_infra.yaml` 및 `site_eva_agent.yaml` 실행에
`-e @install/harbor-endpoint.yaml`을 추가합니다. 별도 Harbor 설치에서는 `--hostname`과
`--registry-endpoint <내부 DNS/IP:32080>`을 실제 접근 주소로 지정합니다. metadata에는 비밀번호를
넣지 않으므로 별도 Harbor 서버의 agent 배포에는 `-e harbor_admin_password='<Harbor 비밀번호>'`도
지정합니다.

스크립트가 `eva/<이름>` 으로 올리면서, **레지스트리 주소가 없는 이미지는 Docker Hub 원본 경로로도**
한 벌 더 올립니다 (`library/busybox`, `library/mysql`, `bitnami/kubectl` …). 다음 단계의 mirror 가 그 경로를 찾습니다.
필요한 Harbor project 도 자동으로 만듭니다.

`adorsys/keycloak-config-cli`, `amazon/aws-cli` 의 mirror push 가 실패해도 무해합니다 —
전자는 role 이 `eva/keycloak-config-cli` 로 주소를 지정하고, 후자는 airgap 에서 쓰이지 않습니다.

### 09. docker.io mirror · **[대상]** · 1회만 ⚑

일부 차트는 `busybox:latest`, `mysql:8.0.42-bookworm` 처럼 이미지를 주소 없이 적어둡니다.
kubelet 은 그런 이름을 docker.io 로 해석하므로 폐쇄망에서 실패합니다.
mirror 는 차트를 건드리지 않고 그 요청을 Harbor 로 돌립니다.

> **bare Ubuntu 에서 시작한다면 10번(ansible 설치)을 먼저 하세요.**
> `site_infra.yaml` 은 ansible 로 실행되므로, 이 단계보다 ansible 이 먼저 있어야 합니다.

**k3s 를 새로 깔 때** — `site_infra.yaml` 이 k3s 와 registries.yaml 을 함께 설치합니다.

```bash
ansible-playbook -i 'localhost,' -c local site_infra.yaml -K \
  -e repository_mode=local_repository -e repository_registry=localhost:32080
```

**k3s 가 이미 있을 때** — 직접 씁니다.

```bash
sudo tee /etc/rancher/k3s/registries.yaml >/dev/null <<'EOF'
mirrors:
  "localhost:32080":
    endpoint:
      - "http://localhost:32080"
  "docker.io":
    endpoint:
      - "http://localhost:32080"
configs:
  "localhost:32080":
    auth:
      username: "admin"
      password: "EVA123@"
    tls:
      insecure_skip_verify: true
EOF

sudo systemctl restart k3s
sleep 25
sudo k3s crictl pull docker.io/library/busybox:latest        && echo "busybox OK"
sudo k3s crictl pull docker.io/bitnami/kubectl:latest        && echo "kubectl OK"
sudo k3s crictl pull docker.io/library/mysql:8.0.42-bookworm && echo "mysql OK"
sudo k3s crictl pull localhost:32080/eva/eva-app:3.1.2       && echo "eva-app OK"
```

네 줄 모두 OK 여야 합니다. **`ctr` 로는 검증되지 않습니다** — registries.yaml 을 읽지 않아 항상 인증 실패합니다.
k3s 재시작은 재설치가 아니고 컨테이너는 containerd 가 계속 돌립니다 (control plane 만 20~30초 끊김).

### 10. ansible · **[대상]**

```bash
./install/install_python_venv_airgap.sh
./install/install_ansible_airgap.sh
source .venv/bin/activate
ansible --version
```

`site_infra.yaml` 을 돌릴 계획이면 **full `ansible` 이 필요합니다** — nfs role 이 `ansible.posix.mount` 를 쓰기 때문입니다.
`requirements-airgap.txt` 전체로 받은 wheelhouse 면 그대로 됩니다.

iam · app 만 배포하고 `site_infra.yaml` 을 건너뛸 때는 `ansible-core` 만으로도 충분합니다
(두 role 은 `ansible.builtin.*` 만 씁니다).

```bash
ANSIBLE_AIRGAP_REQUIREMENTS="ansible-core==2.20.5" ./install/install_ansible_airgap.sh
```
**SSH 세션마다 `source` 를 다시 하세요.**

---

## Phase 3 — 배포

### 11. 인증서와 kubeconfig · **[대상]**

파일명은 `tls.crt` / `tls.key` 고정이고 내용이 PEM 이어야 합니다. eva-iam 과 eva-app 이 같은 디렉터리를 씁니다.

```bash
sudo install -d /home/eva/certs
sudo cp fullchain.pem /home/eva/certs/tls.crt
sudo cp privkey.pem   /home/eva/certs/tls.key
sudo chmod 600 /home/eva/certs/tls.key
sudo head -1 /home/eva/certs/tls.crt        # BEGIN CERTIFICATE

sudo test -f /root/.kube/config \
  || sudo install -Dm600 /etc/rancher/k3s/k3s.yaml /root/.kube/config
```

### 12. values 두 개 · **[대상]** ⚑

heredoc 이 붙여넣기에서 자주 깨지므로 줄 단위로 씁니다.

```bash
printf 'ingress:\n  ingressClassName: traefik\n' > values/eva-iam.yaml

printf '%s\n' \
'localhost:' \
'  app:' \
'    license:' \
'      activation_mode: "offline"' \
'      product_code: "eva-prod"' \
'      api_key: "<라이선스 api_key>"' \
'      shared_key: "<라이선스 shared_key>"' \
'    sso:' \
'      realm: "eva-iam"' \
'      clientId: "eva-app"' \
'  ingress:' \
'    tls:' \
'      hostPath: /home/eva/certs' \
> values/app.yaml
```

ansible 이 읽기 전에 검증합니다.

```bash
python3 -c "
import yaml,json
for f in ('values/eva-iam.yaml','values/app.yaml'):
    d=yaml.safe_load(open(f)); print(f, json.dumps(d, ensure_ascii=False))
assert 'localhost' in yaml.safe_load(open('values/app.yaml')), 'app.yaml host 키 없음'
print('OK')"
```

두 파일의 차이

- **eva-iam** — host 키 없이 최상위에 씁니다
- **eva-app** — **host 키가 필수**입니다. 없으면 파일 전체가 조용히 무시됩니다.
  `localhost` 는 `-i 'localhost,'` 로 돌릴 때의 이름입니다
- `sso.realm` / `clientId` 는 차트 기본값이 `eva-sso` / `eva-app-kr-2` 라 반드시 덮어써야 합니다
  (role 의 `-e` 노브가 없어 이 파일에서만 지정 가능)
- `activation_mode` 는 기본이 `online` 이라 폐쇄망에서 실패합니다

### 13. eva-iam 배포 · **[대상]**

eva-app 이 루트를 쓰므로 eva-iam 은 `/iam` 서브패스로 둡니다.
그리고 eva-app 의 SSO 세션 저장소가 eva-iam 의 Redis 이므로 NodePort 를 열어야 합니다.

```bash
ansible-playbook -i 'localhost,' -c local site_eva_iam.yaml -K \
  -e repository_mode=local_repository \
  -e repository_registry=localhost:32080 \
  -e eva_iam_host=magok.eva.lge.com \
  -e eva_iam_node_user=eva \
  -e eva_iam_ingress_path=/iam \
  -e eva_iam_redis_external_enabled=true \
  -e eva_iam_redis_tls_enabled=true \
  -e eva_iam_redis_nodeport=32070 \
  -e '{"eva_iam_app_redirect_uris": ["https://magok.eva.lge.com/*"]}'

cat config/localhost/eva-iam.yaml     # ssoBaseUrl / adminClientSecret
```

- 같은 host 에서 eva-iam 과 eva-app 이 모두 `/` 를 쓰면 Keycloak 요청이 eva-app 으로 가서
  `{"detail":{"code":"csrf_failed"}}` 같은 엉뚱한 응답이 돌아옵니다
- eva-iam 차트는 `redis.tls.enabled=true` 일 때만 external 을 허용합니다
- `--check` 를 붙이면 아무것도 설치되지 않습니다 (dry-run)

`adminClientSecret` 이 비어 있으면 직접 조회합니다.

```bash
kubectl exec -n eva-iam deploy/eva-iam-keycloak -- bash -c '
set -eu
kc=/opt/keycloak/bin/kcadm.sh
cfg=$(mktemp); trap "rm -f $cfg" EXIT
$kc config credentials --config "$cfg" --server "http://localhost:8080${KC_HTTP_RELATIVE_PATH%/}" \
  --realm master --user "$KEYCLOAK_ADMIN" --password "$KEYCLOAK_ADMIN_PASSWORD" >/dev/null
id=$($kc get clients --config "$cfg" -r eva-iam \
  -q clientId=eva-admin-api --fields id --format csv --noquotes)
$kc get clients/$id/client-secret --config "$cfg" -r eva-iam \
  --fields value --format csv --noquotes
'
```

### 14. eva-app 배포 · **[대상]**

```bash
ansible-playbook -i 'localhost,' -c local site_eva_app.yaml -K \
  -e repository_mode=local_repository \
  -e repository_registry=localhost:32080 \
  -e eva_app_backend_host=magok.eva.lge.com \
  -e eva_app_backend_secure=true \
  -e eva_app_sso_base_url=https://magok.eva.lge.com/iam \
  -e eva_app_sso_admin_client_secret='<13번 hand-off 파일의 값>'
```

- `sso_base_url` 에 **`/iam` 을 빼먹으면** OIDC discovery 를 못 찾습니다
- `backend_host` 는 브라우저가 실제로 쓰는 주소여야 프론트가 백엔드를 찾습니다
- eva-app 이 이미 있으면 upgrade 가 되고 role 이 replicas=0 으로 내린 뒤 올리므로 다운타임이 생깁니다.
  버전을 건너뛰는 재설치라면 `/eva-app` hostPath(MySQL) 를 지우는 편이 안전합니다

### 15. eva.yaml 생성 · **[대상]**

agent · vision role 은 `config/<host>/eva.yaml` 이 없으면 assert 로 멈춥니다.
GPU 개수와 MIG 상태를 `nvidia-smi` 로 자동 감지해 만들어지므로 별도 값은 필요 없습니다.

```bash
ansible-playbook -i 'localhost,' -c local site_eva_config.yaml -K \
  -e repository_mode=local_repository -e repository_registry=localhost:32080

cat config/localhost/eva.yaml
```

airgap 이면 `awscli` role 은 자동으로 건너뜁니다. `nfs_share_path` 기본값은 `/share/eva-agent` 입니다.
iam · app 만 설치할 때는 이 단계가 필요 없습니다.

### 16. eva-agent 배포 · **[대상]**

agent · agent-init · vllm · qdrant 네 릴리스가 함께 올라갑니다.

```bash
ansible-playbook -i 'localhost,' -c local site_eva_agent.yaml -K \
  -e repository_mode=local_repository \
  -e repository_registry=localhost:32080 \
  -e repository_project=eva \
  -e eva_agent_vllm_profile=PRO6000-MIGx4 \
  -e eva_agent_qdrant_values_file=values-k3s.harbor.yaml \
  -e eva_agent_qdrant_snapshot_source=harbor
```

- 모델 캐시(`install/models/agent/hf`, `install/models/vllm/hf`)가 대상 서버에 있어야 합니다
- GPU 프로파일이 05번 다운로드 때와 다르면 Harbor 에 없는 vllm 이미지를 찾게 됩니다
- Qdrant Harbor snapshot profile에서는 `repository_registry`와 `repository_project`가 Qdrant 본체,
  snapshot-sync sidecar, OCI snapshot artifact의 registry/repository가 됩니다. 단일 노드 Local Harbor는
  `localhost:32080`을 씁니다. 이때 role은 Pod 안의 ORAS가 host loopback을 보지 않도록 node
  InternalIP:32080을 snapshot endpoint로 자동 변환하며, `harbor-endpoint.yaml`이 있으면 그 파일의
  endpoint를 우선 적용합니다. 여러 노드/별도 Harbor는 실제 접근 가능한 Harbor hostname/IP:port를
  `harbor-endpoint.yaml`로 전달해 `repository_registry`에 적용합니다.
- `qdrant-snapshot-harbor`는 Harbor 설치 과정에서 생기는 값이 아니라, Qdrant Harbor snapshot
  mode에서 role이 Helm 설치 직전에 만드는 Qdrant 전용 Kubernetes Secret입니다. 이름은 고정이고
  사용자/비밀번호는 `harbor_admin_user`/`harbor_admin_password`를 사용합니다. 표준 Local Harbor
  경로의 `harbor.yml`이 있으면 role이 실제 관리자 비밀번호를 자동으로 읽으며, 명시한
  `harbor_admin_password`가 있으면 그 값이 우선합니다. Harbor를 기본 경로 이외에 설치했다면
  `eva_agent_harbor_config_path`에 해당 `harbor.yml` 경로를 지정합니다.
- 배포 후 `kubectl logs -n eva-agent statefulset/eva-agent-qdrant -c qdrant-snapshot-sync --tail=100`에서
  `pulling <registry>/eva/qdrant-snapshots:...` 및 restore 성공 여부를 확인합니다. `ErrImagePull`이면
  `kubectl get pods -n eva-agent -l app.kubernetes.io/instance=eva-agent-qdrant -o jsonpath='{range .items[*]}{range .spec.containers[*]}{.image}{"\n"}{end}{end}' | sort -u`
  로 이미지가 모두 선택한 Harbor를 가리키는지 확인한 뒤 image/snapshot push 단계를 다시 실행합니다.

### 17. eva-vision 배포 · **[대상]**

```bash
ansible-playbook -i 'localhost,' -c local site_eva_vision.yaml -K \
  -e repository_mode=local_repository \
  -e repository_registry=localhost:32080
```

MIG 설정은 `eva.yaml` 에서 읽습니다.
eva-app 은 `http://eva-vision.eva-vision:8000` 과 `http://eva-agent.eva-agent` 로 이 둘을 찾으므로,
배포되면 주소가 그대로 맞아떨어집니다.

> `site_eva.yaml` 은 agent → vision → app 을 한 번에 돌립니다.
> app 을 이미 올린 뒤라면 개별 playbook(16 · 17번)을 쓰는 게 app 재시작을 피합니다.

---

## Phase 4 — 접속과 검증

### 18. 경로와 응답 확인 · **[대상]**

```bash
kubectl get ingress -A -o custom-columns='NS:.metadata.namespace,HOST:.spec.rules[0].host,PATH:.spec.rules[0].http.paths[0].path'
#   eva-app   magok.eva.lge.com  /
#   eva-iam   magok.eva.lge.com  /iam

curl -sk https://magok.eva.lge.com/iam/realms/eva-iam/.well-known/openid-configuration \
  | python3 -c 'import json,sys; print(json.load(sys.stdin)["issuer"])'
#   https://magok.eva.lge.com/iam/realms/eva-iam

curl -sk https://magok.eva.lge.com/ -o /dev/null -w 'app %{http_code}\n'
nc -vz 10.158.200.113 32070          # redis 열렸는지

sudo KUBECONFIG=/root/.kube/config helm get values eva-app -n eva-app | grep -A5 -E "sso|license"
```

`helm get values` 가 values 파일 적용 여부를 가장 확실하게 보여줍니다.

### 19. 브라우저 로그인 ⚑

**반드시 `https://magok.eva.lge.com/` 로, 새 시크릿 창에서** 접속하세요.

```
https://magok.eva.lge.com/           # eva-app
https://magok.eva.lge.com/iam/admin/ # Keycloak 콘솔 (master admin/adminpass)
```

- NodePort(`:32010`) 로 직접 붙지 마세요 — traefik 을 우회해 TLS 가 없고,
  uvicorn 로그에 `Invalid HTTP request received` 가 찍힙니다
- `http://` 로 로그인을 시작하면 `redirect_uri` 가 등록값과 달라져 `invalid_grant / Code not valid` 가 납니다
- 인가 코드는 **한 번만** 쓸 수 있어서, 실패한 콜백 URL 을 새로고침하면 같은 에러가 반복됩니다.
  캐시를 비우거나 시크릿 창에서 처음부터 시도하세요

```bash
kubectl logs -n eva-app deploy/eva-app --tail=50 | grep -iE "sso|redis|token"
```

### 20. 지우고 다시 · **[대상]**

hostPath 를 남기면 DB 가 복원됩니다 — realm · 사용자 · client secret 이 그대로 살아납니다.

```bash
sudo KUBECONFIG=/root/.kube/config helm uninstall eva-iam -n eva-iam
kubectl delete ns eva-iam --wait=true
sudo rm -rf /home/eva/.eva-iam/host-path     # keycloak DB

sudo KUBECONFIG=/root/.kube/config helm uninstall eva-app -n eva-app
kubectl delete ns eva-app --wait=true
sudo rm -rf /eva-app                          # eva-app / mysql
```

Harbor 이미지 · registries.yaml mirror · 인증서 · `.venv` · values 파일은 남으므로,
재설치는 **11번부터** 하면 됩니다.

---

## 겪었던 함정

전부 “조용히 실패”하는 종류였습니다.

| 증상 | 실제 원인 | 확인 |
|---|---|---|
| push 가 `failed to read config content` | 준비 서버가 containerd 스토어 → tar 에 레이어 누락 | `docker image inspect … {{.Architecture}}` |
| 새 tar 를 load 해도 그대로 | 깨진 기록이 남아 load 가 건너뜀 | `docker rmi -f` 후 재시도 |
| 대상 서버 pip 이 ansible 을 못 찾음 | `&&` 로 묶어 wheel 다운로드가 실행되지 않음 | `ls install/wheels/*.whl \| wc -l` |
| realm-config Job 5분 타임아웃 | `busybox:latest` 가 docker.io 로 해석 → mirror 없음 | `crictl pull docker.io/library/busybox:latest` |
| mysql Pod 만 ErrImagePull | eva-app 3.1.4 가 mysql 이미지를 하드코딩 (2.1.3 엔 키가 있었음) | Pod spec 의 image 값 |
| ImagePullBackOff · 태그 없음 | versions.json 을 다운로드 *후* 에 고침 | Harbor artifacts API |
| values 가 안 먹은 듯한 배포 | helm `-f` 가 2개 — host values 없음/빈 파일 | `helm get values` |
| namespace 조차 안 생김 | `--check` dry-run | PLAY RECAP 의 skipped 수 |
| Keycloak 이 `csrf_failed` 를 반환 | eva-iam · eva-app 이 같은 host 의 `/` — 요청이 eva-app 으로 | 응답 본문 형식 (`detail` = eva-app) |
| Redis `Connection refused` | eva-iam redis external 이 꺼져 있음 (TLS 필요) | `nc -vz <host> 32070` |
| `invalid_grant` · `Invalid HTTP request` | http 로 시작 · NodePort 직접 접속 · 코드 재사용 | https + 시크릿 창으로 재시도 |

---

## 차트 쪽 미해결 과제

이미지 주소를 values 로 바꿀 수 없게 하드코딩된 곳이 네 군데 있습니다.
같은 차트의 다른 컨테이너들은 `.Values.*.image.repository` 패턴을 쓰므로, 그 패턴에 맞추면 mirror 우회가 불필요해집니다.

| 차트 | 파일 | 이미지 |
|---|---|---|
| eva-iam 3.1.0 / 3.1.1 | `templates/keycloak/deployment.yaml:31` | `busybox:latest` |
| eva-iam 3.1.0 / 3.1.1 | `templates/keycloak/realm-config-job.yaml:195` | `busybox:latest` |
| eva-iam 3.1.0 / 3.1.1 | `templates/tls-secret-job.yaml:49` | `bitnami/kubectl:latest` |
| eva-app 3.1.4 | `templates/mysql/deployment.yaml:25` | `mysql:8.0.42-bookworm` (2.1.3 에는 `database.internal.image` 키가 있었음) |

수정안 (eva-iam `ecr-cronjob.yaml:79` 가 이미 쓰는 방식과 동일)

```yaml
# values.yaml
images:
  busybox: ""     # 비우면 기존 동작 유지
  kubectl: ""

# templates
image: {{ .Values.images.busybox | default "busybox:latest" | quote }}
imagePullPolicy: IfNotPresent
```

`imagePullPolicy` 를 함께 지정하는 이유는 `:latest` 태그에 쿠버네티스가 `Always` 를 적용해서,
노드에 이미지가 있어도 매번 레지스트리를 조회하기 때문입니다.
가능하면 `:latest` 대신 버전 고정(`busybox:1.37` 등)도 함께 검토하면 좋습니다 — 폐쇄망에서는 재현성 문제가 됩니다.

---

**비밀번호는 아직 전부 차트 공개 기본값입니다** — realm `admin`/`EVAEVA123@`, master `admin`/`adminpass`,
postgres `strongpassword`, redis `eva-redis-pass`. 운영 전에 교체해야 합니다.
