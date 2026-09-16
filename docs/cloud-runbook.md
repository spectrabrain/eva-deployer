# EVA Cloud Installation Guide

This guide installs EVA on one Ubuntu 24.04 `linux/amd64` server with a supported
NVIDIA GPU. The normal installation is one `eva install` operation for Infra,
Config, IAM, Agent, Vision, and App. It changes Docker, k3s, NVIDIA runtime, and
host persistence; confirm the maintenance window and backup policy first.

## 1. Target Environment

| Item | Required operating baseline |
| --- | --- |
| OS and platform | Ubuntu 24.04, `linux/amd64` |
| Accelerator | Supported NVIDIA GPU, installed NVIDIA driver, and `nvidia-smi` |
| Container platform | Docker, k3s, NVIDIA Container Toolkit/CDI, NVIDIA Device Plugin |
| Network | Cloud Repository, GitHub, Docker Hub, AWS/ECR/S3, and other required HTTPS endpoints |
| Site inputs | AWS credentials and TLS certificate/key on the target host |
| Privilege | A user that can run `sudo eva ...` |

The default components are `infra`, `iam`, `agent`, `vision`, and `app`; `n8n` is
optional. Agent installs EVA Agent, `eva-agent-init`, vLLM, and Qdrant together.

## 2. Install EVA Tool And Verify Release

Extract the Tag Base Release artifact and run the bundled installer from its
Release root.

```bash
unzip -q eva-base-release-*.zip -d eva-base-release
cd eva-base-release
sudo bash ./eva-tool-installer.sh
eva verify .
```

The installer verifies the single `eva-tool_*_linux_amd64.tar.gz` archive against
`checksums.sha256`. `eva install` later prepares the verified Release under
`/opt/eva/releases/<version>/`; no Release version variable is needed.

## 3. Prepare Managed Runtime

Bootstrap only when the server does not already have an EVA Runtime. It installs
and validates the managed Ansible and Kubernetes tools before publishing
`/opt/eva/runtime`.

```bash
sudo eva runtime bootstrap
```

Do not repeat bootstrap merely because the EVA Release changes. `eva install`
validates the managed Runtime before it starts an operation.

## 4. Verify GPU Prerequisites

Before installation, run the EVA GPU prerequisite check. It verifies the NVIDIA
driver, shows detected GPU and MIG state, and explains what must be corrected when
the driver is unavailable.

```bash
sudo eva preflight gpu
```

Configure the intended MIG layout before installation when the site uses MIG.
`infra` configures Docker, CDI, k3s, the NVIDIA Device Plugin, and allocatable
resources. Config then selects `nvidia.com/gpu` or a positive
`nvidia.com/mig-*` resource automatically. Multiple positive MIG resource types
stop installation rather than being selected arbitrarily.

## 5. Create Workspace Inputs

Choose an absolute Workspace path. `/etc/eva/sites/<site-id>` is conventional, but
any location such as `/home/eva/site-dev-196` works when passed with `--workspace`.
The inventory target name is the top-level key for `iam.yaml` and `app.yaml`.
Create `agent.yaml` or `vision.yaml` only for an intentional chart override.

```bash
mkdir -p /home/eva/site-dev-196/{credentials,inventory,site-values}
```

```text
<workspace>/
├── credentials/
│   └── aws_key.ini
├── inventory/
│   └── inventory.ini
└── site-values/
    ├── site.yaml
    ├── iam.yaml
    └── app.yaml

# Optional only for intentional chart overrides:
# site-values/agent.yaml
# site-values/vision.yaml
```

### `site-values/site.yaml`

```yaml
site:
  id: site-dev-196

repository:
  mode: cloud
  project: eva

components:
  infra: true
  iam: true
  agent: true
  vision: true
  app: true
  n8n: false
```

### `inventory/inventory.ini`

```ini
[eva]
site-dev-196 ansible_connection=local ansible_user=eva ansible_password='<SSH_PASSWORD>' ansible_become=true ansible_become_password='<SUDO_PASSWORD>'
```

### `credentials/aws_key.ini`

```ini
aws_access_key_id = <AWS_ACCESS_KEY_ID>
aws_secret_access_key = <AWS_SECRET_ACCESS_KEY>
region = ap-northeast-2
```

`awscli` reads only this Workspace file and configures the inventory target user's
AWS CLI credentials. Keep this file out of Git, chat, and Release artifacts.

### `site-values/iam.yaml`

```yaml
site-dev-196:
  config:
    host: iam196.eva-dev.lge.com
  keycloak:
    realmPatch:
      realmAdmin:
        password: "<REALM_ADMIN_PASSWORD>"
      evaApp:
        redirectUris:
          - "https://app196.eva-dev.lge.com/*"
  ingress:
    tls:
      hostPath: /home/eva/certs
  imagePullSecrets:
    hostPath: /home/eva/.aws
```

### Optional `site-values/agent.yaml`

Create this file only for a deliberate EVA Agent main chart override. The Agent,
vLLM, and Qdrant baseline values are supplied by the Agent Release; do not copy or
edit them in the Workspace. Config automatically selects the vLLM profile from the
GPU model, physical GPU count, MIG state, and Kubernetes allocatable resources.

### Optional `site-values/vision.yaml`

This optional file is only for deliberate Vision chart overrides such as persistent
storage capacity, CPU/memory, image policy, or rollout timeout. Config and
Kubernetes automatically select the GPU or MIG resource; do not create this file
to choose a GPU count, MIG profile, or resource name.

### `site-values/app.yaml`

```yaml
site-dev-196:
  app:
    browserTitleName: "EVA DEV(196)"
    backendHost: "app196.eva-dev.lge.com"
    license:
      activation_mode: online
      product_code: eva-dev
      api_key: "<SITE_API_KEY>"
      shared_key: "<SITE_SHARED_KEY>"
```

With IAM and App in one operation, IAM SSO values are handed to App automatically.
For a separate central IAM server, provide `app.sso.baseUrl` and
`app.sso.adminClientSecret` explicitly in this Workspace file.

`iam.yaml`, optional `agent.yaml` or `vision.yaml`, `app.yaml`, and `aws_key.ini`
can contain credentials. Keep real files in approved Secret management and never commit them.
Place the target TLS certificate and key in the host path configured above, normally
`/home/eva/certs`.

Agent Release values, Config-generated settings, and role k3s/repository overrides
are applied automatically. Optional Agent and Vision Workspace chart overrides are
target-specific and do not select GPU, MIG, or vLLM profiles.

## 6. Install

Run one command from the verified Release root.

```bash
sudo eva install . --workspace /home/eva/site-dev-196 --yes
```

The Plan order is always:

```text
precondition -> infra and GPU/MIG -> config -> iam -> agent -> vision -> app
```

Config is automatically included whenever Agent, Vision, or App is selected. Do
not run Config separately. A component-only operation does not run unselected
product components. Agent installs `eva-agent`, `eva-agent-vllm`, and
`eva-agent-qdrant` together from the Release-managed values.

If precondition detects a supported APT repository issue, diagnose it separately;
`eva install --yes` does not approve external APT repository changes.

```bash
sudo eva troubleshoot apt
sudo eva troubleshoot apt --fix-known --yes
```

## 7. Verify Installation

Start with the installation health check. For selected Agent or Vision components,
it also verifies a node GPU or MIG allocatable resource, NVIDIA Device Plugin
readiness, and an Agent/Vision Pod GPU or MIG allocation.

```bash
sudo eva check --verbose
```

When the check fails, use the operation summary before opening Kubernetes or
private rendered artifacts.

```bash
sudo eva status
```

The App endpoints for Agent and Vision are the internal `eva-agent.eva-agent` and
`eva-vision.eva-vision` Services. Check their selected endpoints from the App pod
when diagnosing a connection failure.

## 8. Rendered Artifacts And Troubleshooting

Each selected Helm component keeps chart defaults, effective Helm input, resolved
Helm values, and source metadata under the prepared Release. Effective and resolved
files are private because they can contain Secret values.

```text
/opt/eva/releases/<version>/out/work/config/<site>/<target>/eva.yaml

/opt/eva/releases/<version>/out/work/rendered/<site>/<target>/agent/
  chart-defaults.yaml              # 0644
  effective-input-values.yaml      # 0600
  resolved-values.yaml             # 0600
  values-sources.yaml              # 0644, no Secret values

/opt/eva/releases/<version>/out/work/rendered/<site>/<target>/vllm/
  chart-defaults.yaml              # 0644
  effective-input-values.yaml      # 0600
  resolved-values.yaml             # 0600
  values-sources.yaml              # 0644, no Secret values

/opt/eva/releases/<version>/out/work/rendered/<site>/<target>/qdrant/
  chart-defaults.yaml              # 0644
  effective-input-values.yaml      # 0600
  resolved-values.yaml             # 0600
  values-sources.yaml              # 0644, no Secret values

/opt/eva/releases/<version>/out/work/rendered/<site>/<target>/vision/
  chart-defaults.yaml              # 0644
  effective-input-values.yaml      # 0600
  resolved-values.yaml             # 0600
  values-sources.yaml              # 0644, no Secret values

/opt/eva/releases/<version>/out/work/rendered/<site>/<target>/app/
  chart-defaults.yaml              # 0644
  effective-input-values.yaml      # 0600
  resolved-values.yaml             # 0600
  values-sources.yaml              # 0644, no Secret values

/var/lib/eva/sites/<site>/<target>/eva-iam.yaml  # root:root, 0600
```

Do not paste or commit private values and handoff files. They can contain license,
SSO, database, or registry credentials.

## 9. Change And Retry

Updating Workspace values or a Release version performs a Helm upgrade and preserves
the existing database and persistent data. A normal upgrade does not delete
`/eva-app` or Agent/Vision persistence. Config is regenerated automatically for
the selected components. After correcting a failed operation, retry only that
operation.

```bash
sudo eva retry --yes
```

## 10. Non-GPU Test Environment

A CPU-only server is an exception intended for limited testing. Set `agent` and
`vision` to `false` in `site.yaml`; Infra and Config skip NVIDIA runtime/CDI work.
It is not the baseline Cloud operating environment in this guide.
