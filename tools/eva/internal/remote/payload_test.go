package remote

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

func TestBuildTargetPayloadIsDeterministicAndExcludesHarborAssets(t *testing.T) {
	root, resolved, identity := writeCompletedPreparation(t)
	first := TargetPayloadPath(root, identity)
	firstArchive, err := os.ReadFile(filepath.Join(first, "remote-payload_"+identity.ReleaseVersion+"_"+payloadIdentityKey(identity)+".tar.gz"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(first); err != nil {
		t.Fatal(err)
	}
	payload, err := BuildTargetPayload(root, filepath.Join(root, "cache"), identity, resolved.Metadata.Platform.OS+"/"+resolved.Metadata.Platform.Arch)
	if err != nil {
		t.Fatalf("BuildTargetPayload() error = %v", err)
	}
	secondArchive, err := os.ReadFile(filepath.Join(payload.Directory, payload.Manifest.Archive))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(firstArchive, secondArchive) {
		t.Fatal("target payload archive is not deterministic")
	}
	entries := payloadArchiveEntries(t, filepath.Join(payload.Directory, payload.Manifest.Archive))
	if !sort.StringsAreSorted(entries) {
		t.Fatalf("archive entries are not stable: %v", entries)
	}
	for _, entry := range entries {
		if !strings.HasPrefix(entry, "cache/") || strings.HasPrefix(entry, "cache/images/") || strings.HasPrefix(entry, "cache/qdrant-snapshots/") || strings.HasPrefix(entry, "cache/aws/") {
			t.Fatalf("unexpected payload archive entry %q", entry)
		}
	}
	if _, err := LoadTargetPayload(payload.Directory, identity); err != nil {
		t.Fatalf("LoadTargetPayload() error = %v", err)
	}
}

func TestBuildTargetPayloadAllowsProductOwnedNames(
	t *testing.T,
) {
	root, resolved, identity := writeCompletedPreparation(t)
	mustRemove(t, TargetPayloadPath(root, identity))

	productFiles := map[string]string{
		"cache/eva-agent/release/3.2.0/" +
			"eva-agent/values-secret.yaml": "existingSecret: eva-agent-secret\n",
		"cache/eva-agent/release/3.2.0/" +
			"eva-agent/token-template.yaml": "tokenTemplate: placeholder\n",
		"cache/eva-agent/release/3.2.0/" +
			"eva-agent/private-values.yaml": "privateValues: placeholder\n",
		"cache/models/agent/hf/tokenizer.json": "{\"model\": \"tokenizer\"}\n",
	}

	for relative, contents := range productFiles {
		mustWrite(
			t,
			filepath.Join(root, relative),
			contents,
		)
	}

	payload, err := BuildTargetPayload(
		root,
		filepath.Join(root, "cache"),
		identity,
		resolved.Metadata.Platform.OS+
			"/"+
			resolved.Metadata.Platform.Arch,
	)
	if err != nil {
		t.Fatalf(
			"BuildTargetPayload() error = %v",
			err,
		)
	}

	entries := payloadArchiveEntries(
		t,
		filepath.Join(
			payload.Directory,
			payload.Manifest.Archive,
		),
	)

	included := map[string]bool{}
	for _, entry := range entries {
		included[entry] = true
	}

	for relative := range productFiles {
		if !included[filepath.ToSlash(relative)] {
			t.Fatalf(
				"product-owned file was excluded: %s",
				relative,
			)
		}
	}
}

func TestBuildTargetPayloadExcludesUnapprovedCacheRoots(
	t *testing.T,
) {
	root, resolved, identity := writeCompletedPreparation(t)
	mustRemove(t, TargetPayloadPath(root, identity))

	excluded := map[string]string{
		"cache/credentials/aws_key.ini": "not-a-real-key",
		"cache/aws/credentials":         "not-a-real-key",
		"cache/images/private.key":      "not-a-real-key",
		"cache/qdrant-snapshots/token":  "not-a-real-token",
	}

	for relative, contents := range excluded {
		mustWrite(
			t,
			filepath.Join(root, relative),
			contents,
		)
	}

	payload, err := BuildTargetPayload(
		root,
		filepath.Join(root, "cache"),
		identity,
		resolved.Metadata.Platform.OS+
			"/"+
			resolved.Metadata.Platform.Arch,
	)
	if err != nil {
		t.Fatalf(
			"BuildTargetPayload() error = %v",
			err,
		)
	}

	entries := payloadArchiveEntries(
		t,
		filepath.Join(
			payload.Directory,
			payload.Manifest.Archive,
		),
	)

	included := map[string]bool{}
	for _, entry := range entries {
		included[entry] = true
	}

	for relative := range excluded {
		if included[filepath.ToSlash(relative)] {
			t.Fatalf(
				"unapproved cache root was included: %s",
				relative,
			)
		}
	}
}

func TestBuildTargetPayloadReportsProgress(
	t *testing.T,
) {
	root, resolved, identity := writeCompletedPreparation(t)
	mustRemove(t, TargetPayloadPath(root, identity))

	var progress bytes.Buffer
	_, err := BuildTargetPayloadWithProgress(
		root,
		filepath.Join(root, "cache"),
		identity,
		resolved.Metadata.Platform.OS+
			"/"+
			resolved.Metadata.Platform.Arch,
		&progress,
	)
	if err != nil {
		t.Fatalf(
			"BuildTargetPayloadWithProgress() error = %v",
			err,
		)
	}

	output := progress.String()
	for _, expected := range []string{
		"[INFO] Building Target payload:",
		"compression=gzip-best-speed",
		"[INFO] Target payload progress:",
		"[OK] Target payload archive written:",
		"[INFO] Validating Target payload archive",
		"[OK] Target payload archive validated",
	} {
		if !strings.Contains(output, expected) {
			t.Fatalf(
				"progress output is missing %q: %s",
				expected,
				output,
			)
		}
	}
}

func TestBuildTargetPayloadRejectsUnsafeOrIncompleteSource(t *testing.T) {
	for _, testCase := range []struct {
		name   string
		mutate func(t *testing.T, root string)
	}{
		{"missing package", func(t *testing.T, root string) { mustRemove(t, filepath.Join(root, "cache/apt/debs/a.deb")) }},
		{"missing model", func(t *testing.T, root string) { mustRemove(t, filepath.Join(root, "cache/models/agent/hf/model.bin")) }},
		{"protected manifest content", func(t *testing.T, root string) {
			mustWrite(t, filepath.Join(root, "cache/models/manifest.txt"), "files:\nauthorization: forbidden\n")
		}},
		{"symbolic link", func(t *testing.T, root string) {
			if err := os.Symlink("model.bin", filepath.Join(root, "cache/models/agent/hf/link")); err != nil {
				t.Fatal(err)
			}
		}},
		{"hard link", func(t *testing.T, root string) {
			if err := os.Link(filepath.Join(root, "cache/models/agent/hf/model.bin"), filepath.Join(root, "cache/models/agent/hf/model-copy.bin")); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			root, resolved, identity := writeCompletedPreparation(t)
			mustRemove(t, TargetPayloadPath(root, identity))
			testCase.mutate(t, root)
			if _, err := BuildTargetPayload(root, filepath.Join(root, "cache"), identity, resolved.Metadata.Platform.OS+"/"+resolved.Metadata.Platform.Arch); err == nil {
				t.Fatal("BuildTargetPayload() succeeded with invalid source")
			}
		})
	}
}

func TestLoadAndMaterializeTargetPayloadFailClosedAndReuse(t *testing.T) {
	preparationRoot, resolved, identity := writeCompletedPreparation(t)
	payloadPath := TargetPayloadPath(preparationRoot, identity)
	if err := copyPayloadDirectory(payloadPath, filepath.Join(resolved.Root, targetPayloadDirectory)); err != nil {
		t.Fatal(err)
	}
	if err := copyPayloadDirectory(RuntimeArtifactPath(preparationRoot, identity), filepath.Join(resolved.Root, "remote-runtime")); err != nil {
		t.Fatal(err)
	}
	writeRemoteDeliveryMarker(t, resolved.Root, identity)
	artifactRoot := t.TempDir()
	cacheRoot, err := MaterializeTargetPayload(resolved, identity.RepositoryRegistry, identity.RepositoryProject, artifactRoot, nil)
	if err != nil {
		t.Fatalf("MaterializeTargetPayload() error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(cacheRoot, "models/agent/hf/model.bin")); err != nil {
		t.Fatalf("materialized model missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(cacheRoot, "apt/debs/a.deb")); err != nil {
		t.Fatalf("materialized package missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(cacheRoot, "images/images-pulled.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Harbor image cache was materialized: %v", err)
	}
	before, err := os.ReadFile(filepath.Join(cacheRoot, "models/agent/hf/model.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if reused, err := MaterializeTargetPayload(resolved, identity.RepositoryRegistry, identity.RepositoryProject, artifactRoot, nil); err != nil || reused != cacheRoot {
		t.Fatalf("materialized payload reuse = %q, %v", reused, err)
	}
	archive := filepath.Join(resolved.Root, targetPayloadDirectory, "remote-payload_"+identity.ReleaseVersion+"_"+payloadIdentityKey(identity)+".tar.gz")
	if err := os.WriteFile(archive, []byte("corrupt"), 0o640); err != nil {
		t.Fatal(err)
	}
	if _, err := MaterializeTargetPayload(resolved, identity.RepositoryRegistry, identity.RepositoryProject, artifactRoot, nil); err == nil {
		t.Fatal("MaterializeTargetPayload() accepted corrupt source archive")
	}
	after, err := os.ReadFile(filepath.Join(cacheRoot, "models/agent/hf/model.bin"))
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("existing materialized cache changed after source failure: %v", err)
	}
	wrong := identity
	wrong.RepositoryProject = "other"
	if _, err := LoadTargetPayload(payloadPath, wrong); err == nil {
		t.Fatal("LoadTargetPayload() accepted a repository identity mismatch")
	}
	mustWrite(t, filepath.Join(payloadPath, "workspace.yaml"), "forbidden")
	if _, err := LoadTargetPayload(payloadPath, identity); err == nil {
		t.Fatal("LoadTargetPayload() accepted an unexpected payload file")
	}
}

func TestPayloadArchiveRejectsTraversalAndSpecialEntries(t *testing.T) {
	for _, testCase := range []struct {
		name   string
		header tar.Header
	}{
		{"traversal", tar.Header{Name: "cache/../secret", Typeflag: tar.TypeReg, Mode: 0o600, Size: 1}},
		{"absolute", tar.Header{Name: "/cache/file", Typeflag: tar.TypeReg, Mode: 0o600, Size: 1}},
		{"link", tar.Header{Name: "cache/file", Typeflag: tar.TypeSymlink, Mode: 0o777}},
		{"outside-cache", tar.Header{Name: "credentials/aws_key.ini", Typeflag: tar.TypeReg, Mode: 0o600, Size: 1}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			archive := filepath.Join(t.TempDir(), "unsafe.tar.gz")
			writeTestArchive(t, archive, testCase.header)
			if _, err := payloadArchiveDigest(archive); err == nil {
				t.Fatal("payloadArchiveDigest() accepted unsafe archive")
			}
			if _, _, err := extractPayloadArchive(archive, t.TempDir(), nil); err == nil {
				t.Fatal("extractPayloadArchive() accepted unsafe archive")
			}
		})
	}
}

func payloadArchiveEntries(t *testing.T, archive string) []string {
	t.Helper()
	file, err := os.Open(archive)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	gzipReader, err := gzip.NewReader(file)
	if err != nil {
		t.Fatal(err)
	}
	defer gzipReader.Close()
	reader := tar.NewReader(gzipReader)
	var entries []string
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		entries = append(entries, header.Name)
	}
	return entries
}

func writeTestArchive(t *testing.T, archive string, header tar.Header) {
	t.Helper()
	file, err := os.Create(archive)
	if err != nil {
		t.Fatal(err)
	}
	gzipWriter := gzip.NewWriter(file)
	writer := tar.NewWriter(gzipWriter)
	if err := writer.WriteHeader(&header); err != nil {
		t.Fatal(err)
	}
	if header.Typeflag == tar.TypeReg {
		if _, err := writer.Write([]byte("x")); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gzipWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}

func copyPayloadDirectory(source, destination string) error {
	return filepath.Walk(source, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		target := filepath.Join(destination, relative)
		if info.IsDir() {
			return os.MkdirAll(target, 0o750)
		}
		contents, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
			return err
		}
		return os.WriteFile(target, contents, info.Mode().Perm())
	})
}

func mustWrite(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o640); err != nil {
		t.Fatal(err)
	}
}

func mustRemove(t *testing.T, path string) {
	t.Helper()
	if err := os.RemoveAll(path); err != nil {
		t.Fatal(err)
	}
}

// Regression: eva install on a Remote Target hashed the payload archive twice
// before extracting it, so the archive was read three times with no output.
// Extraction now computes both digests in the one read and still fails closed.
func TestMaterializeTargetPayloadVerifiesWhileExtracting(t *testing.T) {
	preparationRoot, resolved, identity := writeCompletedPreparation(t)
	if err := copyPayloadDirectory(TargetPayloadPath(preparationRoot, identity), filepath.Join(resolved.Root, targetPayloadDirectory)); err != nil {
		t.Fatal(err)
	}
	if err := copyPayloadDirectory(RuntimeArtifactPath(preparationRoot, identity), filepath.Join(resolved.Root, "remote-runtime")); err != nil {
		t.Fatal(err)
	}
	writeRemoteDeliveryMarker(t, resolved.Root, identity)
	archive := filepath.Join(resolved.Root, targetPayloadDirectory, "remote-payload_"+identity.ReleaseVersion+"_"+payloadIdentityKey(identity)+".tar.gz")

	archiveDigest, contentDigest, err := extractPayloadArchive(archive, t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	wantArchive, err := regularFileSHA256(archive)
	if err != nil {
		t.Fatal(err)
	}
	wantContent, err := payloadArchiveDigest(archive)
	if err != nil {
		t.Fatal(err)
	}
	if archiveDigest != wantArchive || contentDigest != wantContent {
		t.Fatalf("single-pass digests = %s %s, want %s %s", archiveDigest, contentDigest, wantArchive, wantContent)
	}

	// Same-size corruption: only a content read can notice it.
	original, err := os.ReadFile(archive)
	if err != nil {
		t.Fatal(err)
	}
	corrupt := append([]byte(nil), original...)
	corrupt[len(corrupt)/2] ^= 0xff
	if err := os.WriteFile(archive, corrupt, 0o640); err != nil {
		t.Fatal(err)
	}
	if _, _, err := checkPublishedRemoteDelivery(resolved, identity.RepositoryRegistry, identity.RepositoryProject, false); err != nil {
		t.Fatalf("delivery check hashed the payload archive before extraction: %v", err)
	}
	artifactRoot := t.TempDir()
	if _, err := MaterializeTargetPayload(resolved, identity.RepositoryRegistry, identity.RepositoryProject, artifactRoot, nil); err == nil {
		t.Fatal("MaterializeTargetPayload() published a corrupt payload")
	}
	if entries, err := os.ReadDir(filepath.Join(artifactRoot, identity.ReleaseVersion)); err != nil || len(entries) != 0 {
		t.Fatalf("corrupt payload left materialized state: %v, %v", entries, err)
	}

	if err := os.WriteFile(archive, original, 0o640); err != nil {
		t.Fatal(err)
	}
	var progress bytes.Buffer
	if _, err := MaterializeTargetPayload(resolved, identity.RepositoryRegistry, identity.RepositoryProject, artifactRoot, &progress); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Verifying and extracting the published Target payload", "Target payload progress:", "percent=100.0", "[OK] Target payload verified and extracted"} {
		if !strings.Contains(progress.String(), want) {
			t.Fatalf("materialization output lacks %q:\n%s", want, progress.String())
		}
	}
	// A cache materialized by an earlier Tool has no source record; reuse
	// verifies the archive once and records it instead of failing.
	sourceRecords, err := filepath.Glob(filepath.Join(artifactRoot, identity.ReleaseVersion, "*", materializedSourceFile))
	if err != nil || len(sourceRecords) != 1 {
		t.Fatalf("materialized source record = %v, %v", sourceRecords, err)
	}
	if err := os.Remove(sourceRecords[0]); err != nil {
		t.Fatal(err)
	}
	if _, err := MaterializeTargetPayload(resolved, identity.RepositoryRegistry, identity.RepositoryProject, artifactRoot, nil); err != nil {
		t.Fatalf("legacy materialized payload without a source record was rejected: %v", err)
	}
	if _, err := os.Stat(sourceRecords[0]); err != nil {
		t.Fatalf("source record was not written for the legacy materialized payload: %v", err)
	}
}
