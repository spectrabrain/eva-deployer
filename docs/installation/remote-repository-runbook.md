# EVA Remote Repository Runbook

Remote mode installs EVA on a Target that uses Main Harbor and the installation
assets prepared by the Main server. This document is for the Target installation
operator.

> Status: Remote Target installation is not yet approved for live deployment.
> Live Main Harbor and Target E2E validation is still required before production use.

## 1. Prerequisites

Before starting, confirm that the Main preparation and Release publication have
been completed by the approved release process. Work from the published original
Release directory on the Target, and use a Workspace configured for `remote`
repository mode.

The Target must be able to reach its approved Main Harbor endpoint. It must not
use public registries, AWS, S3, Hugging Face, or external Helm repositories for
the installation.

## 2. Workspace

Create the normal EVA Workspace and set the repository inputs in
`site-values/site.yaml`.

```yaml
site:
  id: <site-id>

repository:
  mode: remote
  registry: <main-harbor-host:port>
  project: eva

components:
  infra: true
  iam: true
  agent: true
  vision: true
  app: true
  n8n: false
```

Keep site-specific inventory, TLS material, and approved secrets in the normal
Workspace locations. Do not put them in the Release directory.

## 3. Validate and install

Run the following commands from the published Release directory.

```bash
cd /var/lib/eva/inbox/releases/<version>

sudo eva workspace validate \
  --workspace <workspace>

sudo eva preflight gpu

sudo eva preflight argocd \
  --workspace <workspace>

sudo eva install . \
  --workspace <workspace> \
  --yes

sudo eva check --verbose
sudo eva status
```

`eva preflight argocd` is required when an existing Argo CD management handoff
must be checked before installation. Follow its approval prompts rather than
editing Argo CD state directly.

## 4. Re-run and troubleshooting

If an EVA operation fails after making a safe correction, inspect its status and
use the normal retry flow.

```bash
sudo eva status
sudo eva retry --yes
```

For a Workspace error, correct the Workspace input and run `eva workspace
validate` again. For a Release integrity error, stop and obtain the approved
published Release again; do not modify artifact files in place.

Main-side asset preparation is described in
[Remote Asset Preparation](../preparation/remote-asset-preparation.md).
