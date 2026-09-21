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
	result, err := (VerifyService{PreparationRoot: filepath.Dir(root)}).Verify(VerifyOptions{Release: resolved, Registry: identity.RepositoryRegistry})
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
	resolved := writeOriginalRelease(t, true)
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
	if err := writeYAMLReport(root, "reports/preparation-summary.yaml", map[string]any{"release_version": identity.ReleaseVersion, "repository": identity.RepositoryRegistry + "/" + identity.RepositoryProject, "assets": []string{"offline", "product-images", "infra-images", "models", "qdrant-snapshots"}}); err != nil {
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
	manifest.Status, manifest.CompletedAt, manifest.UpdatedAt = ManifestSucceeded, clock(), clock()
	store := NewManifestStore(filepath.Dir(root), clock)
	if err := store.Save(manifest); err != nil {
		t.Fatal(err)
	}
	return root, resolved, identity
}

func stepEvidence(name string) string {
	values := map[string]string{"validate-release": "reports/release-validation.yaml", "prepare-offline-assets": "cache/manifest.txt", "download-product-images": "cache/images/images-all.txt", "download-infra-images": "cache/images/infra-images-all.txt", "download-models": "cache/models/manifest.txt", "download-qdrant-snapshots": "cache/qdrant-snapshots/manifest.txt", "publish-product-images": "reports/repository-mapping-product.txt", "publish-infra-images": "reports/repository-mapping-infra.txt", "publish-qdrant-snapshots": "reports/qdrant-artifacts.txt", "write-manifest": "reports/preparation-summary.yaml", "verify": "reports/verification.yaml"}
	return values[name]
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
