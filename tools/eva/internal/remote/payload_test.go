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
	payload, err := BuildTargetPayload(root, identity, resolved.Metadata.Platform.OS+"/"+resolved.Metadata.Platform.Arch)
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

func TestBuildTargetPayloadRejectsUnsafeOrIncompleteSource(t *testing.T) {
	for _, testCase := range []struct {
		name   string
		mutate func(t *testing.T, root string)
	}{
		{"missing package", func(t *testing.T, root string) { mustRemove(t, filepath.Join(root, "cache/apt/debs/a.deb")) }},
		{"missing model", func(t *testing.T, root string) { mustRemove(t, filepath.Join(root, "cache/models/agent/hf/model.bin")) }},
		{"sensitive path", func(t *testing.T, root string) {
			mustWrite(t, filepath.Join(root, "cache/models/credentials/token"), "no")
		}},
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
			if _, err := BuildTargetPayload(root, identity, resolved.Metadata.Platform.OS+"/"+resolved.Metadata.Platform.Arch); err == nil {
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
	cacheRoot, err := MaterializeTargetPayload(resolved, identity.RepositoryRegistry, identity.RepositoryProject, artifactRoot)
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
	if reused, err := MaterializeTargetPayload(resolved, identity.RepositoryRegistry, identity.RepositoryProject, artifactRoot); err != nil || reused != cacheRoot {
		t.Fatalf("materialized payload reuse = %q, %v", reused, err)
	}
	archive := filepath.Join(resolved.Root, targetPayloadDirectory, "remote-payload_"+identity.ReleaseVersion+"_"+payloadIdentityKey(identity)+".tar.gz")
	if err := os.WriteFile(archive, []byte("corrupt"), 0o640); err != nil {
		t.Fatal(err)
	}
	if _, err := MaterializeTargetPayload(resolved, identity.RepositoryRegistry, identity.RepositoryProject, artifactRoot); err == nil {
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
		{"secret", tar.Header{Name: "cache/models/token", Typeflag: tar.TypeReg, Mode: 0o600, Size: 1}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			archive := filepath.Join(t.TempDir(), "unsafe.tar.gz")
			writeTestArchive(t, archive, testCase.header)
			if _, err := payloadArchiveDigest(archive); err == nil {
				t.Fatal("payloadArchiveDigest() accepted unsafe archive")
			}
			if err := extractPayloadArchive(archive, t.TempDir()); err == nil {
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
