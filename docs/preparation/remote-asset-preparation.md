# EVA Remote Asset Preparation

This guide is for the Main operator who prepares Remote repository assets and
publishes the original Release to a Target.

## 1. Purpose

`eva remote prepare` prepares the Remote assets for one verified original
Release. `eva remote verify` validates the resulting local preparation evidence.
`eva remote publish` sends the verified original Release to the Target.

Before any download or publish, `eva remote prepare` checks the Main server's
required tools, Docker daemon, AWS credentials, Main Harbor, and preparation
storage. A failed check stops preparation without downloading or publishing.

## 2. Prerequisites

Use an original EVA Release on a Main server that can reach the required source
services. The Release must include its normal checksum manifest and required
offline artifact. Use an approved Main Harbor endpoint.

Current implementation status: Remote preparation and Release publication CLI
commands are implemented. Live Main Harbor and Target E2E validation remains
required before deployment.

## 3. Verify the Release

From the original Release directory, verify its integrity before preparation.

```bash
eva verify .
```

Do not use a prepared Release or an imported Airgap Bundle as this command's
input.

## 4. Prepare Remote assets

Prepare all supported Remote assets for the Main Harbor project.

```bash
sudo eva remote prepare . \
  --registry <main-harbor>
```

The command records the preparation result. If it fails, inspect the reported
manifest path and the CLI error before attempting a later approved recovery
procedure.

## 5. Verify the preparation result

```bash
sudo eva remote verify . \
  --registry <main-harbor>
```

This command verifies local preparation evidence. It does not download, publish,
or query live Harbor availability.

## 6. Publish the Release to the Target

After successful preparation verification, publish the original Release.

```bash
sudo eva remote publish . \
  --target <user@target>
```

## 7. Next step

Continue on the Target with the
[Remote Repository Runbook](../installation/remote-repository-runbook.md).
