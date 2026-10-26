package airgap

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"eva-deployer/tools/eva/internal/remote"
)

func archiveTestVisionCache(t *testing.T, cache string) {
	t.Helper()
	var models []map[string]any
	for _, name := range []string{"Owl-v2", "OmDet", "LLMDet", "RT-DETRV2", "DARTF", "DARTF-light", "DINOv2Small", "E5EmbeddingSmall", "Qwen3VLEmbed2B"} {
		var files []map[string]any
		for _, file := range []struct{ path, body string }{
			{"manifest.txt", "model data, not a relocatable cache manifest: " + cache + "/unchanged\n"},
			{"weights/model.bin", "tiny Vision model " + name + "\n"},
		} {
			body := []byte(file.body)
			archiveTestWriteFile(t, filepath.Join(cache, "vision-models/model_dir", name, file.path), body)
			files = append(files, map[string]any{"path": file.path, "size": len(body), "sha256": archiveTestDigest(body)})
		}
		models = append(models, map[string]any{"name": name, "files": files})
	}
	data, err := json.MarshalIndent(map[string]any{"schema_version": "v1", "source": "s3://spectrabrain-eva-vision/model_dir", "models": models}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	archiveTestWriteFile(t, filepath.Join(cache, "vision-models/manifest.json"), append(data, '\n'))
}

type archiveTestEntry struct {
	header tar.Header
	body   []byte
}

func archiveTestRegular(name string, body []byte) archiveTestEntry {
	return archiveTestEntry{header: tar.Header{Name: name, Mode: 0o644, Typeflag: tar.TypeReg, Size: int64(len(body))}, body: body}
}

func archiveTestTarBytes(t *testing.T, entries []archiveTestEntry) []byte {
	t.Helper()
	var buffer bytes.Buffer
	tw := tar.NewWriter(&buffer)
	for _, entry := range entries {
		if err := tw.WriteHeader(&entry.header); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(entry.body); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func archiveTestWriteTar(t *testing.T, entries []archiveTestEntry, compressed bool) string {
	t.Helper()
	body := archiveTestTarBytes(t, entries)
	if compressed {
		var buffer bytes.Buffer
		gz := gzip.NewWriter(&buffer)
		if _, err := gz.Write(body); err != nil {
			t.Fatal(err)
		}
		if err := gz.Close(); err != nil {
			t.Fatal(err)
		}
		body = buffer.Bytes()
	}
	name := filepath.Join(t.TempDir(), "archive.tar")
	archiveTestWriteFile(t, name, body)
	return name
}

func archiveTestWriteFile(t *testing.T, name string, body []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(name), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(name, body, 0o644); err != nil {
		t.Fatal(err)
	}
}

func archiveTestRuntime(t *testing.T) string {
	t.Helper()
	return archiveTestWriteTar(t, []archiveTestEntry{
		{header: tar.Header{Name: "runtime/", Mode: 0o755, Typeflag: tar.TypeDir}},
		archiveTestRegular("runtime/runtime.yaml", []byte("platform: linux/amd64\n")),
		archiveTestRegular("runtime/venv/bin/python", []byte("synthetic interpreter")),
	}, true)
}

func archiveTestReadOffline(t *testing.T, name string) map[string]archiveTestEntry {
	t.Helper()
	file, err := os.Open(name)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	gz, err := gzip.NewReader(file)
	if err != nil {
		t.Fatal(err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	entries := make(map[string]archiveTestEntry)
	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		if _, exists := entries[header.Name]; exists {
			t.Fatalf("duplicate output entry %q", header.Name)
		}
		entries[header.Name] = archiveTestEntry{header: *header, body: body}
	}
	if _, err := io.Copy(io.Discard, gz); err != nil {
		t.Fatal(err)
	}
	return entries
}

func TestBuildOfflineArchivePortableCache(t *testing.T) {
	root := filepath.Join(t.TempDir(), "cache")
	model := bytes.Repeat([]byte("model weight\x00"), 10000)
	modelPath := "models/agent/hf/nested/model.safetensors"
	archiveTestWriteFile(t, filepath.Join(root, modelPath), model)
	manifest := "files:\n" + root + "/" + modelPath + "\ncache_root=" + root + "\nother=" + root + "-other/not-a-cache\n"
	archiveTestWriteFile(t, filepath.Join(root, "models/manifest.txt"), []byte(manifest))
	archiveTestWriteFile(t, filepath.Join(root, "manifest.txt"), []byte("root: "+root+"\n"))
	archiveTestWriteFile(t, filepath.Join(root, "apt/debs/manifest.txt"), []byte("package="+root+"/apt/debs/p.deb\n"))
	archiveTestWriteFile(t, filepath.Join(root, "airgap-preparation.yaml"), []byte("schema_version: 1\n"))
	// No other filename is rewritten, even if it happens to contain a host path.
	archiveTestWriteFile(t, filepath.Join(root, "models/config.json"), []byte(root))
	archiveTestWriteFile(t, filepath.Join(root, "images/rendered/secrets/secret.yaml"), []byte("kind: Secret\ndata: private\n"))
	for _, category := range []string{"aws", "apt", "docker", "nvidia", "cuda", "helm", "k3s", "k8s", "nfs", "eva-app", "eva-vision", "eva-agent", "eva-iam", "qdrant", "tools", "images", "display_mode_selector", "models", "python-debs", "wheels"} {
		archiveTestWriteFile(t, filepath.Join(root, category, "asset.bin"), []byte(category))
	}
	if err := os.Chmod(filepath.Join(root, "tools/asset.bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(t.TempDir(), "offline.tar.gz")
	if err := buildOfflineArchive(archiveTestRuntime(t), root, destination); err != nil {
		t.Fatal(err)
	}
	entries := archiveTestReadOffline(t, destination)
	if got := entries["out/cache/"+modelPath].body; !bytes.Equal(got, model) {
		t.Fatal("model bytes or nested model tree changed")
	}
	want := strings.ReplaceAll(manifest, root+"/", "out/cache/")
	want = strings.ReplaceAll(want, "cache_root="+root+"\n", "cache_root=out/cache\n")
	if got := string(entries["out/cache/models/manifest.txt"].body); got != want {
		t.Fatalf("model manifest = %q, want %q", got, want)
	}
	if got := string(entries["out/cache/manifest.txt"].body); got != "root: out/cache\n" {
		t.Fatalf("root manifest = %q", got)
	}
	if got := string(entries["out/cache/apt/debs/manifest.txt"].body); got != "package=out/cache/apt/debs/p.deb\n" {
		t.Fatalf("nested manifest = %q", got)
	}
	if got := string(entries["out/cache/models/config.json"].body); got != root {
		t.Fatal("non-manifest file was rewritten")
	}
	if got := entries["out/cache/tools/asset.bin"].header.Mode; got != 0o755 {
		t.Fatalf("executable mode = %o", got)
	}
	for _, required := range []string{"runtime/runtime.yaml", "runtime/venv/bin/python", "out/cache/airgap-preparation.yaml", "out/cache/wheels/asset.bin", "out/cache/python-debs/asset.bin"} {
		if _, ok := entries[required]; !ok {
			t.Errorf("missing output entry %q", required)
		}
	}
	for name, entry := range entries {
		if strings.Contains(name, "images/rendered") {
			t.Errorf("rendered Secret included: %s", name)
		}
		if entry.header.Uid != 0 || entry.header.Gid != 0 || entry.header.Uname != "" || entry.header.Gname != "" {
			t.Errorf("host ownership included: %+v", entry.header)
		}
	}
	// Building must not mutate source manifests or models.
	original, err := os.ReadFile(filepath.Join(root, "models/manifest.txt"))
	if err != nil || string(original) != manifest {
		t.Fatalf("source manifest changed: %q, %v", original, err)
	}
}

func TestBuildOfflineArchiveStripsRuntimeMetadata(t *testing.T) {
	entry := archiveTestRegular("runtime/bin/tool", []byte("tool"))
	entry.header.Mode, entry.header.Uid, entry.header.Gid = 0o6755, 123, 456
	entry.header.Uname, entry.header.Gname = "main-user", "main-group"
	entry.header.PAXRecords = map[string]string{"SCHILY.xattr.user.private": "host-metadata"}
	archive := archiveTestWriteTar(t, []archiveTestEntry{entry}, true)
	destination := filepath.Join(t.TempDir(), "offline.tar.gz")
	if err := buildOfflineArchive(archive, t.TempDir(), destination); err != nil {
		t.Fatal(err)
	}
	header := archiveTestReadOffline(t, destination)[entry.header.Name].header
	if header.Uid != 0 || header.Gid != 0 || header.Uname != "" || header.Gname != "" || header.Mode != 0o755 || len(header.PAXRecords) != 0 || len(header.Xattrs) != 0 {
		t.Fatalf("runtime metadata leaked: %+v", header)
	}
}

func TestBuildOfflineArchiveRejectsUnsafeCache(t *testing.T) {
	tests := []struct {
		name  string
		setup func(*testing.T, string)
	}{
		{"model symlink", func(t *testing.T, root string) {
			outside := filepath.Join(t.TempDir(), "weight")
			archiveTestWriteFile(t, outside, []byte("weights"))
			if err := os.MkdirAll(filepath.Join(root, "models/hf"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(outside, filepath.Join(root, "models/hf/model.bin")); err != nil {
				t.Fatal(err)
			}
		}},
		{"directory symlink", func(t *testing.T, root string) {
			if err := os.Symlink(t.TempDir(), filepath.Join(root, "models")); err != nil {
				t.Fatal(err)
			}
		}},
		{"dangling symlink", func(t *testing.T, root string) {
			if err := os.Symlink("missing", filepath.Join(root, "tools")); err != nil {
				t.Fatal(err)
			}
		}},
		{"hard link", func(t *testing.T, root string) {
			source := filepath.Join(t.TempDir(), "weight")
			archiveTestWriteFile(t, source, []byte("weights"))
			if err := os.MkdirAll(filepath.Join(root, "models"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Link(source, filepath.Join(root, "models/model.bin")); err != nil {
				t.Fatal(err)
			}
		}},
		{"fifo", func(t *testing.T, root string) {
			if err := os.MkdirAll(filepath.Join(root, "models"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := syscall.Mkfifo(filepath.Join(root, "models/pipe"), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{"unsupported category", func(t *testing.T, root string) {
			archiveTestWriteFile(t, filepath.Join(root, "private/report.txt"), []byte("private"))
		}},
		{"category is file", func(t *testing.T, root string) {
			archiveTestWriteFile(t, filepath.Join(root, "models"), []byte("not a directory"))
		}},
		{"root manifest directory", func(t *testing.T, root string) {
			if err := os.Mkdir(filepath.Join(root, "manifest.txt"), 0o755); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, credential := range []string{".aws/config", ".docker/config.json", ".ssh/config", "credentials/file", "secrets/data", "workspace/site.yaml", "aws_key.ini", "server.key", "CERT.PEM", "id_rsa.pub", "id_ed25519_backup", ".env", ".env.production", ".environment"} {
		name := "models/nested/" + credential
		tests = append(tests, struct {
			name  string
			setup func(*testing.T, string)
		}{"credential " + credential, func(t *testing.T, root string) {
			archiveTestWriteFile(t, filepath.Join(root, name), []byte("credential"))
		}})
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			test.setup(t, root)
			destination := filepath.Join(t.TempDir(), "offline.tar.gz")
			if err := buildOfflineArchive(archiveTestRuntime(t), root, destination); err == nil {
				t.Fatal("unsafe cache was accepted")
			}
			if _, err := os.Lstat(destination); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("partial destination remains: %v", err)
			}
		})
	}
}

func TestBuildOfflineArchiveRejectsUnsafeRuntime(t *testing.T) {
	tests := []struct {
		name    string
		entries []archiveTestEntry
	}{
		{"traversal", []archiveTestEntry{archiveTestRegular("runtime/../escape", []byte("bad"))}},
		{"absolute", []archiveTestEntry{archiveTestRegular("/runtime/file", nil)}},
		{"backslash", []archiveTestEntry{archiveTestRegular("runtime\\file", nil)}},
		{"outside runtime", []archiveTestEntry{archiveTestRegular("cache/file", nil)}},
		{"symlink", []archiveTestEntry{{header: tar.Header{Name: "runtime/link", Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd"}}}},
		{"hardlink", []archiveTestEntry{{header: tar.Header{Name: "runtime/link", Typeflag: tar.TypeLink, Linkname: "runtime/file"}}}},
		{"fifo", []archiveTestEntry{{header: tar.Header{Name: "runtime/pipe", Typeflag: tar.TypeFifo}}}},
		{"device", []archiveTestEntry{{header: tar.Header{Name: "runtime/device", Typeflag: tar.TypeChar}}}},
		{"credential", []archiveTestEntry{archiveTestRegular("runtime/.ssh/id_rsa", []byte("bad"))}},
		{"duplicate", []archiveTestEntry{archiveTestRegular("runtime/file", nil), archiveTestRegular("runtime/./file", nil)}},
		{"file parent", []archiveTestEntry{archiveTestRegular("runtime/parent/child", nil), archiveTestRegular("runtime/parent", nil)}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			destination := filepath.Join(t.TempDir(), "offline.tar.gz")
			archive := archiveTestWriteTar(t, test.entries, true)
			if err := buildOfflineArchive(archive, t.TempDir(), destination); err == nil {
				t.Fatal("unsafe runtime was accepted")
			}
			if _, err := os.Lstat(destination); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("partial destination remains: %v", err)
			}
		})
	}
}

func TestBuildOfflineArchiveRejectsLateGzipFailure(t *testing.T) {
	for _, corruption := range []string{"checksum", "truncated trailer", "trailing payload"} {
		t.Run(corruption, func(t *testing.T) {
			archive := archiveTestRuntime(t)
			body, err := os.ReadFile(archive)
			if err != nil {
				t.Fatal(err)
			}
			switch corruption {
			case "checksum":
				body[len(body)-8] ^= 0xff
			case "truncated trailer":
				body = body[:len(body)-4]
			case "trailing payload":
				var extra bytes.Buffer
				gz := gzip.NewWriter(&extra)
				if _, err := gz.Write([]byte("hidden data after tar EOF")); err != nil {
					t.Fatal(err)
				}
				if err := gz.Close(); err != nil {
					t.Fatal(err)
				}
				body = append(body, extra.Bytes()...)
			}
			archiveTestWriteFile(t, archive, body)
			destination := filepath.Join(t.TempDir(), "offline.tar.gz")
			if err := buildOfflineArchive(archive, t.TempDir(), destination); err == nil {
				t.Fatal("incomplete or corrupt gzip was accepted")
			}
			if _, err := os.Lstat(destination); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("partial destination remains: %v", err)
			}
		})
	}
}

func TestBuildOfflineArchiveExclusiveDestination(t *testing.T) {
	for _, symlink := range []bool{false, true} {
		t.Run(map[bool]string{false: "existing file", true: "symlink"}[symlink], func(t *testing.T) {
			destination := filepath.Join(t.TempDir(), "offline.tar.gz")
			original := []byte("do not overwrite")
			target := destination
			if symlink {
				target = filepath.Join(t.TempDir(), "target")
				if err := os.Symlink(target, destination); err != nil {
					t.Fatal(err)
				}
			}
			archiveTestWriteFile(t, target, original)
			if err := buildOfflineArchive(archiveTestRuntime(t), t.TempDir(), destination); err == nil {
				t.Fatal("existing destination was accepted")
			}
			got, err := os.ReadFile(target)
			if err != nil || !bytes.Equal(got, original) {
				t.Fatalf("destination overwritten/removed: %q, %v", got, err)
			}
			if symlink {
				if _, err := os.Readlink(destination); err != nil {
					t.Fatalf("destination symlink removed: %v", err)
				}
			}
		})
	}
	root := t.TempDir()
	if err := buildOfflineArchive(archiveTestRuntime(t), root, filepath.Join(root, "images/offline.tar.gz")); err == nil {
		t.Fatal("destination inside cache was accepted")
	}
}

func TestRewriteCacheManifestStreaming(t *testing.T) {
	root := "/a/cache"
	input := strings.Repeat("x", (32<<10)-3) + root + "/models/file\n" + root + "\n" + root + "-other\n"
	want := strings.ReplaceAll(input, root+"/", "out/cache/")
	want = strings.ReplaceAll(want, root+"\n", "out/cache\n")
	for _, chunk := range []int{1, 3, 1024, 32 << 10} {
		var output bytes.Buffer
		reader := &archiveTestChunkReader{data: []byte(input), chunk: chunk}
		if err := rewriteCacheManifest(&output, reader, root); err != nil {
			t.Fatal(err)
		}
		if output.String() != want {
			t.Fatalf("rewrite differs for chunk size %d", chunk)
		}
	}
}

type archiveTestChunkReader struct {
	data  []byte
	chunk int
}

func (r *archiveTestChunkReader) Read(buffer []byte) (int, error) {
	if len(r.data) == 0 {
		return 0, io.EOF
	}
	n := min(len(buffer), r.chunk, len(r.data))
	copy(buffer, r.data[:n])
	r.data = r.data[n:]
	return n, nil
}

func archiveTestDigest(body []byte) string {
	digest := sha256.Sum256(body)
	return hex.EncodeToString(digest[:])
}

func archiveTestJSON(t *testing.T, value any) []byte {
	t.Helper()
	body, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func archiveTestDockerEntries(t *testing.T) []archiveTestEntry {
	t.Helper()
	layer1 := archiveTestTarBytes(t, []archiveTestEntry{archiveTestRegular("file", bytes.Repeat([]byte("layer contents"), 10000))})
	layer2 := archiveTestTarBytes(t, []archiveTestEntry{archiveTestRegular("other", []byte("second layer"))})
	config := archiveTestJSON(t, map[string]any{
		"os": "linux", "architecture": "amd64",
		"rootfs": map[string]any{"type": "layers", "diff_ids": []string{"sha256:" + archiveTestDigest(layer1), "sha256:" + archiveTestDigest(layer2)}},
	})
	configName := archiveTestDigest(config) + ".json"
	manifest := []dockerSaveManifestEntry{{Config: configName, RepoTags: []string{"example.test/eva:1", "example.test/eva:latest"}, Layers: []string{"first/layer.tar", "second/layer.tar"}}}
	// Manifest deliberately comes last, after layer payloads and their config.
	return []archiveTestEntry{
		archiveTestRegular(configName, config),
		{header: tar.Header{Name: "first/", Typeflag: tar.TypeDir, Mode: 0o755}},
		archiveTestRegular("first/layer.tar", layer1),
		archiveTestRegular("second/layer.tar", layer2),
		archiveTestRegular("manifest.json", archiveTestJSON(t, manifest)),
	}
}

func archiveTestChangeManifest(t *testing.T, entries []archiveTestEntry, change func([]dockerSaveManifestEntry)) []archiveTestEntry {
	t.Helper()
	for index := range entries {
		if entries[index].header.Name != "manifest.json" {
			continue
		}
		var manifest []dockerSaveManifestEntry
		if err := json.Unmarshal(entries[index].body, &manifest); err != nil {
			t.Fatal(err)
		}
		change(manifest)
		entries[index] = archiveTestRegular("manifest.json", archiveTestJSON(t, manifest))
		return entries
	}
	t.Fatal("test fixture has no manifest")
	return nil
}

func archiveTestChangeConfig(t *testing.T, entries []archiveTestEntry, change func(map[string]any)) []archiveTestEntry {
	t.Helper()
	var config map[string]any
	if err := json.Unmarshal(entries[0].body, &config); err != nil {
		t.Fatal(err)
	}
	change(config)
	body := archiveTestJSON(t, config)
	name := archiveTestDigest(body) + ".json"
	entries[0] = archiveTestRegular(name, body)
	return archiveTestChangeManifest(t, entries, func(manifest []dockerSaveManifestEntry) { manifest[0].Config = name })
}

func TestValidateDockerArchive(t *testing.T) {
	for _, variant := range []string{"manifest last", "manifest first", "blob layout", "shared image", "scratch", "PAX config path", "minimal config"} {
		t.Run(variant, func(t *testing.T) {
			entries := archiveTestDockerEntries(t)
			switch variant {
			case "manifest first":
				entries = append(entries[len(entries)-1:], entries[:len(entries)-1]...)
			case "blob layout":
				configName := "blobs/sha256/" + archiveTestDigest(entries[0].body)
				entries[0].header.Name = configName
				entries[2].header.Name = "blobs/sha256/" + archiveTestDigest(entries[2].body)
				entries[3].header.Name = "blobs/sha256/" + archiveTestDigest(entries[3].body)
				entries = archiveTestChangeManifest(t, entries, func(manifest []dockerSaveManifestEntry) {
					manifest[0].Config = configName
					manifest[0].Layers = []string{entries[2].header.Name, entries[3].header.Name}
				})
			case "shared image":
				var manifest []dockerSaveManifestEntry
				if err := json.Unmarshal(entries[len(entries)-1].body, &manifest); err != nil {
					t.Fatal(err)
				}
				manifest = append(manifest, manifest[0])
				manifest[1].RepoTags = []string{"other:1"}
				entries[len(entries)-1] = archiveTestRegular("manifest.json", archiveTestJSON(t, manifest))
			case "scratch":
				entries = archiveTestChangeConfig(t, entries, func(config map[string]any) {
					config["rootfs"].(map[string]any)["diff_ids"] = []string{}
				})
				entries = archiveTestChangeManifest(t, entries, func(manifest []dockerSaveManifestEntry) { manifest[0].Layers = []string{} })
			case "PAX config path":
				name := strings.Repeat("nested/", 30) + "config.json"
				entries[0].header.Name = name
				entries = archiveTestChangeManifest(t, entries, func(manifest []dockerSaveManifestEntry) { manifest[0].Config = name })
			case "minimal config":
				entries = archiveTestChangeConfig(t, entries, func(config map[string]any) {
					delete(config["rootfs"].(map[string]any), "type")
				})
			}
			archive := archiveTestWriteTar(t, entries, false)
			if err := validateDockerArchive(archive, []string{"example.test/eva:1", "example.test/eva:latest"}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestValidateDockerArchiveCompressedOCILayers(t *testing.T) {
	entries := archiveTestDockerEntries(t)
	for _, index := range []int{2, 3} {
		var compressed bytes.Buffer
		writer := gzip.NewWriter(&compressed)
		if _, err := writer.Write(entries[index].body); err != nil {
			t.Fatal(err)
		}
		if err := writer.Close(); err != nil {
			t.Fatal(err)
		}
		body := compressed.Bytes()
		entries[index] = archiveTestRegular("blobs/sha256/"+archiveTestDigest(body), body)
	}
	entries = archiveTestChangeManifest(t, entries, func(manifest []dockerSaveManifestEntry) {
		manifest[0].Layers = []string{entries[2].header.Name, entries[3].header.Name}
	})
	if err := validateDockerArchive(archiveTestWriteTar(t, entries, false), []string{"example.test/eva:1"}); err != nil {
		t.Fatal(err)
	}
	entries[2].body[len(entries[2].body)-1] ^= 0xff
	if err := validateDockerArchive(archiveTestWriteTar(t, entries, false), []string{"example.test/eva:1"}); err == nil {
		t.Fatal("accepted corrupt compressed OCI layer")
	}
}

func TestBuildOfflineArchivePublicRuntimeCAAndAdditionalCaches(t *testing.T) {
	root := filepath.Join(t.TempDir(), "cache")
	for _, name := range []string{"qdrant-snapshots/snapshot.bin", "harbor/harbor-offline-installer.tgz"} {
		archiveTestWriteFile(t, filepath.Join(root, name), []byte("asset"))
	}
	archiveTestVisionCache(t, root)
	runtime := archiveTestWriteTar(t, []archiveTestEntry{
		archiveTestRegular("runtime/runtime.yaml", []byte("descriptor")),
		archiveTestRegular("runtime/venv/lib/python3.12/site-packages/certifi/cacert.pem", []byte("public CA")),
	}, true)
	output := filepath.Join(t.TempDir(), "offline.tar.gz")
	if err := buildOfflineArchive(runtime, root, output); err != nil {
		t.Fatal(err)
	}
	entries := archiveTestReadOffline(t, output)
	if _, ok := entries["out/cache/harbor/harbor-offline-installer.tgz"]; !ok {
		t.Fatal("Harbor installer missing")
	}
	if _, ok := entries["out/cache/qdrant-snapshots/snapshot.bin"]; !ok {
		t.Fatal("snapshot missing")
	}
	for name, entry := range entries {
		if strings.HasPrefix(name, "out/cache/vision-models/") && entry.header.Typeflag == tar.TypeReg {
			original, err := os.ReadFile(filepath.Join(root, strings.TrimPrefix(name, "out/cache/")))
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(original, entry.body) {
				t.Fatalf("checksummed Vision bytes were rewritten: %s", name)
			}
		}
	}
	// Exercise the exact documented target-side selective extraction. It
	// must not require an out/cache directory header or extract Runtime files.
	tarBinary, err := exec.LookPath("tar")
	if err != nil {
		t.Skip("tar is unavailable for target extraction contract")
	}
	target := t.TempDir()
	command := exec.Command(tarBinary, "--extract", "--gzip", "--file", output, "--directory", target, "--no-same-owner", "--keep-old-files", "out/cache")
	if contents, err := command.CombinedOutput(); err != nil {
		t.Fatalf("documented cache extraction: %v: %s", err, contents)
	}
	if _, err := os.Stat(filepath.Join(target, "out/cache/qdrant-snapshots/snapshot.bin")); err != nil {
		t.Fatal(err)
	}
	if err := remote.ValidateVisionModels(filepath.Join(target, "out/cache")); err != nil {
		t.Fatalf("restored Vision cache failed exact validation: %v", err)
	}
	if _, err := os.Stat(filepath.Join(target, "runtime")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("cache extraction materialized Runtime too")
	}
}

func TestBuildOfflineArchiveExcludesVisionBackendLock(t *testing.T) {
	cache := t.TempDir()
	archiveTestVisionCache(t, cache)
	lock := filepath.Join(cache, ".vision-models.lock")
	archiveTestWriteFile(t, lock, nil)
	output := filepath.Join(t.TempDir(), "offline.tar.gz")
	if err := buildOfflineArchive(archiveTestRuntime(t), cache, output); err != nil {
		t.Fatal(err)
	}
	if _, ok := archiveTestReadOffline(t, output)["out/cache/.vision-models.lock"]; ok {
		t.Fatal("backend process lock was archived")
	}
	archiveTestWriteFile(t, lock, []byte("not lock state"))
	if err := buildOfflineArchive(archiveTestRuntime(t), cache, filepath.Join(t.TempDir(), "bad.tar.gz")); err == nil {
		t.Fatal("nonempty file was hidden as a backend lock")
	}
}

func TestBuildOfflineArchiveRejectsInvalidVisionCaches(t *testing.T) {
	for _, tc := range []struct{ name, path, body string }{
		{"corrupt", "vision-models/model_dir/Owl-v2/weights/model.bin", "corrupt"},
		{"extra safe file", "vision-models/model_dir/Owl-v2/extra.bin", "extra"},
		{"credentials", "vision-models/model_dir/Owl-v2/credentials.json", "private"},
		{"empty", "vision-models/model_dir/Owl-v2/weights/model.bin", ""},
		{"bad manifest", "vision-models/manifest.json", "{}"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cache := t.TempDir()
			archiveTestVisionCache(t, cache)
			archiveTestWriteFile(t, filepath.Join(cache, tc.path), []byte(tc.body))
			output := filepath.Join(t.TempDir(), "offline.tar.gz")
			if err := buildOfflineArchive(archiveTestRuntime(t), cache, output); err == nil {
				t.Fatal("invalid Vision cache archived")
			}
			if _, err := os.Lstat(output); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("failed archive exposed output")
			}
		})
	}
}

func TestValidateDockerArchiveRejectsIncompleteImages(t *testing.T) {
	tests := []struct {
		name   string
		change func(*testing.T, []archiveTestEntry) []archiveTestEntry
		want   string
	}{
		{"missing config", func(t *testing.T, e []archiveTestEntry) []archiveTestEntry { return e[1:] }, "config"},
		{"missing layer", func(t *testing.T, e []archiveTestEntry) []archiveTestEntry { return append(e[:2], e[3:]...) }, "layer"},
		{"empty layer", func(t *testing.T, e []archiveTestEntry) []archiveTestEntry {
			e[2] = archiveTestRegular(e[2].header.Name, nil)
			return e
		}, "layer"},
		{"corrupt layer", func(t *testing.T, e []archiveTestEntry) []archiveTestEntry { e[2].body[1000] ^= 1; return e }, "digest"},
		{"missing tag", func(t *testing.T, e []archiveTestEntry) []archiveTestEntry {
			return archiveTestChangeManifest(t, e, func(m []dockerSaveManifestEntry) { m[0].RepoTags = nil })
		}, "tag"},
		{"wrong tag", func(t *testing.T, e []archiveTestEntry) []archiveTestEntry {
			return archiveTestChangeManifest(t, e, func(m []dockerSaveManifestEntry) { m[0].RepoTags = []string{"example.test/eva:other"} })
		}, "tag"},
		{"missing manifest", func(t *testing.T, e []archiveTestEntry) []archiveTestEntry { return e[:len(e)-1] }, "manifest.json"},
		{"index only", func(t *testing.T, e []archiveTestEntry) []archiveTestEntry {
			return []archiveTestEntry{archiveTestRegular("index.json", []byte(`{"schemaVersion":2,"manifests":[]}`)), e[0]}
		}, "manifest.json"},
		{"empty manifest", func(t *testing.T, e []archiveTestEntry) []archiveTestEntry {
			e[len(e)-1] = archiveTestRegular("manifest.json", []byte("[]"))
			return e
		}, "no images"},
		{"invalid manifest", func(t *testing.T, e []archiveTestEntry) []archiveTestEntry {
			e[len(e)-1] = archiveTestRegular("manifest.json", []byte("["))
			return e
		}, "parse"},
		{"invalid config", func(t *testing.T, e []archiveTestEntry) []archiveTestEntry {
			e[0] = archiveTestRegular("config.json", []byte("{"))
			return archiveTestChangeManifest(t, e, func(m []dockerSaveManifestEntry) { m[0].Config = "config.json" })
		}, "parse docker config"},
		{"empty config", func(t *testing.T, e []archiveTestEntry) []archiveTestEntry {
			e[0] = archiveTestRegular(e[0].header.Name, nil)
			return e
		}, "empty"},
		{"config digest", func(t *testing.T, e []archiveTestEntry) []archiveTestEntry {
			e[0].body[0] = ' '
			return e
		}, "filename digest"},
		{"blob config digest", func(t *testing.T, e []archiveTestEntry) []archiveTestEntry {
			name := "blobs/sha256/" + strings.Repeat("0", 64)
			e[0].header.Name = name
			return archiveTestChangeManifest(t, e, func(m []dockerSaveManifestEntry) { m[0].Config = name })
		}, "filename digest"},
		{"wrong architecture", func(t *testing.T, e []archiveTestEntry) []archiveTestEntry {
			return archiveTestChangeConfig(t, e, func(c map[string]any) { c["architecture"] = "arm64" })
		}, "linux/amd64"},
		{"wrong OS", func(t *testing.T, e []archiveTestEntry) []archiveTestEntry {
			return archiveTestChangeConfig(t, e, func(c map[string]any) { c["os"] = "windows" })
		}, "linux/amd64"},
		{"missing rootfs", func(t *testing.T, e []archiveTestEntry) []archiveTestEntry {
			return archiveTestChangeConfig(t, e, func(c map[string]any) { delete(c, "rootfs") })
		}, "rootfs"},
		{"diff ID count", func(t *testing.T, e []archiveTestEntry) []archiveTestEntry {
			return archiveTestChangeConfig(t, e, func(c map[string]any) { c["rootfs"].(map[string]any)["diff_ids"] = []string{} })
		}, "diff_ids"},
		{"diff ID algorithm", func(t *testing.T, e []archiveTestEntry) []archiveTestEntry {
			return archiveTestChangeConfig(t, e, func(c map[string]any) {
				c["rootfs"].(map[string]any)["diff_ids"] = []string{"sha512:bad", "sha256:bad"}
			})
		}, "digest"},
		{"layer order", func(t *testing.T, e []archiveTestEntry) []archiveTestEntry {
			return archiveTestChangeManifest(t, e, func(m []dockerSaveManifestEntry) { m[0].Layers[0], m[0].Layers[1] = m[0].Layers[1], m[0].Layers[0] })
		}, "digest"},
		{"config traversal reference", func(t *testing.T, e []archiveTestEntry) []archiveTestEntry {
			return archiveTestChangeManifest(t, e, func(m []dockerSaveManifestEntry) { m[0].Config = "../config.json" })
		}, "traversal"},
		{"layer traversal reference", func(t *testing.T, e []archiveTestEntry) []archiveTestEntry {
			return archiveTestChangeManifest(t, e, func(m []dockerSaveManifestEntry) { m[0].Layers[0] = "first/../first/layer.tar" })
		}, "traversal"},
		{"config is directory", func(t *testing.T, e []archiveTestEntry) []archiveTestEntry {
			e[0] = archiveTestEntry{header: tar.Header{Name: e[0].header.Name, Typeflag: tar.TypeDir}}
			return e
		}, "config"},
		{"layer is directory", func(t *testing.T, e []archiveTestEntry) []archiveTestEntry {
			e[2] = archiveTestEntry{header: tar.Header{Name: e[2].header.Name, Typeflag: tar.TypeDir}}
			return e
		}, "layer"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			entries := test.change(t, archiveTestDockerEntries(t))
			err := validateDockerArchive(archiveTestWriteTar(t, entries, false), []string{"example.test/eva:1"})
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want containing %q", err, test.want)
			}
		})
	}
}
func TestValidateDockerArchiveRejectsUnsafeEntries(t *testing.T) {
	badEntries := []archiveTestEntry{
		archiveTestRegular("../escape", nil),
		archiveTestRegular("folder/../escape", nil),
		archiveTestRegular("/absolute", nil),
		archiveTestRegular("folder\\escape", nil),
		archiveTestRegular("C:/escape", nil),
		archiveTestRegular("manifest.json", []byte("[]")),
		archiveTestRegular("./manifest.json", []byte("[]")),
		archiveTestRegular("first", nil),
		{header: tar.Header{Name: "link", Typeflag: tar.TypeSymlink, Linkname: "manifest.json"}},
		{header: tar.Header{Name: "link", Typeflag: tar.TypeLink, Linkname: "manifest.json"}},
		{header: tar.Header{Name: "pipe", Typeflag: tar.TypeFifo}},
		{header: tar.Header{Name: "device", Typeflag: tar.TypeBlock}},
	}
	for _, bad := range badEntries {
		t.Run(bad.header.Name+"/"+string(bad.header.Typeflag), func(t *testing.T) {
			// Unsafe entries after the complete manifest must still be checked.
			entries := append(archiveTestDockerEntries(t), bad)
			if err := validateDockerArchive(archiveTestWriteTar(t, entries, false), nil); err == nil {
				t.Fatal("unsafe docker-save member was accepted")
			}
		})
	}
}

func TestValidateDockerArchiveConsumesWholeFile(t *testing.T) {
	for _, corruption := range []string{"truncated member", "trailing data", "tar checksum"} {
		t.Run(corruption, func(t *testing.T) {
			entries := archiveTestDockerEntries(t)
			entries = append(entries, archiveTestRegular("last-file", bytes.Repeat([]byte("x"), 2048)))
			body := archiveTestTarBytes(t, entries)
			switch corruption {
			case "truncated member":
				body = body[:len(body)-1024-1024]
			case "trailing data":
				body = append(body, []byte("hidden payload")...)
			case "tar checksum":
				body[0] ^= 1
			}
			archive := filepath.Join(t.TempDir(), "docker.tar")
			archiveTestWriteFile(t, archive, body)
			if err := validateDockerArchive(archive, []string{"example.test/eva:1"}); err == nil {
				t.Fatal("corrupt docker-save archive was accepted")
			}
		})
	}
}

func TestValidateDockerArchiveJSONLimits(t *testing.T) {
	for _, name := range []string{"manifest.json", "config.json"} {
		t.Run(name, func(t *testing.T) {
			body := bytes.Repeat([]byte(" "), archiveJSONLimit+1)
			entries := archiveTestDockerEntries(t)
			if name == "manifest.json" {
				entries[len(entries)-1] = archiveTestRegular(name, body)
			} else {
				entries[0] = archiveTestRegular(name, body)
				entries = archiveTestChangeManifest(t, entries, func(m []dockerSaveManifestEntry) { m[0].Config = name })
			}
			err := validateDockerArchive(archiveTestWriteTar(t, entries, false), nil)
			if err == nil || !strings.Contains(err.Error(), map[string]string{"manifest.json": "16 MiB", "config.json": "oversized"}[name]) {
				t.Fatalf("oversized JSON error = %v", err)
			}
		})
	}
}

func TestBuildOfflineArchiveRelativeCacheRoot(t *testing.T) {
	root := t.TempDir()
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	relative, err := filepath.Rel(cwd, root)
	if err != nil {
		t.Fatal(err)
	}
	archiveTestWriteFile(t, filepath.Join(root, "models/manifest.txt"), []byte("files:\n"+relative+"/models/weight.bin\n"+root+"/models/weight.bin\n"))
	archiveTestWriteFile(t, filepath.Join(root, "models/weight.bin"), []byte("weights"))
	destination := filepath.Join(t.TempDir(), "offline.tar.gz")
	if err := buildOfflineArchive(archiveTestRuntime(t), relative+"/", destination); err != nil {
		t.Fatal(err)
	}
	manifest := archiveTestReadOffline(t, destination)["out/cache/models/manifest.txt"].body
	if string(manifest) != "files:\nout/cache/models/weight.bin\nout/cache/models/weight.bin\n" {
		t.Fatalf("relative-root manifest is not portable: %q", manifest)
	}
}
