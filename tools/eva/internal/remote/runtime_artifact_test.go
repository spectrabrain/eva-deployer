package remote

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"io"
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
func TestRuntimeArtifactMaterializesVenvPythonLinks(
	t *testing.T,
) {
	resolved := writeOriginalRelease(t, false)
	if err := os.WriteFile(
		filepath.Join(
			resolved.Root,
			"checksums.sha256",
		),
		[]byte("release checksums\n"),
		0o600,
	); err != nil {
		t.Fatal(err)
	}

	identity, err := BuildPreparationIdentity(
		resolved,
		"harbor.example.internal:32080",
		"eva",
	)
	if err != nil {
		t.Fatal(err)
	}

	runtimeRoot := writeRuntimeFixture(t)
	pythonTarget, err := filepath.EvalSymlinks(
		"/usr/bin/python3",
	)
	if err != nil {
		t.Skipf(
			"system Python is unavailable: %v",
			err,
		)
	}
	pythonInfo, err := os.Stat(pythonTarget)
	if err != nil {
		t.Fatal(err)
	}
	if !pythonInfo.Mode().IsRegular() ||
		pythonInfo.Mode().Perm()&0o111 == 0 {
		t.Skip(
			"system Python is not an executable regular file",
		)
	}

	venvBin := filepath.Join(runtimeRoot, "venv", "bin")
	for _, name := range []string{
		"python",
		"python3",
		"python3.12",
	} {
		path := filepath.Join(venvBin, name)
		if err := os.Remove(path); err != nil &&
			!errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
	}

	if err := os.Symlink(
		"python3",
		filepath.Join(venvBin, "python"),
	); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(
		"/usr/bin/python3",
		filepath.Join(venvBin, "python3"),
	); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(
		"python3",
		filepath.Join(venvBin, "python3.12"),
	); err != nil {
		t.Fatal(err)
	}

	artifact, err := BuildRuntimeArtifact(
		t.TempDir(),
		identity,
		runtimeRoot,
	)
	if err != nil {
		t.Fatalf(
			"BuildRuntimeArtifact() error = %v",
			err,
		)
	}

	archivePath := filepath.Join(
		artifact.Directory,
		artifact.Manifest.Runtime.Archive,
	)
	file, err := os.Open(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()

	gzipReader, err := gzip.NewReader(file)
	if err != nil {
		t.Fatal(err)
	}
	defer gzipReader.Close()

	archive := tar.NewReader(gzipReader)
	launchers := map[string]bool{
		"runtime/venv/bin/python":     false,
		"runtime/venv/bin/python3":    false,
		"runtime/venv/bin/python3.12": false,
	}

	for {
		header, err := archive.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if _, exists := launchers[header.Name]; !exists {
			continue
		}
		if header.Typeflag != tar.TypeReg {
			t.Fatalf(
				"Python launcher entry type = %d",
				header.Typeflag,
			)
		}
		if os.FileMode(header.Mode).Perm()&0o111 == 0 {
			t.Fatalf(
				"Python launcher is not executable: %s",
				header.Name,
			)
		}
		if header.Size != pythonInfo.Size() {
			t.Fatalf(
				"Python launcher size = %d, want %d",
				header.Size,
				pythonInfo.Size(),
			)
		}
		launchers[header.Name] = true
	}

	for name, found := range launchers {
		if !found {
			t.Fatalf(
				"Python launcher is missing: %s",
				name,
			)
		}
	}

	if _, err := LoadRuntimeArtifact(
		artifact.Directory,
		identity,
	); err != nil {
		t.Fatalf(
			"LoadRuntimeArtifact() error = %v",
			err,
		)
	}
}

func TestRuntimeArtifactRejectsUnsafePythonLinks(
	t *testing.T,
) {
	resolved := writeOriginalRelease(t, false)
	if err := os.WriteFile(
		filepath.Join(
			resolved.Root,
			"checksums.sha256",
		),
		[]byte("release checksums\n"),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	identity, err := BuildPreparationIdentity(
		resolved,
		"harbor.example.internal:32080",
		"eva",
	)
	if err != nil {
		t.Fatal(err)
	}

	for _, testCase := range []struct {
		name   string
		target string
	}{
		{
			name:   "outside-approved-system-path",
			target: "/bin/sh",
		},
		{
			name:   "dangling",
			target: "/usr/bin/python3.999999",
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			runtimeRoot := writeRuntimeFixture(t)
			launcher := filepath.Join(
				runtimeRoot,
				"venv",
				"bin",
				"python",
			)
			if err := os.Remove(launcher); err != nil &&
				!errors.Is(err, os.ErrNotExist) {
				t.Fatal(err)
			}
			if err := os.Symlink(
				testCase.target,
				launcher,
			); err != nil {
				t.Fatal(err)
			}

			if _, err := BuildRuntimeArtifact(
				t.TempDir(),
				identity,
				runtimeRoot,
			); err == nil {
				t.Fatal(
					"BuildRuntimeArtifact() accepted " +
						"an unsafe Python launcher",
				)
			}
		})
	}

	t.Run("cycle", func(t *testing.T) {
		runtimeRoot := writeRuntimeFixture(t)
		venvBin := filepath.Join(
			runtimeRoot,
			"venv",
			"bin",
		)
		for _, name := range []string{
			"python",
			"python3",
		} {
			if err := os.Remove(
				filepath.Join(venvBin, name),
			); err != nil &&
				!errors.Is(err, os.ErrNotExist) {
				t.Fatal(err)
			}
		}
		if err := os.Symlink(
			"python3",
			filepath.Join(venvBin, "python"),
		); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(
			"python",
			filepath.Join(venvBin, "python3"),
		); err != nil {
			t.Fatal(err)
		}

		if _, err := BuildRuntimeArtifact(
			t.TempDir(),
			identity,
			runtimeRoot,
		); err == nil {
			t.Fatal(
				"BuildRuntimeArtifact() accepted " +
					"a Python launcher cycle",
			)
		}
	})
}

func TestRuntimePythonLauncherPathIsNarrow(
	t *testing.T,
) {
	allowed := []string{
		"venv/bin/python",
		"venv/bin/python3",
		"venv/bin/python3.12",
	}
	for _, relative := range allowed {
		if !runtimePythonLauncherPath(relative) {
			t.Fatalf(
				"Python launcher was rejected: %s",
				relative,
			)
		}
	}

	rejected := []string{
		"bin/python",
		"venv/python",
		"venv/bin/pip",
		"venv/bin/python2",
		"venv/bin/python3.",
		"venv/bin/python3.12rc1",
		"venv/bin/python3.12/child",
	}
	for _, relative := range rejected {
		if runtimePythonLauncherPath(relative) {
			t.Fatalf(
				"Unexpected Python launcher accepted: %s",
				relative,
			)
		}
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

func TestRuntimeExcludedPathExcludesPythonCaches(
	t *testing.T,
) {
	excluded := []string{
		"venv/lib/python3.12/site-packages/ansible/" +
			"galaxy/__pycache__",
		"venv/lib/python3.12/site-packages/ansible/" +
			"galaxy/__pycache__/token.cpython-312.pyc",
		"venv/lib/python3.12/site-packages/module.pyc",
		"venv/lib/python3.12/site-packages/module.pyo",
	}

	for _, relative := range excluded {
		if !runtimeExcludedPath(relative) {
			t.Fatalf(
				"Python cache path was not excluded: %s",
				relative,
			)
		}
	}

	included := []string{
		"venv/lib/python3.12/site-packages/token.py",
		"venv/lib/python3.12/site-packages/" +
			"pycache-helper.py",
		"venv/lib/python3.12/site-packages/module.py",
		"venv/lib/python3.12/site-packages/" +
			"module.pyc.backup",
		"venv/lib/python3.12/site-packages/" +
			"not__pycache__/module.py",
	}

	for _, relative := range included {
		if runtimeExcludedPath(relative) {
			t.Fatalf(
				"Python source path was excluded: %s",
				relative,
			)
		}
	}
}

func TestRuntimeEntriesExcludePythonCaches(
	t *testing.T,
) {
	runtimeRoot := writeRuntimeFixture(t)

	cacheRoot := filepath.Join(
		runtimeRoot,
		"venv",
		"lib",
		"python3.12",
		"site-packages",
		"ansible",
		"galaxy",
		"__pycache__",
	)
	if err := os.MkdirAll(cacheRoot, 0o750); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(
		filepath.Join(
			cacheRoot,
			"token.cpython-312.pyc",
		),
		[]byte("compiled bytecode"),
		0o640,
	); err != nil {
		t.Fatal(err)
	}

	legacyCache := filepath.Join(
		runtimeRoot,
		"venv",
		"lib",
		"python3.12",
		"site-packages",
		"legacy.pyo",
	)
	if err := os.WriteFile(
		legacyCache,
		[]byte("optimized bytecode"),
		0o640,
	); err != nil {
		t.Fatal(err)
	}

	entries, err := runtimeEntries(runtimeRoot)
	if err != nil {
		t.Fatalf(
			"runtimeEntries() error = %v",
			err,
		)
	}

	for _, relative := range entries {
		relative = filepath.ToSlash(relative)

		parts := strings.Split(relative, "/")
		for _, part := range parts {
			if part == "__pycache__" {
				t.Fatalf(
					"Python cache directory was included: %s",
					relative,
				)
			}
		}

		if strings.HasSuffix(relative, ".pyc") ||
			strings.HasSuffix(relative, ".pyo") {
			t.Fatalf(
				"Python cache file was included: %s",
				relative,
			)
		}
	}

}

func TestRuntimeExcludedPathExcludesVenvLib64Alias(
	t *testing.T,
) {
	if !runtimeExcludedPath("venv/lib64") {
		t.Fatal("venv/lib64 alias was not excluded")
	}

	included := []string{
		"venv/lib",
		"venv/lib/python3.12",
		"venv/lib64-backup",
		"venv/bin/lib64",
	}

	for _, relative := range included {
		if runtimeExcludedPath(relative) {
			t.Fatalf(
				"Runtime path was unexpectedly excluded: %s",
				relative,
			)
		}
	}
}

func TestRuntimeEntriesExcludeVenvLib64Alias(
	t *testing.T,
) {
	runtimeRoot := writeRuntimeFixture(t)

	libDirectory := filepath.Join(
		runtimeRoot,
		"venv",
		"lib",
	)
	if err := os.MkdirAll(libDirectory, 0o750); err != nil {
		t.Fatal(err)
	}

	modulePath := filepath.Join(
		libDirectory,
		"runtime-module.py",
	)
	if err := os.WriteFile(
		modulePath,
		[]byte("runtime module"),
		0o640,
	); err != nil {
		t.Fatal(err)
	}

	lib64Path := filepath.Join(
		runtimeRoot,
		"venv",
		"lib64",
	)
	if err := os.Remove(lib64Path); err != nil &&
		!errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	if err := os.Symlink("lib", lib64Path); err != nil {
		t.Fatal(err)
	}

	entries, err := runtimeEntries(runtimeRoot)
	if err != nil {
		t.Fatalf(
			"runtimeEntries() error = %v",
			err,
		)
	}

	included := map[string]bool{}
	for _, relative := range entries {
		included[filepath.ToSlash(relative)] = true
	}

	if included["venv/lib64"] {
		t.Fatal("venv/lib64 alias was included")
	}
	if !included["venv/lib/runtime-module.py"] {
		t.Fatal("venv/lib content was excluded")
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
