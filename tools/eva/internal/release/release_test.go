package release

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"sync"
	"syscall"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestResolveValidDirectory(t *testing.T) {
	root := writeRelease(t, map[string]string{
		"eva-tool.tar.gz":     "tool",
		"eva-infra.tar.gz":    "infra",
		"eva-solution.tar.gz": "solution",
	})

	resolved, err := Resolve(root)
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if resolved.Metadata.Version != "3.2.0" || resolved.Root != root {
		t.Fatalf("unexpected release: %#v", resolved)
	}
}

func TestResolveRejectsChecksumMismatch(t *testing.T) {
	root := writeRelease(t, map[string]string{
		"eva-tool.tar.gz":     "tool",
		"eva-infra.tar.gz":    "infra",
		"eva-solution.tar.gz": "solution",
	})
	if err := os.WriteFile(filepath.Join(root, "eva-tool.tar.gz"), []byte("changed"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Resolve(root); err == nil {
		t.Fatal("Resolve() accepted an artifact checksum mismatch")
	}
}

func TestResolveRejectsTagUntilS3ResolverExists(t *testing.T) {
	if _, err := Resolve("3.2.0"); err == nil {
		t.Fatal("Resolve() accepted a tag without an S3 resolver")
	}
}

func TestPrepareExtractsAndReusesImmutableRelease(t *testing.T) {
	root := writeRelease(t, map[string]string{
		"eva-tool.tar.gz": "tool",
		"eva-infra.tar.gz": gzipTar(t, map[string]string{
			"ansible.cfg":                                "[defaults]\n",
			"src/playbook-preflight.yaml":                "---\n",
			"src/playbook-vars.yaml":                     "---\n",
			"src/infra/playbooks/site_precondition.yaml": "---\n",
			"src/infra/playbooks/site_infra.yaml":        "---\n",
		}),
		"eva-solution.tar.gz": gzipTar(t, map[string]string{
			"src/solution/playbooks/site_eva_app.yaml": "---\n",
		}),
	})
	resolved, err := Resolve(root)
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := Prepare(resolved, filepath.Join(t.TempDir(), "releases"))
	if err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	for _, path := range []string{"ansible.cfg", "src/playbook-preflight.yaml", "src/infra/playbooks/site_precondition.yaml", "src/infra/playbooks/site_infra.yaml", "src/solution/playbooks/site_eva_app.yaml", preparedMarkerName} {
		if _, err := os.Stat(filepath.Join(prepared.Root, path)); err != nil {
			t.Fatalf("prepared Release is missing %s: %v", path, err)
		}
	}
	preparedResolved, err := Resolve(prepared.Root)
	if err != nil || !preparedResolved.Prepared {
		t.Fatalf("Resolve(prepared) = %#v, %v", preparedResolved, err)
	}
	reused, err := Prepare(resolved, filepath.Dir(prepared.Root))
	if err != nil {
		t.Fatal(err)
	}
	if reused.Root != prepared.Root {
		t.Fatalf("reused root = %q, want %q", reused.Root, prepared.Root)
	}
}

func TestPrepareRejectsArchivePathTraversal(t *testing.T) {
	root := writeRelease(t, map[string]string{
		"eva-tool.tar.gz":     "tool",
		"eva-infra.tar.gz":    gzipTar(t, map[string]string{"../escape": "no"}),
		"eva-solution.tar.gz": gzipTar(t, map[string]string{"src/solution/placeholder": "ok"}),
	})
	resolved, err := Resolve(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Prepare(resolved, t.TempDir()); err == nil {
		t.Fatal("Prepare() accepted path traversal archive")
	}
}

func TestImportAirgapBundleValidatesNestedArtifacts(t *testing.T) {
	tool := "tool"
	infra := gzipTar(t, map[string]string{
		"ansible.cfg": "[defaults]\n", "src/playbook-preflight.yaml": "---\n", "src/playbook-vars.yaml": "---\n", "src/infra/playbooks/site_precondition.yaml": "---\n", "src/infra/playbooks/site_infra.yaml": "---\n",
	})
	solution := gzipTar(t, map[string]string{"src/solution/playbooks/site_eva_app.yaml": "---\n"})
	metadata := fmt.Sprintf(`version: 3.2.0
platform:
  os: %s
  arch: %s
artifacts:
  - name: eva-tool
    file: artifacts/eva-tool.tar.gz
    sha256: %s
  - name: eva-infra
    file: artifacts/eva-infra.tar.gz
    sha256: %s
  - name: eva-solution
    file: artifacts/eva-solution.tar.gz
    sha256: %s
  - name: eva-notes
    file: artifacts/README.md
    sha256: %s
`, runtime.GOOS, runtime.GOARCH, checksum(tool), checksum(infra), checksum(solution), checksum("release notes"))
	manifest := fmt.Sprintf("%s  artifacts/eva-tool.tar.gz\n%s  artifacts/eva-infra.tar.gz\n%s  artifacts/eva-solution.tar.gz\n%s  artifacts/README.md\n", checksum(tool), checksum(infra), checksum(solution), checksum("release notes"))
	bundle := gzipTar(t, map[string]string{
		"release.yaml":                  metadata,
		"checksums.sha256":              manifest,
		"README.md":                     "bundle\n",
		"artifacts/eva-tool.tar.gz":     tool,
		"artifacts/eva-infra.tar.gz":    infra,
		"artifacts/eva-solution.tar.gz": solution,
		"artifacts/README.md":           "release notes",
	})
	bundlePath := filepath.Join(t.TempDir(), "eva-airgap-bundle_v3.2.0_ubuntu24.04_amd64.tar.gz")
	if err := os.WriteFile(bundlePath, []byte(bundle), 0o600); err != nil {
		t.Fatal(err)
	}
	verified, err := VerifyAirgapBundle(bundlePath)
	if err != nil {
		t.Fatalf("VerifyAirgapBundle() error = %v", err)
	}
	if verified.Version != "3.2.0" {
		t.Fatalf("verified version = %q", verified.Version)
	}
	resolved, err := ImportAirgapBundle(bundlePath, filepath.Join(t.TempDir(), "artifacts"))
	if err != nil {
		t.Fatalf("ImportAirgapBundle() error = %v", err)
	}
	if resolved.Metadata.Version != "3.2.0" {
		t.Fatalf("imported version = %q", resolved.Metadata.Version)
	}
	if _, err := os.Stat(filepath.Join(resolved.Root, airgapMarkerName)); err != nil {
		t.Fatal(err)
	}
	prepared, err := Prepare(resolved, filepath.Join(t.TempDir(), "releases"))
	if err != nil {
		t.Fatalf("Prepare(imported) error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(prepared.Root, "src", "solution", "playbooks", "site_eva_app.yaml")); err != nil {
		t.Fatal(err)
	}
}

func TestImportAirgapBundleRejectsUnexpectedFiles(t *testing.T) {
	tool := "tool"
	infra := gzipTar(t, map[string]string{
		"ansible.cfg": "[defaults]\n", "src/playbook-preflight.yaml": "---\n", "src/playbook-vars.yaml": "---\n", "src/infra/playbooks/site_precondition.yaml": "---\n", "src/infra/playbooks/site_infra.yaml": "---\n",
	})
	solution := gzipTar(t, map[string]string{"src/solution/placeholder": "ok"})
	metadata := fmt.Sprintf(`version: 3.2.0
platform:
  os: %s
  arch: %s
artifacts:
  - name: eva-tool
    file: artifacts/eva-tool.tar.gz
    sha256: %s
  - name: eva-infra
    file: artifacts/eva-infra.tar.gz
    sha256: %s
  - name: eva-solution
    file: artifacts/eva-solution.tar.gz
    sha256: %s
`, runtime.GOOS, runtime.GOARCH, checksum(tool), checksum(infra), checksum(solution))
	manifest := fmt.Sprintf("%s  artifacts/eva-tool.tar.gz\n%s  artifacts/eva-infra.tar.gz\n%s  artifacts/eva-solution.tar.gz\n", checksum(tool), checksum(infra), checksum(solution))
	bundle := gzipTar(t, map[string]string{
		"release.yaml":                  metadata,
		"checksums.sha256":              manifest,
		"artifacts/eva-tool.tar.gz":     tool,
		"artifacts/eva-infra.tar.gz":    infra,
		"artifacts/eva-solution.tar.gz": solution,
		"credentials.txt":               "not allowed",
	})
	bundlePath := filepath.Join(t.TempDir(), "bundle.tar.gz")
	if err := os.WriteFile(bundlePath, []byte(bundle), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ImportAirgapBundle(bundlePath, t.TempDir()); err == nil || !strings.Contains(err.Error(), "unexpected file") {
		t.Fatalf("ImportAirgapBundle() error = %v, want unexpected file error", err)
	}
}

func TestBuildAirgapBundleFlatVerifyAndImport(t *testing.T) {
	source := writeAirgapSource(t)
	original := snapshotRelease(t, source.Root)
	offline := gzipTar(t, map[string]string{"runtime/runtime.yaml": "version: 1\n", "out/cache/package.deb": "runtime cache"})
	offlinePath := filepath.Join(t.TempDir(), "runtime.tar.gz")
	if err := os.WriteFile(offlinePath, []byte(offline), 0o600); err != nil {
		t.Fatal(err)
	}
	outputRoot := t.TempDir()
	destination := filepath.Join(outputRoot, "new-output", "bundle.tar.gz")
	if err := BuildAirgapBundle(source, offlinePath, destination); err != nil {
		t.Fatalf("BuildAirgapBundle() error = %v", err)
	}
	metadata, err := VerifyAirgapBundle(destination)
	if err != nil {
		t.Fatalf("VerifyAirgapBundle() error = %v", err)
	}
	if metadata.Version != source.Metadata.Version || metadata.Platform != source.Metadata.Platform {
		t.Fatalf("bundle changed version/platform: %#v", metadata)
	}
	if len(metadata.Artifacts) != len(source.Metadata.Artifacts) {
		t.Fatalf("artifacts = %d, want %d (replace old offline, preserve extras)", len(metadata.Artifacts), len(source.Metadata.Artifacts))
	}
	resolved, err := ImportAirgapBundle(destination, filepath.Join(t.TempDir(), "cache"))
	if err != nil {
		t.Fatalf("ImportAirgapBundle() error = %v", err)
	}
	manifest, err := readChecksumManifest(filepath.Join(resolved.Root, "checksums.sha256"))
	if err != nil {
		t.Fatal(err)
	}
	if len(manifest) != len(metadata.Artifacts)+2 {
		t.Fatalf("manifest entries = %d, want artifacts + release.yaml + README.md", len(manifest))
	}
	if _, ok := manifest["release.yaml"]; !ok {
		t.Fatal("installer requires release.yaml digest")
	}
	for name, digest := range manifest {
		if strings.Contains(name, "/") {
			t.Fatalf("checksum is not flat: %q", name)
		}
		if err := verifyChecksum(filepath.Join(resolved.Root, name), digest); err != nil {
			t.Fatalf("installer checksum verification: %v", err)
		}
	}
	offlineCount := 0
	for _, artifact := range metadata.Artifacts {
		if filepath.Base(artifact.File) != artifact.File {
			t.Fatalf("artifact is not flat: %#v", artifact)
		}
		if artifact.Name == "eva-offline" {
			offlineCount++
			if artifact.SHA256 != checksum(offline) {
				t.Fatal("bundle did not replace the old offline artifact")
			}
			continue
		}
		originalPath, err := source.ArtifactPath(artifact.Name)
		if err != nil {
			t.Fatal(err)
		}
		digest, err := fileChecksum(originalPath)
		if err != nil || digest != artifact.SHA256 {
			t.Fatalf("artifact %s changed: %s, %v", artifact.Name, digest, err)
		}
	}
	if offlineCount != 1 {
		t.Fatalf("offline count = %d, want 1", offlineCount)
	}
	installer, err := resolved.ArtifactPath("eva-tool-installer")
	if err != nil {
		t.Fatal(err)
	}
	if installer != filepath.Join(resolved.Root, "eva-tool-installer.sh") {
		t.Fatalf("installer is not canonical: %s", installer)
	}
	info, err := os.Stat(installer)
	if err != nil || info.Mode().Perm() != 0o755 {
		t.Fatalf("installer must remain executable: %v, %v", info, err)
	}
	tool, err := resolved.ArtifactPath("eva-tool")
	if err != nil {
		t.Fatal(err)
	}
	matches, err := filepath.Glob(filepath.Join(resolved.Root, "eva-tool_*_"+runtime.GOOS+"_"+runtime.GOARCH+".tar.gz"))
	if err != nil || len(matches) != 1 || matches[0] != tool || filepath.Dir(tool) != filepath.Dir(installer) {
		t.Fatalf("installer cannot discover its adjacent Tool archive: %v, %v", matches, err)
	}
	if _, err := os.Stat(filepath.Join(resolved.Root, "workspace")); !os.IsNotExist(err) {
		t.Fatalf("customer workspace was included: %v", err)
	}
	if after := snapshotRelease(t, source.Root); !reflect.DeepEqual(original, after) {
		t.Fatal("BuildAirgapBundle changed the input Release")
	}
	assertOnlyBundle(t, filepath.Dir(destination), destination)
	// Verify actual tar order, flat names, entry types and executable mode,
	// rather than relying only on the importer's view of the archive.
	file, err := os.Open(destination)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	compressed, err := gzip.NewReader(file)
	if err != nil {
		t.Fatal(err)
	}
	defer compressed.Close()
	archive := tar.NewReader(compressed)
	var names []string
	for {
		header, err := archive.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if header.Typeflag != tar.TypeReg || header.Linkname != "" || filepath.Base(header.Name) != header.Name {
			t.Fatalf("non-flat regular entry: %#v", header)
		}
		if header.Name == "eva-tool-installer.sh" && header.Mode != 0o755 {
			t.Fatalf("archive installer mode = %o", header.Mode)
		}
		names = append(names, header.Name)
	}
	if !sort.StringsAreSorted(names) || len(names) != len(metadata.Artifacts)+3 {
		t.Fatalf("archive entries not sorted or have unexpected files: %v", names)
	}
}

func TestBuildAirgapBundleAddsOfflineAndReresolvesSource(t *testing.T) {
	source := writeAirgapSource(t)
	metadata := source.Metadata
	metadata.Artifacts = nil
	for _, artifact := range source.Metadata.Artifacts {
		if artifact.Name != "eva-offline" {
			metadata.Artifacts = append(metadata.Artifacts, artifact)
		}
	}
	metadata.Version = "v3.2.0+build.1"
	writeReleaseMetadata(t, source.Root, metadata)
	// Deliberately retain stale caller metadata and an unusable MetadataPath.
	source.MetadataPath = filepath.Join(t.TempDir(), "missing.yaml")
	destination := filepath.Join(t.TempDir(), "bundle.tar.gz")
	offline, _ := source.ArtifactPath("eva-offline")
	if err := BuildAirgapBundle(source, offline, destination); err != nil {
		t.Fatal(err)
	}
	verified, err := VerifyAirgapBundle(destination)
	if err != nil || verified.Version != metadata.Version {
		t.Fatalf("did not use verified on-disk metadata: %#v, %v", verified, err)
	}
	for _, artifact := range verified.Artifacts {
		if artifact.Name == "eva-offline" && !strings.Contains(artifact.File, metadata.Version) {
			t.Fatalf("offline filename does not use safe source version: %q", artifact.File)
		}
	}
}

func TestBuildAirgapBundleRejectsCorruptedOriginal(t *testing.T) {
	for _, name := range []string{"eva-tool", "eva-tool-installer", "eva-infra", "eva-solution", "eva-offline", "eva-manual"} {
		t.Run(name, func(t *testing.T) {
			source := writeAirgapSource(t)
			path, err := source.ArtifactPath(name)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte("corrupted after Resolve"), 0o600); err != nil {
				t.Fatal(err)
			}
			parent := t.TempDir()
			err = BuildAirgapBundle(source, path, filepath.Join(parent, "bundle.tar.gz"))
			if err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
				t.Fatalf("BuildAirgapBundle() error = %v, want checksum mismatch", err)
			}
			assertOnlyBundle(t, parent, "")
		})
	}
}

func TestBuildAirgapBundleRefusesExistingDestinationFirst(t *testing.T) {
	for _, kind := range []string{"regular", "symlink", "dangling symlink", "directory"} {
		t.Run(kind, func(t *testing.T) {
			parent := t.TempDir()
			destination := filepath.Join(parent, "bundle.tar.gz")
			target := filepath.Join(t.TempDir(), "target")
			if err := os.WriteFile(target, []byte("keep target"), 0o600); err != nil {
				t.Fatal(err)
			}
			var err error
			switch kind {
			case "regular":
				err = os.WriteFile(destination, []byte("keep output"), 0o600)
			case "symlink":
				err = os.Symlink(target, destination)
			case "dangling symlink":
				err = os.Symlink(target+"-missing", destination)
			case "directory":
				err = os.Mkdir(destination, 0o700)
			}
			if err != nil {
				t.Fatal(err)
			}
			before, err := os.Lstat(destination)
			if err != nil {
				t.Fatal(err)
			}
			// Invalid inputs prove refusal happens before expensive validation.
			err = BuildAirgapBundle(Resolved{Root: "missing-source"}, "missing-offline", destination)
			if err == nil || !strings.Contains(err.Error(), "already exists") {
				t.Fatalf("BuildAirgapBundle() error = %v, want existing destination", err)
			}
			after, err := os.Lstat(destination)
			if err != nil || !os.SameFile(before, after) {
				t.Fatalf("destination was replaced: %v", err)
			}
			if contents, err := os.ReadFile(target); err != nil || string(contents) != "keep target" {
				t.Fatalf("symlink target changed: %q, %v", contents, err)
			}
			if kind == "regular" {
				if contents, err := os.ReadFile(destination); err != nil || string(contents) != "keep output" {
					t.Fatalf("existing output changed: %q, %v", contents, err)
				}
			}
			assertOnlyBundle(t, parent, destination)
		})
	}
}

func TestBuildAirgapBundleConcurrentPublicationDoesNotClobber(t *testing.T) {
	source := writeAirgapSource(t)
	offline, err := source.ArtifactPath("eva-offline")
	if err != nil {
		t.Fatal(err)
	}
	parent := t.TempDir()
	destination := filepath.Join(parent, "bundle.tar.gz")
	start := make(chan struct{})
	results := make(chan error, 8)
	var writers sync.WaitGroup
	for i := 0; i < cap(results); i++ {
		writers.Add(1)
		go func() {
			defer writers.Done()
			<-start
			results <- BuildAirgapBundle(source, offline, destination)
		}()
	}
	close(start)
	writers.Wait()
	close(results)
	successes := 0
	for err := range results {
		if err == nil {
			successes++
		} else if !strings.Contains(err.Error(), "already exists") && !strings.Contains(err.Error(), "file exists") {
			t.Errorf("unexpected concurrent build error: %v", err)
		}
	}
	if successes != 1 {
		t.Fatalf("successful publishers = %d, want exactly 1", successes)
	}
	if _, err := VerifyAirgapBundle(destination); err != nil {
		t.Fatalf("published bundle is invalid: %v", err)
	}
	assertOnlyBundle(t, parent, destination)
}

func TestBuildAirgapBundleRejectsUnsafeInputs(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*testing.T, *Resolved, *string)
	}{
		{"prepared value", func(t *testing.T, source *Resolved, offline *string) { source.Prepared = true }},
		{"prepared root", func(t *testing.T, source *Resolved, offline *string) {
			if err := os.WriteFile(filepath.Join(source.Root, preparedMarkerName), []byte("prepared"), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{"imported root", func(t *testing.T, source *Resolved, offline *string) {
			if err := os.WriteFile(filepath.Join(source.Root, airgapMarkerName), []byte("imported"), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{"unsafe version", func(t *testing.T, source *Resolved, offline *string) {
			metadata := source.Metadata
			metadata.Version = "../../escape"
			writeReleaseMetadata(t, source.Root, metadata)
		}},
		{"missing installer", func(t *testing.T, source *Resolved, offline *string) {
			metadata := source.Metadata
			metadata.Artifacts = nil
			for _, artifact := range source.Metadata.Artifacts {
				if artifact.Name != "eva-tool-installer" {
					metadata.Artifacts = append(metadata.Artifacts, artifact)
				}
			}
			writeReleaseMetadata(t, source.Root, metadata)
		}},
		{"artifact symlink parent", func(t *testing.T, source *Resolved, offline *string) {
			path := filepath.Join(source.Root, "artifacts")
			target := filepath.Join(t.TempDir(), "artifacts")
			if err := os.Rename(path, target); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, path); err != nil {
				t.Fatal(err)
			}
		}},
		{"customer artifact", func(t *testing.T, source *Resolved, offline *string) {
			metadata := source.Metadata
			metadata.Artifacts = append(metadata.Artifacts, Artifact{Name: "customer", File: "workspace/credentials/customer.key", SHA256: checksum("customer secret")})
			writeReleaseMetadata(t, source.Root, metadata)
		}},
		{"duplicate flat filename", func(t *testing.T, source *Resolved, offline *string) {
			path := filepath.Join(source.Root, "extras")
			if err := os.Mkdir(path, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(path, "eva-manual.txt"), []byte("extra manual"), 0o600); err != nil {
				t.Fatal(err)
			}
			metadata := source.Metadata
			metadata.Artifacts = append(metadata.Artifacts, Artifact{Name: "duplicate", File: "extras/eva-manual.txt", SHA256: checksum("extra manual")})
			writeReleaseMetadata(t, source.Root, metadata)
		}},
		{"empty offline", func(t *testing.T, source *Resolved, offline *string) { *offline = "" }},
		{"missing offline", func(t *testing.T, source *Resolved, offline *string) { *offline += "-missing" }},
		{"offline directory", func(t *testing.T, source *Resolved, offline *string) { *offline = t.TempDir() }},
		{"offline fifo", func(t *testing.T, source *Resolved, offline *string) {
			*offline = filepath.Join(t.TempDir(), "fifo")
			if err := syscall.Mkfifo(*offline, 0o600); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, kind := range []string{"metadata", "artifact", "offline"} {
		for _, link := range []string{"symlink", "hardlink"} {
			tests = append(tests, struct {
				name   string
				mutate func(*testing.T, *Resolved, *string)
			}{kind + " " + link, func(t *testing.T, source *Resolved, offline *string) {
				path := *offline
				if kind == "metadata" {
					path = filepath.Join(source.Root, "release.yaml")
				} else if kind == "artifact" {
					var err error
					path, err = source.ArtifactPath("eva-tool")
					if err != nil {
						t.Fatal(err)
					}
				}
				target := filepath.Join(t.TempDir(), "original")
				if err := os.Rename(path, target); err != nil {
					t.Fatal(err)
				}
				linkFile := os.Symlink
				if link == "hardlink" {
					linkFile = os.Link
				}
				if err := linkFile(target, path); err != nil {
					t.Fatal(err)
				}
			}})
		}
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			source := writeAirgapSource(t)
			offline := filepath.Join(t.TempDir(), "runtime.tar.gz")
			if err := os.WriteFile(offline, []byte("built runtime cache"), 0o600); err != nil {
				t.Fatal(err)
			}
			test.mutate(t, &source, &offline)
			parent := t.TempDir()
			if err := BuildAirgapBundle(source, offline, filepath.Join(parent, "bundle.tar.gz")); err == nil {
				t.Fatal("BuildAirgapBundle accepted unsafe input")
			}
			assertOnlyBundle(t, parent, "")
		})
	}
}

func TestAirgapBundleLayoutRejectsUnsafeFlatEntries(t *testing.T) {
	for _, name := range []string{"extra.txt", "nested/extra.txt", "artifacts/nested/extra.txt", "../escape"} {
		t.Run(name, func(t *testing.T) {
			source := writeAirgapSource(t)
			offline, _ := source.ArtifactPath("eva-offline")
			destination := filepath.Join(t.TempDir(), "bundle.tar.gz")
			if err := BuildAirgapBundle(source, offline, destination); err != nil {
				t.Fatal(err)
			}
			resolved, err := ImportAirgapBundle(destination, t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			entries := make(map[string]string)
			for path := range snapshotRelease(t, resolved.Root) {
				if path == airgapMarkerName {
					continue
				}
				contents, err := os.ReadFile(filepath.Join(resolved.Root, path))
				if err != nil {
					t.Fatal(err)
				}
				entries[path] = string(contents)
			}
			entries[name] = "unexpected"
			badBundle := filepath.Join(t.TempDir(), "bad.tar.gz")
			if err := os.WriteFile(badBundle, []byte(gzipTar(t, entries)), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := VerifyAirgapBundle(badBundle); err == nil {
				t.Fatal("VerifyAirgapBundle accepted unexpected/traversal entry")
			}
		})
	}
}

func TestAirgapBundleLayoutRejectsUnsafeDeclaredPaths(t *testing.T) {
	for _, name := range []string{
		"nested/eva-tool.tar.gz", "artifacts/nested/eva-tool.tar.gz",
		"../eva-tool.tar.gz", "artifacts/../eva-tool.tar.gz", "artifacts/./eva-tool.tar.gz",
		"/eva-tool.tar.gz", "artifacts/", ".", "release.yaml", "README.md", "checksums.sha256",
	} {
		t.Run(name, func(t *testing.T) {
			source := writeAirgapSource(t)
			metadata := source.Metadata
			metadata.Artifacts[0].File = name
			manifest := fmt.Sprintf("%s  artifacts/eva-tool.tar.gz\n", metadata.Artifacts[0].SHA256)
			if err := os.WriteFile(filepath.Join(source.Root, "checksums.sha256"), []byte(manifest), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := validateBundleLayout(source.Root, metadata); err == nil || !strings.Contains(err.Error(), "must be a flat filename") {
				t.Fatalf("validateBundleLayout() error = %v, want unsafe artifact path", err)
			}
		})
	}
}

func TestAirgapBundleChecksAllManifestLines(t *testing.T) {
	source := writeAirgapSource(t)
	offline, _ := source.ArtifactPath("eva-offline")
	destination := filepath.Join(t.TempDir(), "bundle.tar.gz")
	if err := BuildAirgapBundle(source, offline, destination); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"release.yaml", "README.md", "missing.txt"} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			resolved, err := extractAndValidateAirgapBundle(destination, root)
			if err != nil {
				t.Fatal(err)
			}
			if name == "missing.txt" {
				manifest, err := os.OpenFile(filepath.Join(root, "checksums.sha256"), os.O_APPEND|os.O_WRONLY, 0)
				if err != nil {
					t.Fatal(err)
				}
				_, err = fmt.Fprintf(manifest, "%s  missing.txt\n", checksum("missing"))
				manifest.Close()
				if err != nil {
					t.Fatal(err)
				}
			} else {
				file, err := os.OpenFile(filepath.Join(root, name), os.O_APPEND|os.O_WRONLY, 0)
				if err != nil {
					t.Fatal(err)
				}
				_, err = file.WriteString("\n# tampered\n")
				file.Close()
				if err != nil {
					t.Fatal(err)
				}
			}
			if err := validateBundleLayout(root, resolved.Metadata); err == nil {
				t.Fatal("bundle validation ignored an invalid checksum manifest line")
			}
		})
	}
}

func writeAirgapSource(t *testing.T) Resolved {
	t.Helper()
	root := writeRelease(t, map[string]string{
		"eva-tool.tar.gz":     gzipTar(t, map[string]string{"bin/eva": "tool"}),
		"eva-infra.tar.gz":    gzipTar(t, map[string]string{"src/infra/placeholder": "infra"}),
		"eva-solution.tar.gz": gzipTar(t, map[string]string{"src/solution/placeholder": "solution"}),
	})
	source, err := Resolve(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, extra := range []struct{ name, file, contents string }{
		{"eva-tool-installer", "eva-tool-installer.sh", "#!/bin/sh\nexit 0\n"},
		{"eva-offline", "eva-offline-old.tar.gz", "old offline payload"},
		{"eva-manual", "eva-manual.txt", "release artifact, not customer data"},
	} {
		if err := os.WriteFile(filepath.Join(root, extra.file), []byte(extra.contents), 0o644); err != nil {
			t.Fatal(err)
		}
		source.Metadata.Artifacts = append(source.Metadata.Artifacts, Artifact{Name: extra.name, File: extra.file, SHA256: checksum(extra.contents)})
	}
	if err := os.Mkdir(filepath.Join(root, "artifacts"), 0o700); err != nil {
		t.Fatal(err)
	}
	for index := range source.Metadata.Artifacts {
		artifact := &source.Metadata.Artifacts[index]
		if err := os.Rename(filepath.Join(root, artifact.File), filepath.Join(root, "artifacts", artifact.File)); err != nil {
			t.Fatal(err)
		}
		artifact.File = "artifacts/" + artifact.File
	}
	writeReleaseMetadata(t, root, source.Metadata)
	if err := os.MkdirAll(filepath.Join(root, "workspace", "credentials"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "workspace", "credentials", "customer.key"), []byte("customer secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	source, err = Resolve(root)
	if err != nil {
		t.Fatal(err)
	}
	return source
}

func writeReleaseMetadata(t *testing.T, root string, metadata Metadata) {
	t.Helper()
	contents, err := yaml.Marshal(metadata)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "release.yaml"), contents, 0o600); err != nil {
		t.Fatal(err)
	}
}

func snapshotRelease(t *testing.T, root string) map[string]string {
	t.Helper()
	files := make(map[string]string)
	if err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		digest, err := fileChecksum(path)
		files[relative] = fmt.Sprintf("%s:%o", digest, info.Mode())
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return files
}

func assertOnlyBundle(t *testing.T, parent, destination string) {
	t.Helper()
	entries, err := os.ReadDir(parent)
	if err != nil {
		t.Fatal(err)
	}
	if destination == "" {
		if len(entries) != 0 {
			t.Fatalf("failed build left staging/output entries: %v", entries)
		}
	} else if len(entries) != 1 || entries[0].Name() != filepath.Base(destination) {
		t.Fatalf("build left temporary entries: %v", entries)
	}
}

func writeRelease(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	metadata := fmt.Sprintf(`version: 3.2.0
platform:
  os: %s
  arch: %s
artifacts:
  - name: eva-tool
    file: eva-tool.tar.gz
    sha256: %s
  - name: eva-infra
    file: eva-infra.tar.gz
    sha256: %s
  - name: eva-solution
    file: eva-solution.tar.gz
    sha256: %s
`, runtime.GOOS, runtime.GOARCH,
		checksum(files["eva-tool.tar.gz"]),
		checksum(files["eva-infra.tar.gz"]),
		checksum(files["eva-solution.tar.gz"]),
	)
	if err := os.WriteFile(filepath.Join(root, "release.yaml"), []byte(metadata), 0o600); err != nil {
		t.Fatal(err)
	}
	return root
}

func checksum(value string) string {
	hash := sha256.Sum256([]byte(value))
	return fmt.Sprintf("%x", hash)
}

func gzipTar(t *testing.T, entries map[string]string) string {
	t.Helper()
	var buffer bytes.Buffer
	writer := gzip.NewWriter(&buffer)
	archive := tar.NewWriter(writer)
	for name, contents := range entries {
		if err := archive.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(contents)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := archive.Write([]byte(contents)); err != nil {
			t.Fatal(err)
		}
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.String()
}
