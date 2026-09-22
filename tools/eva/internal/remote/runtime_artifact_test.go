package remote

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRuntimeArtifactBuildLoadAndBootstrapTarget(t *testing.T) {
	resolved := writeOriginalRelease(t, false)
	if err := os.WriteFile(filepath.Join(resolved.Root, "checksums.sha256"), []byte("release checksums\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	identity, err := BuildPreparationIdentity(resolved, "harbor.example.internal:32080", "eva")
	if err != nil {
		t.Fatal(err)
	}
	preparationRoot := t.TempDir()
	runtimeRoot := writeRuntimeFixture(t)
	artifact, err := BuildRuntimeArtifact(preparationRoot, identity, runtimeRoot)
	if err != nil {
		t.Fatalf("BuildRuntimeArtifact() error = %v", err)
	}
	first, err := os.ReadFile(filepath.Join(artifact.Directory, artifact.Manifest.Runtime.Archive))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := LoadRuntimeArtifact(artifact.Directory, identity); err != nil {
		t.Fatalf("LoadRuntimeArtifact() error = %v", err)
	}
	if _, err := BuildRuntimeArtifact(preparationRoot, identity, writeRuntimeFixture(t)); err != nil {
		t.Fatalf("BuildRuntimeArtifact() reuse error = %v", err)
	}
	second, err := os.ReadFile(filepath.Join(artifact.Directory, artifact.Manifest.Runtime.Archive))
	if err != nil || !bytes.Equal(first, second) {
		t.Fatalf("Runtime artifact changed on reuse: %v", err)
	}
	again, err := BuildRuntimeArtifact(t.TempDir(), identity, runtimeRoot)
	if err != nil {
		t.Fatal(err)
	}
	againBytes, err := os.ReadFile(filepath.Join(again.Directory, again.Manifest.Runtime.Archive))
	if err != nil || !bytes.Equal(first, againBytes) {
		t.Fatalf("Runtime artifact is not deterministic: %v", err)
	}

	preparationRoot, targetRelease, targetIdentity := writeCompletedPreparation(t)
	if err := copyPayloadDirectory(RuntimeArtifactPath(preparationRoot, targetIdentity), filepath.Join(targetRelease.Root, "remote-runtime")); err != nil {
		t.Fatal(err)
	}
	if err := copyPayloadDirectory(TargetPayloadPath(preparationRoot, targetIdentity), filepath.Join(targetRelease.Root, targetPayloadDirectory)); err != nil {
		t.Fatal(err)
	}
	writeRemoteDeliveryMarker(t, targetRelease.Root, targetIdentity)
	installed, err := BootstrapTargetRuntime(targetRelease, targetIdentity.RepositoryRegistry, targetIdentity.RepositoryProject, filepath.Join(t.TempDir(), "runtime"))
	if err != nil {
		t.Fatalf("BootstrapTargetRuntime() error = %v", err)
	}
	if installed.Descriptor.Version != artifact.Manifest.Runtime.Version {
		t.Fatalf("installed version = %s", installed.Descriptor.Version)
	}
}

func TestRuntimeArtifactFailsClosedForUnsafeSourceAndIdentity(t *testing.T) {
	resolved := writeOriginalRelease(t, false)
	if err := os.WriteFile(filepath.Join(resolved.Root, "checksums.sha256"), []byte("release checksums\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	identity, err := BuildPreparationIdentity(resolved, "harbor.example.internal:32080", "eva")
	if err != nil {
		t.Fatal(err)
	}
	for _, testCase := range []struct {
		name   string
		mutate func(string) error
	}{
		{"symlink", func(root string) error { return os.Symlink("helm", filepath.Join(root, "bin", "link")) }},
		{"unexpected", func(root string) error {
			return os.WriteFile(filepath.Join(root, "workspace.yaml"), []byte("forbidden"), 0o600)
		}},
		{"secret", func(root string) error {
			return os.WriteFile(filepath.Join(root, "bin", "token"), []byte("forbidden"), 0o600)
		}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			runtimeRoot := writeRuntimeFixture(t)
			if err := testCase.mutate(runtimeRoot); err != nil {
				t.Fatal(err)
			}
			if _, err := BuildRuntimeArtifact(t.TempDir(), identity, runtimeRoot); err == nil {
				t.Fatal("BuildRuntimeArtifact() accepted unsafe source")
			}
		})
	}
	artifact, err := BuildRuntimeArtifact(t.TempDir(), identity, writeRuntimeFixture(t))
	if err != nil {
		t.Fatal(err)
	}
	wrong := identity
	wrong.RepositoryProject = "other"
	if _, err := LoadRuntimeArtifact(artifact.Directory, wrong); err == nil {
		t.Fatal("LoadRuntimeArtifact() accepted identity mismatch")
	}
	if err := os.WriteFile(filepath.Join(artifact.Directory, artifact.Manifest.Runtime.Archive), []byte("corrupt"), 0o640); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadRuntimeArtifact(artifact.Directory, identity); err == nil {
		t.Fatal("LoadRuntimeArtifact() accepted corrupt archive")
	}
}
func TestRuntimeEntriesExcludeCollectionTests(
	t *testing.T,
) {
	runtimeRoot := writeRuntimeFixture(t)
	collectionRoot := filepath.Join(
		runtimeRoot,
		"collections",
		"ansible_collections",
		"ansible",
		"posix",
	)

	moduleRoot := filepath.Join(
		collectionRoot,
		"plugins",
		"modules",
	)
	if err := os.MkdirAll(moduleRoot, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(moduleRoot, "mount.py"),
		[]byte("module"),
		0o640,
	); err != nil {
		t.Fatal(err)
	}

	testRoot := filepath.Join(
		collectionRoot,
		"tests",
		"utils",
		"shippable",
	)
	if err := os.MkdirAll(testRoot, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(
		"/not-part-of-runtime",
		filepath.Join(testRoot, "aix.sh"),
	); err != nil {
		t.Fatal(err)
	}

	entries, err := runtimeEntries(runtimeRoot)
	if err != nil {
		t.Fatalf("runtimeEntries() error = %v", err)
	}

	included := map[string]bool{}
	for _, relative := range entries {
		included[filepath.ToSlash(relative)] = true
	}

	modulePath := "collections/ansible_collections/" +
		"ansible/posix/plugins/modules/mount.py"
	if !included[modulePath] {
		t.Fatalf(
			"Runtime collection module was excluded: %s",
			modulePath,
		)
	}

	testsPrefix := "collections/ansible_collections/" +
		"ansible/posix/tests"
	for relative := range included {
		if strings.HasPrefix(relative, testsPrefix) {
			t.Fatalf(
				"Runtime collection test was included: %s",
				relative,
			)
		}
	}

	if err := os.Symlink(
		"/unsafe",
		filepath.Join(moduleRoot, "unsafe.py"),
	); err != nil {
		t.Fatal(err)
	}
	if _, err := runtimeEntries(runtimeRoot); err == nil {
		t.Fatal(
			"runtimeEntries() accepted an unsafe " +
				"non-test collection source",
		)
	}
}

func TestRuntimeExcludedPathMatchesCollectionTestsOnly(
	t *testing.T,
) {
	excluded := []string{
		"collections/ansible_collections/ansible/posix/tests",
		"collections/ansible_collections/ansible/posix/" +
			"tests/utils/shippable/aix.sh",
	}
	for _, relative := range excluded {
		if !runtimeExcludedPath(relative) {
			t.Fatalf(
				"Runtime test path was not excluded: %s",
				relative,
			)
		}
	}

	included := []string{
		"tests",
		"collections/tests",
		"collections/ansible_collections/tests",
		"collections/ansible_collections/ansible/tests",
		"collections/ansible_collections/ansible/posix",
		"collections/ansible_collections/ansible/posix/" +
			"tests-backup/aix.sh",
		"collections/ansible_collections/ansible/posix/" +
			"plugins/tests.py",
	}
	for _, relative := range included {
		if runtimeExcludedPath(relative) {
			t.Fatalf(
				"Runtime non-test path was excluded: %s",
				relative,
			)
		}
	}
}

func TestRuntimeAllowedPathIncludesCollections(
	t *testing.T,
) {
	allowed := []string{
		"collections",
		"collections/ansible_collections",
		"collections/ansible_collections/ansible/posix/MANIFEST.json",
	}

	for _, relative := range allowed {
		if !runtimeAllowedPath(relative) {
			t.Fatalf(
				"Runtime path was rejected: %s",
				relative,
			)
		}
	}

	rejected := []string{
		"collection",
		"collections-backup",
		"workspace",
		"workspace.yaml",
	}

	for _, relative := range rejected {
		if runtimeAllowedPath(relative) {
			t.Fatalf(
				"unexpected Runtime path was accepted: %s",
				relative,
			)
		}
	}
}
