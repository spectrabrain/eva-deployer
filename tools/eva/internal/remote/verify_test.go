package remote

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"eva-deployer/tools/eva/internal/release"
)

func TestVerifyValidatesCompletedPreparationWithoutMutation(t *testing.T) {
	root, resolved, identity := writeCompletedPreparation(t)
	before := preparationFingerprint(t, root)
	result, err := (VerifyService{PreparationRoot: filepath.Dir(root), CacheRoot: filepath.Join(root, "cache")}).Verify(VerifyOptions{Release: resolved, Registry: identity.RepositoryRegistry})
	if err != nil {
		t.Fatalf("Verify() error = %v", err)
	}
	if result.ManifestPath != filepath.Join(root, manifestFileName) || result.LiveRegistryVerified {
		t.Fatalf("Verify() result = %#v", result)
	}
	after := preparationFingerprint(t, root)
	if before != after {
		t.Fatalf("Verify() mutated preparation tree\nbefore=%s\nafter=%s", before, after)
	}
}

func TestVerifyRejectsIncompleteOrMismatchedPreparation(t *testing.T) {
	for _, testCase := range []struct {
		name   string
		mutate func(t *testing.T, root string, resolved release.Resolved, identity PreparationIdentity)
	}{
		{"missing manifest", func(t *testing.T, root string, _ release.Resolved, _ PreparationIdentity) {
			if err := os.Remove(filepath.Join(root, manifestFileName)); err != nil {
				t.Fatal(err)
			}
		}},
		{"missing product evidence", func(t *testing.T, root string, _ release.Resolved, _ PreparationIdentity) {
			if err := os.Remove(filepath.Join(root, "cache/images/images-pulled.txt")); err != nil {
				t.Fatal(err)
			}
		}},
		{"missing image remains", func(t *testing.T, root string, _ release.Resolved, _ PreparationIdentity) {
			if err := os.WriteFile(filepath.Join(root, "cache/images/images-missing.txt"), []byte("example/missing:1\n"), 0o640); err != nil {
				t.Fatal(err)
			}
		}},
		{"registry mismatch", func(t *testing.T, root string, resolved release.Resolved, _ PreparationIdentity) {
			store := NewManifestStore(filepath.Dir(root), nil)
			manifest, err := store.Load(resolved.Metadata.Version)
			if err != nil {
				t.Fatal(err)
			}
			manifest.Repository.Registry = "other.example.internal"
			if err := store.Save(manifest); err != nil {
				t.Fatal(err)
			}
		}},
		{"summary mismatch", func(t *testing.T, root string, _ release.Resolved, _ PreparationIdentity) {
			if err := os.WriteFile(filepath.Join(root, "reports/preparation-summary.yaml"), []byte("release_version: wrong\nrepository: wrong\nassets: []\n"), 0o640); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			root, resolved, identity := writeCompletedPreparation(t)
			testCase.mutate(t, root, resolved, identity)
			_, err := (VerifyService{PreparationRoot: filepath.Dir(root)}).Verify(VerifyOptions{Release: resolved, Registry: identity.RepositoryRegistry})
			if err == nil {
				t.Fatal("Verify() succeeded")
			}
		})
	}
}

func writeCompletedPreparation(t *testing.T) (string, release.Resolved, PreparationIdentity) {
	t.Helper()
	resolved := writeOriginalRelease(t, false)
	if err := os.WriteFile(filepath.Join(resolved.Root, "checksums.sha256"), []byte("release checksums\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	identity, err := BuildPreparationIdentity(resolved, "harbor.example.internal:32080", "eva")
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(t.TempDir(), identity.ReleaseVersion)
	write := func(relative, content string) {
		t.Helper()
		path := filepath.Join(root, relative)
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o640); err != nil {
			t.Fatal(err)
		}
	}
	for _, relative := range []string{"cache/manifest.txt", "cache/apt/debs/manifest.txt", "cache/apt/debs/a.deb", "cache/docker/debs/manifest.txt", "cache/docker/debs/a.deb", "cache/nvidia/container-toolkit-debs/manifest.txt", "cache/nvidia/container-toolkit-debs/a.deb", "cache/tools/oras", "cache/k3s/k3s-v1-linux-amd64", "cache/eva-app/app.tgz", "cache/eva-vision/vision.tgz", "cache/eva-agent/agent.tgz", "cache/eva-agent/release/v1/plugins/eva-agent-qdrant/post-renderer.sh", "cache/eva-agent/release/v1/plugins/eva-agent-qdrant/plugin.yaml"} {
		write(relative, "data\n")
	}
	write("cache/images/images-all.txt", "source/product:1\n")
	write("cache/images/images-pulled.txt", "source/product:1\n")
	write("cache/images/images-missing.txt", "")
	write("cache/images/infra-images-all.txt", "source/infra:1\n")
	write("cache/images/infra-images-pulled.txt", "source/infra:1\n")
	write("cache/images/infra-images-missing.txt", "")
	write("reports/repository-mapping-product.txt", "source/product:1 harbor.example.internal:32080/eva/product:1\n")
	write("reports/repository-mapping-infra.txt", "source/infra:1 harbor.example.internal:32080/eva/infra:1\n")
	write("cache/models/agent/hf/model.bin", "agent\n")
	write("cache/models/vllm/hf/model.bin", "vllm\n")
	write("cache/models/manifest.txt", "files:\n"+filepath.Join(root, "cache/models/agent/hf/model.bin")+"\n"+filepath.Join(root, "cache/models/vllm/hf/model.bin")+"\n")
	write("cache/qdrant-snapshots/snapshot.bin", "snapshot\n")
	write("cache/qdrant-snapshots/manifest.txt", "snapshot_specs:\ndir|snapshot.bin|collection\nfiles:\n"+filepath.Join(root, "cache/qdrant-snapshots/snapshot.bin")+"\n")
	write("reports/qdrant-artifacts.txt", "harbor.example.internal:32080/eva/qdrant-snapshots:tag|snapshot.bin|collection\n")
	if err := writeYAMLReport(root, "reports/release-validation.yaml", map[string]string{"release_version": identity.ReleaseVersion, "release_yaml_sha256": identity.ReleaseYAMLSHA256, "checksums_sha256": identity.ChecksumsSHA256, "platform": resolved.Metadata.Platform.OS + "/" + resolved.Metadata.Platform.Arch, "offline_artifact": offlineArtifactName(resolved), "offline_artifact_sha256": offlineArtifactSHA256(resolved)}); err != nil {
		t.Fatal(err)
	}
	if err := writeYAMLReport(root, "reports/main-preflight.yaml", PreflightReport{SchemaVersion: preflightSchemaVersion, Release: identity.ReleaseVersion, Registry: identity.RepositoryRegistry, Project: identity.RepositoryProject, Categories: []string{"host-tools", "docker", "aws", "harbor", "storage", "external-sources"}, CheckedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	payload, err := BuildTargetPayload(root, filepath.Join(root, "cache"), identity, resolved.Metadata.Platform.OS+"/"+resolved.Metadata.Platform.Arch)
	if err != nil {
		t.Fatal(err)
	}
	runtimeArtifact, err := BuildRuntimeArtifact(root, identity, writeRuntimeFixture(t))
	if err != nil {
		t.Fatal(err)
	}
	if err := writeYAMLReport(root, "reports/preparation-summary.yaml", map[string]any{"release_version": identity.ReleaseVersion, "repository": identity.RepositoryRegistry + "/" + identity.RepositoryProject, "assets": []string{"offline", "product-images", "infra-images", "models", "qdrant-snapshots", "runtime-artifact", "target-payload"}, "runtime_artifact": "prepared", "runtime_version": runtimeArtifact.Manifest.Runtime.Version, "runtime_archive_sha256": runtimeArtifact.Manifest.Runtime.ArchiveSHA256, "runtime_manifest": filepath.ToSlash(filepath.Join(runtimeArtifactDirectory, payloadIdentityKey(identity), "manifest.yaml"))}); err != nil {
		t.Fatal(err)
	}
	if err := writeYAMLReport(root, "reports/verification.yaml", map[string]string{"release_version": identity.ReleaseVersion, "repository": identity.RepositoryRegistry + "/" + identity.RepositoryProject, "status": "validated"}); err != nil {
		t.Fatal(err)
	}
	clock := func() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) }
	manifest, err := NewManifest(identity, DefaultStepNames, clock)
	if err != nil {
		t.Fatal(err)
	}
	for index := range manifest.Steps {
		manifest.Steps[index].Status, manifest.Steps[index].StartedAt, manifest.Steps[index].CompletedAt = StepSucceeded, clock(), clock()
		manifest.Steps[index].Evidence = []string{stepEvidence(manifest.Steps[index].Name)}
	}
	for index, step := range manifest.Steps {
		if step.Name == "build-runtime-artifact" {
			manifest.Steps[index].Evidence = stableEvidence([]string{filepath.ToSlash(filepath.Join(runtimeArtifactDirectory, payloadIdentityKey(identity), "manifest.yaml")), filepath.ToSlash(filepath.Join(runtimeArtifactDirectory, payloadIdentityKey(identity), "checksums.sha256")), filepath.ToSlash(filepath.Join(runtimeArtifactDirectory, payloadIdentityKey(identity), runtimeArtifact.Manifest.Runtime.Archive))})
		}
	}
	for index, step := range manifest.Steps {
		if step.Name == "write-manifest" {
			manifest.Steps[index].Evidence = stableEvidence([]string{stepEvidence("write-manifest"), filepath.ToSlash(filepath.Join(targetPayloadDirectory, payload.Manifest.Identity, "manifest.yaml"))})
		}
	}
	manifest.Status, manifest.CompletedAt, manifest.UpdatedAt = ManifestSucceeded, clock(), clock()
	store := NewManifestStore(filepath.Dir(root), clock)
	if err := store.Save(manifest); err != nil {
		t.Fatal(err)
	}
	return root, resolved, identity
}

func stepEvidence(name string) string {
	values := map[string]string{"validate-release": "reports/release-validation.yaml", "main-preflight": "reports/main-preflight.yaml", "build-runtime-artifact": "runtime/placeholder", "prepare-offline-assets": "cache/manifest.txt", "download-product-images": "cache/images/images-all.txt", "download-infra-images": "cache/images/infra-images-all.txt", "download-models": "cache/models/manifest.txt", "download-qdrant-snapshots": "cache/qdrant-snapshots/manifest.txt", "publish-product-images": "reports/repository-mapping-product.txt", "publish-infra-images": "reports/repository-mapping-infra.txt", "publish-qdrant-snapshots": "reports/qdrant-artifacts.txt", "write-manifest": "reports/preparation-summary.yaml", "verify": "reports/verification.yaml"}
	return values[name]
}

func writeRuntimeFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	tools := map[string]string{"ansible-playbook": "venv/bin/ansible-playbook", "helm": "bin/helm", "kubectl": "bin/kubectl", "kustomize": "bin/kustomize", "oras": "bin/oras"}
	for _, tool := range tools {
		path := filepath.Join(root, tool)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	contents := "schema_version: v1\nversion: 1.0.1\ntools:\n"
	for name, tool := range tools {
		contents += "  " + name + ": " + tool + "\n"
	}
	if err := os.WriteFile(filepath.Join(root, "runtime.yaml"), []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
	collectionPath := filepath.Join(
		root,
		"collections",
		"ansible_collections",
		"ansible",
		"posix",
		"MANIFEST.json",
	)
	if err := os.MkdirAll(
		filepath.Dir(collectionPath),
		0o755,
	); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		collectionPath,
		[]byte("{}\n"),
		0o644,
	); err != nil {
		t.Fatal(err)
	}

	return root
}

func writeRemoteDeliveryMarker(t *testing.T, releaseRoot string, identity PreparationIdentity) {
	t.Helper()
	runtimeDigest, err := regularFileSHA256(filepath.Join(releaseRoot, "remote-runtime", "manifest.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	payloadDigest, err := regularFileSHA256(filepath.Join(releaseRoot, targetPayloadDirectory, "manifest.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	contents := fmt.Sprintf("schema_version: v1\nrelease_version: %s\nrelease_yaml_sha256: %s\nchecksums_sha256: %s\npayload_manifest_sha256: %s\nruntime_manifest_sha256: %s\nregistry: %s\nproject: %s\n", identity.ReleaseVersion, identity.ReleaseYAMLSHA256, identity.ChecksumsSHA256, payloadDigest, runtimeDigest, identity.RepositoryRegistry, identity.RepositoryProject)
	if err := os.WriteFile(filepath.Join(releaseRoot, ".eva-remote-release"), []byte(contents), 0o640); err != nil {
		t.Fatal(err)
	}
}
func preparationFingerprint(t *testing.T, root string) string {
	t.Helper()
	values := []string{}
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		contents, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		relative, _ := filepath.Rel(root, path)
		values = append(values, fmt.Sprintf("%s:%o:%d:%x", relative, info.Mode(), info.ModTime().UnixNano(), sha256.Sum256(contents)))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(values)
	return strings.Join(values, "\n")
}

// Regression: target verify, publish, and a repeated prepare each rehashed the
// tens-of-gigabytes delivery archives that prepare had already verified. They
// must rely on the recorded evidence; explicit `eva remote verify` still
// rehashes, and structural damage is still rejected.
func TestCompletedPreparationReliesOnRecordedArtifactEvidence(t *testing.T) {
	root, resolved, identity := writeCompletedPreparation(t)
	manifest, err := NewManifestStore(filepath.Dir(root), nil).Load(identity.ReleaseVersion)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := InspectTargetPayload(TargetPayloadPath(root, identity), identity)
	if err != nil {
		t.Fatal(err)
	}
	runtimeArtifact, err := InspectRuntimeArtifact(RuntimeArtifactPath(root, identity), identity)
	if err != nil {
		t.Fatal(err)
	}
	// Same-size bit flips: only a content rehash can notice them.
	for _, archive := range []string{filepath.Join(payload.Directory, payload.Manifest.Archive), filepath.Join(runtimeArtifact.Directory, runtimeArtifact.Manifest.Runtime.Archive)} {
		contents, err := os.ReadFile(archive)
		if err != nil {
			t.Fatal(err)
		}
		contents[len(contents)/2] ^= 0xff
		if err := os.WriteFile(archive, contents, 0o640); err != nil {
			t.Fatal(err)
		}
	}
	cacheRoot := filepath.Join(root, "cache")
	if err := ValidateCompletedPreparation(root, cacheRoot, resolved, identity, manifest); err != nil {
		t.Fatalf("completed preparation rehashed delivery archives: %v", err)
	}
	if _, err := InspectTargetPayload(payload.Directory, identity); err != nil {
		t.Fatalf("InspectTargetPayload read the archive: %v", err)
	}
	if _, err := LoadTargetPayload(payload.Directory, identity); err == nil {
		t.Fatal("LoadTargetPayload accepted a corrupted archive")
	}
	if _, err := LoadRuntimeArtifact(runtimeArtifact.Directory, identity); err == nil {
		t.Fatal("LoadRuntimeArtifact accepted a corrupted archive")
	}
	if err := VerifyCompletedPreparation(root, cacheRoot, resolved, identity, manifest); err == nil {
		t.Fatal("VerifyCompletedPreparation accepted a corrupted archive")
	}
	if _, err := (VerifyService{PreparationRoot: filepath.Dir(root), CacheRoot: cacheRoot}).Verify(VerifyOptions{Release: resolved, Registry: identity.RepositoryRegistry}); err == nil {
		t.Fatal("eva remote verify no longer rehashes delivery archives")
	}
}

func TestCompletedPreparationStillRejectsDamagedArtifactEvidence(t *testing.T) {
	for _, testCase := range []struct {
		name   string
		mutate func(t *testing.T, payload PayloadSource)
	}{
		{"missing archive", func(t *testing.T, payload PayloadSource) {
			if err := os.Remove(filepath.Join(payload.Directory, payload.Manifest.Archive)); err != nil {
				t.Fatal(err)
			}
		}},
		{"empty archive", func(t *testing.T, payload PayloadSource) {
			if err := os.WriteFile(filepath.Join(payload.Directory, payload.Manifest.Archive), nil, 0o640); err != nil {
				t.Fatal(err)
			}
		}},
		{"symlinked archive", func(t *testing.T, payload PayloadSource) {
			archive := filepath.Join(payload.Directory, payload.Manifest.Archive)
			moved := filepath.Join(t.TempDir(), "archive")
			if err := os.Rename(archive, moved); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(moved, archive); err != nil {
				t.Fatal(err)
			}
		}},
		{"checksum manifest disagrees", func(t *testing.T, payload PayloadSource) {
			line := strings.Repeat("0", 64) + "  " + payload.Manifest.Archive + "\n"
			if err := os.WriteFile(filepath.Join(payload.Directory, "checksums.sha256"), []byte(line), 0o640); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			root, resolved, identity := writeCompletedPreparation(t)
			manifest, err := NewManifestStore(filepath.Dir(root), nil).Load(identity.ReleaseVersion)
			if err != nil {
				t.Fatal(err)
			}
			payload, err := InspectTargetPayload(TargetPayloadPath(root, identity), identity)
			if err != nil {
				t.Fatal(err)
			}
			testCase.mutate(t, payload)
			if err := ValidateCompletedPreparation(root, filepath.Join(root, "cache"), resolved, identity, manifest); err == nil {
				t.Fatal("damaged payload evidence was accepted")
			}
		})
	}
}
